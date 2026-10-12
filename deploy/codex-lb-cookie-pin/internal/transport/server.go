// Package transport 实现官方 openai.oauth.outbound_transport.v1 传输插件：
// 在 Forward 出站链路上被动捕获/按需注入 LB 粘性 Cookie，零额外探针流量。
package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	hcplugin "github.com/hashicorp/go-plugin"
	"github.com/liyunlong/sub2api-cookie-plugin/internal/cookiestore"
	"github.com/liyunlong/sub2api-cookie-plugin/internal/evidence"
	"github.com/liyunlong/sub2api-cookie-plugin/internal/pluginconfig"
	"github.com/liyunlong/sub2api-cookie-plugin/internal/prober"
	pluginv1 "github.com/liyunlong/sub2api-cookie-plugin/pkg/pluginapi/v1"
)

const (
	// PluginID 与 manifest.json 的 id 必须一致。
	PluginID = "lyunlong.codex.lb-cookie-pin"
	// PluginVersion 与 manifest.json 的 version 必须一致。
	PluginVersion = "0.3.14"
	// A 518n-2 usage pattern is an observation, not proof of model quality.
	// Correct answers with this pattern neither reroll nor certify recovery.
	truncationFingerprintModulus = 518
	// kvNamespace 是宿主 KV 存储的命名空间（按插件隔离）。
	kvNamespace = "lyunlong.codex.lb-cookie-pin"
	kvJarKey    = "cookie-jar"
)

// probeTemplate 是某账号最近一笔业务出站请求的模板。质量探针复用它发请求，
// 指纹与业务流量一致（同 URL/头/代理）。含 Authorization，只存内存、绝不
// 进日志或状态面板。
type probeTemplate struct {
	Method             string
	URL                string
	Host               string
	Headers            http.Header // 深拷贝（含 Authorization）；Cookie 头剔除、探针时现合并
	OriginalCookie     string      // 业务请求原始 Cookie 头（罐内 Cookie 按需合并其上）
	ProxyURL           string
	Model              string
	SeenAt             time.Time
	CreatedAt          time.Time
	Generation         uint64
	IdentityGeneration uint64 // Stable across business failures, not credential/route changes.
	Confirmed          bool   // A complete successful business forward admitted this generation.
	CookieVersion      uint64 // Per-attempt snapshot; only written on the cycle's copy.
	RateLimitPolicy    pluginv1.ProbeRateLimitPolicy
}

// Server 实现 TransportPlugin 服务。
type Server struct {
	pluginv1.UnimplementedTransportPluginServer
	Store          *cookiestore.Store
	Client         *http.Client
	evidence       *evidence.Store
	evidenceFailed atomic.Bool

	mu          sync.Mutex
	broker      *hcplugin.GRPCBroker
	host        pluginv1.HostServiceClient
	persistStop context.CancelFunc
	persistDone chan struct{}

	probeMu                sync.Mutex
	templates              map[int64]*probeTemplate
	states                 map[int64]*prober.State
	probeStop              context.CancelFunc
	probeWg                sync.WaitGroup
	probeWake              chan struct{}
	nextTemplateGeneration uint64
	configGeneration       uint64
	clientMu               sync.Mutex
	proxyClients           map[string]*http.Client
	// probeRunning v0.3.6 在飞守卫：accountID → context.CancelFunc。同账号同时至多一轮
	// 探针——新签即探把排期拉近后 tick 会在上一轮复探链未走完时再次派发，
	// 叠发并发轮（双倍出针+状态竞争）。
	probeRunning sync.Map
}

// New 构建服务端与共享 HTTP 客户端。
func New(store *cookiestore.Store) *Server {
	return NewWithEvidence(store, evidence.NewMemory())
}

// Production supplies a successfully opened durable store. New is retained
// for isolated embedded/test hosts and never selected by the process entrypoint.
func NewWithEvidence(store *cookiestore.Store, journal *evidence.Store) *Server {
	// Let net/http initialize HTTP/2 separately for each clone. Explicit
	// ConfigureTransport installs TLSNextProto closures that clones would
	// share, including the HTTP/2 pool, across different proxy routes.
	baseTransport := &http.Transport{
		DisableCompression:    true,
		ForceAttemptHTTP2:     true,
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   8,
	}
	server := &Server{
		Store:    store,
		evidence: journal,
		Client: &http.Client{
			Transport: baseTransport,
			Timeout:   0,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		persistDone: make(chan struct{}),
		templates:   map[int64]*probeTemplate{},
		states:      map[int64]*prober.State{},
		probeWake:   make(chan struct{}, 1),
	}
	// v0.3.1（2026-10-02 生产实证）：官方宿主的 TransportPlugin 方法集只有
	// GetInfo/Health/ValidateConfig/ApplyConfig/TestConfig/Forward，没有
	// InitHostServices——探针回路挂在里面等于生产上从未启动：Forward 照常
	// 入账模板与抓签，但 states 恒空、探针区恒 accounts:[]/probes:0，救治
	// 链的连过证据断供（开发模拟宿主 tools/testhost 实现了该 RPC，冒烟全
	// 绿掩盖了这层）。回路本就不依赖宿主 KV（原注释「首次初始化即启动」），
	// 按该意图改在构造时启动；InitHostServices 的幂等调用保留。
	server.startProbeLoopLocked()
	return server
}

// startProbeLoopLocked 启动质量探针回路（幂等，须持 probeMu）。构造时与
// InitHostServices 共用；后者的调用在新宿主协议接入后是无害空转。
func (s *Server) startProbeLoopLocked() {
	if s.probeStop == nil {
		ctx, cancel := context.WithCancel(context.Background())
		s.probeStop = cancel
		s.probeWg.Add(1)
		go s.probeLoop(ctx)
	}
}

// SetHostBroker 由插件运行时在注册服务时回调（HostBrokerReceiver 可选能力）。
func (s *Server) SetHostBroker(broker *hcplugin.GRPCBroker) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.broker = broker
}

func (s *Server) GetInfo(context.Context, *pluginv1.GetInfoRequest) (*pluginv1.GetInfoResponse, error) {
	return &pluginv1.GetInfoResponse{
		PluginId:            PluginID,
		PluginVersion:       PluginVersion,
		ProtocolVersion:     pluginv1.ProtocolVersion,
		TransportApiVersion: pluginv1.TransportAPIVersion,
		Capabilities:        []string{"openai.oauth.outbound_transport.v1"},
	}, nil
}

func (s *Server) Health(ctx context.Context, _ *pluginv1.HealthRequest) (*pluginv1.HealthResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Status collection takes probe/journal locks and may wait for disk I/O.
	// Its latency must not cause the host to kill healthy business streams.
	if pluginv1.IsLivenessHealth(ctx) {
		return &pluginv1.HealthResponse{Healthy: true, Message: "lb cookie pin ready"}, nil
	}
	cfg := s.Store.Config()
	now := time.Now()
	return &pluginv1.HealthResponse{
		Healthy:    true,
		Message:    "lb cookie pin ready",
		StatusJson: string(s.mergeStatusJSON(now, s.Store.Status(now), cfg)),
	}, nil
}

func (s *Server) ValidateConfig(_ context.Context, req *pluginv1.ValidateConfigRequest) (*pluginv1.ValidateConfigResponse, error) {
	cfg, message, ok := pluginconfig.Parse(req.GetConfigJson())
	if !ok {
		return &pluginv1.ValidateConfigResponse{Valid: false, Message: message}, nil
	}
	// 规范化输出必须保留 drop_account_ids：官方宿主保存的是本输出、并用它
	// 回放 ApplyConfig——一次性字段若在此剥除将永远无法送达 ApplyConfig（0.1.0 的坑）。
	// 重放导致的重复 Drop 是无害空操作（罐已空）。
	raw, _ := json.Marshal(cfg)
	return &pluginv1.ValidateConfigResponse{Valid: true, NormalizedConfigJson: raw}, nil
}

