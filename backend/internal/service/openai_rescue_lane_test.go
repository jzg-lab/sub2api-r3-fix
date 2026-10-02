package service

// 救治区编排器测试（Phase 3）：入口过滤器精确规则（design 0.3）/ Extra 标记
// JSON 往返容错 / EnterRescue 转换次序（标记-first→改绑→重读→开调度→种子→事件）/
// 幂等重入 / 种子失败容忍 / 开关闸与配置闸。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// rescueLaneRepo 状态化窄桩：嵌入 AccountRepository（nil 内嵌），覆盖编排器
// 触碰的四个方法；变更即时反映到内存账号，模拟 DB 读己之写。
type rescueLaneRepo struct {
	AccountRepository
	account *Account
	// roster 清扫用多号名册（非空时 ListByPlatform/GetByID 按 ID 走它，
	// 空时退单号快路径——既有转换用例不改造）。
	roster    []Account
	getErr    error
	listErr   error
	listCalls int
	calls     []string
	binds     [][]int64
	extraSets []map[string]any
	schedSets []bool
}

// resolve 定位变更目标：名册优先按 ID 找，缺省退单号快路径。
func (r *rescueLaneRepo) resolve(id int64) *Account {
	for i := range r.roster {
		if r.roster[i].ID == id {
			return &r.roster[i]
		}
	}
	return r.account
}

func (r *rescueLaneRepo) ListByPlatform(_ context.Context, _ string) ([]Account, error) {
	r.listCalls++
	if r.listErr != nil {
		return nil, r.listErr
	}
	out := make([]Account, len(r.roster))
	copy(out, r.roster)
	return out, nil
}

func (r *rescueLaneRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	if len(r.roster) > 0 {
		if target := r.resolve(id); target != nil {
			clone := *target
			return &clone, nil
		}
		return nil, errors.New("account not found")
	}
	if r.account == nil {
		return nil, errors.New("account not found")
	}
	clone := *r.account
	return &clone, nil
}

func (r *rescueLaneRepo) UpdateExtra(_ context.Context, id int64, updates map[string]any) error {
	r.calls = append(r.calls, "extra")
	r.extraSets = append(r.extraSets, updates)
	target := r.resolve(id)
	if target == nil {
		return errors.New("account not found")
	}
	if target.Extra == nil {
		target.Extra = map[string]any{}
	}
	for k, v := range updates {
		target.Extra[k] = v
	}
	return nil
}

func (r *rescueLaneRepo) BindGroups(_ context.Context, id int64, groupIDs []int64) error {
	r.calls = append(r.calls, "bind")
	recorded := append([]int64(nil), groupIDs...)
	r.binds = append(r.binds, recorded)
	target := r.resolve(id)
	if target == nil {
		return errors.New("account not found")
	}
	// 真实通道是删光重插 + priority 重写为 i+1；桩模拟其可观察副产物。
	target.GroupIDs = recorded
	target.Priority = 1
	return nil
}

