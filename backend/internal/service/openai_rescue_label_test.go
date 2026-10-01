package service

// 救治区标签计算测试（Phase 3.4）：桥 status_json 容错解析 / 标记 JSON 串
// 通道 / 标签裁决矩阵（救治中/已复活/疑似账号级/无插件证据）/ 健康列表
// 覆盖（paused 与 rate_limited 优先、离线缓存可点、注记字段映射）。

import (
	"context"
	"testing"
	"time"
)

func rescueLaneBridgeJSON(accounts ...string) string {
	joined := ""
	for i, a := range accounts {
		if i > 0 {
			joined += ","
		}
		joined += a
	}
	return `{"enabled":true,"interval_seconds":900,"adaptive_scheduling":true,` +
		`"prober":{"enabled":true,"interval_seconds":900,"adaptive_scheduling":true,"model":"gpt-5",` +
		`"accounts":[` + joined + `],"probes":10,"fails":2,"quality_rerolls":1,"suspect_account_levels":1}}`
}

func TestParseOpenAIPluginBridgeProber(t *testing.T) {
	prober := ParseOpenAIPluginBridgeProber(rescueLaneBridgeJSON(
		`{"account_id":42,"consecutive_passes":7,"consec_fails":0,"suspect_account_level":false,`+
			`"in_backoff":false,"backoff_until":"0001-01-01T00:00:00Z","last_probe_at":"2026-10-02T08:00:00Z",`+
			`"sign_captured_at":"2026-10-02T07:00:00Z","estimated_remaining_seconds":1800,"estimate_basis":"measured"}`,
		`{"account_id":43,"consecutive_passes":0,"consec_fails":3,"suspect_account_level":true,`+
			`"in_backoff":true,"backoff_until":"2026-10-02T09:00:00Z","last_probe_at":"2026-10-02T08:40:00Z"}`,
	))
	if prober == nil {
		t.Fatalf("prober=nil for valid status_json")
	}
	if !prober.Enabled {
		t.Fatalf("enabled=false, want true")
	}
	ok := prober.Accounts[42]
	if ok == nil || ok.ConsecutivePasses != 7 || ok.InBackoff || ok.SuspectAccountLevel {
		t.Fatalf("account 42 = %+v, want 7 passes / not in backoff", ok)
	}
	if ok.LastProbeAt.IsZero() || !ok.LastProbeAt.Equal(time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("last_probe_at=%v, want 2026-10-02T08:00:00Z", ok.LastProbeAt)
	}
	sus := prober.Accounts[43]
	if sus == nil || !sus.InBackoff || !sus.SuspectAccountLevel || sus.ConsecFails != 3 {
		t.Fatalf("account 43 = %+v, want backoff/suspected/3 fails", sus)
	}
	if !sus.BackoffUntil.Equal(time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("backoff_until=%v, want 09:00Z", sus.BackoffUntil)
	}

	for name, raw := range map[string]string{
		"空串":         ``,
		"坏 JSON":     `{"prober":`,
		"无 prober 键": `{"enabled":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			if got := ParseOpenAIPluginBridgeProber(raw); got != nil {
				t.Fatalf("prober=%+v, want nil for %q", got, raw)
			}
		})
	}
}

func TestParseOpenAIRescueLaneMarkerJSON(t *testing.T) {
	raw := `{"entered_at":"2026-10-02T03:04:05Z","trigger":"reconcile","orig_group_ids":[3,5],"orig_priority":7}`
	marker := ParseOpenAIRescueLaneMarkerJSON(raw)
	if marker == nil {
		t.Fatalf("marker=nil for valid JSON")
	}
	if marker.Trigger != OpenAIRescueTriggerReconcile || marker.OrigPriority != 7 {
		t.Fatalf("marker=%+v", marker)
	}
	if len(marker.OrigGroupIDs) != 2 || marker.OrigGroupIDs[0] != 3 || marker.OrigGroupIDs[1] != 5 {
		t.Fatalf("orig_group_ids=%v", marker.OrigGroupIDs)
	}
	for name, bad := range map[string]string{
		"空串":           ``,
		"坏 JSON":       `{`,
		"缺 entered_at": `{"trigger":"auto"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if got := ParseOpenAIRescueLaneMarkerJSON(bad); got != nil {
				t.Fatalf("marker=%+v, want nil for %q", got, bad)
			}
		})
	}
}