func (s *Server) ApplyConfig(_ context.Context, req *pluginv1.ApplyConfigRequest) (*pluginv1.ApplyConfigResponse, error) {
	cfg, message, ok := pluginconfig.Parse(req.GetConfigJson())
	if !ok {
		return &pluginv1.ApplyConfigResponse{Applied: false, Message: message}, nil
	}
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	next := cfg.Sanitized()
	previous := s.Store.Config()
	previous.PausedAccountIDs = nil
	comparison := next
	comparison.PausedAccountIDs = nil
	if !reflect.DeepEqual(previous, comparison) {
		s.configGeneration++
		for accountID := range s.templates {
			s.invalidateProbeLocked(accountID)
		}
	}
	for _, accountID := range next.PausedAccountIDs {
		s.invalidateProbeLocked(accountID)
		delete(s.templates, accountID)
		delete(s.states, accountID)
	}
	for _, accountID := range cfg.DropAccountIDs {
		s.invalidateProbeLocked(accountID)
		s.Store.Drop(accountID)
	}
	s.Store.SetConfig(next)
	s.wakeProbeLoop()
	return &pluginv1.ApplyConfigResponse{Applied: true}, nil
}

// Invalidate evidence without forgiving failure budgets or account backoff.
// Caller holds probeMu; replace templates so in-flight forwards keep their epoch.
func (s *Server) invalidateProbeLocked(accountID int64) {
	s.invalidateProbeResultsLocked(accountID)
	if live := s.templates[accountID]; live != nil {
		live.IdentityGeneration = live.Generation
	}
}

// Business failures fence probe results, but concurrent responses from the same
// identity may still extend its cooldown.
func (s *Server) invalidateProbeResultsLocked(accountID int64) {
	if live := s.templates[accountID]; live != nil {
		next := *live
		s.nextTemplateGeneration++
		next.Generation = s.nextTemplateGeneration
		s.templates[accountID] = &next
	}
	if state := s.states[accountID]; state != nil {
		state.ConsecPasses = 0
		state.PreviousPassAt = time.Time{}
		state.LastVerdict = ""
		state.LastQuestionID = ""
		state.LastAnswer = ""
		state.LastReasoningTokens = 0
	}
	if cancel, ok := s.probeRunning.Load(accountID); ok {
		cancel.(context.CancelFunc)()
	}
}

func (s *Server) TestConfig(context.Context, *pluginv1.TestConfigRequest) (*pluginv1.TestConfigResponse, error) {
	cfg := s.Store.Config()
	now := time.Now()
	return &pluginv1.TestConfigResponse{
		Success:    true,
		Message:    "cookie jar: passive capture, inject on forward, reroll on signal; quality probe: " + probeMode(cfg),
		StatusJson: string(s.mergeStatusJSON(now, s.Store.Status(now), cfg)),
	}, nil
}

// probeMode 一句话概括探针开关（诊断面板用）。
func probeMode(cfg pluginconfig.Config) string {
	if !cfg.Enabled || !cfg.QualityProbeEnabled {
		return "off"
	}
	return "on"
}

// InitHostServices 拨号宿主反向服务并启动 KV 持久化回路。
func (s *Server) InitHostServices(_ context.Context, req *pluginv1.InitHostServicesRequest) (*pluginv1.InitHostServicesResponse, error) {
	// 探针回路 v0.3.1 起在构造时启动（官方宿主不调本 RPC，见 New 处注释）；
	// 此处幂等兜底：新宿主协议接入后也不重复起。
	s.probeMu.Lock()
	s.startProbeLoopLocked()
	s.probeMu.Unlock()

	s.mu.Lock()
	broker := s.broker
	s.mu.Unlock()
	if broker == nil || req == nil || req.GetHostServiceId() == 0 {
		return &pluginv1.InitHostServicesResponse{Ready: true, Message: "no host broker"}, nil
	}
	conn, err := broker.Dial(req.GetHostServiceId())
	if err != nil {
		return &pluginv1.InitHostServicesResponse{Ready: false, Message: "dial host failed"}, nil
	}
	host := pluginv1.NewHostServiceClient(conn)
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.host = host
	if s.persistStop == nil {
		s.persistStop = cancel
		go s.persistLoop(ctx)
	} else {
		cancel()
	}
	s.mu.Unlock()
	go s.restoreJar(host)
	return &pluginv1.InitHostServicesResponse{Ready: true, Message: "cookie pin ready"}, nil
}

// persistLoop 周期落盘脏罐快照到宿主 KV。
func (s *Server) persistLoop(ctx context.Context) {
	defer close(s.persistDone)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !s.Store.TakeDirty() {
				continue
			}
			cfg := s.Store.Config()
			if !cfg.PersistKV || !cfg.Enabled {
				continue
			}
			s.mu.Lock()
			host := s.host
			s.mu.Unlock()
			if host == nil {
				continue
			}
			raw := s.Store.SnapshotJSON()
			if len(raw) == 0 {
				continue
			}
			_, _ = host.KVSet(ctx, &pluginv1.KVSetRequest{
				Namespace:  kvNamespace,
				Key:        kvJarKey,
				Value:      raw,
				TtlSeconds: 86400,
			})
		}
	}
}

// restoreJar 启动时从宿主 KV 恢复罐快照。
func (s *Server) restoreJar(host pluginv1.HostServiceClient) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fetched, err := host.KVGet(ctx, &pluginv1.KVGetRequest{Namespace: kvNamespace, Key: kvJarKey})
	if err != nil || fetched == nil || !fetched.GetFound() {
		return
	}
	s.Store.RestoreJSON(fetched.GetValue(), time.Now())
}

// ---------- 质量探针自愈回路（v0.2） ----------

// stashTemplate 从业务出站请求捕获探针模板（仅探针开启时调用，避免热路径
// 解析请求体）。模板存的是宿主下发的原始形态（注入前的 Cookie 单独存放）。
func (s *Server) stashTemplate(start *pluginv1.ForwardRequestStart, body []byte, now time.Time) {
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	s.stashTemplateLocked(start, body, now)
}

// Caller holds probeMu, including the cookie snapshot for business forwards.
func (s *Server) stashTemplateLocked(start *pluginv1.ForwardRequestStart, body []byte, now time.Time) *probeTemplate {
	if s.probePaused(start.GetAccountId()) || start.GetAccountId() <= 0 || !strings.EqualFold(valueOr(start.GetMethod(), http.MethodGet), http.MethodPost) {
		return nil
	}
	headers := make(http.Header, len(start.GetHeaders()))
	for key, values := range start.GetHeaders() {
		if values == nil || strings.EqualFold(key, "Cookie") {
			continue
		}
		for _, value := range values.GetValues() {
			headers.Add(key, value)
		}
	}
	originalCookie := ""
	for name, values := range start.GetHeaders() {
		if strings.EqualFold(name, "Cookie") && values != nil {
			originalCookie = strings.Join(values.GetValues(), "; ")
		}
	}
	accountID := start.GetAccountId()
	next := &probeTemplate{
		Method:         http.MethodPost,
		URL:            probeEndpoint(start.GetUrl()),
		Host:           start.GetHost(),
		Headers:        headers,
		OriginalCookie: originalCookie,
		ProxyURL:       start.GetProxyUrl(),
		SeenAt:         now,
		CreatedAt:      now,
	}
	cfg := s.Store.Config()
	if cfg.QualityProbeEnabled {
		next.Model = cfg.ProbeModel
		if next.Model == "" {
			next.Model = extractModel(body)
		}
	}
	previous := s.templates[accountID]
	if previous != nil && sameProbeIdentity(previous, next) {
		next.Generation = previous.Generation
		next.IdentityGeneration = previous.IdentityGeneration
		next.CreatedAt = previous.CreatedAt
		if previous.Model != next.Model {
			s.invalidateProbeLocked(accountID)
			next.Generation = s.templates[accountID].Generation
			next.IdentityGeneration = s.templates[accountID].IdentityGeneration
		} else {
			next.Confirmed = previous.Confirmed
		}
	} else {
		s.nextTemplateGeneration++
		next.Generation = s.nextTemplateGeneration
		next.IdentityGeneration = next.Generation
		// A new credential or route must not inherit the old probe's graduation.
		s.resetProbeStateLocked(accountID, now)
		if previous != nil {
			s.Store.Drop(accountID)
			if cancel, ok := s.probeRunning.Load(accountID); ok {
				cancel.(context.CancelFunc)()
			}
		}
	}
	s.templates[accountID] = next
	return next
}

