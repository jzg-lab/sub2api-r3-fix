package service

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// =============================================================================
// x-codex-turn-state 票据（换票打票体系相位B，2026-09-21）
//
// 票 = 上游响应头原值（Fernet token）。密码学定案（upstream-ticket-reference
// /cpa-plugin/FINDINGS.md + 我们 9/18-9/20 实测）：
//   - 结构：base64url(0x80版本 + 8B大端Unix秒签发时刻 + 16B IV + AES-CBC密文
//     + 32B HMAC)；签发时刻无钥可读——TTL 判据用内嵌时间戳，绝不用捕获时刻
//     （捕获晚于铸造，按捕获算会把旧票当新票）。
//   - TTL 1h 签进 token（过期上游直接拒），提前 600s 刷新 → 可用窗=签发后
//     3000s。
//   - 长度双态：健康 292(社区)/332(我们实测)，降级 312/356（多一个 AES 块）。
//     采集验收=长度白名单 {292, 332}；降级长度不入库。
//   - 复用规则：跨账号✗ / 跨模型✗ / 同账号+同模型跨IP✓（社区 Pro 玩法前提）。
//
// 采集载体 = 现有探针针次顺带（响应头本带 x-codex-turn-state，零新增流量
// 形态——绝不抄上游 pong 词面针，那是官方自删的最易聚类指纹）。
//
// 注入纪律（sx120609 保守版 + 我们被动中继哲学）：
//   - 仅当客户端已回带该头时替换（首请求不携带不注入）；
//   - 出口指纹校验：票绑定的业务出口与当前账号出口一致才替换；
//   - FailOpen：无票/票过期/长度不识 → 原样透传，绝不因票体系阻断业务。
//
// ticket_value 是敏感凭据：不进日志、不进遥测、导出脱敏。
// =============================================================================

const (
	// openAICodexTicketTTL Fernet 签发后 1 小时过期（签进 token）。
	openAICodexTicketTTL = time.Hour
	// openAICodexTicketRefreshMargin 提前 600s 刷新（上游工程惯例）。
	openAICodexTicketRefreshMargin = 10 * time.Minute
	// openAICodexTicketLenCommunity / LenOurs 长度白名单（精确两口令）：
	// 292=社区实测健康态，332=我们 9/18-9/20 全健康号实测。312/356 降级态
	// 不在白名单内（多一个 AES 块），天然拒收。
	openAICodexTicketLenCommunity = 292
	openAICodexTicketLenOurs      = 332
	// OpenAICodexTicketHarvestProbe 探针顺带采集。
	OpenAICodexTicketHarvestProbe = "probe"
	// OpenAICodexTicketHarvestDynamic 动态IP采集（分型器口径）。
	OpenAICodexTicketHarvestDynamic = "dynamic"
	// OpenAICodexTicketHarvestRelay 真实流量响应钩子采集。
	OpenAICodexTicketHarvestRelay = "relay"
)

var (
	errOpenAICodexTicketMalformed = errors.New("codex ticket malformed")
	errOpenAICodexTicketBadVersion = errors.New("codex ticket version byte mismatch")
)

// OpenAICodexTicket 票行（repo ↔ service 契约）。TicketValue 敏感：
// 只在采集/注入两端流动，绝不经手日志。
type OpenAICodexTicket struct {
	AccountID       int64
	Model           string
	TicketValue     string
	TicketLen       int
	IssuedAt        time.Time
	ExpiresAt       time.Time
	ExitFingerprint string
	HarvestedMode   string
	UpdatedAt       time.Time
}

// OpenAICodexTicketStore 窄可选能力（与 HistoryCleaner 同模式）：只有真实
// repository 实现，探针 runner 类型断言取得，测试桩不受影响。
type OpenAICodexTicketStore interface {
	UpsertOpenAICodexTicket(ctx context.Context, ticket *OpenAICodexTicket) error
	GetOpenAICodexTicket(ctx context.Context, accountID int64, model string) (*OpenAICodexTicket, error)
	DeleteExpiredOpenAICodexTickets(ctx context.Context, now time.Time) (int64, error)
}