func TestLabelOpenAIRescueAccountMatrix(t *testing.T) {
	marker := &OpenAIRescueLaneMarker{EnteredAt: time.Now().UTC(), Trigger: OpenAIRescueTriggerAuto}
	cases := []struct {
		name      string
		threshold int
		bridge    *OpenAIPluginBridgeAccount
		wantLabel string
		wantColor string
		wantClick bool
	}{
		{"无插件证据→救治中", 6, nil, OpenAIHealthLabelRescuing, OpenAIHealthColorBluePurple, false},
		{"连过不足→救治中", 6, &OpenAIPluginBridgeAccount{ConsecutivePasses: 5}, OpenAIHealthLabelRescuing, OpenAIHealthColorBluePurple, false},
		{"连过达标→已复活可点", 6, &OpenAIPluginBridgeAccount{ConsecutivePasses: 6}, OpenAIHealthLabelRevived, OpenAIHealthColorGreen, true},
		{"连过超额→已复活", 6, &OpenAIPluginBridgeAccount{ConsecutivePasses: 9}, OpenAIHealthLabelRevived, OpenAIHealthColorGreen, true},
		{"退避中→疑似账号级不可点", 6, &OpenAIPluginBridgeAccount{ConsecutivePasses: 2, InBackoff: true}, OpenAIHealthLabelSuspected, OpenAIHealthColorGrayRed, false},
		{"退避期满复探窗口（backoff 翻回 false）→回升救治中", 6, &OpenAIPluginBridgeAccount{ConsecutivePasses: 0, SuspectAccountLevel: true}, OpenAIHealthLabelRescuing, OpenAIHealthColorBluePurple, false},
		{"阈值零→按默认 6 裁决", 0, &OpenAIPluginBridgeAccount{ConsecutivePasses: 6}, OpenAIHealthLabelRevived, OpenAIHealthColorGreen, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			label, color, clickable, _ := LabelOpenAIRescueAccount(marker, tc.threshold, tc.bridge)
			if label != tc.wantLabel || color != tc.wantColor || clickable != tc.wantClick {
				t.Fatalf("label=%s color=%s clickable=%v, want %s/%s/%v",
					label, color, clickable, tc.wantLabel, tc.wantColor, tc.wantClick)
			}
		})
	}
}

func TestBuildOpenAIAccountRescueHealth(t *testing.T) {
	entered := time.Now().UTC().Truncate(time.Second)
	until := entered.Add(time.Hour)
	marker := &OpenAIRescueLaneMarker{EnteredAt: entered, Trigger: OpenAIRescueTriggerReconcile}
	bridge := &OpenAIPluginBridgeAccount{
		ConsecutivePasses:   4,
		ConsecFails:         0,
		SuspectAccountLevel: false,
		InBackoff:           false,
		BackoffUntil:        until,
	}
	note := BuildOpenAIAccountRescueHealth(marker, 6, bridge, true)
	if note == nil {
		t.Fatalf("note=nil, want annotation")
	}
	if !note.EnteredAt.Equal(entered) || note.Trigger != OpenAIRescueTriggerReconcile {
		t.Fatalf("note=%+v", note)
	}
	if note.ConsecutivePasses != 4 || note.GraduationThreshold != 6 || !note.PluginOffline {
		t.Fatalf("note=%+v, want passes=4 threshold=6 offline=true", note)
	}
	if note.BackoffUntil == nil || !note.BackoffUntil.Equal(until) {
		t.Fatalf("backoff_until=%v, want %v", note.BackoffUntil, until)
	}
	// 无桥数据：计数字段零值呈现（诚实：无数据 ≠ 探针结论为零）。
	bare := BuildOpenAIAccountRescueHealth(marker, 6, nil, false)
	if bare == nil || bare.ConsecutivePasses != 0 || bare.PluginOffline {
		t.Fatalf("bare=%+v, want zero passes / not offline", bare)
	}
	if BuildOpenAIAccountRescueHealth(nil, 6, bridge, false) != nil {
		t.Fatalf("nil marker must yield nil annotation")
	}
}

// ---------- 健康列表覆盖（ListOpenAIAccountHealth × 救治区标签） ----------