func sameProbeIdentity(a, b *probeTemplate) bool {
	return a.Method == b.Method && a.URL == b.URL && a.Host == b.Host &&
		a.ProxyURL == b.ProxyURL &&
		a.Headers.Get("Authorization") == b.Headers.Get("Authorization") &&
		a.Headers.Get("Chatgpt-Account-Id") == b.Headers.Get("Chatgpt-Account-Id")
}

// Caller holds probeMu. Generations survive ordinary business-template refreshes.
func (s *Server) probeTemplateCurrent(accountID int64, tmpl *probeTemplate) bool {
	live := s.templates[accountID]
	return !s.probePaused(accountID) && live != nil && live.Generation == tmpl.Generation
}

func (s *Server) probePaused(accountID int64) bool {
	for _, id := range s.Store.Config().PausedAccountIDs {
		if id == accountID {
			return true
		}
	}
	return false
}

func (s *Server) forwardTemplateCurrent(accountID int64, tmpl *probeTemplate) bool {
	live := s.templates[accountID]
	return live != nil && live.IdentityGeneration == tmpl.IdentityGeneration
}

// Credential rotation invalidates passing evidence, never account protection.
func (s *Server) resetProbeStateLocked(accountID int64, now time.Time) {
	state := prober.NewState(accountID)
	if previous := s.states[accountID]; previous != nil {
		state.RetryNotBefore = previous.RetryNotBefore
		state.BackoffUntil = previous.BackoffUntil
		state.ConsecFails = previous.ConsecFails
		s.saveProtectionLocked(previous)
	}
	s.restoreProtectionLocked(accountID, state)
	state.NextProbeAt = maxTime(state.RetryNotBefore, state.BackoffUntil)
	// Durable protection survives retirement; an idle in-memory state does not
	// admit an unconfirmed template or skip the first-capture settling delay.
	if state.NextProbeAt.After(now) {
		s.states[accountID] = state
	} else {
		delete(s.states, accountID)
	}
}

// extractModel 从业务请求体提取 model 字段（Responses API JSON）。探针开启
// 才调用；超限/非 JSON 返回空串（该账号本周期跳过探针，不误判）。
func extractModel(body []byte) string {
	if len(body) == 0 || len(body) > 2<<20 {
		return ""
	}
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	return strings.TrimSpace(probe.Model)
}

const (
	// templateMaxAge 模板绝对上限：探针续命下，无业务流量的账号（救治号）
	// 最长探到 24h（与罐 KV TTL 对齐），之后随模板退役、探针自然停止。
	templateMaxAge = 24 * time.Hour
	// postCaptureProbeDelay 新签即探的落点延迟：捕获响应后给落定留 5 秒。
	postCaptureProbeDelay = 5 * time.Second
	// freshSignWindow 新签即探的「新鲜」判据：捕获时刻距今 10 分钟内才算
	// 新事件（KV 恢复的旧签只对齐 KnownSignAt，不触发拉近，防启动风暴）。
	freshSignWindow = 10 * time.Minute
)

// templateRetired 判定模板是否退役（v0.3.2 双轨）：绝对上限 24h；业务
// horizon 内新鲜必留；超龄但已有探针状态（探针周期回写 SeenAt 续命）保留。
// 救治号停调度后零业务流量，探针是唯一证据源——旧逻辑（超 horizon 即删）
// 会让救治号探针静默停摆，2026-10-02 生产实测：种子 5/5 封顶后 10 分钟
// 四号全部失探。
func templateRetired(tmpl *probeTemplate, state *prober.State, now time.Time, horizon time.Duration) bool {
	createdAt := tmpl.CreatedAt
	if createdAt.IsZero() {
		createdAt = tmpl.SeenAt
	}
	if now.Sub(createdAt) > templateMaxAge {
		return true
	}
	if (!tmpl.Confirmed || state == nil) && now.Sub(tmpl.SeenAt) > horizon {
		return true // 无业务流量且从没探过：弃模板
	}
	return false
}

// pullForFreshSign 新签即探（v0.3.2）：捕获到比已见更新的签就把下一针拉近
// 到 +5s——新签好坏立验：坏签立刻进重摇搜索链，好签立刻开攒连过。
// 只拉近不推远；超过新鲜窗的旧签（KV 恢复）只对齐不拉近。
// v0.3.6：退避压过新鲜——连错退避期不被新签拽醒。败针重摇必然在探针响应
// 里回捕到新签，若新签能把 +1h 退避拽到 +5s，退避就永远不生效（0.3.5 上产
// 实测：六个号 8 分钟 106 针/58 重摇的热循环）。KnownSignAt 仍对齐，退避期满
// 后旧签不会重复触发。
func pullForFreshSign(state *prober.State, capturedAt, now time.Time) {
	if !state.KnownSignAt.Before(capturedAt) {
		return
	}
	state.KnownSignAt = capturedAt
	if now.Before(state.BackoffUntil) {
		return
	}
	if now.Sub(capturedAt) > freshSignWindow {
		return
	}
	target := capturedAt.Add(postCaptureProbeDelay)
	if state.NextProbeAt.After(target) {
		state.NextProbeAt = target
	}
}

func (s *Server) wakeProbeLoop() {
	select {
	case s.probeWake <- struct{}{}:
	default:
	}
}

// Business responses and completed cycles wake the scheduler. The timer follows
// the nearest deadline, with a bounded maintenance scan even when nothing is due.
func (s *Server) probeLoop(ctx context.Context) {
	defer s.probeWg.Done()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.probeWake:
		case <-timer.C:
		}
		timer.Reset(s.scanProbeTemplates(ctx, time.Now()))
	}
}

func (s *Server) scanProbeTemplates(ctx context.Context, now time.Time) time.Duration {
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	nextScan := 10 * time.Second
	if ctx.Err() != nil {
		return nextScan
	}
	cfg := s.Store.Config()
	horizon := 2 * time.Duration(cfg.ProbeIntervalSeconds) * time.Second
	for accountID, state := range s.states {
		if s.templates[accountID] == nil && !maxTime(state.RetryNotBefore, state.BackoffUntil).After(now) {
			delete(s.states, accountID)
		}
	}
	for accountID, tmpl := range s.templates {
		if s.probePaused(accountID) {
			continue
		}
		state := s.states[accountID]
		if templateRetired(tmpl, state, now, horizon) {
			s.invalidateProbeLocked(accountID)
			delete(s.templates, accountID)
			s.resetProbeStateLocked(accountID, now)
			continue
		}
		if !tmpl.Confirmed || !cfg.Enabled || !cfg.QualityProbeEnabled || !scopeMatch(cfg.InjectScope, tmpl.URL) || tmpl.Model == "" {
			continue
		}
		if state == nil {
			state = prober.NewState(accountID)
			state.NextProbeAt = now.Add(postCaptureProbeDelay)
			s.states[accountID] = state
		}
		if !s.restoreProtectionLocked(accountID, state) {
			continue
		}
		if info := s.Store.SignInfo(accountID, now); info.HasSign {
			pullForFreshSign(state, info.CapturedAt, now)
		}
		if _, busy := s.probeRunning.Load(accountID); busy {
			continue
		}
		dueAt := state.NextProbeAt
		if state.BackoffUntil.After(dueAt) {
			dueAt = state.BackoffUntil
		}
		dueAt = maxTime(dueAt, state.RetryNotBefore)
		if dueAt.After(now) {
			nextScan = min(nextScan, dueAt.Sub(now))
			continue
		}
		cycleCtx, cancel := context.WithCancel(ctx)
		if _, busy := s.probeRunning.LoadOrStore(accountID, cancel); busy {
			cancel()
			continue
		}
		tmplCopy := *tmpl
		go s.runReservedProbeCycle(cycleCtx, cancel, accountID, &tmplCopy, state, cfg)
	}
	return nextScan
}

