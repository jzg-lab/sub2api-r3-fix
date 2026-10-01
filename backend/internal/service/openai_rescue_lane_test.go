package service

// 救治区编排器测试（Phase 3）：入口过滤器精确规则（design 0.3）/ Extra 标记
// JSON 往返容错 / EnterRescue 转换次序（标记-first→改绑→重读→开调度→种子→事件）/
// 幂等重入 / 种子失败容忍 / 开关闸与配置闸。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"
)

// rescueLaneRepo 状态化窄桩：嵌入 AccountRepository（nil 内嵌），覆盖编排器
// 触碰的四个方法；变更即时反映到内存账号，模拟 DB 读己之写。
type rescueLaneRepo struct {
	AccountRepository
	account   *Account
	getErr    error
	calls     []string
	binds     [][]int64
	extraSets []map[string]any
	schedSets []bool
}

func (r *rescueLaneRepo) GetByID(context.Context, int64) (*Account, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	if r.account == nil {
		return nil, errors.New("account not found")
	}
	clone := *r.account
	return &clone, nil
}

func (r *rescueLaneRepo) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	r.calls = append(r.calls, "extra")
	r.extraSets = append(r.extraSets, updates)
	if r.account.Extra == nil {
		r.account.Extra = map[string]any{}
	}
	for k, v := range updates {
		r.account.Extra[k] = v
	}
	return nil
}

func (r *rescueLaneRepo) BindGroups(_ context.Context, _ int64, groupIDs []int64) error {
	r.calls = append(r.calls, "bind")
	recorded := append([]int64(nil), groupIDs...)
	r.binds = append(r.binds, recorded)
	// 真实通道是删光重插 + priority 重写为 i+1；桩模拟其可观察副产物。
	r.account.GroupIDs = recorded
	r.account.Priority = 1
	return nil
}

func (r *rescueLaneRepo) SetSchedulable(_ context.Context, _ int64, enabled bool) error {
	r.calls = append(r.calls, "sched")
	r.schedSets = append(r.schedSets, enabled)
	r.account.Schedulable = enabled
	return nil
}

// rescueLaneSink 事件落库桩（同表同列的真实现 = 探针 store）。
type rescueLaneSink struct {
	OpenAIRescueLaneEventSink
	events []rescueLaneEventRecord
	err    error
}

type rescueLaneEventRecord struct {
	accountID int64
	eventType string
	details   map[string]any
}

func (s *rescueLaneSink) AppendOpenAIDowngradeEvent(
	_ context.Context, accountID int64, _ *int64, eventType string, details map[string]any,
) error {
	if s.err != nil {
		return s.err
	}
	s.events = append(s.events, rescueLaneEventRecord{accountID, eventType, details})
	return nil
}

func rescueLaneTestAccount() *Account {
	return &Account{
		ID:          42,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Priority:    7,
		GroupIDs:    []int64{3, 5},
		Schedulable: false,
		Extra:       map[string]any{},
	}
}

func rescueLaneEnabledConfig() OpenAIRescueLaneConfig {
	return OpenAIRescueLaneConfig{
		Enabled:                true,
		GroupID:                99,
		ConsecutiveCleanPasses: 6,
		ReconcileInterval:      5 * time.Minute,
	}
}

func newRescueLaneTestLane(repo *rescueLaneRepo, sink *rescueLaneSink) *OpenAIRescueLane {
	return NewOpenAIRescueLane(repo, sink, rescueLaneEnabledConfig, nil)
}

// ---------- 入口过滤器（design 0.3） ----------

func rescueLanePendingMutation(results ...OpenAIDowngradeProbeResult) *OpenAIDowngradeMutation {
	return &OpenAIDowngradeMutation{
		AccountID: 42,
		State:     &OpenAIDowngradeProbeState{State: OpenAIDowngradeStatePendingReplace},
		Results:   results,
	}
}

