package cookiestore

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/liyunlong/sub2api-cookie-plugin/internal/pluginconfig"
)

func TestCaptureWhitelistAndSessionTTL(t *testing.T) {
	store := New()
	now := time.Now()
	updated := store.Capture(1210, []string{
		"__cflb=abc123; Path=/",           // 会话 Cookie → 兜底 TTL
		"__oailb=xyz; Path=/; Max-Age=60", // 显式 Max-Age
		"__cf_bm=junk; Path=/",            // 非白名单，忽略
		"other=1; Path=/",                 // 非白名单，忽略
	}, now)
	if len(updated) != 2 {
		t.Fatalf("应只吸收白名单 2 个，得到 %v", updated)
	}
	merged := store.MergeHeader(1210, "", now)
	if !strings.Contains(merged, "__cflb=abc123") || !strings.Contains(merged, "__oailb=xyz") {
		t.Fatalf("注入内容异常: %q", merged)
	}
	// Max-Age=60 的条目 60s 后仍在，会话条目走兜底 TTL 也仍在。
	if got := store.MergeHeader(1210, "", now.Add(50*time.Second)); got == "" {
		t.Fatal("50s 后两枚 Cookie 仍应新鲜")
	}
}

func TestCaptureDeleteDirective(t *testing.T) {
	store := New()
	now := time.Now()
	store.Capture(1210, []string{"__cflb=abc; Path=/"}, now)
	// 空值 = 删除指令。
	store.Capture(1210, []string{"__cflb=; Path=/"}, now)
	if got := store.MergeHeader(1210, "", now); got != "" {
		t.Fatalf("删除指令后罐应空，得到 %q", got)
	}
	// Max-Age=0 同理。
	store.Capture(1210, []string{"__cflb=abc; Path=/"}, now)
	store.Capture(1210, []string{"__cflb=abc; Max-Age=0"}, now)
	if got := store.MergeHeader(1210, "", now); got != "" {
		t.Fatalf("Max-Age=0 后罐应空，得到 %q", got)
	}
}

func TestExpiryHonorsRefreshHorizon(t *testing.T) {
	store := New()
	now := time.Now()
	store.Capture(1210, []string{"__cflb=abc; Max-Age=60"}, now)
	// 剩余寿命 60-40=20s < refresh_before(默认30s) → 视为陈旧，注入时清理。
	if got := store.MergeHeader(1210, "", now.Add(40*time.Second)); got != "" {
		t.Fatalf("低于刷新窗口不应注入: %q", got)
	}
	// 再取：罐应已清空。
	if got := store.MergeHeader(1210, "", now.Add(41*time.Second)); got != "" {
		t.Fatalf("陈旧条目应被顺带清理: %q", got)
	}
}

func TestMergePreservesUnrelatedCookies(t *testing.T) {
	store := New()
	now := time.Now()
	store.Capture(1210, []string{"__cflb=fresh; Path=/"}, now)
	got := store.MergeHeader(1210, "session_tok=keep; __cflb=stale; other=2", now)
	if !strings.Contains(got, "session_tok=keep") || !strings.Contains(got, "other=2") {
		t.Fatalf("无关 Cookie 必须保留: %q", got)
	}
	if !strings.Contains(got, "__cflb=fresh") || strings.Contains(got, "__cflb=stale") {
		t.Fatalf("同名应以罐内新值为准: %q", got)
	}
}

func TestAccountIsolation(t *testing.T) {
	store := New()
	now := time.Now()
	store.Capture(1209, []string{"__cflb=a1209; Path=/"}, now)
	store.Capture(1210, []string{"__cflb=a1210; Path=/"}, now)
	if got := store.MergeHeader(1209, "", now); !strings.Contains(got, "a1209") || strings.Contains(got, "a1210") {
		t.Fatalf("账号 1209 不应拿到 1210 的 Cookie: %q", got)
	}
	store.Drop(1209)
	if got := store.MergeHeader(1209, "", now); got != "" {
		t.Fatalf("Drop 后应空: %q", got)
	}
	if got := store.MergeHeader(1210, "", now); !strings.Contains(got, "a1210") {
		t.Fatalf("Drop 1209 不应影响 1210: %q", got)
	}
}