// runProbeCycle 单账号一轮探针：判定 → （必要时）丢罐重摇 → 立即复探，直至
// 答对 / 非质量错误 / 判账号级退避。复探链长度被 MaxConsecutiveProbeFailures
// 自然封顶（第 N 连错返回退避而非复探），另设硬保险防状态异常时空转。
// v0.3.6 在飞守卫：同账号同时至多一轮。0.3.5 生产热循环的第二个放大器——
// pullForFreshSign 把 NextProbeAt 拉到 +5s 后，tick 在上一轮复探链（含丢罐
// 落定 2s 等待）未走完时就可能再次派发同号，叠发并发轮=双倍出针+对同一
// state 的并发写。LoadOrStore 原子占位，全部 return 路径经 defer 释放。
func (s *Server) runProbeCycle(ctx context.Context, accountID int64, tmpl *probeTemplate, state *prober.State, cfg pluginconfig.Config) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if _, busy := s.probeRunning.LoadOrStore(accountID, cancel); busy {
		return
	}
	s.runReservedProbeCycle(ctx, cancel, accountID, tmpl, state, cfg)
}

func (s *Server) runReservedProbeCycle(ctx context.Context, cancel context.CancelFunc, accountID int64,
	tmpl *probeTemplate, state *prober.State, cfg pluginconfig.Config,
) {
	defer func() {
		cancel()
		s.probeRunning.Delete(accountID)
		s.wakeProbeLoop()
	}()
	s.probeMu.Lock()
	snapshot := *tmpl
	s.probeMu.Unlock()
	tmpl = &snapshot
	for attempt := 0; attempt <= cfg.MaxConsecutiveProbeFailures+1; attempt++ {
		if err := ctx.Err(); err != nil {
			return
		}
		s.probeMu.Lock()
		if !s.probeTemplateCurrent(accountID, tmpl) {
			s.probeMu.Unlock()
			return
		}
		question := prober.QuestionFor(state.Probes)
		s.probeMu.Unlock()
		verdict, answer, reasoningTokens := s.sendProbe(ctx, accountID, tmpl, question, cfg)
		if strings.HasPrefix(answer, "blocked:") {
			return
		}
		s.probeMu.Lock()
		if s.probePaused(accountID) || strings.HasPrefix(answer, "stale:") ||
			(!s.probeTemplateCurrent(accountID, tmpl) &&
				!(s.templates[accountID] == nil && strings.HasSuffix(answer, "+tmpl-dropped"))) {
			s.probeMu.Unlock()
			return
		}
		now := time.Now()
		nextState := *state
		decision := nextState.Record(verdict, question.ID, answer, now, cfg)
		if !s.Store.AcceptProbe(accountID, tmpl.CookieVersion, decision.ShouldReroll) {
			s.probeMu.Unlock()
			return
		}
		if nextState.TruncationRateAlert && !state.TruncationRateAlert {
			slog.Warn("probe_truncation_rate_alert", "account_id", accountID,
				"hits", nextState.TruncationWindowHits, "samples", nextState.TruncationWindowSamples)
		}
		*state = nextState
		s.saveProtectionLocked(state)
		state.LastReasoningTokens = reasoningTokens
		// v0.3.6：探针自捕的签不喂新签即探。探针响应（含 5xx/读错的响应头）
		// 同样走 Capture 回捕 Set-Cookie——若这些时刻推进「有新签」判定，
		// tick 就拉近 +5s→出针→再回捕：error 针不计连败也不进退避，纯错号
		// 就是 10s 拍频的自激励热循环（0.3.5 上产实测，1217 尾段 read 错）。
		// 此处对齐 KnownSignAt 把探针源捕获「消费掉」；业务流量（Forward 路径）
		// 的新签不受影响，重摇后的立验也早由 ProbeAgainNow 链覆盖。
		if info := s.Store.SignInfo(accountID, now); info.HasSign && info.CapturedAt.After(state.KnownSignAt) {
			state.KnownSignAt = info.CapturedAt
		}
		// 探针续命（v0.3.2）：本轮探针真实出站过，证明模板仍可用——回写
		// SeenAt 让零业务流量的救治号不被 horizon 过期删除（详见 probeLoop）。
		if live := s.templates[accountID]; live != nil {
			live.SeenAt = now
		}
		if cfg.AdaptiveProbeScheduling && !state.InBurst && !decision.ProbeAgainNow && !decision.EnterBackoff {
			// 卡点改排（v0.3）：按实测签寿命把下一针挪到预计死亡前；样本
			// <MinAdaptiveSamples 或无签时 AdaptiveNextProbe 自退固定间隔。
			// 锁序：probeMu → store.mu（store 从不反向持锁）。
			if info := s.Store.SignInfo(accountID, now); info.HasSign && info.Stats.Samples >= prober.MinAdaptiveSamples {
				state.NextProbeAt = prober.AdaptiveNextProbe(now, info.CapturedAt,
					time.Duration(info.Stats.P80*float64(time.Second)), cfg, rand.Float64)
			}
		}
		observation := newProbeObservation(tmpl, state, decision, now)
		retryAt := now.Add(2 * time.Second)
		if decision.ProbeAgainNow {
			if !s.restoreProtectionLocked(accountID, state) {
				s.probeMu.Unlock()
				observation.log(slog.Default())
				return
			}
			retryAt = maxTime(retryAt, maxTime(state.RetryNotBefore, state.BackoffUntil))
			state.NextProbeAt = retryAt
		}
		s.probeMu.Unlock()
		observation.log(slog.Default())
		if !decision.ProbeAgainNow {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(retryAt)):
		}
	}
}