func TestShouldAutoEnterRescueLaneFilterMatrix(t *testing.T) {
	now := time.Now()
	cleanResult := OpenAIDowngradeProbeResult{TransportOK: true, AnswerCorrect: true, HTTPStatus: http.StatusOK}
	authResult := OpenAIDowngradeProbeResult{TransportOK: false, HTTPStatus: http.StatusUnauthorized}
	rateLimitedAccount := func(resetAt *time.Time) *Account {
		account := rescueLaneTestAccount()
		account.RateLimitResetAt = resetAt
		return account
	}
	cases := []struct {
		name     string
		mutation *OpenAIDowngradeMutation
		account  *Account
		want     bool
	}{
		{"判死提交+结果干净→入区", rescueLanePendingMutation(cleanResult), rescueLaneTestAccount(), true},
		{"无结果（纯状态推进）→入区", rescueLanePendingMutation(), rescueLaneTestAccount(), true},
		{"复核含401→排除（凭据线自理）", rescueLanePendingMutation(cleanResult, authResult), rescueLaneTestAccount(), false},
		{"仅401→排除", rescueLanePendingMutation(authResult), rescueLaneTestAccount(), false},
		{"限流持有中（重置点在未来）→排除", rescueLanePendingMutation(cleanResult), rateLimitedAccount(timePtr(now.Add(time.Hour))), false},
		{"限流已过（重置点在过去）→入区", rescueLanePendingMutation(cleanResult), rateLimitedAccount(timePtr(now.Add(-time.Minute))), true},
		{"非判死态→不进", &OpenAIDowngradeMutation{
			AccountID: 42,
			State:     &OpenAIDowngradeProbeState{State: OpenAIDowngradeStateOnDuty},
			Results:   []OpenAIDowngradeProbeResult{cleanResult},
		}, rescueLaneTestAccount(), false},
		{"nil mutation→不进", nil, rescueLaneTestAccount(), false},
		{"nil State→不进", &OpenAIDowngradeMutation{AccountID: 42}, rescueLaneTestAccount(), false},
		{"已在区（标记在场）→幂等不进", rescueLanePendingMutation(cleanResult), rescueLaneMarkedAccount(t), false},
		{"nil account→按无账号证据入区（钩子侧账号快照缺席）", rescueLanePendingMutation(cleanResult), nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ShouldAutoEnterRescueLane(tc.mutation, tc.account, now)
			if got != tc.want {
				t.Fatalf("ShouldAutoEnterRescueLane=%v, want %v", got, tc.want)
			}
		})
	}
}

func rescueLaneMarkedAccount(t *testing.T) *Account {
	t.Helper()
	account := rescueLaneTestAccount()
	marker := OpenAIRescueLaneMarker{
		EnteredAt:    time.Now().UTC().Truncate(time.Second),
		Trigger:      OpenAIRescueTriggerAuto,
		OrigGroupIDs: []int64{3, 5},
		OrigPriority: 7,
	}
	raw, err := json.Marshal(rescueLaneMarkerExtraValue(marker))
	if err != nil {
		t.Fatalf("marshal marker: %v", err)
	}
	var round map[string]any
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatalf("unmarshal marker: %v", err)
	}
	account.Extra[openAIRescueLaneExtraKey] = round
	return account
}

// ---------- Extra 标记读写 ----------

func TestRescueLaneMarkerJSONRoundtrip(t *testing.T) {
	account := rescueLaneMarkedAccount(t)
	marker := GetOpenAIRescueLaneMarker(account)
	if marker == nil {
		t.Fatalf("marker=nil after JSON roundtrip, want parsed")
	}
	if marker.Trigger != OpenAIRescueTriggerAuto {
		t.Fatalf("trigger=%q, want auto", marker.Trigger)
	}
	if marker.OrigPriority != 7 {
		t.Fatalf("orig_priority=%d, want 7", marker.OrigPriority)
	}
	if len(marker.OrigGroupIDs) != 2 || marker.OrigGroupIDs[0] != 3 || marker.OrigGroupIDs[1] != 5 {
		t.Fatalf("orig_group_ids=%v, want [3 5]", marker.OrigGroupIDs)
	}
}