func rescueLaneHealthRunner(snapshot OpenAIProbeHealthSnapshot, bridge func(context.Context) *PluginBridgeStatus, lane *OpenAIRescueLane) *OpenAIDowngradeProbeRunner {
	runner := NewOpenAIDowngradeProbeRunner(&healthListerStore{snapshots: []OpenAIProbeHealthSnapshot{snapshot}}, nil, nil, nil, nil, nil)
	if bridge != nil {
		runner.SetPluginBridgeSource(bridge)
	}
	runner.SetRescueLane(lane)
	return runner
}

func rescueLaneInLaneSnapshot(t *testing.T, accountID int64, mutate func(*OpenAIProbeHealthSnapshot)) OpenAIProbeHealthSnapshot {
	t.Helper()
	marker := &OpenAIRescueLaneMarker{
		EnteredAt:    time.Now().UTC().Truncate(time.Second),
		Trigger:      OpenAIRescueTriggerAuto,
		OrigGroupIDs: []int64{3},
		OrigPriority: 5,
	}
	snapshot := OpenAIProbeHealthSnapshot{
		AccountID: accountID, State: OpenAIDowngradeStatePendingReplace, ProbeMode: "normal",
		Schedulable: true, RescueMarker: marker,
	}
	if mutate != nil {
		mutate(&snapshot)
	}
	return snapshot
}

func TestListOpenAIAccountHealthRescueRevivedOverride(t *testing.T) {
	lane := newRescueLaneTestLane(&rescueLaneRepo{account: rescueLaneTestAccount()}, &rescueLaneSink{})
	runner := rescueLaneHealthRunner(
		rescueLaneInLaneSnapshot(t, 42, nil),
		func(context.Context) *PluginBridgeStatus {
			return &PluginBridgeStatus{
				PluginID: 7, Running: true, Healthy: true, Offline: false,
				StatusJSON: rescueLaneBridgeJSON(`{"account_id":42,"consecutive_passes":7,"consec_fails":0,"in_backoff":false}`),
			}
		},
		lane,
	)

	result, err := runner.ListOpenAIAccountHealth(context.Background(), []int64{42})
	if err != nil {
		t.Fatalf("ListOpenAIAccountHealth: %v", err)
	}
	row := result.Accounts[0]
	if row.Label != OpenAIHealthLabelRevived || row.LabelColor != OpenAIHealthColorGreen || !row.Clickable {
		t.Fatalf("row=%+v, want revived/green/clickable", row)
	}
	if row.Rescue == nil || row.Rescue.ConsecutivePasses != 7 || row.Rescue.GraduationThreshold != 6 {
		t.Fatalf("rescue=%+v, want annotation passes=7 threshold=6", row.Rescue)
	}
	if row.Rescue.PluginOffline {
		t.Fatalf("plugin_offline=true, want false（桥在线）")
	}
}

func TestListOpenAIAccountHealthRescuingWithoutBridge(t *testing.T) {
	// 桥源未注入（无启用插件/旧装配）：救治中 + 离线降级注记，调度语义不变。
	lane := newRescueLaneTestLane(&rescueLaneRepo{account: rescueLaneTestAccount()}, &rescueLaneSink{})
	runner := rescueLaneHealthRunner(rescueLaneInLaneSnapshot(t, 42, nil), nil, lane)

	result, err := runner.ListOpenAIAccountHealth(context.Background(), []int64{42})
	if err != nil {
		t.Fatalf("ListOpenAIAccountHealth: %v", err)
	}
	row := result.Accounts[0]
	if row.Label != OpenAIHealthLabelRescuing || row.Clickable {
		t.Fatalf("row=%+v, want rescuing/not clickable", row)
	}
	if row.Rescue == nil || !row.Rescue.PluginOffline || row.Rescue.ConsecutivePasses != 0 {
		t.Fatalf("rescue=%+v, want offline zero-passes annotation", row.Rescue)
	}
}

func TestListOpenAIAccountHealthOfflineBridgeKeepsRevivedFromCache(t *testing.T) {
	// 桥离线但缓存 status_json 连过达标：已复活仍可点（转正必须考证，冷针
	// 拦截过期数据），离线事实经 plugin_offline 透出给前端角标。
	lane := newRescueLaneTestLane(&rescueLaneRepo{account: rescueLaneTestAccount()}, &rescueLaneSink{})
	runner := rescueLaneHealthRunner(
		rescueLaneInLaneSnapshot(t, 42, nil),
		func(context.Context) *PluginBridgeStatus {
			return &PluginBridgeStatus{
				PluginID: 7, Running: true, Offline: true,
				StatusJSON: rescueLaneBridgeJSON(`{"account_id":42,"consecutive_passes":8,"consec_fails":0,"in_backoff":false}`),
			}
		},
		lane,
	)

	result, err := runner.ListOpenAIAccountHealth(context.Background(), []int64{42})
	if err != nil {
		t.Fatalf("ListOpenAIAccountHealth: %v", err)
	}
	row := result.Accounts[0]
	if row.Label != OpenAIHealthLabelRevived || !row.Clickable {
		t.Fatalf("row=%+v, want revived clickable from cached bridge data", row)
	}
	if row.Rescue == nil || !row.Rescue.PluginOffline {
		t.Fatalf("rescue=%+v, want plugin_offline=true", row.Rescue)
	}
}