// sendProbe 发一道判别题并返回判定与该针 completed usage 的推理 token 数
// （0=未解析到 usage）。请求体按 codex 后端硬约束构造（2026-10-02 实测 400
// 报文逐条反推）：input 必须是消息数组、store 必须 false、stream 必须
// true（后端强制 SSE）——恰好也是 codex CLI 的原生形态，指纹同形。响应的
// Set-Cookie 走同一被动捕获路径——探针本身就能把重摇后的新签重新钉住。
// 每次操作只外呼一次；模糊结果持久化为 unknown，等待人工对账。
// 明确失败保留账号退避与 Retry-After，不记 Authorization/Cookie 到日志或答案摘要。
// v0.3.4：①401/403 当场丢模板（模板 Authorization 已死，留着只会无限空转——
// 1227 实证；下一笔真实 Forward 自动用新鲜 token 重stash）；②判过钉推理门槛
// （答对但 reasoning_tokens 低于 probe_min_reasoning_tokens 不计连过）。
// v0.3.10：低 rt 单信号及不明确的最终答案只作中性观察。v0.3.8：答对但命中 518n-2 指纹只记中性观察，
// 不计质量失败、不触发重摇，也不计入连续通过证据。
func (s *Server) sendProbe(ctx context.Context, accountID int64, tmpl *probeTemplate, q prober.Question, cfg pluginconfig.Config) (verdict prober.Verdict, answer string, reasoning int) {
	model := cfg.ProbeModel
	if model == "" {
		model = tmpl.Model
	}
	if model == "" {
		return prober.VerdictError, "skip:no-model", 0
	}
	payload := map[string]any{
		"model": model,
		// Required by Codex OAuth independently of reasoning effort.
		"instructions": "",
		"input": []map[string]any{{
			"type": "message",
			"role": "user",
			"content": []map[string]any{{
				"type": "input_text",
				"text": q.Prompt,
			}},
		}},
		"stream": true,
		"store":  false,
		// 注意：codex 后端不收 max_output_tokens（2026-10-02 实测 400
		// "Unsupported parameter"），判别题的输出长度交给模型默认——判分按
		// 精确 token 匹配，长解释不影响判对。
	}
	// 业务同形（v0.3.3）：effort 非空时附 reasoning/instructions/
	// parallel_tool_calls/include，与宿主资格针及真实业务流量同形。裸形态
	// 简单题在降智号上全对而资格针全错（2026-10-02 生产实证）——探针考卷
	// 必须与裁判考卷同难度，连过才是真毕业证据，重摇才对着真考卷搜节点。
	if effort := cfg.ProbeReasoningEffort; effort != "" {
		payload["reasoning"] = map[string]any{"effort": effort, "summary": "auto"}
		payload["parallel_tool_calls"] = true
		payload["include"] = []string{"reasoning.encrypted_content"}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return prober.VerdictError, "skip:build", 0
	}
	c := cohort(model, cfg.ProbeReasoningEffort, tmpl.ProxyURL)
	op, err := s.evidence.Begin(accountID, c, time.Now())
	if err != nil {
		if errors.Is(err, evidence.ErrBackoff) {
			return prober.VerdictError, "blocked:backoff", 0
		}
		if errors.Is(err, evidence.ErrBlocked) {
			return prober.VerdictError, "blocked:unknown", 0
		}
		s.evidenceError(err)
		return prober.VerdictError, "blocked:persistence", 0
	}
	result := "failed"
	var retryUntil time.Time
	defer func() {
		if err := s.evidence.Finish(accountID, op, c, result, time.Now(), retryUntil); err != nil {
			s.evidenceError(err)
			verdict, answer, reasoning = prober.VerdictError, "blocked:persistence", 0
		} else if result == "unknown" {
			slog.Warn("plugin_probe_unknown", "account_id", accountID, "operation_id", op)
		}
	}()
	{
		if err := ctx.Err(); err != nil {
			return prober.VerdictError, "cancel", 0
		}
		attemptCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
		request, err := http.NewRequestWithContext(attemptCtx, tmpl.Method, tmpl.URL, bytes.NewReader(body))
		if err != nil {
			cancel()
			return prober.VerdictError, "skip:build", 0
		}
		request.Host = tmpl.Host
		request.Header = probeHeaders(tmpl.Headers)
		request.GetBody = nil // Never let the transport rewind an ambiguous POST.
		now := time.Now()
		s.probeMu.Lock()
		if !s.probeTemplateCurrent(accountID, tmpl) {
			s.probeMu.Unlock()
			cancel()
			return prober.VerdictError, "stale:template", 0
		}
		cookie, version := s.Store.HeaderSnapshot(accountID, tmpl.OriginalCookie, now)
		tmpl.CookieVersion = version
		request.Header.Set("Cookie", cookie)
		s.probeMu.Unlock()

		client, err := s.clientFor(tmpl.ProxyURL)
		if err != nil {
			cancel()
			return prober.VerdictError, "proxy:invalid", 0
		}
		attemptStarted := time.Now()
		request, failureTrace := traceUpstreamRequest(request)
		// Commit intent before handing bytes to the HTTP transport. Even a
		// process crash between this write and Do cannot cause silent replay.
		if err := s.evidence.Dispatch(accountID, op, c, attemptStarted); err != nil {
			s.evidenceError(err)
			cancel()
			return prober.VerdictError, "blocked:persistence", 0
		}
		result = "unknown"
		response, err := client.Do(request)
		if err != nil {
			logUpstreamFailure(slog.Default(), accountID, "probe_connect", tmpl.ProxyURL, attemptStarted, err, attemptCtx.Err(), failureTrace)
			cancel()
			if failureTrace.definitelyNotConnected(err) {
				result = "failed"
				return prober.VerdictError, "failed:connect", 0
			}
			return prober.VerdictError, "unknown:transport", 0
		}
		observedAt := time.Now()
		s.probeMu.Lock()
		if !s.probeTemplateCurrent(accountID, tmpl) {
			s.probeMu.Unlock()
			response.Body.Close()
			cancel()
			return prober.VerdictError, "stale:template", 0
		}
		// Authentication errors are not evidence of a healthy replacement cookie.
		if response.StatusCode != http.StatusUnauthorized && response.StatusCode != http.StatusForbidden &&
			response.StatusCode != http.StatusTooManyRequests && response.StatusCode != http.StatusServiceUnavailable {
			var current bool
			tmpl.CookieVersion, current = s.Store.CaptureIfCurrent(accountID,
				response.Header.Values("Set-Cookie"), time.Now(), tmpl.CookieVersion)
			if !current {
				s.probeMu.Unlock()
				response.Body.Close()
				cancel()
				return prober.VerdictError, "stale:cookie", 0
			}
		}
		s.probeMu.Unlock()
		if response.StatusCode != 200 {
			code := response.StatusCode
			// Error bodies are untrusted and may echo credentials or prompts.
			// Read a bounded prefix under a short deadline; publish only a
			// finite diagnostic vocabulary and a correlation fingerprint.
			stopRead := time.AfterFunc(3*time.Second, cancel)
			raw, readErr := io.ReadAll(io.LimitReader(response.Body, maxProbeErrorBody+1))
			stopRead.Stop()
			if readErr != nil {
				logUpstreamFailure(slog.Default(), accountID, "probe_body", tmpl.ProxyURL, attemptStarted, readErr, attemptCtx.Err(), failureTrace)
			}
			response.Body.Close()
			cancel()
			failure := classifyProbeHTTPError(code, raw, readErr)
			if readErr == nil && len(raw) <= maxProbeErrorBody {
				result = "failed"
				s.observeAdmission(accountID, c, admissionCategory(raw))
			}
			slog.Warn("quality_probe_http_error", "account_id", accountID,
				"http_status", code, "kind", failure.Kind, "parameter", failure.Parameter,
				"body_fingerprint", failure.Fingerprint, "body_truncated", len(raw) > maxProbeErrorBody,
				"body_read_failed", readErr != nil)
			lastFailure := failure.Summary()
			deadline := probeRetryDeadlineWithPolicy(code, response.Header, observedAt, tmpl.RateLimitPolicy)
			if seconds, ok := retrySeconds(response.Header.Get("Retry-After")); ok {
				deadline = maxTime(deadline, retryResetAfter(seconds, observedAt))
			} else if until, err := http.ParseTime(response.Header.Get("Retry-After")); err == nil {
				deadline = maxTime(deadline, until)
			}
			if code == http.StatusTooManyRequests && readErr == nil {
				deadline = maxTime(deadline, probeQuotaBodyDeadline(raw, observedAt))
			}
			retryUntil = deadline
			if !deadline.IsZero() {
				s.probeMu.Lock()
				if !s.probeTemplateCurrent(accountID, tmpl) {
					s.probeMu.Unlock()
					return prober.VerdictError, "stale:template", 0
				}
				if state := s.states[accountID]; state != nil {
					state.RetryNotBefore = maxTime(state.RetryNotBefore, deadline)
					s.saveProtectionLocked(state)
				}
				s.probeMu.Unlock()
				return prober.VerdictError, lastFailure, 0
			}
			if code == http.StatusUnauthorized || code == http.StatusForbidden {
				// 模板凭据被拒（v0.3.4，1227 实证）：模板里的 Authorization
				// 已死，重试只会再吃 401——当场丢模板。下一笔真实 Forward
				// （救治号 = 宿主资格针每 ~5 分钟一针）自动用新鲜 token 重
				// stash。Cookie 罐不动：401/403 是凭据级不是签级。
				s.probeMu.Lock()
				if !s.probeTemplateCurrent(accountID, tmpl) {
					s.probeMu.Unlock()
					return prober.VerdictError, "stale:template", 0
				}
				delete(s.templates, accountID)
				s.resetProbeStateLocked(accountID, time.Now())
				s.probeMu.Unlock()
				return prober.VerdictError, "http:" + strconv.Itoa(code) + "+tmpl-dropped", 0
			}
			return prober.VerdictError, lastFailure, 0
		}
		const maxProbeResponse = 2 << 20
		raw, readErr := io.ReadAll(io.LimitReader(response.Body, maxProbeResponse+1))
		if readErr != nil {
			logUpstreamFailure(slog.Default(), accountID, "probe_body", tmpl.ProxyURL, attemptStarted, readErr, attemptCtx.Err(), failureTrace)
		}
		response.Body.Close()
		cancel()
		if readErr != nil {
			return prober.VerdictError, "unknown:read", 0
		}
		if len(raw) > maxProbeResponse {
			return prober.VerdictError, "unknown:read-too-large", 0
		}
		observer := responseObserver{sse: true}
		observer.write(raw)
		s.observeAdmission(accountID, c, observer.category)
		if !observer.finish() {
			return prober.VerdictError, result + ":not-completed", 0
		}
		result = "failed"
		answer := extractAnswerFromSSE(raw)
		if answer == "" {
			return prober.VerdictError, "parse:no-text", 0
		}
		reasoningTokens, usageKnown := extractUsageFromSSE(raw)
		if _, known := prober.ExtractFinalAnswer(answer); !known {
			return prober.VerdictError, "parse:ambiguous-answer", reasoningTokens
		}
		if q.Grade(answer) {
			if cfg.ProbeMinReasoningTokens > 0 && !usageKnown {
				return prober.VerdictError, "parse:missing-usage", 0
			}
			if usageKnown && (reasoningTokens+2)%truncationFingerprintModulus == 0 {
				// A correct but truncated response is inconclusive, not an
				// incorrect answer. Never reroll or enter backoff on it alone.
				return prober.VerdictError, "trunc-fp:" + strconv.Itoa(reasoningTokens) + " | " + answer, reasoningTokens
			}
			if min := cfg.ProbeMinReasoningTokens; usageKnown && min > 0 && reasoningTokens < min {
				// A token threshold is not proof of an incorrect answer.
				return prober.VerdictError, "low-rt:" + strconv.Itoa(reasoningTokens) + " | " + answer, reasoningTokens
			}
			result = "pass"
			return prober.VerdictPass, answer, reasoningTokens
		}
		return prober.VerdictFail, answer, reasoningTokens
	}
}