func TestRerollOnlyWhenNonEmpty(t *testing.T) {
	store := New()
	store.Reroll(1210) // 空罐重摇不计次
	if s := store.Status(time.Now()); s.Rerolls != 0 {
		t.Fatalf("空罐重摇不应计数: %d", s.Rerolls)
	}
	now := time.Now()
	store.Capture(1210, []string{"__cflb=abc; Path=/"}, now)
	store.Reroll(1210)
	if s := store.Status(time.Now()); s.Rerolls != 1 {
		t.Fatalf("应计 1 次重摇: %d", s.Rerolls)
	}
}

func TestDisabledStoreIsInert(t *testing.T) {
	store := New()
	cfg, _, _ := parseHelper(`{"enabled":false}`)
	store.SetConfig(cfg.Sanitized())
	now := time.Now()
	store.Capture(1210, []string{"__cflb=abc; Path=/"}, now)
	if got := store.MergeHeader(1210, "", now); got != "" {
		t.Fatalf("关闭态不得注入: %q", got)
	}
	if s := store.Status(now); s.Captures != 0 || s.Enabled {
		t.Fatalf("关闭态不应捕获: %+v", s)
	}
}

func TestSnapshotRestoreRoundtrip(t *testing.T) {
	store := New()
	now := time.Now()
	store.Capture(1210, []string{"__cflb=abc; Max-Age=3600"}, now)
	raw := store.SnapshotJSON()
	if raw == nil {
		t.Fatal("快照为空")
	}
	// 快照含值但 Status 不含值（脱敏边界）。
	statusRaw, _ := json.Marshal(store.Status(now))
	if strings.Contains(string(statusRaw), "abc") {
		t.Fatal("Status 不得泄漏 Cookie 值")
	}
	restored := New()
	restored.RestoreJSON(raw, now.Add(time.Minute))
	if got := restored.MergeHeader(1210, "", now.Add(time.Minute)); !strings.Contains(got, "__cflb=abc") {
		t.Fatalf("恢复后应可注入: %q", got)
	}
	// 过期快照条目不得恢复。
	dead := New()
	dead.RestoreJSON(raw, now.Add(2*time.Hour))
	if got := dead.MergeHeader(1210, "", now.Add(2*time.Hour)); got != "" {
		t.Fatalf("过期条目不得恢复: %q", got)
	}
}

func TestSnapshotNeverTouchesLogs(t *testing.T) {
	store := New()
	now := time.Now()
	store.Capture(1210, []string{"__cflb=secretvalue; Max-Age=60"}, now)
	snapshot := string(store.SnapshotJSON())
	if !strings.Contains(snapshot, "secretvalue") {
		t.Fatal("快照应含值（进宿主 KV 的持久化形态）")
	}
	status := store.Status(now)
	// Status 的所有字段都不得携带值。
	if strings.Contains(status.Accounts[0].Names, "secretvalue") {
		t.Fatal("Status 泄漏 Cookie 值")
	}
}

// parseHelper 构造关闭态配置。
func parseHelper(raw string) (pluginconfig.Config, string, bool) {
	return pluginconfig.Parse([]byte(raw))
}

// ---------- v0.3 签寿命计量 ----------

func TestSignMeterOverwriteAndExpiry(t *testing.T) {
	store := New()
	now := time.Now()
	store.Capture(1210, []string{"__cflb=v1; Max-Age=3600"}, now)
	// 覆写换签：旧签寿命 = 100s。
	store.Capture(1210, []string{"__cflb=v2; Max-Age=3600"}, now.Add(100*time.Second))
	info := store.SignInfo(1210, now.Add(100*time.Second))
	if info.Stats.Samples != 1 || int(info.Stats.P50) != 100 {
		t.Fatalf("覆写应归档 100s 寿命: %+v", info.Stats)
	}
	// TTL 到期清理：第二签寿命 = 60s（Max-Age=60 的另一条）。
	store.Capture(1210, []string{"__oailb=o1; Max-Age=60"}, now.Add(100*time.Second))
	store.MergeHeader(1210, "", now.Add(160*time.Second)) // __oailb 过期触发清理
	info = store.SignInfo(1210, now.Add(160*time.Second))
	if info.Stats.Samples != 2 {
		t.Fatalf("到期清理应归档第二条寿命: %+v", info.Stats)
	}
}