func TestRescueLaneMarkerToleratesNativeTypes(t *testing.T) {
	// 非 JSON 往返形态（UpdateExtra 直接写内存对象后不经 DB 读回的路径）。
	account := rescueLaneTestAccount()
	account.Extra[openAIRescueLaneExtraKey] = map[string]any{
		"entered_at":     time.Now().UTC().Format(time.RFC3339),
		"trigger":        "manual",
		"orig_group_ids": []int64{11},
		"orig_priority":  int(4),
	}
	marker := GetOpenAIRescueLaneMarker(account)
	if marker == nil {
		t.Fatalf("marker=nil for native types")
	}
	if len(marker.OrigGroupIDs) != 1 || marker.OrigGroupIDs[0] != 11 {
		t.Fatalf("orig_group_ids=%v, want [11]", marker.OrigGroupIDs)
	}
	if marker.OrigPriority != 4 {
		t.Fatalf("orig_priority=%d, want 4", marker.OrigPriority)
	}
}

func TestRescueLaneMarkerRejectsMalformed(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name  string
		extra map[string]any
	}{
		{"entered_at 非字符串", map[string]any{"entered_at": 123}},
		{"entered_at 非法格式", map[string]any{"entered_at": "not-a-time"}},
		{"标记不是对象", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			account := rescueLaneTestAccount()
			if tc.extra == nil {
				account.Extra[openAIRescueLaneExtraKey] = "bogus"
			} else {
				account.Extra[openAIRescueLaneExtraKey] = tc.extra
			}
			if got := GetOpenAIRescueLaneMarker(account); got != nil {
				t.Fatalf("marker=%+v, want nil for malformed", got)
			}
		})
	}
	_ = now
}

// ---------- EnterRescue 转换 ----------

func TestEnterRescueRejectsWhenDisabled(t *testing.T) {
	lane := NewOpenAIRescueLane(&rescueLaneRepo{account: rescueLaneTestAccount()},
		&rescueLaneSink{}, nil, nil)
	if err := lane.EnterRescue(context.Background(), 42, OpenAIRescueTriggerAuto); !errors.Is(err, ErrRescueLaneDisabled) {
		t.Fatalf("err=%v, want ErrRescueLaneDisabled", err)
	}
}

func TestEnterRescueRejectsWhenGroupUnconfigured(t *testing.T) {
	cfg := rescueLaneEnabledConfig()
	cfg.GroupID = 0
	lane := NewOpenAIRescueLane(&rescueLaneRepo{account: rescueLaneTestAccount()},
		&rescueLaneSink{}, func() OpenAIRescueLaneConfig { return cfg }, nil)
	if err := lane.EnterRescue(context.Background(), 42, OpenAIRescueTriggerAuto); !errors.Is(err, ErrRescueLaneNotConfigured) {
		t.Fatalf("err=%v, want ErrRescueLaneNotConfigured", err)
	}
}

func TestEnterRescueRejectsIneligibleAccount(t *testing.T) {
	account := rescueLaneTestAccount()
	account.Platform = PlatformAnthropic
	lane := newRescueLaneTestLane(&rescueLaneRepo{account: account}, &rescueLaneSink{})
	if err := lane.EnterRescue(context.Background(), 42, OpenAIRescueTriggerAuto); !errors.Is(err, ErrRescueLaneIneligible) {
		t.Fatalf("err=%v, want ErrRescueLaneIneligible", err)
	}
}