// Only a valid completed terminal can certify a probe; deltas and incomplete
// answers may contain the expected prefix even when generation was aborted.
func probeCompleted(raw []byte) bool {
	observer := responseObserver{sse: true}
	observer.write(raw)
	return observer.finish()
}

// extractAnswerFromSSE 从 SSE 流里提取答案文本：优先 response.completed 事件
// 里的完整 output（最终态），否则累积 output_text.delta 增量（completed 缺失
// 时的兜底）。后端强制 stream:true，探针必须说 SSE。
func extractAnswerFromSSE(raw []byte) string {
	type contentItem struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	var completed struct {
		Response struct {
			Output []struct {
				Type    string        `json:"type"`
				Content []contentItem `json:"content"`
			} `json:"output"`
		} `json:"response"`
	}
	completedSeen := false
	var deltas strings.Builder
	forEachResponseSSE(raw, func(eventType string, payload []byte) {
		var event struct {
			Delta    string          `json:"delta"`
			Response json.RawMessage `json:"response"`
		}
		if err := json.Unmarshal(payload, &event); err != nil {
			return
		}
		switch eventType {
		case "response.output_text.delta":
			deltas.WriteString(event.Delta)
		case "response.completed", "response.done", "response.incomplete":
			// event.Response 已是内层 response 对象，直接解进 Output 层。
			if err := json.Unmarshal(event.Response, &completed.Response); err == nil {
				completedSeen = true
			}
		}
	})
	if completedSeen {
		var text strings.Builder
		for _, item := range completed.Response.Output {
			for _, part := range item.Content {
				if part.Type == "output_text" {
					text.WriteString(part.Text)
				}
			}
		}
		if text.Len() > 0 {
			return text.String()
		}
	}
	return deltas.String()
}

// extractUsageFromSSE 从 SSE 流的 response.completed/incomplete 终态事件里
// 提取 usage 推理 token 数（v0.3.4）：优先 usage.output_tokens_details.
// reasoning_tokens，回退 usage.reasoning_tokens——与宿主
// parseOpenAIDowngradeProbeCompletion 同源双查。多事件时取最后一个终态。
// 第二个返回值 false = 流里没有带有效计数的 usage。
func extractUsageFromSSE(raw []byte) (int, bool) {
	best, seen := 0, false
	forEachResponseSSE(raw, func(eventType string, payload []byte) {
		if eventType != "response.completed" && eventType != "response.done" {
			return
		}
		var event struct {
			Response json.RawMessage `json:"response"`
		}
		if err := json.Unmarshal(payload, &event); err != nil {
			return
		}
		var inner struct {
			Usage *struct {
				ReasoningTokens     *int `json:"reasoning_tokens"`
				OutputTokensDetails *struct {
					ReasoningTokens *int `json:"reasoning_tokens"`
				} `json:"output_tokens_details"`
			} `json:"usage"`
		}
		best, seen = 0, false
		if err := json.Unmarshal(event.Response, &inner); err != nil || inner.Usage == nil {
			return
		}
		switch {
		case inner.Usage.OutputTokensDetails != nil && inner.Usage.OutputTokensDetails.ReasoningTokens != nil:
			best, seen = *inner.Usage.OutputTokensDetails.ReasoningTokens, true
		case inner.Usage.ReasoningTokens != nil:
			best, seen = *inner.Usage.ReasoningTokens, true
		}
	})
	return best, seen && best >= 0
}

// cloneHeader 深拷贝头映射（探针请求体独立于模板，避免并发污染）。
func cloneHeader(src http.Header) http.Header {
	out := make(http.Header, len(src))
	for key, values := range src {
		out[key] = append([]string(nil), values...)
	}
	return out
}

// probeStatus 是状态面板的探针区段（无任何敏感值）。
type probeStatus struct {
	Enabled              bool               `json:"enabled"`
	IntervalSeconds      int                `json:"interval_seconds"`
	AdaptiveScheduling   bool               `json:"adaptive_scheduling"`
	Model                string             `json:"model"`
	ReasoningEffort      string             `json:"reasoning_effort"`
	MinReasoningTokens   int                `json:"min_reasoning_tokens"`
	Accounts             []accountProbeView `json:"accounts"`
	Probes               int64              `json:"probes"`
	Fails                int64              `json:"fails"`
	QualityRerolls       int64              `json:"quality_rerolls"`
	SuspectAccountLevels int                `json:"suspect_account_levels"`
}

// accountProbeView 是状态桥的单账号视图（v0.3）：探针状态机内联，叠加退避
// 判定、签捕获时刻与剩余寿命估计。estimated_remaining_seconds：>=0 估计
// 剩余秒数；-1 无签；-2 有签无实测样本。estimate_basis = measured|fallback|
// none。估计值不是保证值——会话 Cookie 无 Expires，窗口是观测推断（前端须标注）。
type accountProbeView struct {
	prober.State
	InBackoff              bool                   `json:"in_backoff"`
	SignCapturedAt         time.Time              `json:"sign_captured_at"`
	EstimatedRemainingSecs int64                  `json:"estimated_remaining_seconds"`
	EstimateBasis          string                 `json:"estimate_basis"`
	SignLifetimeStats      *cookiestore.SignStats `json:"sign_lifetime_stats,omitempty"`
}

