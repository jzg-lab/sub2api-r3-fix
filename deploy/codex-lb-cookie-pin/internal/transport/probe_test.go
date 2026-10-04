package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/liyunlong/sub2api-cookie-plugin/internal/cookiestore"
	"github.com/liyunlong/sub2api-cookie-plugin/internal/pluginconfig"
	"github.com/liyunlong/sub2api-cookie-plugin/internal/prober"
	pluginv1 "github.com/liyunlong/sub2api-cookie-plugin/pkg/pluginapi/v1"
)

// fakeUpstream 模拟 chatgpt.com/backend-api/codex/responses：记录探针请求形态、
// 按剧本回判别题答案、并总是下发新 LB Cookie（验证探针回捕再钉扎）。
type fakeUpstream struct {
	server     *httptest.Server
	requests   int64
	verdict    string // pass | fail | 503-first
	rt         int    // >0：completed usage 带该 reasoning_tokens（v0.3.4 门槛测试）；0：不带 usage
	forceState int    // >0：一律回该状态码不回 SSE（401 丢模板测试）
	seenAuth   atomic.Value
	seenCookie atomic.Value
	seenBody   atomic.Value
}

func newFakeUpstream(t *testing.T, verdict string) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{verdict: verdict, rt: 1200}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&f.requests, 1)
		raw, _ := io.ReadAll(r.Body)
		f.seenAuth.Store(r.Header.Get("Authorization"))
		f.seenCookie.Store(r.Header.Get("Cookie"))
		f.seenBody.Store(string(raw))
		w.Header().Add("Set-Cookie", fmt.Sprintf("__cflb=fresh-%d; Max-Age=3500", n))
		if f.verdict == "503-first" && n == 1 {
			w.WriteHeader(503)
			return
		}
		if f.forceState != 0 {
			w.WriteHeader(f.forceState)
			return
		}
		var body struct {
			Model  string `json:"model"`
			Stream *bool  `json:"stream"`
			Store  *bool  `json:"store"`
			Input  []struct {
				Type    string `json:"type"`
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"input"`
		}
		_ = json.Unmarshal(raw, &body)
		// codex 后端的三条硬约束（400 报文反推）：input 数组、store=false、stream=true。
		if body.Model == "" || len(body.Input) == 0 || body.Input[0].Content[0].Text == "" ||
			body.Stream == nil || !*body.Stream || body.Store == nil || *body.Store {
			w.WriteHeader(400)
			return
		}
		question := body.Input[0].Content[0].Text
		answer := "2" // 一律答错（降智剧本）
		if f.verdict != "fail" {
			answer = canaryAnswer(question)
		}
		writeSSEWithUsage(w, body.Model, answer, f.rt)
	}))
	t.Cleanup(f.server.Close)
	return f
}

// canaryAnswer 按 v0.3.5 题库语义正确作答：糖果题（含「糖果」）恒答 21；
// 双维题（含「不同类别」）按题尾两行各取行内最后 3 个数字（流标签「5号的/
// 7号的」自带数字，不能整题取尾 6 个）复原数表，用承诺闭式复算答案。
// 其余题面（不应出现）返回错答 "2"。
func canaryAnswer(question string) string {
	switch {
	case strings.Contains(question, "糖果"):
		return "21"
	case strings.Contains(question, "不同类别"):
		var counts [2][3]int
		lines := strings.Split(question, "\n")
		found := 0
		for i := len(lines) - 1; i >= 0 && found < 2; i-- {
			line := strings.TrimSpace(lines[i])
			if line == "" {
				continue
			}
			toks := numTokenRe.FindAllString(line, -1)
			if len(toks) < 3 {
				continue
			}
			row := toks[len(toks)-3:]
			s := 1 - found
			for h := 0; h < 3; h++ {
				v, _ := strconv.Atoi(row[h])
				counts[s][h] = v
			}
			found++
		}
		if found != 2 {
			return "2"
		}
		return strconv.Itoa(prober.TwoDimCommitAnswer(counts))
	}
	return "2"
}

var numTokenRe = regexp.MustCompile(`[0-9]+`)

// writeSSEWithUsage 在 writeSSE 基础上按需附带 completed usage（rt>0 时；
// v0.3.4 判过钉门槛测试），形态对齐宿主解析器：usage.output_tokens_details.
// reasoning_tokens 为主、顶层 usage.reasoning_tokens 为兜底。
func writeSSEWithUsage(w http.ResponseWriter, model, answer string, rt int) {
	if rt <= 0 {
		writeSSE(w, model, answer)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	flusher := w.(http.Flusher)
	final := map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"model": model,
			"output": []map[string]any{{
				"type": "message", "role": "assistant",
				"content": []map[string]any{{"type": "output_text", "text": answer}},
			}},
			"usage": map[string]any{
				"output_tokens":         999,
				"reasoning_tokens":      rt,
				"output_tokens_details": map[string]any{"reasoning_tokens": rt},
			},
		},
	}
	enc, _ := json.Marshal(final)
	fmt.Fprintf(w, "event: response.completed\ndata: %s\n\ndata: [DONE]\n\n", enc)
	flusher.Flush()
}

// writeSSE 按后端强制的 SSE 形态回判别题答案（delta 增量 + completed 终态）。
func writeSSE(w http.ResponseWriter, model, answer string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	flusher := w.(http.Flusher)
	fmt.Fprintf(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\n", answer)
	final := map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"model": model,
			"output": []map[string]any{{
				"type": "message", "role": "assistant",
				"content": []map[string]any{{"type": "output_text", "text": answer}},
			}},
		},
	}
	enc, _ := json.Marshal(final)
	fmt.Fprintf(w, "event: response.completed\ndata: %s\n\ndata: [DONE]\n\n", enc)
	flusher.Flush()
}

func probeTestConfig() pluginconfig.Config {
	cfg := pluginconfig.Default()
	cfg.InjectScope = "all" // 测试上游非 chatgpt.com 域
	cfg.QualityProbeEnabled = true
	cfg.ProbeIntervalSeconds = 300
	cfg.MaxConsecutiveProbeFailures = 3
	cfg.ProbeBackoffSeconds = 3600
	return cfg
}

// stashFrom 构造业务请求帧并捕获模板（模拟 Forward 的捕获路径）。
func stashFrom(s *Server, accountID int64, url string, now time.Time) {
	start := &pluginv1.ForwardRequestStart{
		Method:    http.MethodPost,
		Url:       url,
		Host:      strings.TrimPrefix(strings.TrimPrefix(url, "https://"), "http://"),
		AccountId: accountID,
		Headers: map[string]*pluginv1.HeaderValues{
			"Authorization": {Values: []string{"Bearer test-token-xyz"}},
			"Content-Type":  {Values: []string{"application/json"}},
			"Cookie":        {Values: []string{"session=legacy"}},
		},
	}
	s.stashTemplate(start, []byte(`{"model":"gpt-6-astra","input":"业务请求"}`), now)
}

// TestProbePassPath 满血剧本：一答即中，请求与业务流量同形
// （Authorization/原始 Cookie 保留+罐内钉扎合并/模板模型/题面）。
func TestProbePassPath(t *testing.T) {
	up := newFakeUpstream(t, "pass")
	store := cookiestore.New()
	cfg := probeTestConfig()
	store.SetConfig(cfg)
	srv := New(store)

	now := time.Now()
	store.Capture(42, []string{"__cflb=pinned-abc; Max-Age=3500"}, now)
	stashFrom(srv, 42, up.server.URL, now)

	tmpl := *srv.templates[42]
	state := prober.NewState(42)
	srv.runProbeCycle(context.Background(), 42, &tmpl, state, cfg)

	if state.LastVerdict != prober.VerdictPass {
		t.Fatalf("应判 pass，得 %s（答案 %q）", state.LastVerdict, state.LastAnswer)
	}
	if up.requests != 1 {
		t.Fatalf("满血一发即止，请求数应 1，得 %d", up.requests)
	}
	if got := up.seenAuth.Load(); got != "Bearer test-token-xyz" {
		t.Errorf("探针未携带业务模板 Authorization: %v", got)
	}
	cookie := fmt.Sprint(up.seenCookie.Load())
	if !strings.Contains(cookie, "__cflb=pinned-abc") {
		t.Errorf("探针未注入罐内钉扎 Cookie: %q", cookie)
	}
	if !strings.Contains(cookie, "session=legacy") {
		t.Errorf("探针应保留业务原始 Cookie 条目: %q", cookie)
	}
	body := fmt.Sprint(up.seenBody.Load())
	for _, want := range []string{`"model":"gpt-6-astra"`, `"stream":true`, `"store":false`, "糖果", `"input_text"`,
		`"instructions":""`, `"reasoning"`, `"effort":"medium"`, `"summary":"auto"`,
		`"parallel_tool_calls":true`, `"include":["reasoning.encrypted_content"]`} {
		if !strings.Contains(body, want) {
			t.Errorf("探针请求体缺 %s: %s", want, body)
		}
	}
	if state.SuspectAccountLevel || state.ConsecFails != 0 {
		t.Errorf("pass 后状态异常: %+v", state)
	}
}

// TestProbeFailRerollBackoff 降智剧本：连错 3 次 → 重摇 2 次 → 账号级退避；
// 且每次探针响应的新 Cookie 被回捕（重摇后探针自己把新签钉回罐）。
func TestProbeFailRerollBackoff(t *testing.T) {
	up := newFakeUpstream(t, "fail")
	store := cookiestore.New()
	cfg := probeTestConfig()
	store.SetConfig(cfg)
	srv := New(store)

	now := time.Now()
	store.Capture(7, []string{"__cflb=stale-old; Max-Age=3500"}, now)
	stashFrom(srv, 7, up.server.URL, now)

	tmpl := *srv.templates[7]
	state := prober.NewState(7)
	srv.runProbeCycle(context.Background(), 7, &tmpl, state, cfg)

	if up.requests != 3 { // 错→摇→错→摇→错→退避 = 3 发
		t.Fatalf("连错 3 次应发 3 探即退避，请求数 %d", up.requests)
	}
	if !state.SuspectAccountLevel {
		t.Fatal("连续答错达阈值应判疑似账号级")
	}
	if state.QualityRerolls != 2 {
		t.Errorf("退避前应重摇 2 次（第 3 错不再摇），得 %d", state.QualityRerolls)
	}
	// 探针响应的新签应已回捕入罐（fresh-3 是最后一发）。
	status := store.Status(time.Now())
	found := false
	for _, acc := range status.Accounts {
		if acc.AccountID == 7 && strings.Contains(acc.Names, "__cflb") {
			found = true
		}
	}
	if !found {
		t.Fatal("探针响应的 Set-Cookie 未回捕入罐")
	}
	if !state.Due(time.Now().Add(2 * time.Hour)) {
		t.Errorf("退避期满后应可复探")
	}
}

// TestProbeRetryOn503 冷会话剧本：首发 503 重试即愈，不记质量失败。
func TestProbeRetryOn503(t *testing.T) {
	up := newFakeUpstream(t, "503-first")
	store := cookiestore.New()
	cfg := probeTestConfig()
	store.SetConfig(cfg)
	srv := New(store)

	now := time.Now()
	stashFrom(srv, 9, up.server.URL, now)

	tmpl := *srv.templates[9]
	state := prober.NewState(9)
	srv.runProbeCycle(context.Background(), 9, &tmpl, state, cfg)

	if state.LastVerdict != prober.VerdictPass {
		t.Fatalf("503 重试后应判 pass，得 %s", state.LastVerdict)
	}
	if up.requests != 2 {
		t.Fatalf("应 503 一次+重试一次，请求数 %d", up.requests)
	}
	if state.Fails != 0 || state.ConsecFails != 0 {
		t.Errorf("503 不应记质量失败: %+v", state)
	}
}

// TestProbeCycleRescuesAfterReroll 池级降智剧本：首签错→重摇→新签对，
// 总请求数 2、无退避、无嫌疑——对应 10/1 二号 25%→0% 的实测救回路径。
func TestProbeCycleRescuesAfterReroll(t *testing.T) {
	store := cookiestore.New()
	cfg := probeTestConfig()
	store.SetConfig(cfg)
	srv := New(store)

	var requests int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&requests, 1)
		w.Header().Add("Set-Cookie", fmt.Sprintf("__cflb=pin-%d; Max-Age=3500", n))
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Model string `json:"model"`
			Input []struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"input"`
		}
		_ = json.Unmarshal(raw, &body)
		question := ""
		if len(body.Input) > 0 && len(body.Input[0].Content) > 0 {
			question = body.Input[0].Content[0].Text
		}
		answer := "2" // 坏签一律答错
		if n >= 2 {   // 首签（坏签）答错；重摇后的新签按题正确作答
			answer = canaryAnswer(question)
		}
		writeSSEWithUsage(w, body.Model, answer, 1200)
	}))
	defer up.Close()

	now := time.Now()
	store.Capture(11, []string{"__cflb=bad-pin; Max-Age=3500"}, now)
	stashFrom(srv, 11, up.URL, now)

	tmpl := *srv.templates[11]
	state := prober.NewState(11)
	srv.runProbeCycle(context.Background(), 11, &tmpl, state, cfg)

	if state.LastVerdict != prober.VerdictPass || state.SuspectAccountLevel {
		t.Fatalf("重摇救回应以 pass 收尾且无嫌疑: %+v", state)
	}
	if requests != 2 {
		t.Fatalf("坏签一探+新签复探=2 发，得 %d", requests)
	}
	if state.QualityRerolls != 1 {
		t.Errorf("应恰好重摇一次: %d", state.QualityRerolls)
	}
}