func TestEnterRescueTransitionOrderAndEffects(t *testing.T) {
	repo := &rescueLaneRepo{account: rescueLaneTestAccount()}
	sink := &rescueLaneSink{}
	lane := newRescueLaneTestLane(repo, sink)
	fixed := time.Now().UTC().Truncate(time.Second)
	lane.now = func() time.Time { return fixed }
	seedCalls := 0
	lane.seed = func(context.Context, int64) error {
		seedCalls++
		return nil
	}

	if err := lane.EnterRescue(context.Background(), 42, OpenAIRescueTriggerReconcile); err != nil {
		t.Fatalf("EnterRescue: %v", err)
	}
	// 次序：标记-first（崩溃可对账补救）→ 改绑 → 重读后开调度。
	if len(repo.calls) != 3 || repo.calls[0] != "extra" || repo.calls[1] != "bind" || repo.calls[2] != "sched" {
		t.Fatalf("call order=%v, want [extra bind sched]", repo.calls)
	}
	if len(repo.binds) != 1 || len(repo.binds[0]) != 1 || repo.binds[0][0] != 99 {
		t.Fatalf("binds=%v, want single bind to rescue group 99", repo.binds)
	}
	if len(repo.schedSets) != 1 || !repo.schedSets[0] {
		t.Fatalf("schedSets=%v, want [true]", repo.schedSets)
	}
	if seedCalls != 1 {
		t.Fatalf("seed calls=%d, want 1", seedCalls)
	}
	// 标记快照保住原组与优先级（BindGroups 已把内存账号改写为救治组）。
	marker := GetOpenAIRescueLaneMarker(repo.account)
	if marker == nil {
		t.Fatalf("marker missing after EnterRescue")
	}
	if marker.Trigger != OpenAIRescueTriggerReconcile {
		t.Fatalf("trigger=%q, want reconcile", marker.Trigger)
	}
	if marker.OrigPriority != 7 {
		t.Fatalf("orig_priority=%d, want 7", marker.OrigPriority)
	}
	if len(marker.OrigGroupIDs) != 2 || marker.OrigGroupIDs[0] != 3 || marker.OrigGroupIDs[1] != 5 {
		t.Fatalf("orig_group_ids=%v, want [3 5]", marker.OrigGroupIDs)
	}
	if !marker.EnteredAt.Equal(fixed) {
		t.Fatalf("entered_at=%v, want %v", marker.EnteredAt, fixed)
	}
	// 事件：rescue_entered 带触发源与快照。
	if len(sink.events) != 1 {
		t.Fatalf("events=%d, want 1", len(sink.events))
	}
	event := sink.events[0]
	if event.eventType != OpenAIDowngradeEventRescueEntered || event.accountID != 42 {
		t.Fatalf("event=%+v, want rescue_entered for 42", event)
	}
	if event.details["trigger"] != OpenAIRescueTriggerReconcile {
		t.Fatalf("details.trigger=%v, want reconcile", event.details["trigger"])
	}
	if event.details["seed_ok"] != true {
		t.Fatalf("details.seed_ok=%v, want true", event.details["seed_ok"])
	}
}

func TestEnterRescueIdempotentSecondCall(t *testing.T) {
	repo := &rescueLaneRepo{account: rescueLaneTestAccount()}
	sink := &rescueLaneSink{}
	lane := newRescueLaneTestLane(repo, sink)

	if err := lane.EnterRescue(context.Background(), 42, OpenAIRescueTriggerAuto); err != nil {
		t.Fatalf("first EnterRescue: %v", err)
	}
	if err := lane.EnterRescue(context.Background(), 42, OpenAIRescueTriggerReconcile); err != nil {
		t.Fatalf("second EnterRescue: %v", err)
	}
	if len(repo.binds) != 1 {
		t.Fatalf("binds=%d, want 1 (idempotent re-entry must not rebind)", len(repo.binds))
	}
	if len(sink.events) != 1 {
		t.Fatalf("events=%d, want 1 (no duplicate rescue_entered)", len(sink.events))
	}
}