// snapshotProber 汇总探针状态区段。
func (s *Server) snapshotProber(now time.Time, cfg pluginconfig.Config) probeStatus {
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	cfg = s.Store.Config()
	status := probeStatus{
		Enabled:            cfg.Enabled && cfg.QualityProbeEnabled,
		IntervalSeconds:    cfg.ProbeIntervalSeconds,
		AdaptiveScheduling: cfg.AdaptiveProbeScheduling,
		Model:              cfg.ProbeModel,
		ReasoningEffort:    cfg.ProbeReasoningEffort,
		MinReasoningTokens: cfg.ProbeMinReasoningTokens,
		Accounts:           make([]accountProbeView, 0, len(s.states)),
	}
	for _, state := range s.states {
		view := accountProbeView{
			State:                  *state,
			InBackoff:              now.Before(maxTime(state.BackoffUntil, state.RetryNotBefore)),
			EstimatedRemainingSecs: -1,
			EstimateBasis:          "none",
		}
		view.NextProbeAt = maxTime(view.NextProbeAt, maxTime(state.BackoffUntil, state.RetryNotBefore))
		info := s.Store.SignInfo(state.AccountID, now)
		if info.HasSign {
			view.SignCapturedAt = info.CapturedAt
			switch {
			case info.Stats.Samples >= prober.MinAdaptiveSamples:
				remaining := int64(info.Stats.P80) - int64(now.Sub(info.CapturedAt).Seconds())
				if remaining < 0 {
					remaining = 0
				}
				view.EstimatedRemainingSecs = remaining
				view.EstimateBasis = "measured"
				stats := info.Stats
				view.SignLifetimeStats = &stats
			default:
				if rem := int64(info.ExpiresAt.Sub(now).Seconds()); rem >= 0 {
					view.EstimatedRemainingSecs = rem
					view.EstimateBasis = "fallback"
				}
			}
		}
		status.Accounts = append(status.Accounts, view)
		status.Probes += state.Probes
		status.Fails += state.Fails
		status.QualityRerolls += state.QualityRerolls
		if state.SuspectAccountLevel {
			status.SuspectAccountLevels++
		}
	}
	sort.Slice(status.Accounts, func(i, j int) bool { return status.Accounts[i].AccountID < status.Accounts[j].AccountID })
	return status
}

// mergeStatusJSON 把罐状态与探针区段合并成一份状态 JSON（顶层保留罐字段，
// 追加 "prober" 键——旧面板无感知）。
func (s *Server) mergeStatusJSON(now time.Time, jarStatus cookiestore.Status, cfg pluginconfig.Config) []byte {
	merged := map[string]any{}
	if raw, err := json.Marshal(jarStatus); err == nil {
		_ = json.Unmarshal(raw, &merged)
	}
	merged["prober"] = s.snapshotProber(now, cfg)
	states, evidenceErr := s.evidence.Snapshot()
	s.evidenceError(evidenceErr)
	merged["evidence"] = map[string]any{
		"available": evidenceErr == nil && !s.evidenceFailed.Load(),
		"accounts":  states,
		"semantics": "probe_passed is not business_succeeded; business_succeeded requires a complete Responses terminal and transport delivery",
	}
	out, err := json.Marshal(merged)
	if err != nil {
		return nil
	}
	return out
}

// Forward 是传输主链路：start/body 帧 → 上游请求 → start/chunk/end 帧。
func (s *Server) Forward(stream pluginv1.TransportPlugin_ForwardServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	start := first.GetStart()
	if start == nil {
		return stream.Send(errorFrame("PLUGIN_PROTOCOL", "缺少请求头帧", false))
	}
	body, err := readBody(stream, start.GetHasBody())
	if err != nil {
		return stream.Send(errorFrame("PLUGIN_BODY", "读取请求体失败", false))
	}
	request, err := http.NewRequestWithContext(stream.Context(), valueOr(start.GetMethod(), http.MethodGet), start.GetUrl(), bytes.NewReader(body))
	if err != nil {
		return stream.Send(errorFrame("PLUGIN_REQUEST", "无法构造上游请求", false))
	}
	request.Host = start.GetHost()
	request.Header = headersFromPlugin(start.GetHeaders())

	now := time.Now()
	s.probeMu.Lock()
	cfg := s.Store.Config()
	configGeneration := s.configGeneration
	inScope := cfg.Enabled && !s.probePaused(start.GetAccountId()) && scopeMatch(cfg.InjectScope, start.GetUrl())
	var tmpl *probeTemplate
	var cookieVersion uint64
	if inScope {
		tmpl = s.stashTemplateLocked(start, body, now)
		if tmpl != nil {
			// Do not mutate snapshots retained by concurrent forwards/probes.
			next := *tmpl
			next.RateLimitPolicy = pluginv1.ReadProbeRateLimitPolicy(stream.Context())
			s.templates[start.GetAccountId()] = &next
			tmpl = &next
			if next.RateLimitPolicy.NotBefore.After(now) {
				s.extendProbeCooldownLocked(start.GetAccountId(), next.RateLimitPolicy.NotBefore)
			}
		}
		merged, version := s.Store.HeaderSnapshot(start.GetAccountId(), request.Header.Get("Cookie"), now)
		cookieVersion = version
		if merged != request.Header.Get("Cookie") {
			request.Header.Set("Cookie", merged)
		}
	}
	s.probeMu.Unlock()

	// Headers alone are not a completed forward. Fence failed first attempts so
	// an older concurrent success cannot admit their replacement generation.
	completed := false
	businessSucceeded := false
	businessDispatched := false
	businessFailed := false
	businessCohort := bodyCohort(body, start.GetProxyUrl())
	hostProbe := pluginv1.IsProbeTraffic(stream.Context())
	business := !hostProbe && responsesRequest(start.GetMethod(), start.GetUrl())
	hostOperation, hostResult := "", "failed"
	var hostRetryUntil time.Time
	defer func() {
		if hostOperation != "" {
			s.evidenceError(s.evidence.Finish(start.GetAccountId(), hostOperation, businessCohort,
				hostResult, time.Now(), hostRetryUntil))
			if hostResult == "unknown" {
				slog.Warn("plugin_probe_unknown", "account_id", start.GetAccountId(), "operation_id", hostOperation)
			}
		} else if businessDispatched {
			result := "unknown"
			if businessSucceeded {
				result = "succeeded"
			} else if businessFailed {
				result = "failed"
			}
			s.evidenceError(s.evidence.Business(start.GetAccountId(), businessCohort, result, time.Now()))
		}
		if tmpl == nil {
			return
		}
		s.probeMu.Lock()
		if configGeneration == s.configGeneration && s.probeTemplateCurrent(start.GetAccountId(), tmpl) {
			live := s.templates[start.GetAccountId()]
			if completed {
				next := *live
				next.Confirmed = true
				s.templates[start.GetAccountId()] = &next
			} else if !live.Confirmed {
				s.invalidateProbeResultsLocked(start.GetAccountId())
			}
		}
		s.probeMu.Unlock()
		if completed {
			s.wakeProbeLoop()
		}
	}()

	client, err := s.clientFor(start.GetProxyUrl())
	if err != nil {
		return stream.Send(errorFrame("PLUGIN_PROXY", "无效代理配置，拒绝直连回退", false))
	}
	if hostProbe {
		hostOperation, err = s.evidence.Begin(start.GetAccountId(), businessCohort, time.Now())
		if err != nil {
			if !errors.Is(err, evidence.ErrBlocked) && !errors.Is(err, evidence.ErrBackoff) {
				s.evidenceError(err)
			}
			return stream.Send(errorFrame("PLUGIN_PROBE_BLOCKED", "探针受持久退避或待对账操作保护", false))
		}
		request.GetBody = nil
		if err := s.evidence.Dispatch(start.GetAccountId(), hostOperation, businessCohort, time.Now()); err != nil {
			s.evidenceError(err)
			return stream.Send(errorFrame("PLUGIN_PROBE_BLOCKED", "探针派发状态不可用", false))
		}
		hostResult = "unknown"
	}
	requestStarted := time.Now()
	request, failureTrace := traceUpstreamRequest(request)
	if start.GetAccountId() > 0 && business {
		s.evidenceError(s.evidence.Business(start.GetAccountId(), businessCohort, "dispatched", requestStarted))
		businessDispatched = true
	}
	response, err := client.Do(request)
	if err != nil {
		if hostProbe && failureTrace.definitelyNotConnected(err) {
			hostResult = "failed"
		}
		suffix := logUpstreamFailure(slog.Default(), start.GetAccountId(), "forward_connect", start.GetProxyUrl(), requestStarted, err, stream.Context().Err(), failureTrace)
		return stream.Send(errorFrame("UPSTREAM", "上游连接失败"+suffix, true))
	}
	defer response.Body.Close()
	observer := responseObserver{sse: strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream")}
	defer func() {
		s.observeAdmission(start.GetAccountId(), businessCohort, observer.category)
	}()

	after := time.Now()
	if hostProbe {
		hostRetryUntil = probeRetryDeadlineWithPolicy(response.StatusCode, response.Header, after,
			pluginv1.ReadProbeRateLimitPolicy(stream.Context()))
		if seconds, ok := retrySeconds(response.Header.Get("Retry-After")); ok {
			hostRetryUntil = maxTime(hostRetryUntil, retryResetAfter(seconds, after))
		} else if until, parseErr := http.ParseTime(response.Header.Get("Retry-After")); parseErr == nil {
			hostRetryUntil = maxTime(hostRetryUntil, until)
		}
	}
	var cooldownTemplate *probeTemplate
	s.Store.ObserveResponse()
	if inScope {
		s.probeMu.Lock()
		if !s.probePaused(start.GetAccountId()) && configGeneration == s.configGeneration && (tmpl == nil || s.forwardTemplateCurrent(start.GetAccountId(), tmpl)) {
			if !s.observeForwardFailureLocked(start.GetAccountId(), tmpl, response.StatusCode, response.Header, after) {
				if tmpl == nil || s.probeTemplateCurrent(start.GetAccountId(), tmpl) {
					s.Store.ObserveIfCurrent(start.GetAccountId(), response.Header.Values("Set-Cookie"), after,
						cookieVersion, cfg.RerollOnFasterModel && response.Header.Get("Faster-Model") != "")
				}
			} else if response.StatusCode == http.StatusTooManyRequests {
				cooldownTemplate = s.templates[start.GetAccountId()]
			}
		}
		s.probeMu.Unlock()
		s.wakeProbeLoop()
	}

	if err := stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_Start{Start: &pluginv1.ForwardResponseStart{
		StatusCode:    int32(response.StatusCode),
		Status:        response.Status,
		Protocol:      response.Proto,
		ProtocolMajor: int32(response.ProtoMajor),
		ProtocolMinor: int32(response.ProtoMinor),
		Headers:       headersToPlugin(response.Header),
		ContentLength: response.ContentLength,
	}}}); err != nil {
		return err
	}
	buffer := make([]byte, 32*1024)
	var quotaBody []byte
	bodyTooLarge := false
	var received int64
	started := time.Now()
	for {
		n, readErr := response.Body.Read(buffer)
		if n > 0 {
			observer.write(buffer[:n])
			received += int64(n)
			if (cooldownTemplate != nil || hostProbe && response.StatusCode == http.StatusTooManyRequests) && !bodyTooLarge {
				if len(quotaBody)+n > maxProbeErrorBody {
					quotaBody = nil
					bodyTooLarge = true
				} else {
					quotaBody = append(quotaBody, buffer[:n]...)
				}
			}
			chunk := append([]byte(nil), buffer[:n]...)
			if err := stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_BodyChunk{BodyChunk: chunk}}); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			suffix := logUpstreamFailure(slog.Default(), start.GetAccountId(), "forward_body", start.GetProxyUrl(), requestStarted, readErr, stream.Context().Err(), failureTrace)
			return stream.Send(errorFrame("UPSTREAM_BODY", "读取上游响应失败"+suffix, true))
		}
	}
	if hostProbe && response.StatusCode == http.StatusTooManyRequests {
		hostRetryUntil = maxTime(hostRetryUntil, probeQuotaBodyDeadline(quotaBody, after))
	}
	if cooldownTemplate != nil {
		deadline := probeQuotaBodyDeadline(quotaBody, after)
		if deadline.After(after) {
			s.probeMu.Lock()
			if configGeneration == s.configGeneration && s.forwardTemplateCurrent(start.GetAccountId(), cooldownTemplate) {
				s.extendProbeCooldownLocked(start.GetAccountId(), deadline)
			}
			s.probeMu.Unlock()
		}
	}
	err = stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_End{End: &pluginv1.ForwardResponseEnd{
		BytesReceived: received,
		DurationMs:    time.Since(started).Milliseconds(),
	}}})
	completed = err == nil && stream.Context().Err() == nil &&
		response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices
	businessSucceeded = completed && observer.finish()
	businessFailed = err == nil && stream.Context().Err() == nil &&
		response.StatusCode >= http.StatusMultipleChoices
	if hostProbe {
		if businessSucceeded {
			hostResult = "observed"
		} else if businessFailed {
			hostResult = "failed"
		}
	} else if !business && start.GetAccountId() > 0 {
		s.evidenceError(s.evidence.Business(start.GetAccountId(), businessCohort, "observed", time.Now()))
	}
	if !observer.sse && !completed {
		observer.parse()
	}
	return err
}

