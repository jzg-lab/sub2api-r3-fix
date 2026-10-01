package service

// 插件状态桥测试（救治区 Phase 2.5）：旧插件无字段兼容 / 非法 JSON 丢弃 /
// RPC 失败不报错不推进健康锚点 / 缓存命中不重探 / 离线判定矩阵（读取时刻
// 现算）/ 离线自愈 / BridgeStatus 无启用插件 → nil / 健康快照信封透传。

import (
	"context"
	"errors"
	"testing"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"google.golang.org/grpc"
)

// bridgeHealthClient 只伪造 Health RPC：嵌入接口获得其余方法（本文件内
// 不会被调用），runtime 经 &pluginRuntime{api: fake} 注入——probeStatus 只
// 触碰 .api。
type bridgeHealthClient struct {
	pluginv1.TransportPluginClient
	health *pluginv1.HealthResponse
	err    error
	calls  int
}

func (c *bridgeHealthClient) Health(
	ctx context.Context, in *pluginv1.HealthRequest, opts ...grpc.CallOption,
) (*pluginv1.HealthResponse, error) {
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	return c.health, nil
}

// bridgeStatusRepo 只覆盖 List：嵌入 PluginRepository 接口（nil 内嵌），
// 与 pluginTokenRepository 同款窄桩模式。
type bridgeStatusRepo struct {
	PluginRepository
	installations []*PluginInstallation
}

func (r *bridgeStatusRepo) List(context.Context) ([]*PluginInstallation, error) {
	return r.installations, nil
}

func (r *bridgeStatusRepo) GetByID(_ context.Context, id int64) (*PluginInstallation, error) {
	for _, installation := range r.installations {
		if installation.ID == id {
			return installation, nil
		}
	}
	return nil, errors.New("plugin not found")
}

func newBridgeTestManager(t *testing.T) (*PluginManager, *bridgeHealthClient) {
	t.Helper()
	manager := NewPluginManager(
		&bridgeStatusRepo{installations: []*PluginInstallation{enabledBridgeInstallation()}},
		nil, nil, PluginHostInfo{})
	fake := &bridgeHealthClient{}
	manager.mu.Lock()
	manager.runtimes[7] = &pluginRuntime{api: fake}
	manager.mu.Unlock()
	return manager, fake
}

func TestPluginStatusRunningPluginNoStatusField(t *testing.T) {
	// 旧插件（0.2.x）Health 不带 status_json：健康结论正常，StatusJSON 空。
	manager, fake := newBridgeTestManager(t)
	fake.health = &pluginv1.HealthResponse{Healthy: true, Message: "ok"}

	status, err := manager.Status(context.Background(), 7)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !status.Running || !status.Healthy {
		t.Fatalf("running=%v healthy=%v, want true/true", status.Running, status.Healthy)
	}
	if status.StatusJSON != "" {
		t.Fatalf("StatusJSON=%q, want empty for old plugin", status.StatusJSON)
	}
	if status.Offline {
		t.Fatalf("fresh successful Health must not be offline")
	}
	if status.LastHealthyAt.IsZero() {
		t.Fatalf("LastHealthyAt must advance on successful Health RPC")
	}
}

func TestPluginStatusDropsInvalidStatusJSON(t *testing.T) {
	manager, fake := newBridgeTestManager(t)
	fake.health = &pluginv1.HealthResponse{Healthy: true, StatusJson: "{not-json"}

	status, err := manager.Status(context.Background(), 7)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !status.Healthy {
		t.Fatalf("healthy=false, want true (JSON validity不影响健康结论)")
	}
	if status.StatusJSON != "" {
		t.Fatalf("StatusJSON=%q, want dropped invalid JSON", status.StatusJSON)
	}
}