// TestExtractModelAndSSE 请求体模型提取与 SSE 答案提取的边界。
func TestExtractModelAndSSE(t *testing.T) {
	if got := extractModel([]byte(`{"model":"gpt-6-astra","input":"x"}`)); got != "gpt-6-astra" {
		t.Errorf("extractModel: %q", got)
	}
	if got := extractModel(nil); got != "" {
		t.Errorf("空体应返回空: %q", got)
	}
	if got := extractModel([]byte(`{"instructions":"..."}`)); got != "" {
		t.Errorf("无 model 字段应返回空: %q", got)
	}
	big := append([]byte(`{"model":"x","pad":"`), make([]byte, 2<<20)...)
	if got := extractModel(big); got != "" {
		t.Errorf("超限体应返回空: %q", got)
	}
	// completed 终态优先。
	sse := "event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"错"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"3"}]}]}}` + "\n\ndata: [DONE]\n\n"
	if got := extractAnswerFromSSE([]byte(sse)); got != "3" {
		t.Errorf("completed 优先: %q", got)
	}
	// completed 缺失时用 delta 兜底。
	deltas := `data: {"type":"response.output_text.delta","delta":"4"}` + "\n" +
		`data: {"type":"response.output_text.delta","delta":"34"}` + "\n\n"
	if got := extractAnswerFromSSE([]byte(deltas)); got != "434" {
		t.Errorf("delta 兜底: %q", got)
	}
	if extractAnswerFromSSE([]byte("data: [DONE]")) != "" {
		t.Error("无文本事件应为空")
	}
	if extractAnswerFromSSE(nil) != "" {
		t.Error("空响应应为空")
	}
}