func TestSignMeterDropAndReroll(t *testing.T) {
	store := New()
	// Drop/Reroll 用真实墙钟计量（调用即死亡时刻），捕获时刻须在过去侧，
	// 否则被时钟跳变守卫正确拦下。
	now := time.Now()
	store.Capture(1210, []string{"__cflb=drop; Max-Age=3600"}, now.Add(-80*time.Second))
	store.Drop(1210) // 探针答错重摇路径
	if info := store.SignInfo(1210, now); info.Stats.Samples != 1 || info.Stats.P50 < 79 {
		t.Fatalf("Drop 应归档 ~80s 寿命: %+v", info.Stats)
	}
	store.Capture(1210, []string{"__cflb=reroll; Max-Age=3600"}, now.Add(-30*time.Second))
	store.Reroll(1210) // faster-model 路径
	if info := store.SignInfo(1210, now); info.Stats.Samples != 2 || info.Stats.P50 < 29 {
		t.Fatalf("Reroll 应归档 ~30s 寿命: %+v", info.Stats)
	}
	// 罐已空但统计保留（账号历史不丢；HasSign 才描述罐）。
	if info := store.SignInfo(1210, now); info.HasSign {
		t.Fatal("Reroll 后罐应空")
	}
	if got := store.SignInfo(1210, now).Stats.Samples; got != 2 {
		t.Fatalf("罐空后统计应保留: %d", got)
	}
}

func TestSignInfoNewestAndPercentiles(t *testing.T) {
	store := New()
	now := time.Now()
	store.Capture(1210, []string{"__cflb=old; Max-Age=3600"}, now)
	store.Capture(1210, []string{"__oailb=new; Max-Age=120"}, now.Add(30*time.Second))
	info := store.SignInfo(1210, now.Add(31*time.Second))
	if !info.HasSign || !info.CapturedAt.Equal(now.Add(30*time.Second)) {
		t.Fatalf("CapturedAt 应取最新: %+v", info)
	}
	// 连续换签注入已知寿命 100s..3300s（33 条），验证最近秩百分位与窗口封顶。
	start := now.Add(time.Minute)
	for i := 1; i <= 33; i++ {
		life := time.Duration(100 * i * int(time.Second))
		store.Capture(1211, []string{"__cflb=seq; Max-Age=3600"}, start)
		store.Capture(1211, []string{"__cflb=seq; Max-Age=3600"}, start.Add(life))
		start = start.Add(life)
	}
	stats := store.SignInfo(1211, start).Stats
	if stats.Samples != 32 {
		t.Fatalf("窗口应封顶 32，得到 %d", stats.Samples)
	}
	// 33 条记录后环形窗口持有最近 32 条：寿命 200..3300。
	if stats.Min != 200 {
		t.Fatalf("Min 应为窗口内最短 200，得到 %v", stats.Min)
	}
	// 最近秩 p50：32 样本 idx=int(0.5*31+0.5)=16（0 基）→ 第 17 小 = 1800。
	if int(stats.P50) != 1800 {
		t.Fatalf("P50 最近秩应为 1800，得到 %v", stats.P50)
	}
	if stats.P50 > stats.P80 || stats.P80 < stats.Min {
		t.Fatalf("分位单调异常: %+v", stats)
	}
}

func TestStatusExposesSignFields(t *testing.T) {
	store := New()
	now := time.Now()
	store.Capture(1210, []string{"__cflb=abc; Max-Age=3600"}, now)
	store.Capture(1210, []string{"__cflb=abc; Max-Age=3600"}, now.Add(90*time.Second)) // 覆写归档 90s
	status := store.Status(now.Add(95 * time.Second))
	if len(status.Accounts) != 1 {
		t.Fatalf("应有一个账号: %+v", status.Accounts)
	}
	account := status.Accounts[0]
	if !account.SignCapturedAt.Equal(now.Add(90 * time.Second)) {
		t.Fatalf("SignCapturedAt 应为最新捕获: %+v", account)
	}
	if account.LifetimeStats == nil || account.LifetimeStats.Samples != 1 || int(account.LifetimeStats.P50) != 90 {
		t.Fatalf("Status 应携带寿命统计: %+v", account.LifetimeStats)
	}
	// 脱敏边界：统计与时刻里不得出现 Cookie 值。
	raw, _ := json.Marshal(status)
	if strings.Contains(string(raw), "abc") {
		t.Fatal("Status 泄漏 Cookie 值")
	}
}