func TestListOpenAIAccountHealthPausedWinsOverRescue(t *testing.T) {
	// 手动暂停是账号级压制态：暂停标签优先，救治注记不挂（恢复后自然回显）。
	lane := newRescueLaneTestLane(&rescueLaneRepo{account: rescueLaneTestAccount()}, &rescueLaneSink{})
	snapshot := rescueLaneInLaneSnapshot(t, 42, func(s *OpenAIProbeHealthSnapshot) { s.ManualPaused = true })
	runner := rescueLaneHealthRunner(snapshot, nil, lane)

	result, err := runner.ListOpenAIAccountHealth(context.Background(), []int64{42})
	if err != nil {
		t.Fatalf("ListOpenAIAccountHealth: %v", err)
	}
	row := result.Accounts[0]
	if row.Label != OpenAIHealthLabelPaused || row.Rescue != nil {
		t.Fatalf("row=%+v, want paused without rescue annotation", row)
	}
}

func TestListOpenAIAccountHealthSuspectedBackoff(t *testing.T) {
	lane := newRescueLaneTestLane(&rescueLaneRepo{account: rescueLaneTestAccount()}, &rescueLaneSink{})
	// RFC3339 串无亚秒：期望值先截秒再入 JSON，回读后 Equal 成立。
	until := time.Now().UTC().Add(40 * time.Minute).Truncate(time.Second)
	runner := rescueLaneHealthRunner(
		rescueLaneInLaneSnapshot(t, 42, nil),
		func(context.Context) *PluginBridgeStatus {
			return &PluginBridgeStatus{
				PluginID: 7, Running: true, Healthy: true,
				StatusJSON: rescueLaneBridgeJSON(`{"account_id":42,"consecutive_passes":0,"consec_fails":3,` +
					`"suspect_account_level":true,"in_backoff":true,"backoff_until":"` + until.Format(time.RFC3339) + `"}`),
			}
		},
		lane,
	)

	result, err := runner.ListOpenAIAccountHealth(context.Background(), []int64{42})
	if err != nil {
		t.Fatalf("ListOpenAIAccountHealth: %v", err)
	}
	row := result.Accounts[0]
	if row.Label != OpenAIHealthLabelSuspected || row.Clickable {
		t.Fatalf("row=%+v, want suspected/not clickable", row)
	}
	if row.Rescue == nil || !row.Rescue.InBackoff || !row.Rescue.SuspectAccountLevel {
		t.Fatalf("rescue=%+v, want backoff annotation", row.Rescue)
	}
	if row.Rescue.BackoffUntil == nil || !row.Rescue.BackoffUntil.Equal(until) {
		t.Fatalf("backoff_until=%v, want %v", row.Rescue.BackoffUntil, until)
	}
}

func TestListOpenAIAccountHealthNonMemberUnchanged(t *testing.T) {
	// 不在区的判死号：沿用相位A 标签（problem/红/可点→手动启用），无注记。
	lane := newRescueLaneTestLane(&rescueLaneRepo{account: rescueLaneTestAccount()}, &rescueLaneSink{})
	snapshot := OpenAIProbeHealthSnapshot{AccountID: 42, State: OpenAIDowngradeStatePendingReplace, ProbeMode: "normal"}
	runner := rescueLaneHealthRunner(snapshot, nil, lane)

	result, err := runner.ListOpenAIAccountHealth(context.Background(), []int64{42})
	if err != nil {
		t.Fatalf("ListOpenAIAccountHealth: %v", err)
	}
	row := result.Accounts[0]
	if row.Label != OpenAIHealthLabelProblem || !row.Clickable || row.Rescue != nil {
		t.Fatalf("row=%+v, want problem/clickable/no rescue", row)
	}
}