// TestStashSkipsGetAndForeignURL 模板捕获的门槛：非 POST 不收。
func TestStashSkipsGetAndForeignURL(t *testing.T) {
	srv := New(cookiestore.New())
	start := &pluginv1.ForwardRequestStart{
		Method:    http.MethodGet,
		Url:       "https://chatgpt.com/backend-api/codex/responses",
		AccountId: 1,
		Headers:   map[string]*pluginv1.HeaderValues{},
	}
	srv.stashTemplate(start, nil, time.Now())
	if len(srv.templates) != 0 {
		t.Fatal("GET 请求不应入模板台账")
	}
}

// v0.3 状态桥契约：status_json 的 prober 区段必须携带救治区标签计算与读秒
// 需要的全部字段（fork 宿主 GET /admin/plugins/:id/status 的数据源）。
func TestStatusJSONExposesRescueFields(t *testing.T) {
	store := cookiestore.New()
	srv := New(store)
	now := time.Now()
	cfg := store.Config()
	store.Capture(1210, []string{"__cflb=bridge; Max-Age=3600"}, now)
	srv.probeMu.Lock()
	st := prober.NewState(1210)
	st.Record(prober.VerdictPass, "r_count", "3", now, cfg)
	st.Record(prober.VerdictPass, "arith", "434", now, cfg)
	srv.states[1210] = st
	srv.probeMu.Unlock()
	raw := string(srv.mergeStatusJSON(now, store.Status(now), cfg))
	for _, want := range []string{
		`"consecutive_passes":2`,
		`"in_backoff":false`,
		`"sign_captured_at"`,
		`"estimated_remaining_seconds"`,
		`"estimate_basis":"fallback"`,
		`"adaptive_scheduling":true`,
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("status_json 缺字段 %s: %s", want, raw)
		}
	}
	if strings.Contains(raw, "bridge") {
		t.Fatal("status_json 泄漏 Cookie 值")
	}
	// 罐侧同报：账号区段带寿命统计占位（有样本才出现）。
	if !strings.Contains(raw, `"sign_captured_at"`) {
		t.Fatal("罐区段应带 sign_captured_at")
	}
}

