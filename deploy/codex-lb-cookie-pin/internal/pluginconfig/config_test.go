package pluginconfig

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestParsePersistentProbePauses(t *testing.T) {
	cfg, message, ok := Parse([]byte(`{"paused_account_ids":[42,7,42]}`))
	if !ok || !reflect.DeepEqual(cfg.Sanitized().PausedAccountIDs, []int64{7, 42}) {
		t.Fatalf("pause IDs must survive sanitization and be normalized: %v %s", cfg, message)
	}
	for _, raw := range []string{`{"paused_account_ids":[0]}`, `{"paused_account_ids":[-1]}`, `{"paused_account_ids":[1.5]}`} {
		if _, _, ok := Parse([]byte(raw)); ok {
			t.Fatalf("invalid pause accepted: %s", raw)
		}
	}
}

// testIncomingTagParity 锁住 incomingConfig 与 Config 的 json 标签一一对应，
// 防止后续加字段时只改一边。
func TestIncomingTagParity(t *testing.T) {
	want := map[string]string{}
	cfgType := reflect.TypeOf(Config{})
	for i := 0; i < cfgType.NumField(); i++ {
		tag := cfgType.Field(i).Tag.Get("json")
		want[tag] = cfgType.Field(i).Name
	}
	got := map[string]string{}
	incType := reflect.TypeOf(incomingConfig{})
	for i := 0; i < incType.NumField(); i++ {
		got[incType.Field(i).Tag.Get("json")] = incType.Field(i).Name
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("incomingConfig 与 Config 的 json 标签漂移:\n want=%v\n got=%v", want, got)
	}
}

func TestParseEmptyUsesDefaults(t *testing.T) {
	for _, raw := range []string{"", "  ", "\n"} {
		cfg, msg, ok := Parse([]byte(raw))
		if !ok {
			t.Fatalf("空输入应返回默认配置，却被拒: %s", msg)
		}
		if !cfg.Enabled || !cfg.RerollOnFasterModel || !cfg.PersistKV {
			t.Fatalf("空输入默认值应全开: %+v", cfg)
		}
		if cfg.InjectScope != "codex" || cfg.DefaultTTLSeconds != 240 {
			t.Fatalf("默认值异常: %+v", cfg)
		}
	}
}

// 核心：省略的布尔字段不得把默认 true 冲成 false（mock 全链路曾踩过的坑）。
func TestParseOmittedBooleansKeepDefaults(t *testing.T) {
	cfg, _, ok := Parse([]byte(`{"enabled":true,"inject_scope":"all"}`))
	if !ok {
		t.Fatal("合法配置被拒")
	}
	if !cfg.RerollOnFasterModel {
		t.Fatal("省略 reroll_on_faster_model 时应保留默认 true")
	}
	if !cfg.PersistKV {
		t.Fatal("省略 persist_kv 时应保留默认 true")
	}
	if cfg.InjectScope != "all" {
		t.Fatalf("inject_scope 应为 all，得到 %q", cfg.InjectScope)
	}
}

func TestParseExplicitFalseOverrides(t *testing.T) {
	cfg, _, ok := Parse([]byte(`{"enabled":false,"reroll_on_faster_model":false,"persist_kv":false}`))
	if !ok {
		t.Fatal("合法配置被拒")
	}
	if cfg.Enabled || cfg.RerollOnFasterModel || cfg.PersistKV {
		t.Fatalf("显式 false 应生效: %+v", cfg)
	}
}

func TestParseRejects(t *testing.T) {
	cases := []string{
		`{"unknown_field":1}`,                                      // 未知字段
		`{"cookie_names":["$$"]}`,                                  // 非法 Cookie 名
		`{"cookie_names":[]}`,                                      // 空白名单（显式空数组）
		`{"default_ttl_seconds":10}`,                               // TTL 太短
		`{"default_ttl_seconds":7200}`,                             // TTL 太长
		`{"default_ttl_seconds":240,"refresh_before_seconds":300}`, // 刷新窗 ≥ TTL
		`{"inject_scope":"everywhere"}`,                            // 非法 scope
		`{"drop_account_ids":[0]}`,                                 // 非法账号 ID
		`{}{}`,                                                     // 多个 JSON 对象
	}
	for _, raw := range cases {
		if _, _, ok := Parse([]byte(raw)); ok {
			t.Errorf("应被拒绝却通过: %s", raw)
		}
	}
}