func TestPluginStatusRPCFailureKeepsQuiet(t *testing.T) {
	// RPC 失败是观测事实不是异常：不报错、Running=false、健康锚点不推进。
	manager, fake := newBridgeTestManager(t)
	fake.err = errors.New("boom")

	status, err := manager.Status(context.Background(), 7)
	if err != nil {
		t.Fatalf("Status must not error on RPC failure: %v", err)
	}
	if status.Running || status.Healthy {
		t.Fatalf("running=%v healthy=%v, want false/false", status.Running, status.Healthy)
	}
	if status.Message == "" {
		t.Fatalf("failure message must be present")
	}
	if !status.LastHealthyAt.IsZero() {
		t.Fatalf("LastHealthyAt=%v, want zero (never healthy this probe)", status.LastHealthyAt)
	}
}

func TestPluginStatusCacheServedWithoutReprobe(t *testing.T) {
	manager, fake := newBridgeTestManager(t)
	fake.health = &pluginv1.HealthResponse{Healthy: true}

	for i := 0; i < 3; i++ {
		if _, err := manager.Status(context.Background(), 7); err != nil {
			t.Fatalf("Status #%d: %v", i, err)
		}
	}
	if fake.calls != 1 {
		t.Fatalf("Health calls=%d, want 1 (cache-first)", fake.calls)
	}
}

func TestPluginStatusOfflineMatrix(t *testing.T) {
	// 离线在读取时刻现算：直接种子缓存验证矩阵。
	manager, _ := newBridgeTestManager(t)
	now := time.Now()
	cases := []struct {
		name          string
		lastHealthyAt time.Time
		checkedAt     time.Time
		wantOffline   bool
	}{
		{"成功锚点3分钟前→离线", now.Add(-3 * time.Minute), now.Add(-3 * time.Minute), true},
		{"成功锚点1分钟前→在线", now.Add(-1 * time.Minute), now.Add(-1 * time.Minute), false},
		{"从未成功+探测证据3分钟前→离线", time.Time{}, now.Add(-3 * time.Minute), true},
		{"从未成功+探测证据30秒前→宽限在线", time.Time{}, now.Add(-30 * time.Second), false},
		{"两时间戳皆零（从未探测）→不结论", time.Time{}, time.Time{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manager.statusMu.Lock()
			manager.statusCache[7] = &PluginStatus{
				PluginID: 7, CheckedAt: tc.checkedAt, LastHealthyAt: tc.lastHealthyAt,
			}
			manager.statusMu.Unlock()
			got := manager.statusSnapshot(context.Background(), 7)
			if got.Offline != tc.wantOffline {
				t.Fatalf("offline=%v, want %v", got.Offline, tc.wantOffline)
			}
		})
	}
}

func TestPluginStatusOfflineSelfHealsOnRecovery(t *testing.T) {
	// 离线（成功锚点3分钟前）→ 插件恢复应答 → 下一次探测自愈回在线。
	manager, fake := newBridgeTestManager(t)
	now := time.Now()
	manager.statusMu.Lock()
	manager.statusCache[7] = &PluginStatus{
		PluginID: 7, CheckedAt: now.Add(-3 * time.Minute),
		LastHealthyAt: now.Add(-3 * time.Minute),
	}
	manager.statusMu.Unlock()
	if got := manager.statusSnapshot(context.Background(), 7); !got.Offline {
		t.Fatalf("precondition: must start offline")
	}

	fake.health = &pluginv1.HealthResponse{Healthy: true}
	// statusSnapshot 缓存命中不会重探；直接经 probeStatus 走一轮轮询恢复。
	if _, err := manager.probeStatus(context.Background(), 7); err != nil {
		t.Fatalf("probeStatus: %v", err)
	}
	got := manager.statusSnapshot(context.Background(), 7)
	if got.Offline {
		t.Fatalf("offline after recovery, want self-healed online")
	}
	if got.LastHealthyAt.IsZero() {
		t.Fatalf("LastHealthyAt must advance on recovery probe")
	}
}