// TestNewStartsProbeLoop（v0.3.1 回归）：官方宿主的 TransportPlugin 方法集
// 没有 InitHostServices（只有 GetInfo/Health/ValidateConfig/ApplyConfig/
// TestConfig/Forward），0.3.0 把探针回路挂在该 RPC 里，生产上从未启动——
// 模板照常入账但 states 恒空、探针区恒 accounts:[]/probes:0，救治链连过
// 证据断供。回路必须构造即启动，本测试锁死该行为。
func TestNewStartsProbeLoop(t *testing.T) {
	s := New(cookiestore.New())
	s.probeMu.Lock()
	started := s.probeStop != nil
	s.probeMu.Unlock()
	if !started {
		t.Fatal("probe loop must start at construction: official host never calls InitHostServices")
	}
	// 幂等兜底：InitHostServices 到来时不得重复起协程。
	s.InitHostServices(context.Background(), &pluginv1.InitHostServicesRequest{})
	s.probeMu.Lock()
	stillOne := s.probeStop != nil
	s.probeMu.Unlock()
	if !stillOne {
		t.Fatal("InitHostServices must not stop the construction-started probe loop")
	}
}

// ---------- v0.3.2 救治提速：模板续命 / 新签即探 / 密集档 ----------

// TestTemplateRetiredDualTrack 模板退役双轨：绝对上限 24h；无探针状态且超
// 业务 horizon → 弃（原语义）；有探针状态（探针会回写 SeenAt 续命）→ 保留。
// 救治号零业务流量，第二轨是它探针不停摆的生命线（2026-10-02 生产实测教训）。
func TestTemplateRetiredDualTrack(t *testing.T) {
	now := time.Now()
	horizon := 10 * time.Minute
	fresh := &probeTemplate{SeenAt: now.Add(-time.Minute)}
	stale := &probeTemplate{SeenAt: now.Add(-time.Hour)}
	ancient := &probeTemplate{SeenAt: now.Add(-25 * time.Hour)}
	if templateRetired(fresh, nil, now, horizon) {
		t.Fatal("horizon 内新鲜模板不应退役")
	}
	if !templateRetired(stale, nil, now, horizon) {
		t.Fatal("无探针状态且超 horizon 应退役（原语义）")
	}
	if templateRetired(stale, prober.NewState(1), now, horizon) {
		t.Fatal("有探针状态的超龄模板应保留（探针续命轨）")
	}
	if !templateRetired(ancient, prober.NewState(1), now, horizon) {
		t.Fatal("超 24h 绝对上限必须退役")
	}
}

// TestPullForFreshSign 新签即探：更新更晚的签 → 下一针拉近到 +5s；旧排期
// 更近时不推远；KV 恢复的旧签只对齐不拉近；同签重复调用幂等。
func TestPullForFreshSign(t *testing.T) {
	now := time.Now()
	st := prober.NewState(2)
	st.NextProbeAt = now.Add(4 * time.Minute)
	pullForFreshSign(st, now.Add(-30*time.Second), now)
	if !st.NextProbeAt.Equal(now.Add(postCaptureProbeDelay)) {
		t.Fatalf("新签应拉近到 +5s: %v", st.NextProbeAt)
	}
	// 不推远：下一针本就更近，保持。
	st2 := prober.NewState(3)
	st2.NextProbeAt = now.Add(2 * time.Second)
	pullForFreshSign(st2, now.Add(-30*time.Second), now)
	if !st2.NextProbeAt.Equal(now.Add(2 * time.Second)) {
		t.Fatalf("只拉近不推远: %v", st2.NextProbeAt)
	}
	// 旧签（超出新鲜窗）：只对齐 KnownSignAt，不拉近。
	st3 := prober.NewState(4)
	st3.NextProbeAt = now.Add(4 * time.Minute)
	pullForFreshSign(st3, now.Add(-time.Hour), now)
	if !st3.NextProbeAt.Equal(now.Add(4 * time.Minute)) {
		t.Fatalf("旧签不应拉近: %v", st3.NextProbeAt)
	}
	// 幂等：同签第二次调用不再动排期。
	pullForFreshSign(st3, now.Add(-time.Hour), now)
	if !st3.NextProbeAt.Equal(now.Add(4 * time.Minute)) {
		t.Fatalf("同签重放应幂等: %v", st3.NextProbeAt)
	}
}