func TestParseNormalizesCookieNames(t *testing.T) {
	cfg, _, ok := Parse([]byte(`{"cookie_names":["__CFLB"," __oailb ","__cflb"]}`))
	if !ok {
		t.Fatal("合法配置被拒")
	}
	if len(cfg.CookieNames) != 2 || cfg.CookieNames[0] != "__cflb" || cfg.CookieNames[1] != "__oailb" {
		t.Fatalf("应小写化+去空格+去重: %v", cfg.CookieNames)
	}
}

func TestSanitizedStripsOneShot(t *testing.T) {
	cfg, _, _ := Parse([]byte(`{"drop_account_ids":[1209,1209,1210]}`))
	out := cfg.Sanitized()
	if len(out.DropAccountIDs) != 0 {
		t.Fatal("Sanitized 应清除一次性字段")
	}
	// 去重应已发生在 Parse 阶段。
	if len(cfg.DropAccountIDs) != 2 {
		t.Fatalf("drop_account_ids 应去重: %v", cfg.DropAccountIDs)
	}
	// Sanitized 必须可再解析（宿主保存后重放）。
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if _, msg, ok := Parse(raw); !ok {
		t.Fatalf("Sanitized 输出无法再解析: %s", msg)
	}
}

// v0.2：质量探针默认关（主动流量与零探针原则冲突，必须显式 opt-in）。
func TestProbeDefaultsOff(t *testing.T) {
	cfg, _, ok := Parse([]byte(`{}`))
	if !ok {
		t.Fatal("空对象应取默认")
	}
	if cfg.QualityProbeEnabled {
		t.Fatal("quality_probe_enabled 默认必须 false")
	}
	if cfg.ProbeIntervalSeconds != 900 || cfg.MaxConsecutiveProbeFailures != 3 || cfg.ProbeBackoffSeconds != 3600 {
		t.Fatalf("探针默认参数异常: %+v", cfg)
	}
	if cfg.ProbeModel != "" {
		t.Fatalf("probe_model 默认空（跟随业务模型），得到 %q", cfg.ProbeModel)
	}
}

func TestProbeExplicitConfig(t *testing.T) {
	raw := `{"quality_probe_enabled":true,"probe_interval_seconds":300,` +
		`"probe_model":"gpt-6-astra","max_consecutive_probe_failures":2,` +
		`"probe_backoff_seconds":1800}`
	cfg, _, ok := Parse([]byte(raw))
	if !ok {
		t.Fatal("合法探针配置被拒")
	}
	if !cfg.QualityProbeEnabled || cfg.ProbeIntervalSeconds != 300 ||
		cfg.ProbeModel != "gpt-6-astra" || cfg.MaxConsecutiveProbeFailures != 2 ||
		cfg.ProbeBackoffSeconds != 1800 {
		t.Fatalf("探针配置未生效: %+v", cfg)
	}
	// 模型名应去空格。
	cfg2, _, _ := Parse([]byte(`{"probe_model":" gpt-6-astra "}`))
	if cfg2.ProbeModel != "gpt-6-astra" {
		t.Fatalf("probe_model 应 trim: %q", cfg2.ProbeModel)
	}
}