func TestPluginStatusRPCFailurePreservesPreviousHealthyAnchor(t *testing.T) {
	// 一轮成功后跟一轮失败：失败轮不得抹掉旧的成功时刻（cacheStatus 整条
	// 替换，证据保全逻辑必须沿用 previous.LastHealthyAt）。
	manager, fake := newBridgeTestManager(t)
	fake.health = &pluginv1.HealthResponse{Healthy: true}
	if _, err := manager.Status(context.Background(), 7); err != nil {
		t.Fatalf("first Status: %v", err)
	}
	manager.statusMu.RLock()
	anchor := manager.statusCache[7].LastHealthyAt
	manager.statusMu.RUnlock()
	if anchor.IsZero() {
		t.Fatalf("precondition: anchor must be set")
	}

	fake.health = nil
	fake.err = errors.New("boom")
	if _, err := manager.probeStatus(context.Background(), 7); err != nil {
		t.Fatalf("failed probeStatus: %v", err)
	}
	manager.statusMu.RLock()
	preserved := manager.statusCache[7].LastHealthyAt
	manager.statusMu.RUnlock()
	if !preserved.Equal(anchor) {
		t.Fatalf("LastHealthyAt=%v, want preserved %v", preserved, anchor)
	}
}

func enabledBridgeInstallation() *PluginInstallation {
	return &PluginInstallation{
		ID: 7, Name: "lb-cookie-pin", Version: "0.3.0",
		State:    PluginStateEnabled,
		Bindings: []PluginBinding{{
			ID: 1, PluginID: 7, Capability: PluginCapabilityOpenAIOAuthOutbound,
			Platform: PlatformOpenAI, AccountType: AccountTypeOAuth,
			Enabled: true, RolloutPercent: 100,
		}},
	}
}

func TestPluginManagerBridgeStatusNilWithoutEnabledPlugin(t *testing.T) {
	manager := NewPluginManager(&bridgeStatusRepo{}, nil, nil, PluginHostInfo{})
	if got := manager.BridgeStatus(context.Background()); got != nil {
		t.Fatalf("BridgeStatus=%+v, want nil without enabled plugin", got)
	}
}

func TestPluginManagerBridgeStatusNilOnRepoError(t *testing.T) {
	// 仓库读失败按无桥处理，绝不报错拖垮健康快照。
	repo := &bridgeStatusRepo{}
	manager := NewPluginManager(repo, nil, nil, PluginHostInfo{})
	// bridgeStatusRepo.List 不返回错误；换一个桩注入错误路径。
	manager.repo = &bridgeErrorRepo{}
	if got := manager.BridgeStatus(context.Background()); got != nil {
		t.Fatalf("BridgeStatus=%+v, want nil on repo error", got)
	}
}

type bridgeErrorRepo struct {
	PluginRepository
}

func (r *bridgeErrorRepo) List(context.Context) ([]*PluginInstallation, error) {
	return nil, errors.New("db down")
}

func TestPluginManagerBridgeStatusMapsEnabledPlugin(t *testing.T) {
	repo := &bridgeStatusRepo{installations: []*PluginInstallation{enabledBridgeInstallation()}}
	manager := NewPluginManager(repo, nil, nil, PluginHostInfo{})
	manager.mu.Lock()
	manager.runtimes[7] = &pluginRuntime{api: &bridgeHealthClient{
		health: &pluginv1.HealthResponse{Healthy: true, StatusJson: `{"adaptive_scheduling":true}`},
	}}
	manager.mu.Unlock()

	got := manager.BridgeStatus(context.Background())
	if got == nil {
		t.Fatalf("BridgeStatus=nil, want bridge block for enabled plugin")
	}
	if got.PluginID != 7 || got.Name != "lb-cookie-pin" || got.Version != "0.3.0" {
		t.Fatalf("bridge identity = %d/%s/%s, want 7/lb-cookie-pin/0.3.0",
			got.PluginID, got.Name, got.Version)
	}
	if !got.Running || !got.Healthy || got.Offline {
		t.Fatalf("running=%v healthy=%v offline=%v, want true/true/false",
			got.Running, got.Healthy, got.Offline)
	}
	if got.StatusJSON != `{"adaptive_scheduling":true}` {
		t.Fatalf("StatusJSON=%q, want passthrough", got.StatusJSON)
	}
}