// scopeMatch 判断该 URL 是否在注入范围内。
// codex = 仅 chatgpt.com 的 /backend-api/codex*（LB 粘性的作用面）；
// all = 全部出站请求。
func scopeMatch(scope, rawURL string) bool {
	if scope == "all" {
		return true
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "chatgpt.com" && !strings.HasSuffix(host, ".chatgpt.com") {
		return false
	}
	return strings.HasPrefix(parsed.Path, "/backend-api/codex")
}

// A proxy-specific transport owns a reusable pool. Invalid routing fails closed.
func (s *Server) clientFor(proxyRaw string) (*http.Client, error) {
	proxyRaw = strings.TrimSpace(proxyRaw)
	if proxyRaw == "" {
		return s.Client, nil
	}
	parsed, err := url.Parse(proxyRaw)
	if err != nil || parsed.Hostname() == "" || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Path != "" && parsed.Path != "/") {
		return nil, errors.New("invalid proxy configuration")
	}
	switch parsed.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, errors.New("unsupported proxy scheme")
	}
	base, ok := s.Client.Transport.(*http.Transport)
	if !ok {
		return nil, errors.New("proxy transport unavailable")
	}
	s.clientMu.Lock()
	defer s.clientMu.Unlock()
	if client := s.proxyClients[proxyRaw]; client != nil {
		return client, nil
	}
	if s.proxyClients == nil {
		s.proxyClients = make(map[string]*http.Client)
	}
	const maxProxyClients = 32
	if len(s.proxyClients) >= maxProxyClients {
		for key, client := range s.proxyClients {
			client.CloseIdleConnections()
			delete(s.proxyClients, key)
			break
		}
	}
	transport := base.Clone()
	transport.Proxy = http.ProxyURL(parsed)
	client := &http.Client{Transport: transport, Timeout: 0, CheckRedirect: s.Client.CheckRedirect}
	s.proxyClients[proxyRaw] = client
	return client, nil
}

func readBody(stream pluginv1.TransportPlugin_ForwardServer, hasBody bool) ([]byte, error) {
	if !hasBody {
		return nil, nil
	}
	var body bytes.Buffer
	for {
		frame, err := stream.Recv()
		if err != nil {
			return nil, err
		}
		if chunk := frame.GetBodyChunk(); len(chunk) > 0 {
			_, _ = body.Write(chunk)
		}
		if frame.GetBodyEnd() {
			return body.Bytes(), nil
		}
	}
}

func headersFromPlugin(src map[string]*pluginv1.HeaderValues) http.Header {
	header := make(http.Header, len(src))
	for key, values := range src {
		if values == nil {
			continue
		}
		for _, value := range values.GetValues() {
			header.Add(key, value)
		}
	}
	return header
}

func headersToPlugin(src http.Header) map[string]*pluginv1.HeaderValues {
	out := make(map[string]*pluginv1.HeaderValues, len(src))
	for key, values := range src {
		copied := append([]string(nil), values...)
		out[key] = &pluginv1.HeaderValues{Values: copied}
	}
	return out
}

func errorFrame(code, message string, sent bool) *pluginv1.ForwardResponse {
	return &pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_Error{Error: &pluginv1.ForwardResponseError{
		Code: code, Message: message, RequestSent: sent,
	}}}
}

func valueOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