func TestEnterRescueToleratesSeedFailure(t *testing.T) {
	repo := &rescueLaneRepo{account: rescueLaneTestAccount()}
	sink := &rescueLaneSink{}
	lane := newRescueLaneTestLane(repo, sink)
	lane.seed = func(context.Context, int64) error { return errors.New("seed upstream refused") }

	if err := lane.EnterRescue(context.Background(), 42, OpenAIRescueTriggerAuto); err != nil {
		t.Fatalf("EnterRescue must not fail on seed error: %v", err)
	}
	if len(sink.events) != 1 || sink.events[0].details["seed_ok"] != false {
		t.Fatalf("events=%+v, want one rescue_entered with seed_ok=false", sink.events)
	}
	// 入区本身已落成：标记与改绑都在。
	if GetOpenAIRescueLaneMarker(repo.account) == nil {
		t.Fatalf("marker missing despite seed failure")
	}
	if len(repo.binds) != 1 {
		t.Fatalf("binds=%d, want 1", len(repo.binds))
	}
}

func TestEnterRescueSkipsSchedulableWhenAlreadyEnabled(t *testing.T) {
	account := rescueLaneTestAccount()
	account.Schedulable = true
	repo := &rescueLaneRepo{account: account}
	lane := newRescueLaneTestLane(repo, &rescueLaneSink{})

	if err := lane.EnterRescue(context.Background(), 42, OpenAIRescueTriggerManual); err != nil {
		t.Fatalf("EnterRescue: %v", err)
	}
	if len(repo.schedSets) != 0 {
		t.Fatalf("schedSets=%v, want none (already schedulable)", repo.schedSets)
	}
}

// ---------- 自动钩子（MaybeAutoEnterRescue，task 3.2） ----------

func TestMaybeAutoEnterRescueGateMatrix(t *testing.T) {
	clean := OpenAIDowngradeProbeResult{TransportOK: true, AnswerCorrect: true, HTTPStatus: http.StatusOK}
	auth := OpenAIDowngradeProbeResult{TransportOK: false, HTTPStatus: http.StatusUnauthorized}
	newLane := func(repo *rescueLaneRepo, sink *rescueLaneSink, enabled bool) *OpenAIRescueLane {
		cfg := rescueLaneEnabledConfig()
		cfg.Enabled = enabled
		return NewOpenAIRescueLane(repo, sink, func() OpenAIRescueLaneConfig { return cfg }, nil)
	}
	cases := []struct {
		name    string
		enabled bool
		mutation *OpenAIDowngradeMutation
		wantBinds int
	}{
		{"开关关→零调用（上线默认态）", false, rescueLanePendingMutation(clean), 0},
		{"开关开+判死干净→入区", true, rescueLanePendingMutation(clean), 1},
		{"开关开+401复核→不入区", true, rescueLanePendingMutation(auth), 0},
		{"开关开+非判死→不入区", true, &OpenAIDowngradeMutation{
			AccountID: 42,
			State:     &OpenAIDowngradeProbeState{State: OpenAIDowngradeStateOnDuty},
		}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &rescueLaneRepo{account: rescueLaneTestAccount()}
			sink := &rescueLaneSink{}
			lane := newLane(repo, sink, tc.enabled)
			lane.MaybeAutoEnterRescue(context.Background(), tc.mutation, repo.account)
			if len(repo.binds) != tc.wantBinds {
				t.Fatalf("binds=%d, want %d", len(repo.binds), tc.wantBinds)
			}
		})
	}
}

func TestMaybeAutoEnterRescueNilSafety(t *testing.T) {
	// nil 编排器（未注入）与 nil mutation 都不得 panic。
	var lane *OpenAIRescueLane
	lane.MaybeAutoEnterRescue(context.Background(), rescueLanePendingMutation(), nil)
	live := newRescueLaneTestLane(&rescueLaneRepo{account: rescueLaneTestAccount()}, &rescueLaneSink{})
	live.MaybeAutoEnterRescue(context.Background(), nil, nil)
}
