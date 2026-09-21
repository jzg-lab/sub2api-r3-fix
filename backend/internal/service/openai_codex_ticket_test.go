package service

import (
	"context"
	"encoding/base64"
	"net/http"
	"testing"
	"time"
)

// buildCodexTicketValue 构造 Fernet 形态测试票：0x80 版本 + 8B 大端签发秒
// + 填充到健康票长度（292/332 由 padLen 决定，base64url 无填充字符数精确）。
func buildCodexTicketValue(issuedAt time.Time, padLen int) string {
	raw := make([]byte, 0, 64)
	raw = append(raw, 0x80)
	seconds := uint64(issuedAt.Unix())
	for shift := uint(56); ; shift -= 8 {
		raw = append(raw, byte(seconds>>shift))
		if shift == 0 {
			break
		}
	}
	// IV+密文+HMAC 填充：长度由调用方钉死（模拟健康/降级两态的字符长度）。
	targetBytes := padLen * 3 / 4 // base64 近似字节数
	for len(raw) < targetBytes {
		raw = append(raw, byte(len(raw)%251+1))
	}
	return base64.URLEncoding.EncodeToString(raw)[:padLen]
}

func TestParseCodexTicketIssuedAt(t *testing.T) {
	issued := time.Date(2026, 9, 21, 3, 30, 0, 0, time.UTC)
	value := buildCodexTicketValue(issued, 332)
	parsed, err := ParseCodexTicketIssuedAt(value)
	if err != nil {
		t.Fatalf("healthy 332 ticket must parse: %v", err)
	}
	if !parsed.Equal(issued) {
		t.Fatalf("issued_at mismatch: got %v want %v", parsed, issued)
	}
	// 空/短/坏版本。
	if _, err := ParseCodexTicketIssuedAt(""); err == nil {
		t.Fatal("empty must fail")
	}
	if _, err := ParseCodexTicketIssuedAt("gAAAAA"); err == nil {
		t.Fatal("short must fail")
	}
	bad := []byte{0x00, 1, 2, 3, 4, 5, 6, 7, 8}
	if _, err := ParseCodexTicketIssuedAt(base64.URLEncoding.EncodeToString(bad)); err == nil {
		t.Fatal("non-0x80 version must fail")
	}
}

// TestIsCodexTicketHarvestableLength 白名单精确两口令（用户 2026-09-21 裁定
// 「打票292/332是这两个哦别漏」）：292/332 及 ±4 边界容差收；312/356 及
// 其它长度一律拒。
func TestIsCodexTicketHarvestableLength(t *testing.T) {
	for _, ok := range []int{292, 332, 294, 330} {
		if !IsCodexTicketHarvestableLength(ok) {
			t.Fatalf("len=%d must be harvestable", ok)
		}
	}
	for _, bad := range []int{0, 100, 289, 295, 312, 329, 335, 356, 400} {
		if IsCodexTicketHarvestableLength(bad) {
			t.Fatalf("len=%d must be rejected", bad)
		}
	}
}

type codexTicketStoreStub struct {
	tickets map[[2]string]*OpenAICodexTicket
}

func (s *codexTicketStoreStub) UpsertOpenAICodexTicket(_ context.Context, ticket *OpenAICodexTicket) error {
	if s.tickets == nil {
		s.tickets = make(map[[2]string]*OpenAICodexTicket)
	}
	key := [2]string{int64ToString(ticket.AccountID), ticket.Model}
	existing := s.tickets[key]
	if existing == nil || !ticket.IssuedAt.Before(existing.IssuedAt) {
		s.tickets[key] = ticket
	}
	return nil
}

func (s *codexTicketStoreStub) GetOpenAICodexTicket(_ context.Context, accountID int64, model string) (*OpenAICodexTicket, error) {
	if ticket, ok := s.tickets[[2]string{int64ToString(accountID), model}]; ok {
		return ticket, nil
	}
	return nil, nil
}

func (s *codexTicketStoreStub) DeleteExpiredOpenAICodexTickets(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}