// ParseCodexTicketIssuedAt 无钥解析 Fernet 内嵌签发时刻：
// base64url 解码 → 验 0x80 版本字节 → 取 raw[1:9] 大端 Unix 秒。
// 长度不足 57 字节（1+8+16+32 下界）视为损坏。
func ParseCodexTicketIssuedAt(value string) (time.Time, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return time.Time{}, errOpenAICodexTicketMalformed
	}
	raw, err := base64.URLEncoding.DecodeString(padBase64URL(trimmed))
	if err != nil {
		return time.Time{}, errOpenAICodexTicketMalformed
	}
	if len(raw) < 57 {
		return time.Time{}, errOpenAICodexTicketMalformed
	}
	if raw[0] != 0x80 {
		return time.Time{}, errOpenAICodexTicketBadVersion
	}
	var seconds uint64
	for _, b := range raw[1:9] {
		seconds = seconds<<8 | uint64(b)
	}
	if seconds == 0 || seconds > 1<<53 {
		return time.Time{}, errOpenAICodexTicketMalformed
	}
	return time.Unix(int64(seconds), 0).UTC(), nil
}

func padBase64URL(s string) string {
	if r := len(s) % 4; r != 0 {
		return s + strings.Repeat("=", 4-r)
	}
	return s
}

// IsCodexTicketHarvestableLength 采集验收：精确白名单 {292, 332} 两口径
//（292=社区实测，332=我们 9/18-9/20 全健康号实测）——用户 2026-09-21 裁定
//「打票292/332是这两个哦别漏」。312/356 降级态天然出界。±2 容差仅吸收
// base64 尾部填充字符差异（同头差一个 AES 块=20-24 字符，2 容差不可能跨态）。
func IsCodexTicketHarvestableLength(length int) bool {
	return (length >= openAICodexTicketLenCommunity-2 && length <= openAICodexTicketLenCommunity+2) ||
		(length >= openAICodexTicketLenOurs-2 && length <= openAICodexTicketLenOurs+2)
}

// codexTicketUsableWindow 票可用性三重判据：长度白名单 + 内嵌签发时刻新鲜
// （签发+TTL-提前量 > now）+ 出口指纹一致。
func codexTicketUsable(ticket *OpenAICodexTicket, now time.Time, exitFingerprint string) bool {
	if ticket == nil {
		return false
	}
	if !IsCodexTicketHarvestableLength(ticket.TicketLen) {
		return false
	}
	if now.After(ticket.IssuedAt.Add(openAICodexTicketTTL - openAICodexTicketRefreshMargin)) {
		return false
	}
	// 动态桶采的票跨出口放行（换票主线：动态采→静态用）。社区实证+我们
	// 2026-09-21 分型（IP 级降智号在动态 IP 上 332）= 票成色由铸造时 IP
	// 语境决定，换 IP 仍有效。账号+模型+时效闸保持不变。
	if ticket.HarvestedMode == OpenAICodexTicketHarvestDynamic {
		return true
	}
	if exitFingerprint != "" && ticket.ExitFingerprint != exitFingerprint {
		return false
	}
	return true
}