// TestProbeCycleKeepsTemplateAlive 探针续命：一轮探针后模板 SeenAt 被回写，
// 零业务流量的救治号不会因 horizon 过期丢模板（失探根因的回归测试）。
func TestProbeCycleKeepsTemplateAlive(t *testing.T) {
	up := newFakeUpstream(t, "pass")
	store := cookiestore.New()
	cfg := probeTestConfig()
	store.SetConfig(cfg)
	srv := New(store)

	staleSeen := time.Now().Add(-9 * time.Minute) // 距 horizon(10min) 只剩 1 分钟
	store.Capture(42, []string{"__cflb=pinned-abc; Max-Age=3500"}, staleSeen)
	stashFrom(srv, 42, up.server.URL, staleSeen)

	tmpl := *srv.templates[42]
	state := prober.NewState(42)
	before := time.Now()
	srv.runProbeCycle(context.Background(), 42, &tmpl, state, cfg)

	if state.LastVerdict != prober.VerdictPass {
		t.Fatalf("应判 pass，得 %s", state.LastVerdict)
	}
	live := srv.templates[42]
	if live == nil || live.SeenAt.Before(before) {
		t.Fatalf("探针后应回写模板 SeenAt（续命）: %+v", live)
	}
	// 双轨判定：此刻（超原 horizon 的时钟下）模板必须存活。
	if templateRetired(live, state, time.Now(), 10*time.Minute) {
		t.Fatal("探针续命后模板不应退役")
	}
}

// TestBurstExposedOnStatusBridge 密集档字段上桥：未验证态账号的 prober
// 状态含 in_burst/burst_probes（宿主与前端可观测救治进度）。
func TestAdaptiveSchedulingCannotOverrideBurst(t *testing.T) {
	up := newFakeUpstream(t, "pass")
	store := cookiestore.New()
	cfg := probeTestConfig()
	cfg.AdaptiveProbeScheduling = true
	store.SetConfig(cfg)
	now := time.Now()
	for i := 0; i < 4; i++ {
		store.Capture(42, []string{fmt.Sprintf("__cflb=sample-%d; Max-Age=7200", i)},
			now.Add(time.Duration(i-4)*30*time.Minute))
	}
	srv := New(store)
	stashFrom(srv, 42, up.server.URL, now)
	tmpl := *srv.templates[42]
	state := prober.NewState(42)
	srv.states[42] = state
	srv.runProbeCycle(t.Context(), 42, &tmpl, state, cfg)
	if store.SignInfo(42, time.Now()).Stats.Samples < prober.MinAdaptiveSamples {
		t.Fatal("fixture must exercise the adaptive scheduling branch")
	}
	if !state.InBurst || state.ConsecPasses != 1 {
		t.Fatal("first successful probe must remain in burst")
	}
	want := state.LastProbeAt.Add(time.Duration(cfg.ProbeBurstIntervalSeconds) * time.Second)
	if !state.NextProbeAt.Equal(want) {
		t.Fatalf("burst schedule overwritten: got %s want %s", state.NextProbeAt, want)
	}
}

func TestBurstExposedOnStatusBridge(t *testing.T) {
	up := newFakeUpstream(t, "pass")
	store := cookiestore.New()
	cfg := probeTestConfig()
	store.SetConfig(cfg)
	srv := New(store)

	now := time.Now()
	stashFrom(srv, 42, up.server.URL, now)
	tmpl := *srv.templates[42]
	state := prober.NewState(42)
	srv.probeMu.Lock()
	srv.states[42] = state // probeLoop 职责，此处手动登记以便上桥
	srv.probeMu.Unlock()
	srv.runProbeCycle(context.Background(), 42, &tmpl, state, cfg)

	raw := srv.mergeStatusJSON(time.Now(), store.Status(time.Now()), cfg)
	var status struct {
		Prober struct {
			Accounts []struct {
				AccountID  int64 `json:"account_id"`
				ConsecPass int   `json:"consecutive_passes"`
				InBurst    bool  `json:"in_burst"`
				BurstN     int   `json:"burst_probes"`
			} `json:"accounts"`
		} `json:"prober"`
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatalf("状态桥 JSON 解析失败: %v", err)
	}
	for _, a := range status.Prober.Accounts {
		if a.AccountID == 42 {
			if a.ConsecPass != 1 || !a.InBurst || a.BurstN < 1 {
				t.Fatalf("未验证态应上桥密集档字段: pass=%d in_burst=%v burst=%d", a.ConsecPass, a.InBurst, a.BurstN)
			}
			return
		}
	}
	t.Fatal("状态桥缺账号 42")
}

// ---------- v0.3.6 热循环双修：退避压新签 / 在飞互斥 ----------