func (r *rescueLaneRepo) SetSchedulable(_ context.Context, id int64, enabled bool) error {
	r.calls = append(r.calls, "sched")
	r.schedSets = append(r.schedSets, enabled)
	target := r.resolve(id)
	if target == nil {
		return errors.New("account not found")
	}
	target.Schedulable = enabled
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
	// 次序：标记-first（崩溃可对账补救）→ 改绑 → 种子记账（末位 extra =
	// SeedOK/SeedAttempts 写回标记，清扫补种子的依据）。r17ba 起入区不开
	// 调度（在区期望形态 schedulable=false，唯一开调度点=考证通过）。
	if len(repo.calls) != 3 || repo.calls[0] != "extra" || repo.calls[1] != "bind" ||
		repo.calls[2] != "extra" {
		t.Fatalf("call order=%v, want [extra bind extra]", repo.calls)
	}
	if len(repo.extraSets) != 2 {
		t.Fatalf("extraSets=%d, want 2 (marker + seed bookkeeping)", len(repo.extraSets))
	}
	if len(repo.binds) != 1 || len(repo.binds[0]) != 1 || repo.binds[0][0] != 99 {
		t.Fatalf("binds=%v, want single bind to rescue group 99", repo.binds)
	}
	if len(repo.schedSets) != 0 {
		t.Fatalf("schedSets=%v, want none（入区不开调度）", repo.schedSets)
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

func TestEnterRescueTurnsOffPreexistingScheduling(t *testing.T) {
	// r17ba 用户裁定：在区一律不调度——判死前在岗残留的 schedulable=true
	// 入区即强制关（绝不在救治阶段进任何调度）。
	account := rescueLaneTestAccount()
	account.Schedulable = true
	repo := &rescueLaneRepo{account: account}
	lane := newRescueLaneTestLane(repo, &rescueLaneSink{})

	if err := lane.EnterRescue(context.Background(), 42, OpenAIRescueTriggerManual); err != nil {
		t.Fatalf("EnterRescue: %v", err)
	}
	if len(repo.schedSets) != 1 || repo.schedSets[0] {
		t.Fatalf("schedSets=%v, want [false]（在岗残留强制关）", repo.schedSets)
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
		name      string
		enabled   bool
		mutation  *OpenAIDowngradeMutation
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

// rescueLaneApplyMarker 以 JSON 往返形态打救治区标记（与 DB 读写形态一致）。
func rescueLaneApplyMarker(t *testing.T, account *Account, marker OpenAIRescueLaneMarker) {
	t.Helper()
	raw, err := json.Marshal(rescueLaneMarkerExtraValue(marker))
	if err != nil {
		t.Fatalf("marshal marker: %v", err)
	}
	var round map[string]any
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatalf("unmarshal marker: %v", err)
	}
	account.Extra[openAIRescueLaneExtraKey] = round
}

// ---------- 对账清扫（task 3.3） ----------

func rescueLaneSweepAccount(id int64) *Account {
	account := rescueLaneTestAccount()
	account.ID = id
	account.Status = StatusActive
	return account
}

func TestRunReconcileSweepHealsMarkedAccount(t *testing.T) {
	account := rescueLaneSweepAccount(51)
	rescueLaneApplyMarker(t, account, OpenAIRescueLaneMarker{
		EnteredAt:    time.Now().UTC(),
		Trigger:      OpenAIRescueTriggerAuto,
		OrigGroupIDs: []int64{3},
		OrigPriority: 5,
	})
	// 标记-first 崩溃窗残留：救治组绑定丢失（schedulable=false 是在区
	// 期望形态，r17ba 起不再是自愈项——唯一开调度点=考证通过）。
	account.GroupIDs = []int64{3}
	account.Schedulable = false
	repo := &rescueLaneRepo{roster: []Account{*account}}
	sink := &rescueLaneSink{}
	lane := newRescueLaneTestLane(repo, sink)

	entered, healed, _, err := lane.RunReconcileSweep(context.Background())
	if err != nil || entered != 0 || healed != 1 {
		t.Fatalf("entered=%d healed=%d err=%v, want 0/1/nil（只补绑不开调度）", entered, healed, err)
	}
	if len(repo.binds) != 1 || len(repo.binds[0]) != 1 || repo.binds[0][0] != 99 {
		t.Fatalf("binds=%v, want rebind to rescue group 99", repo.binds)
	}
	if len(repo.schedSets) != 0 {
		t.Fatalf("schedSets=%v, want none（在区不开调度）", repo.schedSets)
	}
	if len(sink.events) != 0 {
		t.Fatalf("heal must not emit events, got %d", len(sink.events))
	}
}

func TestRunReconcileSweepDoesNotResurrectSuspected(t *testing.T) {
	account := rescueLaneSweepAccount(52)
	rescueLaneApplyMarker(t, account, OpenAIRescueLaneMarker{
		EnteredAt:    time.Now().UTC(),
		Trigger:      OpenAIRescueTriggerAuto,
		OrigGroupIDs: []int64{3},
		OrigPriority: 5,
	})
	// 疑似账号级撤调（task 3.5 写入）：有意状态，清扫不得复活调度。
	account.Extra[openAIRescueSuspectedExtraKey] = true
	account.GroupIDs = []int64{99}
	account.Schedulable = false
	repo := &rescueLaneRepo{roster: []Account{*account}}
	lane := newRescueLaneTestLane(repo, &rescueLaneSink{})

	entered, healed, _, err := lane.RunReconcileSweep(context.Background())
	if err != nil || entered != 0 || healed != 0 {
		t.Fatalf("entered=%d healed=%d err=%v, want 0/0/nil", entered, healed, err)
	}
	if len(repo.schedSets) != 0 {
		t.Fatalf("schedSets=%v, want none（疑似撤调不是崩溃残留）", repo.schedSets)
	}
}

func TestRunReconcileSweepEntersPendingReplaceCandidate(t *testing.T) {
	repo := &rescueLaneRepo{roster: []Account{*rescueLaneSweepAccount(53)}}
	sink := &rescueLaneSink{}
	lane := newRescueLaneTestLane(repo, sink)
	lane.SetProbeStateSource(func(_ context.Context, ids []int64) (map[int64]OpenAIProbeHealthSnapshot, error) {
		states := make(map[int64]OpenAIProbeHealthSnapshot, len(ids))
		for _, id := range ids {
			states[id] = OpenAIProbeHealthSnapshot{AccountID: id, State: OpenAIDowngradeStatePendingReplace}
		}
		return states, nil
	})

	entered, healed, _, err := lane.RunReconcileSweep(context.Background())
	if err != nil || entered != 1 || healed != 0 {
		t.Fatalf("entered=%d healed=%d err=%v, want 1/0/nil", entered, healed, err)
	}
	if GetOpenAIRescueLaneMarker(&repo.roster[0]) == nil {
		t.Fatalf("marker missing after sweep enter")
	}
	if len(repo.binds) != 1 || repo.binds[0][0] != 99 {
		t.Fatalf("binds=%v, want bind to rescue group 99", repo.binds)
	}
	if len(sink.events) != 1 || sink.events[0].details["trigger"] != OpenAIRescueTriggerReconcile {
		t.Fatalf("events=%+v, want one rescue_entered{trigger:reconcile}", sink.events)
	}
}

func TestRunReconcileSweepSkipsIneligibleCandidates(t *testing.T) {
	authDead := rescueLaneSweepAccount(54)
	authDead.Status = StatusError // 凭据死（auth 两振 SetError）
	rateHeld := rescueLaneSweepAccount(55)
	reset := time.Now().Add(time.Hour)
	rateHeld.RateLimitResetAt = &reset // 429 持有中
	onDuty := rescueLaneSweepAccount(56)
	repo := &rescueLaneRepo{roster: []Account{*authDead, *rateHeld, *onDuty}}
	var asked []int64
	lane := newRescueLaneTestLane(repo, &rescueLaneSink{})
	lane.SetProbeStateSource(func(_ context.Context, ids []int64) (map[int64]OpenAIProbeHealthSnapshot, error) {
		asked = append(asked, ids...)
		states := make(map[int64]OpenAIProbeHealthSnapshot, len(ids))
		for _, id := range ids {
			states[id] = OpenAIProbeHealthSnapshot{AccountID: id, State: OpenAIDowngradeStateOnDuty}
		}
		return states, nil
	})

	entered, healed, _, err := lane.RunReconcileSweep(context.Background())
	if err != nil || entered != 0 || healed != 0 {
		t.Fatalf("entered=%d healed=%d err=%v, want 0/0/nil", entered, healed, err)
	}
	// 凭据死与限流持有不进候选；只有 in-service 号被问探针状态。
	if len(asked) != 1 || asked[0] != 56 {
		t.Fatalf("probeStates asked=%v, want [56]", asked)
	}
	if len(repo.extraSets) != 0 || len(repo.binds) != 0 {
		t.Fatalf("no mutation expected, extraSets=%d binds=%d", len(repo.extraSets), len(repo.binds))
	}
}

func TestRunReconcileSweepWithoutStateSourceSkipsEntering(t *testing.T) {
	// wire 断言失败降级态：无状态源 → 清扫只做标记侧自愈，不补进新号。
	repo := &rescueLaneRepo{roster: []Account{*rescueLaneSweepAccount(57)}}
	lane := newRescueLaneTestLane(repo, &rescueLaneSink{})

	entered, healed, _, err := lane.RunReconcileSweep(context.Background())
	if err != nil || entered != 0 || healed != 0 {
		t.Fatalf("entered=%d healed=%d err=%v, want 0/0/nil", entered, healed, err)
	}
	if len(repo.extraSets) != 0 || len(repo.binds) != 0 {
		t.Fatalf("no mutation expected without probe state source")
	}
}

func TestRunReconcileSweepDisabledIsNoOp(t *testing.T) {
	cfg := rescueLaneEnabledConfig()
	cfg.Enabled = false
	repo := &rescueLaneRepo{roster: []Account{*rescueLaneSweepAccount(58)}}
	lane := NewOpenAIRescueLane(repo, &rescueLaneSink{}, func() OpenAIRescueLaneConfig { return cfg }, nil)

	entered, healed, _, err := lane.RunReconcileSweep(context.Background())
	if err != nil || entered != 0 || healed != 0 {
		t.Fatalf("entered=%d healed=%d err=%v, want 0/0/nil", entered, healed, err)
	}
	if repo.listCalls != 0 {
		t.Fatalf("listCalls=%d, want 0（开关关连名单都不拉）", repo.listCalls)
	}
}

// ---------- 撤调/恢复（task 3.5，桥证据驱动） ----------

// rescueLaneBridgeAccountJSON 造单账号桥 JSON（in_backoff/连过/连错可调）。
func rescueLaneBridgeAccountJSON(id int64, inBackoff bool, passes, fails int, suspect bool, backoffUntil string) string {
	return `{"account_id":` + strconv.FormatInt(id, 10) +
		`,"consecutive_passes":` + strconv.Itoa(passes) +
		`,"consec_fails":` + strconv.Itoa(fails) +
		`,"suspect_account_level":` + strconv.FormatBool(suspect) +
		`,"in_backoff":` + strconv.FormatBool(inBackoff) +
		`,"backoff_until":"` + backoffUntil + `"}`
}

func rescueLaneSweepLaneWithBridge(repo *rescueLaneRepo, sink *rescueLaneSink, statusJSON string, haveStatusJSON bool) *OpenAIRescueLane {
	lane := newRescueLaneTestLane(repo, sink)
	lane.SetBridgeSource(func(context.Context) *PluginBridgeStatus {
		if !haveStatusJSON {
			return nil
		}
		return &PluginBridgeStatus{PluginID: 7, Running: true, Healthy: true, StatusJSON: statusJSON}
	})
	return lane
}

func TestRunReconcileSweepWithdrawsOnPluginBackoff(t *testing.T) {
	account := rescueLaneSweepAccount(61)
	rescueLaneApplyMarker(t, account, OpenAIRescueLaneMarker{
		EnteredAt: time.Now().UTC(), Trigger: OpenAIRescueTriggerAuto,
		OrigGroupIDs: []int64{3}, OrigPriority: 5,
	})
	account.GroupIDs = []int64{99}
	account.Schedulable = true
	repo := &rescueLaneRepo{roster: []Account{*account}}
	sink := &rescueLaneSink{}
	lane := rescueLaneSweepLaneWithBridge(repo, sink,
		rescueLaneBridgeJSON(rescueLaneBridgeAccountJSON(61, true, 0, 3, true, "2026-10-02T09:00:00Z")), true)

	_, _, withdrawn, err := lane.RunReconcileSweep(context.Background())
	if err != nil || withdrawn != 1 {
		t.Fatalf("withdrawn=%d err=%v, want 1/nil", withdrawn, err)
	}
	if len(repo.schedSets) != 1 || repo.schedSets[0] {
		t.Fatalf("schedSets=%v, want [false]（撤调停烧额度）", repo.schedSets)
	}
	if !GetOpenAIRescueSuspected(&repo.roster[0]) {
		t.Fatalf("suspected marker missing after withdraw")
	}
	if len(sink.events) != 1 || sink.events[0].eventType != OpenAIDowngradeEventRescueSuspected {
		t.Fatalf("events=%+v, want one rescue_suspected_account", sink.events)
	}
	if sink.events[0].details["consec_fails"] != 3 {
		t.Fatalf("details=%+v, want consec_fails=3", sink.events[0].details)
	}
}

func TestRunReconcileSweepWithdrawIsIdempotent(t *testing.T) {
	// 已撤调（标记在场）：桥仍报退避 → 不重撤、不重复事件。
	account := rescueLaneSweepAccount(62)
	rescueLaneApplyMarker(t, account, OpenAIRescueLaneMarker{
		EnteredAt: time.Now().UTC(), Trigger: OpenAIRescueTriggerAuto,
		OrigGroupIDs: []int64{3}, OrigPriority: 5,
	})
	account.GroupIDs = []int64{99}
	account.Schedulable = false
	account.Extra[openAIRescueSuspectedExtraKey] = true
	repo := &rescueLaneRepo{roster: []Account{*account}}
	sink := &rescueLaneSink{}
	lane := rescueLaneSweepLaneWithBridge(repo, sink,
		rescueLaneBridgeJSON(rescueLaneBridgeAccountJSON(62, true, 0, 3, true, "2026-10-02T09:00:00Z")), true)

	_, _, withdrawn, err := lane.RunReconcileSweep(context.Background())
	if err != nil || withdrawn != 0 {
		t.Fatalf("withdrawn=%d err=%v, want 0/nil（幂等）", withdrawn, err)
	}
	if len(repo.schedSets) != 0 || len(sink.events) != 0 {
		t.Fatalf("schedSets=%v events=%d, want no repeat action", repo.schedSets, len(sink.events))
	}
}

func TestRunReconcileSweepRestoresOnPluginPass(t *testing.T) {
	// 回暖证据：退避期满（in_backoff=false）+ 新过针（连过>0，suspect 已随
	// pass 清除）→ 清标记+恢复调度+事件。
	account := rescueLaneSweepAccount(63)
	rescueLaneApplyMarker(t, account, OpenAIRescueLaneMarker{
		EnteredAt: time.Now().UTC(), Trigger: OpenAIRescueTriggerAuto,
		OrigGroupIDs: []int64{3}, OrigPriority: 5,
	})
	account.GroupIDs = []int64{99}
	account.Schedulable = false
	account.Extra[openAIRescueSuspectedExtraKey] = true
	repo := &rescueLaneRepo{roster: []Account{*account}}
	sink := &rescueLaneSink{}
	lane := rescueLaneSweepLaneWithBridge(repo, sink,
		rescueLaneBridgeJSON(rescueLaneBridgeAccountJSON(63, false, 2, 0, false, "0001-01-01T00:00:00Z")), true)

	_, healed, _, err := lane.RunReconcileSweep(context.Background())
	if err != nil || healed != 1 {
		t.Fatalf("healed=%d err=%v, want 1/nil", healed, err)
	}
	if GetOpenAIRescueSuspected(&repo.roster[0]) {
		t.Fatalf("suspected marker still present after restore")
	}
	if len(repo.schedSets) != 0 {
		t.Fatalf("schedSets=%v, want none（回暖回救治中攒证据，不开调度——r17ba）", repo.schedSets)
	}
	if len(sink.events) != 1 || sink.events[0].eventType != OpenAIDowngradeEventRescueRecovered {
		t.Fatalf("events=%+v, want one rescue_recovered", sink.events)
	}
	if sink.events[0].details["basis"] != "plugin_pass" {
		t.Fatalf("details=%+v, want basis=plugin_pass", sink.events[0].details)
	}
}

func TestRunReconcileSweepHoldsWithdrawWithoutRecoveryEvidence(t *testing.T) {
	// 退避期满但尚无新过针（连过=0，还没复探）：维持撤调，等下一轮。
	account := rescueLaneSweepAccount(64)
	rescueLaneApplyMarker(t, account, OpenAIRescueLaneMarker{
		EnteredAt: time.Now().UTC(), Trigger: OpenAIRescueTriggerAuto,
		OrigGroupIDs: []int64{3}, OrigPriority: 5,
	})
	account.GroupIDs = []int64{99}
	account.Schedulable = false
	account.Extra[openAIRescueSuspectedExtraKey] = true
	repo := &rescueLaneRepo{roster: []Account{*account}}
	sink := &rescueLaneSink{}
	lane := rescueLaneSweepLaneWithBridge(repo, sink,
		rescueLaneBridgeJSON(rescueLaneBridgeAccountJSON(64, false, 0, 3, true, "0001-01-01T00:00:00Z")), true)

	_, healed, _, err := lane.RunReconcileSweep(context.Background())
	if err != nil || healed != 0 {
		t.Fatalf("healed=%d err=%v, want 0/nil（无回暖证据不动）", healed, err)
	}
	if len(repo.schedSets) != 0 || len(sink.events) != 0 {
		t.Fatalf("schedSets=%v events=%d, want no action", repo.schedSets, len(sink.events))
	}
	if !GetOpenAIRescueSuspected(&repo.roster[0]) {
		t.Fatalf("suspected marker must persist without recovery evidence")
	}
}

func TestRunReconcileSweepRestoresOnPluginStateLost(t *testing.T) {
	// 插件重启（账号从桥消失）：撤调依据已不存在，按陈旧撤调恢复——防永钉死。
	account := rescueLaneSweepAccount(65)
	rescueLaneApplyMarker(t, account, OpenAIRescueLaneMarker{
		EnteredAt: time.Now().UTC(), Trigger: OpenAIRescueTriggerAuto,
		OrigGroupIDs: []int64{3}, OrigPriority: 5,
	})
	account.GroupIDs = []int64{99}
	account.Schedulable = false
	account.Extra[openAIRescueSuspectedExtraKey] = true
	repo := &rescueLaneRepo{roster: []Account{*account}}
	sink := &rescueLaneSink{}
	lane := rescueLaneSweepLaneWithBridge(repo, sink, rescueLaneBridgeJSON(), true)

	_, healed, _, err := lane.RunReconcileSweep(context.Background())
	if err != nil || healed != 1 {
		t.Fatalf("healed=%d err=%v, want 1/nil", healed, err)
	}
	if GetOpenAIRescueSuspected(&repo.roster[0]) {
		t.Fatalf("suspected marker must be cleared after state-lost restore")
	}
	if len(repo.schedSets) != 0 {
		t.Fatalf("schedSets=%v, want none（回暖不开调度——r17ba）", repo.schedSets)
	}
	if len(sink.events) != 1 || sink.events[0].details["basis"] != "plugin_state_lost" {
		t.Fatalf("events=%+v, want basis=plugin_state_lost", sink.events)
	}
}

func TestRunReconcileSweepNoBridgeLeavesSchedulingAlone(t *testing.T) {
	// 桥缺席（未注入/无启用插件）：不撤不恢复——3.3 用例已证疑似不复活，
	// 此处证健康在区号也不会被无证据撤调。
	account := rescueLaneSweepAccount(66)
	rescueLaneApplyMarker(t, account, OpenAIRescueLaneMarker{
		EnteredAt: time.Now().UTC(), Trigger: OpenAIRescueTriggerAuto,
		OrigGroupIDs: []int64{3}, OrigPriority: 5,
	})
	account.GroupIDs = []int64{99}
	account.Schedulable = true
	repo := &rescueLaneRepo{roster: []Account{*account}}
	lane := rescueLaneSweepLaneWithBridge(repo, &rescueLaneSink{}, "", false)

	_, healed, _, err := lane.RunReconcileSweep(context.Background())
	if err != nil || healed != 0 {
		t.Fatalf("healed=%d err=%v, want 0/nil", healed, err)
	}
	if len(repo.schedSets) != 0 {
		t.Fatalf("schedSets=%v, want none（无桥证据不动调度）", repo.schedSets)
	}
}

func TestRescueLaneRecoveryEvidenceMatrix(t *testing.T) {
	cases := []struct {
		name      string
		bridge    *OpenAIPluginBridgeAccount
		want      bool
		wantBasis string
	}{
		{"账号从桥消失→恢复(state_lost)", nil, true, "plugin_state_lost"},
		{"退避中→维持", &OpenAIPluginBridgeAccount{InBackoff: true}, false, ""},
		{"suspect 旗标未清→维持", &OpenAIPluginBridgeAccount{SuspectAccountLevel: true}, false, ""},
		{"期满无新过针→维持", &OpenAIPluginBridgeAccount{}, false, ""},
		{"期满新过针→恢复(pass)", &OpenAIPluginBridgeAccount{ConsecutivePasses: 1}, true, "plugin_pass"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, basis := rescueLaneRecoveryEvidence(tc.bridge)
			if got != tc.want || basis != tc.wantBasis {
				t.Fatalf("evidence=%v basis=%q, want %v/%q", got, basis, tc.want, tc.wantBasis)
			}
		})
	}
}

// ---------- 转正（task 3.6：急挂钩 GraduateRescue + 清扫崩溃窗收敛） ----------

func TestGraduateRescueRebindsAndStamps(t *testing.T) {
	account := rescueLaneSweepAccount(71)
	rescueLaneApplyMarker(t, account, OpenAIRescueLaneMarker{
		EnteredAt: time.Now().UTC(), Trigger: OpenAIRescueTriggerAuto,
		OrigGroupIDs: []int64{3, 5}, OrigPriority: 7,
	})
	account.GroupIDs = []int64{99}
	account.Extra[openAIRescueSuspectedExtraKey] = true
	repo := &rescueLaneRepo{roster: []Account{*account}}
	sink := &rescueLaneSink{}
	lane := newRescueLaneTestLane(repo, sink)
	fixed := time.Now().UTC().Truncate(time.Second)
	lane.now = func() time.Time { return fixed }

	if err := lane.GraduateRescue(context.Background(), 71, "qualification_pass"); err != nil {
		t.Fatalf("GraduateRescue: %v", err)
	}
	// 改绑回原池组（快照还原），不再挂救治组。
	if len(repo.binds) != 1 || len(repo.binds[0]) != 2 ||
		repo.binds[0][0] != 3 || repo.binds[0][1] != 5 {
		t.Fatalf("binds=%v, want rebind to orig groups [3 5]", repo.binds)
	}
	saved := repo.resolve(71)
	if GetOpenAIRescueLaneMarker(saved) != nil {
		t.Fatalf("lane marker must be cleared after graduation")
	}
	if GetOpenAIRescueSuspected(saved) {
		t.Fatalf("suspected marker must be cleared after graduation")
	}
	rescuedAt, ok := saved.Extra[openAIRescueRescuedAtExtraKey].(string)
	if !ok || rescuedAt != fixed.Format(time.RFC3339) {
		t.Fatalf("rescued_at=%v, want %s", saved.Extra[openAIRescueRescuedAtExtraKey], fixed.Format(time.RFC3339))
	}
	if saved.Extra[openAIRescueRescueCountKey] != 1 {
		t.Fatalf("rescue_count=%v, want 1", saved.Extra[openAIRescueRescueCountKey])
	}
	if len(sink.events) != 1 || sink.events[0].eventType != OpenAIDowngradeEventRescueGraduated {
		t.Fatalf("events=%+v, want one rescue_graduated", sink.events)
	}
	if sink.events[0].details["basis"] != "qualification_pass" ||
		sink.events[0].details["rescue_count"] != 1 {
		t.Fatalf("details=%+v, want basis=qualification_pass count=1", sink.events[0].details)
	}
}

func TestGraduateRescueIdempotentWithoutMarker(t *testing.T) {
	repo := &rescueLaneRepo{roster: []Account{*rescueLaneSweepAccount(72)}}
	sink := &rescueLaneSink{}
	lane := newRescueLaneTestLane(repo, sink)

	if err := lane.GraduateRescue(context.Background(), 72, "sweep_converge"); err != nil {
		t.Fatalf("GraduateRescue on unmarked account: %v", err)
	}
	if len(repo.binds) != 0 || len(repo.extraSets) != 0 || len(sink.events) != 0 {
		t.Fatalf("binds=%d extraSets=%d events=%d, want zero mutations (idempotent)",
			len(repo.binds), len(repo.extraSets), len(sink.events))
	}
}

func TestGraduateRescueIncrementsRescueCount(t *testing.T) {
	account := rescueLaneSweepAccount(73)
	rescueLaneApplyMarker(t, account, OpenAIRescueLaneMarker{
		EnteredAt: time.Now().UTC(), Trigger: OpenAIRescueTriggerReconcile,
		OrigGroupIDs: []int64{3}, OrigPriority: 5,
	})
	account.Extra[openAIRescueRescueCountKey] = 2 // 二进宫：此前已救活过一次
	repo := &rescueLaneRepo{roster: []Account{*account}}
	lane := newRescueLaneTestLane(repo, &rescueLaneSink{})

	if err := lane.GraduateRescue(context.Background(), 73, "qualification_pass"); err != nil {
		t.Fatalf("GraduateRescue: %v", err)
	}
	if got := repo.resolve(73).Extra[openAIRescueRescueCountKey]; got != 3 {
		t.Fatalf("rescue_count=%v, want 3", got)
	}
}

// rescueLaneSweepLaneWithStates 注入批量探针快照源的清扫测试形态。
func rescueLaneSweepLaneWithStates(
	repo *rescueLaneRepo, sink *rescueLaneSink, states map[int64]OpenAIProbeHealthSnapshot,
) *OpenAIRescueLane {
	lane := newRescueLaneTestLane(repo, sink)
	lane.SetProbeStateSource(func(context.Context, []int64) (map[int64]OpenAIProbeHealthSnapshot, error) {
		return states, nil
	})
	return lane
}

func TestRunReconcileSweepGraduatesLeftoverMarker(t *testing.T) {
	// 急挂钩崩溃窗残留：考证已过（on_duty+normal）但标记还在 → 清扫收敛转正。
	account := rescueLaneSweepAccount(74)
	rescueLaneApplyMarker(t, account, OpenAIRescueLaneMarker{
		EnteredAt: time.Now().UTC(), Trigger: OpenAIRescueTriggerAuto,
		OrigGroupIDs: []int64{3}, OrigPriority: 5,
	})
	account.GroupIDs = []int64{99}
	account.Schedulable = true
	repo := &rescueLaneRepo{roster: []Account{*account}}
	sink := &rescueLaneSink{}
	lane := rescueLaneSweepLaneWithStates(repo, sink, map[int64]OpenAIProbeHealthSnapshot{
		74: {AccountID: 74, State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal"},
	})

	entered, healed, _, err := lane.RunReconcileSweep(context.Background())
	if err != nil || entered != 0 || healed != 1 {
		t.Fatalf("entered=%d healed=%d err=%v, want 0/1/nil", entered, healed, err)
	}
	if len(repo.binds) != 1 || len(repo.binds[0]) != 1 || repo.binds[0][0] != 3 {
		t.Fatalf("binds=%v, want rebind to orig group [3]", repo.binds)
	}
	if GetOpenAIRescueLaneMarker(&repo.roster[0]) != nil {
		t.Fatalf("marker must be cleared by sweep graduation")
	}
	if len(sink.events) != 1 || sink.events[0].details["basis"] != "sweep_converge" {
		t.Fatalf("events=%+v, want rescue_graduated{basis:sweep_converge}", sink.events)
	}
}

func TestRunReconcileSweepHoldsGraduationMidQualification(t *testing.T) {
	// on_duty+qualification：考证针还在飞——不收敛，维持救治。
	account := rescueLaneSweepAccount(75)
	rescueLaneApplyMarker(t, account, OpenAIRescueLaneMarker{
		EnteredAt: time.Now().UTC(), Trigger: OpenAIRescueTriggerAuto,
		OrigGroupIDs: []int64{3}, OrigPriority: 5,
	})
	account.GroupIDs = []int64{99}
	account.Schedulable = true
	repo := &rescueLaneRepo{roster: []Account{*account}}
	sink := &rescueLaneSink{}
	lane := rescueLaneSweepLaneWithStates(repo, sink, map[int64]OpenAIProbeHealthSnapshot{
		75: {AccountID: 75, State: OpenAIDowngradeStateOnDuty, ProbeMode: "qualification"},
	})

	entered, healed, _, err := lane.RunReconcileSweep(context.Background())
	if err != nil || entered != 0 || healed != 0 {
		t.Fatalf("entered=%d healed=%d err=%v, want 0/0/nil", entered, healed, err)
	}
	if len(repo.binds) != 0 || len(repo.extraSets) != 0 || len(sink.events) != 0 {
		t.Fatalf("binds=%d extraSets=%d events=%d, want no action mid-qualification",
			len(repo.binds), len(repo.extraSets), len(sink.events))
	}
	if GetOpenAIRescueLaneMarker(&repo.roster[0]) == nil {
		t.Fatalf("marker must persist while qualification is in flight")
	}
}

func TestRunReconcileSweepDoesNotGraduateReplacedAccount(t *testing.T) {
	// pending_replace+normal：考证挂了回判死——转正条件不成立，维持救治。
	account := rescueLaneSweepAccount(76)
	rescueLaneApplyMarker(t, account, OpenAIRescueLaneMarker{
		EnteredAt: time.Now().UTC(), Trigger: OpenAIRescueTriggerAuto,
		OrigGroupIDs: []int64{3}, OrigPriority: 5,
	})
	account.GroupIDs = []int64{99}
	account.Schedulable = true
	repo := &rescueLaneRepo{roster: []Account{*account}}
	sink := &rescueLaneSink{}
	lane := rescueLaneSweepLaneWithStates(repo, sink, map[int64]OpenAIProbeHealthSnapshot{
		76: {AccountID: 76, State: OpenAIDowngradeStatePendingReplace, ProbeMode: "normal"},
	})

	entered, healed, _, err := lane.RunReconcileSweep(context.Background())
	if err != nil || entered != 0 || healed != 0 {
		t.Fatalf("entered=%d healed=%d err=%v, want 0/0/nil", entered, healed, err)
	}
	if GetOpenAIRescueLaneMarker(&repo.roster[0]) == nil {
		t.Fatalf("marker must persist for replaced account")
	}
	if len(repo.binds) != 0 {
		t.Fatalf("binds=%v, want none（挂救治组不动）", repo.binds)
	}
}

// ---------- 救治调度通道 + 种子记账 + 凭据级出区 + 手动入口（补丁包 2026-10-02） ----------

func TestRescueSeedAuthRejectedClassifier(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("connection reset by peer"), false},
		{errors.New("context deadline exceeded"), false},
		{errors.New("API returned 401: Unauthorized"), true},
		{errors.New("API returned 403: Forbidden"), true},
		{errors.New("refresh failed: token_revoked"), true},
		{errors.New("oauth2: \"invalid_grant\""), true},
	}
	for _, tc := range cases {
		if got := rescueSeedAuthRejected(tc.err); got != tc.want {
			t.Fatalf("rescueSeedAuthRejected(%v)=%v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestEnterRescueSeedSuccessStampsMarker(t *testing.T) {
	repo := &rescueLaneRepo{account: rescueLaneTestAccount()}
	lane := newRescueLaneTestLane(repo, &rescueLaneSink{})
	lane.seed = func(context.Context, int64) error { return nil }

	if err := lane.EnterRescue(context.Background(), 42, OpenAIRescueTriggerAuto); err != nil {
		t.Fatalf("EnterRescue: %v", err)
	}
	marker := GetOpenAIRescueLaneMarker(repo.account)
	if marker == nil {
		t.Fatalf("marker missing")
	}
	if !marker.SeedOK || marker.SeedAttempts != 0 {
		// r17ba：成功清零连续失败计数（跨插件换代补种不吃上限额度）。
		t.Fatalf("seed bookkeeping: seed_ok=%v attempts=%d, want true/0", marker.SeedOK, marker.SeedAttempts)
	}
	if marker.LastSeedAt.IsZero() {
		t.Fatalf("seed bookkeeping: last_seed_at missing")
	}
}

func TestEnterRescueSeedAuthRejectedExitsAndReturnsSentinel(t *testing.T) {
	repo := &rescueLaneRepo{account: rescueLaneTestAccount()}
	sink := &rescueLaneSink{}
	lane := newRescueLaneTestLane(repo, sink)
	lane.seed = func(context.Context, int64) error {
		return errors.New("API returned 401: token_revoked")
	}

	err := lane.EnterRescue(context.Background(), 42, OpenAIRescueTriggerAuto)
	if !errors.Is(err, ErrRescueSeedAuthRejected) {
		t.Fatalf("EnterRescue err=%v, want ErrRescueSeedAuthRejected", err)
	}
	// 号回判死原位：标记清空、原组还回、调度关、冷却戳在窗内。
	if marker := GetOpenAIRescueLaneMarker(repo.account); marker != nil {
		t.Fatalf("marker must be cleared after auth-reject exit, got %+v", marker)
	}
	if len(repo.account.GroupIDs) != 2 || repo.account.GroupIDs[0] != 3 || repo.account.GroupIDs[1] != 5 {
		t.Fatalf("group_ids=%v, want orig [3 5]", repo.account.GroupIDs)
	}
	if repo.account.Schedulable {
		t.Fatalf("schedulable must be off after exit")
	}
	if !OpenAIRescueAuthRejectSuppressed(repo.account, time.Now()) {
		t.Fatalf("auth-reject cooldown tombstone missing after exit")
	}
	// 事件账：只有 rescue_auth_rejected（入区没成，不发 rescue_entered）。
	if len(sink.events) != 1 || sink.events[0].eventType != OpenAIDowngradeEventRescueAuthRejected {
		t.Fatalf("events=%+v, want single rescue_auth_rejected", sink.events)
	}
	// 调度轨迹：入区不开（r17ba），号本来就是关的，出区也无需再关。
	if len(repo.schedSets) != 0 {
		t.Fatalf("schedSets=%v, want none（全程零调度动作）", repo.schedSets)
	}
}

func TestEnterRescueSeedTransportFailureStaysInLane(t *testing.T) {
	repo := &rescueLaneRepo{account: rescueLaneTestAccount()}
	sink := &rescueLaneSink{}
	lane := newRescueLaneTestLane(repo, sink)
	lane.seed = func(context.Context, int64) error { return errors.New("dial tcp: timeout") }

	if err := lane.EnterRescue(context.Background(), 42, OpenAIRescueTriggerAuto); err != nil {
		t.Fatalf("non-auth seed failure must not fail EnterRescue: %v", err)
	}
	marker := GetOpenAIRescueLaneMarker(repo.account)
	if marker == nil || marker.SeedOK || marker.SeedAttempts != 1 {
		t.Fatalf("marker=%+v, want in-lane with seed_ok=false attempts=1", marker)
	}
	if len(repo.account.GroupIDs) != 1 || repo.account.GroupIDs[0] != 99 {
		t.Fatalf("group_ids=%v, account must stay bound to rescue group", repo.account.GroupIDs)
	}
	if len(sink.events) != 1 || sink.events[0].eventType != OpenAIDowngradeEventRescueEntered ||
		sink.events[0].details["seed_ok"] != false {
		t.Fatalf("events=%+v, want rescue_entered{seed_ok:false}", sink.events)
	}
}

func TestSweepResumesExitInProgress(t *testing.T) {
	account := rescueLaneSweepAccount(61)
	rescueLaneApplyMarker(t, account, OpenAIRescueLaneMarker{
		EnteredAt:    time.Now().UTC(),
		Trigger:      OpenAIRescueTriggerAuto,
		OrigGroupIDs: []int64{3},
		OrigPriority: 5,
		ExitReason:   openAIRescueExitAuthRejected, // 出区半途崩溃窗：清扫续走
	})
	account.GroupIDs = []int64{99}
	account.Schedulable = true
	repo := &rescueLaneRepo{roster: []Account{*account}}
	sink := &rescueLaneSink{}
	lane := newRescueLaneTestLane(repo, sink)

	entered, healed, _, err := lane.RunReconcileSweep(context.Background())
	if err != nil || entered != 0 || healed != 0 {
		t.Fatalf("entered=%d healed=%d err=%v, want 0/0/nil (exit is not healing)", entered, healed, err)
	}
	target := &repo.roster[0]
	if GetOpenAIRescueLaneMarker(target) != nil {
		t.Fatalf("marker must be cleared after exit resume")
	}
	if len(target.GroupIDs) != 1 || target.GroupIDs[0] != 3 {
		t.Fatalf("group_ids=%v, want orig [3]", target.GroupIDs)
	}
	if target.Schedulable {
		t.Fatalf("schedulable must be off after exit resume")
	}
	if len(sink.events) != 1 || sink.events[0].eventType != OpenAIDowngradeEventRescueAuthRejected {
		t.Fatalf("events=%+v, want rescue_auth_rejected", sink.events)
	}
}

func TestSweepReseedsUnseededMarkedAccount(t *testing.T) {
	account := rescueLaneSweepAccount(62)
	rescueLaneApplyMarker(t, account, OpenAIRescueLaneMarker{
		EnteredAt:    time.Now().UTC(),
		Trigger:      OpenAIRescueTriggerAuto,
		OrigGroupIDs: []int64{3},
		OrigPriority: 5,
		SeedAttempts: 1, // 半进区：种子还没打成功
	})
	account.GroupIDs = []int64{99}
	account.Schedulable = true
	repo := &rescueLaneRepo{roster: []Account{*account}}
	lane := newRescueLaneTestLane(repo, &rescueLaneSink{})
	seedCalls := 0
	lane.seed = func(context.Context, int64) error {
		seedCalls++
		return nil
	}

	if _, _, _, err := lane.RunReconcileSweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if seedCalls != 1 {
		t.Fatalf("seed calls=%d, want 1 (reseed unseeded marked account)", seedCalls)
	}
	marker := GetOpenAIRescueLaneMarker(&repo.roster[0])
	if marker == nil || !marker.SeedOK || marker.SeedAttempts != 0 {
		// 成功补种清零连续失败计数（原语义 attempts=2 → r17ba 连续失败语义 0）。
		t.Fatalf("marker=%+v, want seed_ok=true attempts=0", marker)
	}
}

func TestSweepSeedAttemptsCapStopsReseed(t *testing.T) {
	account := rescueLaneSweepAccount(63)
	rescueLaneApplyMarker(t, account, OpenAIRescueLaneMarker{
		EnteredAt:    time.Now().UTC(),
		Trigger:      OpenAIRescueTriggerAuto,
		OrigGroupIDs: []int64{3},
		OrigPriority: 5,
		SeedAttempts: openAIRescueSeedMaxAttempts, // 上限已到：不再空打
	})
	account.GroupIDs = []int64{99}
	account.Schedulable = true
	repo := &rescueLaneRepo{roster: []Account{*account}}
	lane := newRescueLaneTestLane(repo, &rescueLaneSink{})
	seedCalls := 0
	lane.seed = func(context.Context, int64) error {
		seedCalls++
		return nil
	}

	if _, _, _, err := lane.RunReconcileSweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if seedCalls != 0 {
		t.Fatalf("seed calls=%d, want 0 (attempts cap reached)", seedCalls)
	}
}

// r17ba 插件失忆补种：SeedOK=true 只证种子曾打过；插件进程换代丢光内存
// 模板后 prober 未跟踪本号，清扫须补种（生产实证：0.2.1→0.3.1 换代后
// 1218/1219 停在零针）。
func TestSweepReseedsWhenPluginForgotTemplate(t *testing.T) {
	account := rescueLaneSweepAccount(65)
	rescueLaneApplyMarker(t, account, OpenAIRescueLaneMarker{
		EnteredAt:    time.Now().UTC(),
		Trigger:      OpenAIRescueTriggerAuto,
		OrigGroupIDs: []int64{3},
		OrigPriority: 5,
		SeedOK:       true,
		LastSeedAt:   time.Now().UTC().Add(-openAIRescueReseedInterval - time.Minute),
	})
	account.GroupIDs = []int64{99}
	account.Schedulable = true
	repo := &rescueLaneRepo{roster: []Account{*account}}
	lane := rescueLaneSweepLaneWithBridge(repo, &rescueLaneSink{},
		rescueLaneBridgeJSON(), true) // 桥在线、prober 启用、accounts 空=失忆
	seedCalls := 0
	lane.seed = func(context.Context, int64) error {
		seedCalls++
		return nil
	}

	if _, _, _, err := lane.RunReconcileSweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if seedCalls != 1 {
		t.Fatalf("seed calls=%d, want 1 (amnesia reseed)", seedCalls)
	}
}

func TestSweepNoReseedWhileAwaitingFirstProbe(t *testing.T) {
	// 模板刚喂上（LastSeedAt 新鲜）：等插件首针进视图，不重复补种。
	account := rescueLaneSweepAccount(66)
	rescueLaneApplyMarker(t, account, OpenAIRescueLaneMarker{
		EnteredAt:    time.Now().UTC(),
		Trigger:      OpenAIRescueTriggerAuto,
		OrigGroupIDs: []int64{3},
		OrigPriority: 5,
		SeedOK:       true,
		LastSeedAt:   time.Now().UTC().Add(-time.Minute),
	})
	account.GroupIDs = []int64{99}
	account.Schedulable = true
	repo := &rescueLaneRepo{roster: []Account{*account}}
	lane := rescueLaneSweepLaneWithBridge(repo, &rescueLaneSink{}, rescueLaneBridgeJSON(), true)
	seedCalls := 0
	lane.seed = func(context.Context, int64) error {
		seedCalls++
		return nil
	}

	if _, _, _, err := lane.RunReconcileSweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if seedCalls != 0 {
		t.Fatalf("seed calls=%d, want 0 (throttled: awaiting first probe)", seedCalls)
	}
}

func TestSweepNoReseedWhenProberTracksAccount(t *testing.T) {
	// prober 已跟踪（有模板有探针）：SeedOK=true 的常规态，不补种。
	account := rescueLaneSweepAccount(67)
	rescueLaneApplyMarker(t, account, OpenAIRescueLaneMarker{
		EnteredAt:    time.Now().UTC(),
		Trigger:      OpenAIRescueTriggerAuto,
		OrigGroupIDs: []int64{3},
		OrigPriority: 5,
		SeedOK:       true,
		LastSeedAt:   time.Now().UTC().Add(-time.Hour),
	})
	account.GroupIDs = []int64{99}
	account.Schedulable = true
	repo := &rescueLaneRepo{roster: []Account{*account}}
	lane := rescueLaneSweepLaneWithBridge(repo, &rescueLaneSink{},
		rescueLaneBridgeJSON(rescueLaneBridgeAccountJSON(67, false, 3, 0, false, "0001-01-01T00:00:00Z")), true)
	seedCalls := 0
	lane.seed = func(context.Context, int64) error {
		seedCalls++
		return nil
	}

	if _, _, _, err := lane.RunReconcileSweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if seedCalls != 0 {
		t.Fatalf("seed calls=%d, want 0 (tracked by prober)", seedCalls)
	}
}

func TestSweepNoAmnesiaReseedWithoutBridge(t *testing.T) {
	// 桥缺席（无启用插件/旧装配）：无失忆证据，SeedOK=true 不补种
	//（无桥补种=每轮空打上游）。
	account := rescueLaneSweepAccount(68)
	rescueLaneApplyMarker(t, account, OpenAIRescueLaneMarker{
		EnteredAt:    time.Now().UTC(),
		Trigger:      OpenAIRescueTriggerAuto,
		OrigGroupIDs: []int64{3},
		OrigPriority: 5,
		SeedOK:       true,
		LastSeedAt:   time.Now().UTC().Add(-time.Hour),
	})
	account.GroupIDs = []int64{99}
	account.Schedulable = true
	repo := &rescueLaneRepo{roster: []Account{*account}}
	lane := rescueLaneSweepLaneWithBridge(repo, &rescueLaneSink{}, "", false)
	seedCalls := 0
	lane.seed = func(context.Context, int64) error {
		seedCalls++
		return nil
	}

	if _, _, _, err := lane.RunReconcileSweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if seedCalls != 0 {
		t.Fatalf("seed calls=%d, want 0 (no bridge evidence)", seedCalls)
	}
}

func TestSweepReseedAuthRejectedExitsLane(t *testing.T) {
	account := rescueLaneSweepAccount(64)
	rescueLaneApplyMarker(t, account, OpenAIRescueLaneMarker{
		EnteredAt:    time.Now().UTC(),
		Trigger:      OpenAIRescueTriggerAuto,
		OrigGroupIDs: []int64{3},
		OrigPriority: 5,
	})
	account.GroupIDs = []int64{99}
	account.Schedulable = true
	repo := &rescueLaneRepo{roster: []Account{*account}}
	sink := &rescueLaneSink{}
	lane := newRescueLaneTestLane(repo, sink)
	lane.seed = func(context.Context, int64) error {
		return errors.New("API returned 403: forbidden")
	}

	entered, _, _, err := lane.RunReconcileSweep(context.Background())
	if err != nil || entered != 0 {
		t.Fatalf("entered=%d err=%v, want 0/nil (suppressed by cooldown after exit)", entered, err)
	}
	target := &repo.roster[0]
	if GetOpenAIRescueLaneMarker(target) != nil {
		t.Fatalf("marker must be cleared after reseed auth-reject")
	}
	if len(target.GroupIDs) != 1 || target.GroupIDs[0] != 3 {
		t.Fatalf("group_ids=%v, want orig [3]", target.GroupIDs)
	}
}

func TestSweepDoesNotReenterWithinAuthRejectCooldown(t *testing.T) {
	account := rescueLaneSweepAccount(65)
	// 上轮凭据级出区留下的冷却戳（标记已清）：Status 仍 Active + 探针态
	// pending_replace，若无冷却闸清扫会每轮再补进空转。
	account.Extra[openAIRescueAuthRejectedAtExtraKey] = time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	repo := &rescueLaneRepo{roster: []Account{*account}}
	lane := newRescueLaneTestLane(repo, &rescueLaneSink{})
	lane.SetProbeStateSource(func(_ context.Context, ids []int64) (map[int64]OpenAIProbeHealthSnapshot, error) {
		states := make(map[int64]OpenAIProbeHealthSnapshot, len(ids))
		for _, id := range ids {
			states[id] = OpenAIProbeHealthSnapshot{AccountID: id, State: OpenAIDowngradeStatePendingReplace}
		}
		return states, nil
	})

	entered, _, _, err := lane.RunReconcileSweep(context.Background())
	if err != nil || entered != 0 {
		t.Fatalf("entered=%d err=%v, want 0/nil (cooldown suppresses re-entry)", entered, err)
	}
	if len(repo.binds) != 0 {
		t.Fatalf("binds=%v, want none during cooldown", repo.binds)
	}
}

func TestEnterRescueClearsStaleAuthRejectCooldown(t *testing.T) {
	account := rescueLaneTestAccount()
	account.Extra[openAIRescueAuthRejectedAtExtraKey] = time.Now().UTC().Format(time.RFC3339)
	repo := &rescueLaneRepo{account: account}
	lane := newRescueLaneTestLane(repo, &rescueLaneSink{})

	// 手动入口不受冷却闸限制，且成功入区清掉旧戳。
	entered, err := lane.EnterRescueManual(context.Background(), 42)
	if err != nil || !entered {
		t.Fatalf("EnterRescueManual entered=%v err=%v, want true/nil (manual bypasses cooldown)", entered, err)
	}
	// 桩的 UpdateExtra 以 nil 值代键删除（真实现 JSONB 合并删键），读侧
	// nil = 不压制——按行为断言而非键存在性。
	if OpenAIRescueAuthRejectSuppressed(repo.account, time.Now()) {
		t.Fatalf("stale cooldown must stop suppressing after entry")
	}
}

func TestMaybeAutoEnterRescueRespectsAuthRejectCooldown(t *testing.T) {
	account := rescueLaneTestAccount()
	account.Status = StatusActive
	account.Extra[openAIRescueAuthRejectedAtExtraKey] = time.Now().UTC().Format(time.RFC3339)
	repo := &rescueLaneRepo{account: account}
	lane := newRescueLaneTestLane(repo, &rescueLaneSink{})

	clean := OpenAIDowngradeProbeResult{TransportOK: true, AnswerCorrect: true, HTTPStatus: http.StatusOK}
	lane.MaybeAutoEnterRescue(context.Background(), rescueLanePendingMutation(clean), repo.account)
	if len(repo.binds) != 0 {
		t.Fatalf("binds=%v, auto hook must respect cooldown tombstone", repo.binds)
	}
}

func TestEnterRescueManualIdempotentSecondCall(t *testing.T) {
	repo := &rescueLaneRepo{account: rescueLaneTestAccount()}
	lane := newRescueLaneTestLane(repo, &rescueLaneSink{})

	entered, err := lane.EnterRescueManual(context.Background(), 42)
	if err != nil || !entered {
		t.Fatalf("first manual enter: entered=%v err=%v, want true/nil", entered, err)
	}
	entered, err = lane.EnterRescueManual(context.Background(), 42)
	if err != nil || entered {
		t.Fatalf("second manual enter: entered=%v err=%v, want false/nil (already in lane)", entered, err)
	}
	if len(repo.binds) != 1 {
		t.Fatalf("binds=%d, want 1 (idempotent)", len(repo.binds))
	}
}

func TestEnterRescueManualPropagatesAuthRejection(t *testing.T) {
	repo := &rescueLaneRepo{account: rescueLaneTestAccount()}
	lane := newRescueLaneTestLane(repo, &rescueLaneSink{})
	lane.seed = func(context.Context, int64) error {
		return errors.New("API returned 401: unauthorized")
	}

	entered, err := lane.EnterRescueManual(context.Background(), 42)
	if entered {
		t.Fatalf("entered=true, want false on auth rejection")
	}
	if !errors.Is(err, ErrRescueSeedAuthRejected) {
		t.Fatalf("err=%v, want ErrRescueSeedAuthRejected (manual surfaces it)", err)
	}
	// 号已回判死原位。
	if GetOpenAIRescueLaneMarker(repo.account) != nil || repo.account.Schedulable {
		t.Fatalf("account must be back at pending_replace rest position")
	}
}

// ---------- 调度闸预检（r17ba：手动暂停被清扫完整尊重） ----------

// TestSweepNeverOpensSchedulingInLane r17ba 用户裁定「没确认救活绝不进
// 正式调用」的结构性锁：在区号（无论暂停与否）清扫没有任何开调度路径
//（r17az 每轮撞 DB 触发器 WARN 刷屏的根治）。
func TestSweepNeverOpensSchedulingInLane(t *testing.T) {
	account := rescueLaneSweepAccount(81)
	rescueLaneApplyMarker(t, account, OpenAIRescueLaneMarker{
		EnteredAt:    time.Now().UTC(),
		Trigger:      OpenAIRescueTriggerAuto,
		OrigGroupIDs: []int64{3},
		OrigPriority: 5,
	})
	account.GroupIDs = []int64{99}
	account.Schedulable = false // 用户暂停后的现场形态
	repo := &rescueLaneRepo{roster: []Account{*account}}
	lane := newRescueLaneTestLane(repo, &rescueLaneSink{}) // 无闸也成立：结构性无开调度路径

	entered, healed, withdrawn, err := lane.RunReconcileSweep(context.Background())
	if err != nil || entered != 0 || healed != 0 || withdrawn != 0 {
		t.Fatalf("entered=%d healed=%d withdrawn=%d err=%v, want 0/0/0/nil", entered, healed, withdrawn, err)
	}
	if len(repo.schedSets) != 0 {
		t.Fatalf("schedSets=%v, want none（在区零开调度路径）", repo.schedSets)
	}
}

// TestSweepSkipsEnteringManuallyPausedCandidates 手动暂停的判死号不入区：
// 静置刹车（r17an 语义）下清扫强拉入区 = 换绑 + 开调度双违反。
func TestSweepSkipsEnteringManuallyPausedCandidates(t *testing.T) {
	repo := &rescueLaneRepo{roster: []Account{*rescueLaneSweepAccount(82)}}
	lane := newRescueLaneTestLane(repo, &rescueLaneSink{})
	lane.SetProbeStateSource(func(_ context.Context, ids []int64) (map[int64]OpenAIProbeHealthSnapshot, error) {
		states := make(map[int64]OpenAIProbeHealthSnapshot, len(ids))
		for _, id := range ids {
			states[id] = OpenAIProbeHealthSnapshot{AccountID: id, State: OpenAIDowngradeStatePendingReplace}
		}
		return states, nil
	})
	lane.SetSchedulingGate(func(context.Context, int64) (bool, error) { return false, nil })

	entered, _, _, err := lane.RunReconcileSweep(context.Background())
	if err != nil || entered != 0 {
		t.Fatalf("entered=%d err=%v, want 0/nil（暂停候选不入区）", entered, err)
	}
	if len(repo.binds) != 0 || GetOpenAIRescueLaneMarker(&repo.roster[0]) != nil {
		t.Fatalf("binds=%d marker=%v, want 未入区原样", len(repo.binds), GetOpenAIRescueLaneMarker(&repo.roster[0]))
	}
}

// TestEnterRescueKeepsPauseBrakeOnManualEntry 手动送入暂停号：入区照常
//（种子/插件探针不经宿主调度），调度保持关——解暂停由转正点击显式完成。
func TestEnterRescueKeepsPauseBrakeOnManualEntry(t *testing.T) {
	repo := &rescueLaneRepo{account: rescueLaneTestAccount()}
	lane := newRescueLaneTestLane(repo, &rescueLaneSink{})
	seedCalls := 0
	lane.seed = func(context.Context, int64) error {
		seedCalls++
		return nil
	}

	entered, err := lane.EnterRescueManual(context.Background(), 42)
	if err != nil || !entered {
		t.Fatalf("EnterRescueManual entered=%v err=%v, want true/nil", entered, err)
	}
	if seedCalls != 1 {
		t.Fatalf("seed calls=%d, want 1（种子照喂）", seedCalls)
	}
	if len(repo.schedSets) != 0 {
		t.Fatalf("schedSets=%v, want none（入区零调度动作）", repo.schedSets)
	}
	if GetOpenAIRescueLaneMarker(repo.account) == nil {
		t.Fatalf("marker missing after manual entry")
	}
}