func TestProbeRejectsOutOfRange(t *testing.T) {
	cases := []string{
		`{"probe_interval_seconds":299}`,
		`{"probe_interval_seconds":7201}`,
		`{"max_consecutive_probe_failures":1}`,
		`{"max_consecutive_probe_failures":11}`,
		`{"probe_backoff_seconds":599}`,
		`{"probe_backoff_seconds":86401}`,
		`{"probe_model":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
	}
	for _, raw := range cases {
		if _, _, ok := Parse([]byte(raw)); ok {
			t.Errorf("应被拒绝却通过: %s", raw)
		}
	}
}

// v0.3：卡点排程默认开（探针已开启的前提下它只改排期密度，不加流量类型），
// 提前量默认 120s。旧宿主回放无新字段的配置必须得到这套默认。
func TestAdaptiveDefaults(t *testing.T) {
	cfg, _, ok := Parse([]byte(`{}`))
	if !ok {
		t.Fatal("空对象应取默认")
	}
	if !cfg.AdaptiveProbeScheduling {
		t.Fatal("adaptive_probe_scheduling 默认必须 true")
	}
	if cfg.ProbeScheduleMarginSeconds != 120 {
		t.Fatalf("probe_schedule_margin_seconds 默认 120，得到 %d", cfg.ProbeScheduleMarginSeconds)
	}
}

func TestAdaptiveExplicitConfig(t *testing.T) {
	cfg, _, ok := Parse([]byte(`{"adaptive_probe_scheduling":false,"probe_schedule_margin_seconds":300}`))
	if !ok {
		t.Fatal("合法卡点配置被拒")
	}
	if cfg.AdaptiveProbeScheduling || cfg.ProbeScheduleMarginSeconds != 300 {
		t.Fatalf("卡点配置未生效: %+v", cfg)
	}
}

func TestAdaptiveRejectsOutOfRange(t *testing.T) {
	for _, raw := range []string{
		`{"probe_schedule_margin_seconds":29}`,
		`{"probe_schedule_margin_seconds":601}`,
	} {
		if _, _, ok := Parse([]byte(raw)); ok {
			t.Errorf("应被拒绝却通过: %s", raw)
		}
	}
}

// ---------- v0.3.2 密集档（救治提速） ----------

// 密集档默认 120s/3/30：旧宿主回放无新字段的配置必须得到这套默认
// （救治号零业务流量，密集档是它唯一的快速攒证据通道）。
func TestBurstDefaults(t *testing.T) {
	cfg, _, ok := Parse([]byte(`{}`))
	if !ok {
		t.Fatal("空对象应取默认")
	}
	if cfg.ProbeBurstIntervalSeconds != 120 || cfg.ProbeBurstUntilPasses != 6 || cfg.ProbeBurstMaxProbes != 30 {
		t.Fatalf("密集档默认应 120/3/30: %+v", cfg)
	}
}

func TestBurstExplicitConfig(t *testing.T) {
	cfg, _, ok := Parse([]byte(`{"probe_burst_interval_seconds":90,"probe_burst_until_passes":4,"probe_burst_max_probes":20}`))
	if !ok {
		t.Fatal("合法密集档配置被拒")
	}
	if cfg.ProbeBurstIntervalSeconds != 90 || cfg.ProbeBurstUntilPasses != 4 || cfg.ProbeBurstMaxProbes != 20 {
		t.Fatalf("密集档配置未生效: %+v", cfg)
	}
}

func TestBurstRejectsOutOfRange(t *testing.T) {
	for _, raw := range []string{
		`{"probe_burst_interval_seconds":59}`,
		`{"probe_burst_interval_seconds":301}`,
		`{"probe_burst_until_passes":1}`,
		`{"probe_burst_until_passes":11}`,
		`{"probe_burst_max_probes":4}`,
		`{"probe_burst_max_probes":101}`,
	} {
		if _, _, ok := Parse([]byte(raw)); ok {
			t.Errorf("应被拒绝却通过: %s", raw)
		}
	}
}

// ---------- v0.3.3 探针考卷同形（probe_reasoning_effort） ----------

func TestProbeReasoningEffortDefault(t *testing.T) {
	cfg, _, ok := Parse([]byte(`{}`))
	if !ok {
		t.Fatal("空对象应取默认")
	}
	if cfg.ProbeReasoningEffort != "medium" {
		t.Fatalf("probe_reasoning_effort 默认 medium（对齐宿主资格针），得到 %q", cfg.ProbeReasoningEffort)
	}
	// 未配置该键的旧配置（存量宿主存储）也应落到 medium。
	cfg2, _, _ := Parse([]byte(`{"quality_probe_enabled":true,"probe_model":"gpt-6-astra"}`))
	if cfg2.ProbeReasoningEffort != "medium" {
		t.Fatalf("旧配置缺键应保留缺省 medium，得到 %q", cfg2.ProbeReasoningEffort)
	}
}

func TestProbeReasoningEffortExplicit(t *testing.T) {
	cfg, _, ok := Parse([]byte(`{"probe_reasoning_effort":"xhigh"}`))
	if !ok || cfg.ProbeReasoningEffort != "xhigh" {
		t.Fatalf("显式 effort 未生效: %+v", cfg)
	}
	// none（大小写不敏感）= 显式回退裸形态。
	for _, raw := range []string{`{"probe_reasoning_effort":"none"}`, `{"probe_reasoning_effort":" NONE "}`} {
		c, _, ok2 := Parse([]byte(raw))
		if !ok2 || c.ProbeReasoningEffort != "" {
			t.Fatalf("none 应归一为空串（裸形态）: %s -> %q", raw, c.ProbeReasoningEffort)
		}
	}
	// 大小写归一。
	c3, _, _ := Parse([]byte(`{"probe_reasoning_effort":"High"}`))
	if c3.ProbeReasoningEffort != "high" {
		t.Fatalf("effort 应小写归一: %q", c3.ProbeReasoningEffort)
	}
}

func TestProbeReasoningEffortRejects(t *testing.T) {
	for _, raw := range []string{
		`{"probe_reasoning_effort":"ultra"}`,
		`{"probe_reasoning_effort":"极高"}`,
	} {
		if _, _, ok := Parse([]byte(raw)); ok {
			t.Errorf("应被拒绝却通过: %s", raw)
		}
	}
}

// ---------- v0.3.4 判过钉推理门槛（probe_min_reasoning_tokens） ----------

func TestProbeMinReasoningTokensDefault(t *testing.T) {
	cfg, _, ok := Parse([]byte(`{}`))
	if !ok {
		t.Fatal("空对象应取默认")
	}
	if cfg.ProbeMinReasoningTokens != 800 {
		t.Fatalf("probe_min_reasoning_tokens 默认 800（对齐宿主资格针 rt 门槛），得到 %d", cfg.ProbeMinReasoningTokens)
	}
	// 未配置该键的旧配置（存量宿主存储）也应落到 800。
	cfg2, _, _ := Parse([]byte(`{"quality_probe_enabled":true,"probe_model":"gpt-6-astra"}`))
	if cfg2.ProbeMinReasoningTokens != 800 {
		t.Fatalf("旧配置缺键应保留缺省 800，得到 %d", cfg2.ProbeMinReasoningTokens)
	}
}

func TestProbeMinReasoningTokensExplicit(t *testing.T) {
	cfg, _, ok := Parse([]byte(`{"probe_min_reasoning_tokens":500}`))
	if !ok || cfg.ProbeMinReasoningTokens != 500 {
		t.Fatalf("显式门槛未生效: %+v", cfg)
	}
	// 1 = 事实关闭（逃生闸），必须可配。
	c, _, ok2 := Parse([]byte(`{"probe_min_reasoning_tokens":1}`))
	if !ok2 || c.ProbeMinReasoningTokens != 1 {
		t.Fatalf("门槛 1（事实关闭）应可配: %+v", c)
	}
}

func TestProbeMinReasoningTokensRejects(t *testing.T) {
	// 注：0 与缺省不可区分（本配置整型旋钮统一语义），0 落缺省 800 合法。
	for _, raw := range []string{
		`{"probe_min_reasoning_tokens":-5}`,
		`{"probe_min_reasoning_tokens":20000}`,
	} {
		if _, _, ok := Parse([]byte(raw)); ok {
			t.Errorf("应被拒绝却通过: %s", raw)
		}
	}
}