// TestPullForFreshSignRespectsBackoff 退避压过新鲜（0.3.5 生产热循环根因）：
// 连错退避期内，新鲜签只对齐 KnownSignAt、绝不把 +1h 的 NextProbeAt 拽到
// +5s——否则败针重摇回捕的新签会让退避永远不生效（六号 8 分钟 106 针/58
// 重摇实测）。退避期满后：已对齐过的旧事件不触发，更新的新签恢复拉近。
func TestPullForFreshSignRespectsBackoff(t *testing.T) {
	now := time.Now()
	st := prober.NewState(5)
	st.BackoffUntil = now.Add(1 * time.Hour)
	st.NextProbeAt = now.Add(1 * time.Hour) // EnterBackoff 语义：下一针=退避期满

	pullForFreshSign(st, now.Add(-30*time.Second), now) // 新鲜签，但在退避期内
	if !st.NextProbeAt.Equal(now.Add(1 * time.Hour)) {
		t.Fatalf("退避期内新签不得拉近下一针: %v", st.NextProbeAt)
	}
	if !st.KnownSignAt.Equal(now.Add(-30 * time.Second)) {
		t.Fatalf("KnownSignAt 仍须对齐（退避期满后旧签不重复触发）: %v", st.KnownSignAt)
	}

	after := now.Add(90 * time.Minute) // 退避已期满
	pullForFreshSign(st, now.Add(-30*time.Second), after)
	if !st.NextProbeAt.Equal(now.Add(1 * time.Hour)) {
		t.Fatalf("已对齐过的旧事件在退避期满后也不应触发拉近: %v", st.NextProbeAt)
	}
	// 期满后恢复拉近只作用于「比 +5s 更远」的排期（只拉近不推远；已到期的
	// 排期由 Due 正常触发，无需拉近）。
	st.NextProbeAt = after.Add(4 * time.Minute) // 期满后的远排期（如稀疏档）
	fresh2 := after.Add(-10 * time.Second)      // 比 KnownSignAt 更新的新签
	pullForFreshSign(st, fresh2, after)
	if !st.NextProbeAt.Equal(after.Add(postCaptureProbeDelay)) {
		t.Fatalf("退避期满后的新签应恢复拉近: %v", st.NextProbeAt)
	}
}