// HarvestOpenAICodexTicket 探针顺带采票入口（零新增流量）：value 非空、
// 长度过白名单、Fernet 签发时刻可解 → upsert 票表。任何失败只记日志，
// 绝不影响探针主判定。
func HarvestOpenAICodexTicket(
	ctx context.Context,
	store OpenAICodexTicketStore,
	accountID int64,
	proxyID *int64,
	model string,
	value string,
	harvestedMode string,
	now time.Time,
) {
	if store == nil || accountID <= 0 || model == "" {
		return
	}
	if harvestedMode == "" {
		harvestedMode = OpenAICodexTicketHarvestProbe
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	if !IsCodexTicketHarvestableLength(len(value)) {
		return
	}
	issuedAt, err := ParseCodexTicketIssuedAt(value)
	if err != nil {
		// 只记元数据（长度+错误类），不记票值。
		slog.Debug("openai_codex_ticket_parse_failed",
			"account_id", accountID, "len", len(value), "error", err.Error())
		return
	}
	exitFingerprint := "unbound"
	if proxyID != nil {
		exitFingerprint = "proxy:" + int64ToString(*proxyID)
	}
	ticket := &OpenAICodexTicket{
		AccountID:       accountID,
		Model:           model,
		TicketValue:     value,
		TicketLen:       len(value),
		IssuedAt:        issuedAt,
		ExpiresAt:       issuedAt.Add(openAICodexTicketTTL),
		ExitFingerprint: exitFingerprint,
		HarvestedMode:   harvestedMode,
		UpdatedAt:       now,
	}
	if err := store.UpsertOpenAICodexTicket(ctx, ticket); err != nil {
		slog.Warn("openai_codex_ticket_upsert_failed",
			"account_id", accountID, "model", model, "error", err.Error())
		return
	}
	slog.Info("openai_codex_ticket_harvested",
		"account_id", accountID, "model", model,
		"len", len(value), "issued_at", issuedAt.Format(time.RFC3339),
		"mode", OpenAICodexTicketHarvestProbe)
}

func int64ToString(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// GetInjectableOpenAICodexTicket 注入查询：账号+模型+当前业务出口三重匹配，
// 票不可用（过期/长度/指纹不符）返回 nil（调用方 FailOpen 原样透传）。
func GetInjectableOpenAICodexTicket(
	ctx context.Context,
	store OpenAICodexTicketStore,
	accountID int64,
	model string,
	proxyID *int64,
	now time.Time,
) *OpenAICodexTicket {
	if store == nil || accountID <= 0 || model == "" {
		return nil
	}
	ticket, err := store.GetOpenAICodexTicket(ctx, accountID, model)
	if err != nil || ticket == nil {
		return nil
	}
	exitFingerprint := ""
	if proxyID != nil {
		exitFingerprint = "proxy:" + int64ToString(*proxyID)
	}
	if !codexTicketUsable(ticket, now, exitFingerprint) {
		return nil
	}
	return ticket
}

// OpenAICodexTicketStatusView 票状态脱敏视图（管理端查询专用）：
// 绝不含 TicketValue（敏感凭据不进 API 响应/日志/导出）。
type OpenAICodexTicketStatusView struct {
	AccountID       int64      `json:"account_id"`
	Model           string     `json:"model"`
	TicketLen       int        `json:"ticket_len"`
	IssuedAt        time.Time  `json:"issued_at"`
	ExpiresAt       time.Time  `json:"expires_at"`
	ExitFingerprint string     `json:"exit_fingerprint"`
	HarvestedMode   string     `json:"harvested_mode"`
	Usable          bool       `json:"usable"`
	HarvestableLen  bool       `json:"harvestable_len"`
}

// ListOpenAICodexTicketStatus 票状态批量脱敏查询（问题号点击后的状态回显）。
// model 维度取主档 gpt-6-astra（sol 线账号另有行，逐模型返回）。
func ListOpenAICodexTicketStatus(
	ctx context.Context,
	store OpenAICodexTicketStore,
	accountIDs []int64,
	now time.Time,
) ([]OpenAICodexTicketStatusView, error) {
	if store == nil || len(accountIDs) == 0 {
		return nil, nil
	}
	lister, ok := store.(interface {
		ListOpenAICodexTicketsByAccounts(ctx context.Context, accountIDs []int64) ([]OpenAICodexTicket, error)
	})
	if !ok {
		return nil, nil
	}
	tickets, err := lister.ListOpenAICodexTicketsByAccounts(ctx, accountIDs)
	if err != nil {
		return nil, err
	}
	out := make([]OpenAICodexTicketStatusView, 0, len(tickets))
	for i := range tickets {
		t := tickets[i]
		out = append(out, OpenAICodexTicketStatusView{
			AccountID:       t.AccountID,
			Model:           t.Model,
			TicketLen:       t.TicketLen,
			IssuedAt:        t.IssuedAt,
			ExpiresAt:       t.ExpiresAt,
			ExitFingerprint: t.ExitFingerprint,
			HarvestedMode:   t.HarvestedMode,
			Usable:          codexTicketUsable(&t, now, t.ExitFingerprint),
			HarvestableLen:  IsCodexTicketHarvestableLength(t.TicketLen),
		})
	}
	return out, nil
}

// maybeInjectOpenAICodexTicket 保守注入（相位B）：仅当客户端已回带
// x-codex-turn-state（sx120609 保守版：首请求不携带不注入）且票可用时，
// 用桶内活票替换客户端回带值。替换语义=「同账号同模型的票换新」，与
// guardOpenAICodexTurnStateEcho 的跨账号剥离互补：guard 先剥异账号值，
// 此处再补本账号活票。任何不满足都原样放行（FailOpen，绝不阻断业务）。
// 只在出站头构建临界区调用；查询失败静默降级为不注入。
func maybeInjectOpenAICodexTicket(
	ctx context.Context,
	store OpenAICodexTicketStore,
	account *Account,
	model string,
	h http.Header,
	now time.Time,
) {
	if store == nil || h == nil || account == nil || account.ID <= 0 {
		return
	}
	echoed := strings.TrimSpace(h.Get(openAICodexTurnStateHeader))
	if echoed == "" {
		// 客户端未回带：不注入（保守策略——真实 Codex 首请求本来就不带）。
		return
	}
	ticket := GetInjectableOpenAICodexTicket(ctx, store, account.ID, model, account.ProxyID, now)
	if ticket == nil || ticket.TicketValue == echoed {
		return
	}
	h.Set(openAICodexTurnStateHeader, ticket.TicketValue)
	slog.Debug("openai_codex_ticket_injected",
		"account_id", account.ID, "model", model, "len", ticket.TicketLen)
}