// TestHarvestOpenAICodexTicket 采集验收：健康长度+Fernet 时刻可解才入库；
// 降级长度(356)拒收；坏票值拒收且不上抛。
func TestHarvestOpenAICodexTicket(t *testing.T) {
	store := &codexTicketStoreStub{}
	now := time.Date(2026, 9, 21, 4, 0, 0, 0, time.UTC)
	proxyID := int64(7)

	healthy := buildCodexTicketValue(now.Add(-time.Minute), 332)
	HarvestOpenAICodexTicket(context.Background(), store, 1115, &proxyID, "gpt-6-astra", healthy, OpenAICodexTicketHarvestProbe, now)
	ticket := store.tickets[[2]string{"1115", "gpt-6-astra"}]
	if ticket == nil {
		t.Fatal("healthy 332 must be harvested")
	}
	if ticket.ExitFingerprint != "proxy:7" {
		t.Fatalf("exit fingerprint must bind proxy:7, got %s", ticket.ExitFingerprint)
	}

	degraded := buildCodexTicketValue(now.Add(-time.Minute), 356)
	HarvestOpenAICodexTicket(context.Background(), store, 1116, &proxyID, "gpt-6-astra", degraded, OpenAICodexTicketHarvestProbe, now)
	if store.tickets[[2]string{"1116", "gpt-6-astra"}] != nil {
		t.Fatal("degraded 356 must be rejected")
	}

	HarvestOpenAICodexTicket(context.Background(), store, 1117, &proxyID, "gpt-6-astra", "garbage!!", OpenAICodexTicketHarvestProbe, now)
	if store.tickets[[2]string{"1117", "gpt-6-astra"}] != nil {
		t.Fatal("malformed value must be rejected")
	}
}

// TestMaybeInjectOpenAICodexTicket 注入保守语义（sx120609 口径）：
// 1) 客户端未回带 → 不注入；2) 已回带+活票 → 替换；3) 出口指纹不符 → 不替换；
// 4) 票过期（签发+TTL-提前量）→ 不替换（FailOpen 原样）。
func TestMaybeInjectOpenAICodexTicket(t *testing.T) {
	now := time.Date(2026, 9, 21, 4, 0, 0, 0, time.UTC)
	proxyID := int64(7)
	account := &Account{ID: 1115, ProxyID: &proxyID}
	store := &codexTicketStoreStub{}
	live := buildCodexTicketValue(now.Add(-30*time.Minute), 332)
	if err := store.UpsertOpenAICodexTicket(context.Background(), &OpenAICodexTicket{
		AccountID: 1115, Model: "gpt-6-astra", TicketValue: live, TicketLen: 332,
		IssuedAt: now.Add(-30 * time.Minute), ExpiresAt: now.Add(30 * time.Minute),
		ExitFingerprint: "proxy:7", HarvestedMode: OpenAICodexTicketHarvestProbe,
	}); err != nil {
		t.Fatal(err)
	}

	// 1) 未回带：不注入。
	h := http.Header{}
	maybeInjectOpenAICodexTicket(context.Background(), store, account, "gpt-6-astra", h, now)
	if h.Get(openAICodexTurnStateHeader) != "" {
		t.Fatal("must not inject when client did not echo the header")
	}

	// 2) 已回带旧值：替换为活票。
	h = http.Header{}
	h.Set(openAICodexTurnStateHeader, "client-echoed-old-value")
	maybeInjectOpenAICodexTicket(context.Background(), store, account, "gpt-6-astra", h, now)
	if h.Get(openAICodexTurnStateHeader) != live {
		t.Fatal("echoed stale value must be replaced with live ticket")
	}

	// 3) 出口指纹不符（票绑 proxy:7，账号换到 proxy:9）：不替换。
	otherProxy := int64(9)
	movedAccount := &Account{ID: 1115, ProxyID: &otherProxy}
	h = http.Header{}
	h.Set(openAICodexTurnStateHeader, "client-echoed-old-value")
	maybeInjectOpenAICodexTicket(context.Background(), store, movedAccount, "gpt-6-astra", h, now)
	if h.Get(openAICodexTurnStateHeader) != "client-echoed-old-value" {
		t.Fatal("exit fingerprint mismatch must keep the echoed value (fail-open)")
	}

	// 4) 票临近过期（签发+50min > TTL-10min 窗）：不替换。
	stale := buildCodexTicketValue(now.Add(-52*time.Minute), 332)
	staleStore := &codexTicketStoreStub{}
	if err := staleStore.UpsertOpenAICodexTicket(context.Background(), &OpenAICodexTicket{
		AccountID: 1115, Model: "gpt-6-astra", TicketValue: stale, TicketLen: 332,
		IssuedAt: now.Add(-52 * time.Minute), ExpiresAt: now.Add(8 * time.Minute),
		ExitFingerprint: "proxy:7", HarvestedMode: OpenAICodexTicketHarvestProbe,
	}); err != nil {
		t.Fatal(err)
	}
	h = http.Header{}
	h.Set(openAICodexTurnStateHeader, "client-echoed-old-value")
	maybeInjectOpenAICodexTicket(context.Background(), staleStore, account, "gpt-6-astra", h, now)
	if h.Get(openAICodexTurnStateHeader) != "client-echoed-old-value" {
		t.Fatal("near-expiry ticket must not be injected")
	}
}