// TestProbeCycleInFlightGuard 同账号在飞互斥（0.3.5 热循环第二放大器）：
// 新签即探把排期拉近后，tick 可能在上一轮复探链未走完时再次派发同号——
// 在飞守卫必须让并发轮直接返回、零出针。
func TestProbeCycleInFlightGuard(t *testing.T) {
	block := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(block) }) }
	defer release()

	var requests int64
	firstSeen := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&requests, 1)
		if n == 1 {
			close(firstSeen)
			<-block // 第一针悬停，把第一轮探针钉在在飞态
		}
		w.Header().Add("Set-Cookie", fmt.Sprintf("__cflb=guard-%d; Max-Age=3500", n))
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Model string `json:"model"`
			Input []struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"input"`
		}
		_ = json.Unmarshal(raw, &body)
		answer := "2"
		writeSSEWithUsage(w, body.Model, answer, 1200)
	}))
	t.Cleanup(func() { release(); up.Close() })

	store := cookiestore.New()
	cfg := probeTestConfig()
	store.SetConfig(cfg)
	srv := New(store)

	now := time.Now()
	stashFrom(srv, 60, up.URL, now)
	tmpl := *srv.templates[60]
	state := prober.NewState(60)

	go srv.runProbeCycle(context.Background(), 60, &tmpl, state, cfg)
	<-firstSeen // 第一轮已在飞

	before := atomic.LoadInt64(&requests)
	done := make(chan struct{})
	go func() {
		srv.runProbeCycle(context.Background(), 60, &tmpl, state, cfg)
		close(done)
	}()
	select {
	case <-done: // 守卫应立即返回
	case <-time.After(2 * time.Second):
		t.Fatal("在飞守卫应让并发轮立即返回")
	}
	if got := atomic.LoadInt64(&requests); got != before {
		t.Fatalf("并发轮不得出针: before=%d after=%d", before, got)
	}

	release()                                    // 放行第一轮，随后守卫应解除
	deadline := time.Now().Add(20 * time.Second) // 复探链含两次 2s 落定等待，放宽
	for {
		if _, busy := srv.probeRunning.Load(60); !busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("第一轮结束后守卫应释放")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestProbeOwnCaptureDoesNotFeedPull 探针自捕的签不喂新签即探（0.3.5 热循环
// 真发动机）：探针响应（含错误响应头）回捕的 Set-Cookie 若触发拉近，error
// 针不计连败不进退避 = 10s 拍频自激励循环。一轮探针后 KnownSignAt 必须已
// 对齐到罐内最新签——同签再喂 pullForFreshSign 不得拉近。
func TestProbeOwnCaptureDoesNotFeedPull(t *testing.T) {
	up := newFakeUpstream(t, "fail")
	store := cookiestore.New()
	cfg := probeTestConfig()
	store.SetConfig(cfg)
	srv := New(store)

	now := time.Now()
	store.Capture(7, []string{"__cflb=stale-old; Max-Age=3500"}, now)
	stashFrom(srv, 7, up.server.URL, now)
	state := prober.NewState(7)
	srv.runProbeCycle(context.Background(), 7, srv.templates[7], state, cfg)

	info := store.SignInfo(7, time.Now())
	if !info.HasSign {
		t.Fatal("探针响应应已回捕入罐")
	}
	if !state.KnownSignAt.Equal(info.CapturedAt) {
		t.Fatalf("轮内应已对齐 KnownSignAt: state=%v jar=%v", state.KnownSignAt, info.CapturedAt)
	}
	// 同签（探针源）喂 pull：不得拉近。
	state.NextProbeAt = time.Now().Add(4 * time.Minute)
	before := state.NextProbeAt
	pullForFreshSign(state, info.CapturedAt, time.Now())
	if !state.NextProbeAt.Equal(before) {
		t.Fatal("探针自捕的签不得触发新签即探（自激励热循环根因）")
	}
}

// ---------- v0.3.3 考卷同形 ----------

// TestProbeBareShapeOnNoneEffort：probe_reasoning_effort=none 时回退旧裸形态
// （不带 reasoning/instructions/parallel_tool_calls/include）。
func TestProbeBareShapeOnNoneEffort(t *testing.T) {
	up := newFakeUpstream(t, "pass")
	store := cookiestore.New()
	cfg := probeTestConfig()
	cfg.ProbeReasoningEffort = ""
	store.SetConfig(cfg)
	srv := New(store)

	now := time.Now()
	store.Capture(43, []string{"__cflb=pinned-abc; Max-Age=3500"}, now)
	stashFrom(srv, 43, up.server.URL, now)

	tmpl := *srv.templates[43]
	state := prober.NewState(43)
	srv.runProbeCycle(context.Background(), 43, &tmpl, state, cfg)

	if state.LastVerdict != prober.VerdictPass {
		t.Fatalf("应判 pass，得 %s（答案 %q）", state.LastVerdict, state.LastAnswer)
	}
	body := fmt.Sprint(up.seenBody.Load())
	for _, ban := range []string{`"reasoning"`, `"instructions"`, `"parallel_tool_calls"`, `"include"`} {
		if strings.Contains(body, ban) {
			t.Errorf("裸形态不应包含 %s: %s", ban, body)
		}
	}
}

// ---------- v0.3.4 判过钉门槛 + 401 丢模板 ----------

// TestProbeLowRTGateFails：答对但 usage 推理 token 低于门槛（缺省 800）→ 判
// Fail 触发重摇（连错达阈值进退避），连过计数不吃低 rt 针。
func TestProbeLowRTGateFails(t *testing.T) {
	up := newFakeUpstream(t, "pass") // 答案全对
	up.rt = 300                      // 但推理预算低（< 800）
	store := cookiestore.New()
	cfg := probeTestConfig()
	store.SetConfig(cfg)
	srv := New(store)

	now := time.Now()
	store.Capture(52, []string{"__cflb=lowrt; Max-Age=3500"}, now)
	stashFrom(srv, 52, up.server.URL, now)

	tmpl := *srv.templates[52]
	state := prober.NewState(52)
	srv.runProbeCycle(context.Background(), 52, &tmpl, state, cfg)

	if state.LastVerdict != prober.VerdictFail {
		t.Fatalf("低 rt 答对应判 Fail，得 %s（答案 %q）", state.LastVerdict, state.LastAnswer)
	}
	if state.ConsecPasses != 0 {
		t.Errorf("低 rt 针不得计入连过: %d", state.ConsecPasses)
	}
	if !strings.HasPrefix(state.LastAnswer, "low-rt:300") {
		t.Errorf("答案摘要应带 low-rt 标记: %q", state.LastAnswer)
	}
	if state.LastReasoningTokens != 300 {
		t.Errorf("LastReasoningTokens 应为 300，得 %d", state.LastReasoningTokens)
	}
	if !state.SuspectAccountLevel || state.QualityRerolls != 2 {
		t.Errorf("低 rt 连错应与普通连错同路（3 发退避/2 次重摇）: %+v", state)
	}
}

// TestProbeHighRTPasses：答对且 rt≥门槛 → 正常 pass，rt 记入状态。
func TestProbeHighRTPasses(t *testing.T) {
	up := newFakeUpstream(t, "pass")
	up.rt = 1500
	store := cookiestore.New()
	cfg := probeTestConfig()
	store.SetConfig(cfg)
	srv := New(store)

	now := time.Now()
	store.Capture(53, []string{"__cflb=highrt; Max-Age=3500"}, now)
	stashFrom(srv, 53, up.server.URL, now)

	tmpl := *srv.templates[53]
	state := prober.NewState(53)
	srv.runProbeCycle(context.Background(), 53, &tmpl, state, cfg)

	if state.LastVerdict != prober.VerdictPass {
		t.Fatalf("高 rt 答对应判 pass，得 %s", state.LastVerdict)
	}
	if state.ConsecPasses != 1 || state.LastReasoningTokens != 1500 {
		t.Errorf("状态异常: consec=%d rt=%d", state.ConsecPasses, state.LastReasoningTokens)
	}
}

// TestProbeGateDisabledAtOne：probe_min_reasoning_tokens=1 = 事实关闭——低 rt
// 答对照判 pass（逃生闸）。
func TestProbeGateDisabledAtOne(t *testing.T) {
	up := newFakeUpstream(t, "pass")
	up.rt = 100
	store := cookiestore.New()
	cfg := probeTestConfig()
	cfg.ProbeMinReasoningTokens = 1
	store.SetConfig(cfg)
	srv := New(store)

	now := time.Now()
	store.Capture(54, []string{"__cflb=gateoff; Max-Age=3500"}, now)
	stashFrom(srv, 54, up.server.URL, now)

	tmpl := *srv.templates[54]
	state := prober.NewState(54)
	srv.runProbeCycle(context.Background(), 54, &tmpl, state, cfg)

	if state.LastVerdict != prober.VerdictPass {
		t.Fatalf("门槛=1 应事实关闭判 pass，得 %s", state.LastVerdict)
	}
}

// Correct fingerprint responses are inconclusive: no quality penalty and no
// recovery credit. Adjacent non-fingerprint values retain normal grading.
func TestProbeTruncationFingerprintIsNeutral(t *testing.T) {
	for _, tc := range []struct {
		rt     int
		fail   bool
		marker string
	}{
		{516, true, "trunc-fp:516"},   // n=1（同时 <800，指纹优先报）
		{1034, true, "trunc-fp:1034"}, // n=2（≥800，rt 门槛拦不住、只有指纹拦）
		{1552, true, "trunc-fp:1552"}, // n=3（宿主针库实测出现的截断带）
		{1033, false, ""},             // 家族外邻近值
		{1035, false, ""},
	} {
		up := newFakeUpstream(t, "pass")
		up.rt = tc.rt
		store := cookiestore.New()
		cfg := probeTestConfig()
		store.SetConfig(cfg)
		srv := New(store)

		now := time.Now()
		store.Capture(56, []string{"__cflb=trunc; Max-Age=3500"}, now)
		stashFrom(srv, 56, up.server.URL, now)

		state := prober.NewState(56)
		srv.runProbeCycle(context.Background(), 56, srv.templates[56], state, cfg)

		if tc.fail {
			if state.LastVerdict != prober.VerdictError {
				t.Errorf("rt=%d: want inconclusive error, got %s", tc.rt, state.LastVerdict)
			}
			if state.Fails != 0 || state.QualityRerolls != 0 || state.ConsecFails != 0 || state.SuspectAccountLevel {
				t.Fatalf("fingerprint was counted as a quality failure: %+v", state)
			}
			if state.Probes != 1 || state.TruncationObservations != 1 {
				t.Fatalf("fingerprint must be observed once without immediate retry: %+v", state)
			}
			if !strings.HasPrefix(state.LastAnswer, tc.marker) {
				t.Errorf("rt=%d 答案摘要应带 %s 前缀: %q", tc.rt, tc.marker, state.LastAnswer)
			}
			if state.ConsecPasses != 0 {
				t.Errorf("rt=%d 截断针不得计入连过: %d", tc.rt, state.ConsecPasses)
			}
		} else if state.LastVerdict != prober.VerdictPass {
			t.Errorf("rt=%d 家族外应照常 pass，得 %s（%q）", tc.rt, state.LastVerdict, state.LastAnswer)
		}
	}
}

// TestProbe401DropsTemplate：401 = 模板 Authorization 已死——当场丢模板、记
// error（不动质量计数）、不重试。下一笔真实 Forward 才会用新鲜 token 重stash。
func TestProbe401DropsTemplate(t *testing.T) {
	up := newFakeUpstream(t, "pass")
	up.forceState = 401
	store := cookiestore.New()
	cfg := probeTestConfig()
	store.SetConfig(cfg)
	srv := New(store)

	now := time.Now()
	store.Capture(55, []string{"__cflb=deadtok; Max-Age=3500"}, now)
	stashFrom(srv, 55, up.server.URL, now)

	tmpl := *srv.templates[55]
	state := prober.NewState(55)
	srv.runProbeCycle(context.Background(), 55, &tmpl, state, cfg)

	if state.LastVerdict != prober.VerdictError {
		t.Fatalf("401 应记 error，得 %s", state.LastVerdict)
	}
	if !strings.Contains(state.LastAnswer, "tmpl-dropped") {
		t.Errorf("答案摘要应带 tmpl-dropped 标记: %q", state.LastAnswer)
	}
	srv.probeMu.Lock()
	_, still := srv.templates[55]
	srv.probeMu.Unlock()
	if still {
		t.Fatal("401 后模板应被当场丢弃")
	}
	if state.ConsecFails != 0 || state.Fails != 0 {
		t.Errorf("401 是凭据级错误，不得动质量计数: %+v", state)
	}
	if up.requests != 1 {
		t.Errorf("401 不应重试，请求数应 1，得 %d", up.requests)
	}
}

// TestExtractUsageFromSSE：usage 解析的优先级与兜底（对齐宿主双查）。
func TestExtractUsageFromSSE(t *testing.T) {
	details := `data: {"type":"response.completed","response":{"usage":{"reasoning_tokens":111,"output_tokens_details":{"reasoning_tokens":222}}}}` + "\n\n"
	if rt, ok := extractUsageFromSSE([]byte(details)); !ok || rt != 222 {
		t.Errorf("details 应优先: rt=%d ok=%v", rt, ok)
	}
	plain := `data: {"type":"response.completed","response":{"usage":{"reasoning_tokens":333}}}` + "\n\n"
	if rt, ok := extractUsageFromSSE([]byte(plain)); !ok || rt != 333 {
		t.Errorf("顶层兜底: rt=%d ok=%v", rt, ok)
	}
	noUsage := `data: {"type":"response.completed","response":{"output":[]}}` + "\n\n"
	if rt, ok := extractUsageFromSSE([]byte(noUsage)); ok || rt != 0 {
		t.Errorf("无 usage 应 (0,false): rt=%d ok=%v", rt, ok)
	}
	if rt, ok := extractUsageFromSSE(nil); ok || rt != 0 {
		t.Errorf("空流应 (0,false): rt=%d ok=%v", rt, ok)
	}
	incomplete := `data: {"type":"response.incomplete","response":{"usage":{"output_tokens_details":{"reasoning_tokens":444}}}}` + "\n\n"
	if rt, ok := extractUsageFromSSE([]byte(incomplete)); ok || rt != 0 {
		t.Errorf("incomplete must not certify reasoning usage: rt=%d ok=%v", rt, ok)
	}
}