// ---------- 健康快照信封（ListOpenAIAccountHealth + plugin_bridge） ----------

// healthListerStore 只覆盖批量快照聚合：嵌入探针 Store 接口（nil 内嵌）。
type healthListerStore struct {
	OpenAIDowngradeProbeStore
	snapshots []OpenAIProbeHealthSnapshot
}

func (s *healthListerStore) ListOpenAIProbeHealthSnapshots(
	ctx context.Context, accountIDs []int64,
) ([]OpenAIProbeHealthSnapshot, error) {
	return s.snapshots, nil
}

func TestListOpenAIAccountHealthCarriesPluginBridge(t *testing.T) {
	runner := NewOpenAIDowngradeProbeRunner(&healthListerStore{
		snapshots: []OpenAIProbeHealthSnapshot{{
			AccountID: 42, State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal",
			Schedulable: true,
		}},
	}, nil, nil, nil, nil, nil)
	runner.SetPluginBridgeSource(func(ctx context.Context) *PluginBridgeStatus {
		return &PluginBridgeStatus{PluginID: 7, Name: "lb-cookie-pin", Offline: true}
	})

	result, err := runner.ListOpenAIAccountHealth(context.Background(), []int64{42})
	if err != nil {
		t.Fatalf("ListOpenAIAccountHealth: %v", err)
	}
	if len(result.Accounts) != 1 || result.Accounts[0].AccountID != 42 {
		t.Fatalf("accounts = %+v, want one account 42", result.Accounts)
	}
	if result.PluginBridge == nil || !result.PluginBridge.Offline {
		t.Fatalf("plugin_bridge = %+v, want offline bridge block", result.PluginBridge)
	}
}

func TestListOpenAIAccountHealthWithoutBridgeSource(t *testing.T) {
	// 桥源未注入（旧装配/测试桩）：信封不带 plugin_bridge，账号列表照常。
	runner := NewOpenAIDowngradeProbeRunner(&healthListerStore{
		snapshots: []OpenAIProbeHealthSnapshot{{
			AccountID: 42, State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal",
			Schedulable: true,
		}},
	}, nil, nil, nil, nil, nil)

	result, err := runner.ListOpenAIAccountHealth(context.Background(), []int64{42})
	if err != nil {
		t.Fatalf("ListOpenAIAccountHealth: %v", err)
	}
	if len(result.Accounts) != 1 {
		t.Fatalf("accounts = %+v, want one account", result.Accounts)
	}
	if result.PluginBridge != nil {
		t.Fatalf("plugin_bridge = %+v, want nil without injected source", result.PluginBridge)
	}
}

func TestListOpenAIAccountHealthBridgeFailureDoesNotBreakList(t *testing.T) {
	// 桥源返回 nil（PluginManager 桥读失败的吞错语义）：账号列表不受影响。
	runner := NewOpenAIDowngradeProbeRunner(&healthListerStore{
		snapshots: []OpenAIProbeHealthSnapshot{{
			AccountID: 42, State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal",
			Schedulable: true,
		}},
	}, nil, nil, nil, nil, nil)
	runner.SetPluginBridgeSource(func(ctx context.Context) *PluginBridgeStatus {
		return nil
	})

	result, err := runner.ListOpenAIAccountHealth(context.Background(), []int64{42})
	if err != nil {
		t.Fatalf("ListOpenAIAccountHealth: %v", err)
	}
	if len(result.Accounts) != 1 {
		t.Fatalf("accounts = %+v, want one account despite bridge failure", result.Accounts)
	}
	if result.PluginBridge != nil {
		t.Fatalf("plugin_bridge = %+v, want nil on bridge failure", result.PluginBridge)
	}
}