// r17t：动态桶采的票跨出口放行（换票主线：动态采→静态用）。
// 账号+模型+时效闸保持；仅出口指纹匹配对 dynamic 票豁免。
func TestCodexTicketUsableDynamicCrossExit(t *testing.T) {
	now := time.Date(2026, 9, 21, 7, 0, 0, 0, time.UTC)
	dyn := &OpenAICodexTicket{
		TicketLen:       332,
		IssuedAt:        now.Add(-time.Minute),
		ExitFingerprint: "proxy:11",
		HarvestedMode:   OpenAICodexTicketHarvestDynamic,
	}
	// 静态出口 proxy:6 ≠ 采集出口 proxy:11 → dynamic 票放行。
	if !codexTicketUsable(dyn, now, "proxy:6") {
		t.Fatal("dynamic ticket must be usable on a different exit")
	}
	// 时效闸仍然生效（过期前 600s 刷新窗口外拒用）。
	stale := *dyn
	stale.IssuedAt = now.Add(-openAICodexTicketTTL)
	if codexTicketUsable(&stale, now, "proxy:6") {
		t.Fatal("expired dynamic ticket must be rejected")
	}
	// 长度闸仍然生效（356 降级票拒用）。
	degraded := *dyn
	degraded.TicketLen = 356
	if codexTicketUsable(&degraded, now, "proxy:6") {
		t.Fatal("degraded-length dynamic ticket must be rejected")
	}
	// probe 票（静态桶采）仍受出口指纹闸约束。
	prb := &OpenAICodexTicket{
		TicketLen:       332,
		IssuedAt:        now.Add(-time.Minute),
		ExitFingerprint: "proxy:6",
		HarvestedMode:   OpenAICodexTicketHarvestProbe,
	}
	if !codexTicketUsable(prb, now, "proxy:6") {
		t.Fatal("same-exit probe ticket must be usable")
	}
	if codexTicketUsable(prb, now, "proxy:9") {
		t.Fatal("cross-exit probe ticket must be rejected")
	}
}

// 桶名约定判动态桶：novproxy-dynamic-residential 命中，静态桶不命中。
func TestIsOpenAIDynamicProxyBucket(t *testing.T) {
	if !isOpenAIDynamicProxyBucket(&Proxy{Name: "novproxy-dynamic-residential"}) {
		t.Fatal("dynamic bucket name must match")
	}
	if isOpenAIDynamicProxyBucket(&Proxy{Name: "att-12.235.50.4"}) {
		t.Fatal("static bucket name must not match")
	}
	if isOpenAIDynamicProxyBucket(nil) {
		t.Fatal("nil proxy must not match")
	}
}
