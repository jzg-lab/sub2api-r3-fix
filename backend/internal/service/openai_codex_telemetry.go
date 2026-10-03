package service

// OpenAI Codex 客户端遥测模拟。
//
// 背景：真实 Codex Desktop/CLI 在每次 Responses 会话之外，还会向上游异步上报
// 客户端分析事件（chatgpt.com/backend-api/codex/analytics-events/events）与
// Statsig OTLP 指标（ab.chatgpt.com/otlp/v1/metrics）。经中转的账号长期只消耗
// 额度却零遥测，是"非真实客户端"的统计特征。本模块按所选 Codex 指纹为每个
// OAuth 透传请求补齐这两路遥测：
//
//   - 身份完全复用最终出站请求头（Authorization / chatgpt-account-id /
//     User-Agent / originator / version / session_id / conversation_id），
//     保证遥测与推理流量同源自洽，不引入任何本机真实标识；
//   - 发送走与主请求完全相同的 HTTPUpstream（同账号代理出口、同并发计费），
//     且经 DoWithTLS 携带账号的 TLS 指纹模板——遥测与推理若向同一上游呈现
//     两套 ClientHello，本身就是"跨链路身份冲突"判据；
//   - 异步队列，发送失败只记日志，绝不影响代理响应；
//   - OPENAI_CODEX_TELEMETRY_ENABLED=0/false/off 可部署级关闭，默认开启。
//
// 实现参考 codex2api PR #666（bxb1337），按本仓透传链路重写接入。

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

const (
	openAICodexTelemetryAnalyticsURL = "https://chatgpt.com/backend-api/codex/analytics-events/events"
	openAICodexTelemetryMetricsURL   = "https://ab.chatgpt.com/otlp/v1/metrics"
	// codex-rs 源码中的公开 Statsig 客户端 SDK key（非机密，官方客户端同款）。
	openAICodexTelemetryStatsigKey    = "client-MkRuleRQBd6qakfnDYqJVR9JuXcY57Ljly3vi5JVUIO"
	openAICodexTelemetryQueueSize     = 256
	openAICodexTelemetryTimeout       = 10 * time.Second
	openAICodexTelemetryStateTTL      = 5 * time.Minute
	openAICodexTelemetryMaxEventBytes = 8 << 20
)

// openAICodexTelemetryIdentity 一次请求固化的发送身份，全部取自最终出站请求。
// tlsProfile 取自该账号的 TLS 指纹配置：遥测必须与推理同 ClientHello 出站，
// 同一账号一分钟内对 chatgpt.com / ab.chatgpt.com 呈现两套 TLS 指纹是最直接的
// 跨链路身份冲突判据。
type openAICodexTelemetryIdentity struct {
	account     *Account
	accessToken string
	accountID   string
	proxyURL    string
	userAgent   string
	originator  string
	version     string
	tlsProfile  *tlsfingerprint.Profile
}

// openAICodexTelemetryProfile 本轮 turn 的遥测上下文。
type openAICodexTelemetryProfile struct {
	client      openAICodexTelemetryIdentity
	sessionID   string
	threadID    string
	turnID      string
	rootTurnID  string
	model       string
	effort      string
	serviceTier string
	started     time.Time
	firstThread bool
	dynamicTool bool
	command     bool
	fileChange  bool
	turnMeta    gjson.Result
}

// openAICodexTelemetryTerminal 一轮 turn 的终态观测。
type openAICodexTelemetryTerminal struct {
	status     string
	body       []byte
	firstEvent time.Time
	firstToken time.Time
}

// openAICodexTelemetryAttempt 单次上游尝试的遥测句柄。
type openAICodexTelemetryAttempt struct {
	profile    openAICodexTelemetryProfile
	firstEvent time.Time
	firstToken time.Time
	done       sync.Once
}

// openAICodexTelemetryJob 一条待发送的遥测批次。
type openAICodexTelemetryJob struct {
	client  openAICodexTelemetryIdentity
	url     string
	body    []byte
	metrics bool
}

// openAICodexTelemetryManager 进程内遥测队列与账号级状态容器。
type openAICodexTelemetryManager struct {
	once     sync.Once
	queue    chan openAICodexTelemetryJob
	mu       sync.Mutex
	threads  map[string]time.Time
	metrics  map[int64]*openAICodexMetricState
	upstream HTTPUpstream
	// proxyLookup 在发送期补齐账号的代理出口（见 bindProxyLookup）。
	proxyLookup func(ctx context.Context, account *Account) (string, error)
}

// The lookup distinguishes a valid direct route from an unavailable proxy.
func (m *openAICodexTelemetryManager) bindProxyLookup(
	lookup func(ctx context.Context, account *Account) (string, error),
) {
	if m == nil || lookup == nil {
		return
	}
	m.mu.Lock()
	m.proxyLookup = lookup
	m.mu.Unlock()
}

// resolveProxyURL reloads the route; an empty URL without error means direct.
func (m *openAICodexTelemetryManager) resolveProxyURL(
	ctx context.Context, job openAICodexTelemetryJob,
) (string, error) {
	if job.client.account == nil {
		return "", errOpenAIOAuthProxyUnavailable
	}
	m.mu.Lock()
	lookup := m.proxyLookup
	m.mu.Unlock()
	if lookup == nil {
		return "", errOpenAIOAuthProxyUnavailable
	}
	current, err := lookup(ctx, job.client.account)
	if err != nil {
		return "", err
	}
	if validateOpenAIOAuthProxyURL(current) != nil {
		return "", errOpenAIOAuthProxyInvalid
	}
	// A queued request is not authority to reuse or change an old route.
	if queued := strings.TrimSpace(job.client.proxyURL); (queued != "" || job.client.account.ProxyID == nil) && queued != current {
		return "", errOpenAIOAuthProxyInvalid
	}
	return current, nil
}

var openAICodexTelemetryGlobal = &openAICodexTelemetryManager{
	queue:   make(chan openAICodexTelemetryJob, openAICodexTelemetryQueueSize),
	threads: make(map[string]time.Time),
	metrics: make(map[int64]*openAICodexMetricState),
}

// openAICodexTelemetryEnabled 合并部署级开关；测试进程默认关闭。
func openAICodexTelemetryEnabled() bool {
	if strings.HasSuffix(os.Args[0], ".test") || strings.HasSuffix(os.Args[0], ".test.exe") {
		return os.Getenv("OPENAI_CODEX_TELEMETRY_TEST_ENABLE") == "1"
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("OPENAI_CODEX_TELEMETRY_ENABLED"))) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// openAIProbeTelemetryEnabled 探针 turn 专用遥测闸，默认关闭，需双开关显式开启
// （全局 OPENAI_CODEX_TELEMETRY_ENABLED 开 + OPENAI_PROBE_TELEMETRY=1）。
// 2026-09-18 生产实证：探针遥测经主请求同款 HTTPUpstream 发送，analytics 事件
// （chatgpt.com，服务端 400）与随后的探针请求复用同一条 h2 连接，服务端按连接
// 降级对待探针（200 全长流但缺 response.completed usage，生产 0/N，遥测关闭后
// 恢复）；同 body/同代理/同 TLS 传输的进程外复现全部通过，仅"同连接先 400 再
// 探针"这一生产行为失败。真实客户端网络故障时同样会丢遥测，缺遥测不构成异常
// 特征（见 send 的出口硬闸注释），探针默认不发。
func openAIProbeTelemetryEnabled() bool {
	return openAICodexTelemetryEnabled() &&
		strings.EqualFold(strings.TrimSpace(os.Getenv("OPENAI_PROBE_TELEMETRY")), "1")
}

// beginOpenAICodexTelemetry 为符合条件的透传请求创建遥测观测并发送初始化数据。
// upstreamHeaders 必须是 buildUpstreamRequestOpenAIPassthrough 收口后的终态头，
// 遥测身份与推理请求因此逐字节一致。不符合条件时返回 nil（所有方法 nil 安全）。
func beginOpenAICodexTelemetry(
	s *OpenAIGatewayService,
	account *Account,
	body []byte,
	upstreamHeaders http.Header,
	proxyURL string,
	imageIntent bool,
	compactPath bool,
) *openAICodexTelemetryAttempt {
	if s == nil || s.httpUpstream == nil || account == nil || upstreamHeaders == nil {
		return nil
	}
	if !account.UsesOpenAICodexProtocol() || compactPath || imageIntent {
		return nil
	}
	if !openAICodexTelemetryEnabled() || !gjson.ValidBytes(body) {
		return nil
	}
	identity := openAICodexTelemetryIdentity{
		account:     account,
		accessToken: strings.TrimSpace(strings.TrimPrefix(upstreamHeaders.Get("authorization"), "Bearer ")),
		accountID:   strings.TrimSpace(upstreamHeaders.Get("chatgpt-account-id")),
		proxyURL:    proxyURL,
		userAgent:   strings.TrimSpace(upstreamHeaders.Get("user-agent")),
		originator:  strings.TrimSpace(upstreamHeaders.Get("originator")),
		version:     strings.TrimSpace(upstreamHeaders.Get("version")),
		tlsProfile:  s.resolveTLSProfile(account),
	}
	if identity.accessToken == "" || identity.accountID == "" || identity.userAgent == "" {
		return nil
	}
	if identity.originator == "" {
		identity.originator = resolveCodexOutboundIdentity(identity.userAgent).originator
	}
	if identity.version == "" {
		identity.version = CodexCanonicalClientVersion()
	}

	turnMeta := openAICodexTurnMetadata(body, upstreamHeaders)
	sessionID := firstNonEmptyOpenAIString(
		strings.TrimSpace(upstreamHeaders.Get("session_id")),
		gjson.GetBytes(body, "client_metadata.session_id").String(),
		turnMeta.Get("session_id").String(),
	)
	threadID := firstNonEmptyOpenAIString(
		strings.TrimSpace(upstreamHeaders.Get("conversation_id")),
		gjson.GetBytes(body, "client_metadata.thread_id").String(),
		turnMeta.Get("thread_id").String(),
		sessionID,
	)
	if sessionID == "" {
		sessionID = uuid.NewString()
	}
	if threadID == "" {
		threadID = sessionID
	}
	turnID := firstNonEmptyOpenAIString(turnMeta.Get("turn_id").String(), uuid.NewString())

	profile := openAICodexTelemetryProfile{
		client:      identity,
		sessionID:   sessionID,
		threadID:    threadID,
		turnID:      turnID,
		rootTurnID:  firstNonEmptyOpenAIString(turnMeta.Get("root_turn_id").String(), turnID),
		model:       firstNonEmptyOpenAIString(gjson.GetBytes(body, "model").String(), "gpt-6-astra"),
		effort:      firstNonEmptyOpenAIString(gjson.GetBytes(body, "reasoning.effort").String(), "medium"),
		serviceTier: firstNonEmptyOpenAIString(gjson.GetBytes(body, "service_tier").String(), "default"),
		started:     time.Now(),
		turnMeta:    turnMeta,
	}
	manager := openAICodexTelemetryGlobal
	manager.bindUpstream(s.httpUpstream)
	profile.firstThread = manager.markThread(account.ID, profile.threadID, profile.started)
	// 与真实 Codex 会话同分布的工具事件抽样：编码会话的 turn 大多伴随
	// 动态工具 / 命令执行 / 文件修改事件，全零反而异常。
	profile.dynamicTool = rand.IntN(5) < 2
	profile.command = profile.dynamicTool && rand.IntN(2) == 0
	profile.fileChange = rand.IntN(5) == 0

	manager.enqueueAnalytics(openAICodexInitializationEvents(profile))
	manager.touchMetrics(profile)
	return &openAICodexTelemetryAttempt{profile: profile}
}

// finishFailed 在上游传输失败或 HTTP>=400 时结束本轮遥测。
func (a *openAICodexTelemetryAttempt) finishFailed() {
	a.finish("failed", nil)
}

// observe 用遥测观测器包裹成功响应体，解析 SSE 终态与用时。
func (a *openAICodexTelemetryAttempt) observe(resp *http.Response) {
	if a == nil || resp == nil || resp.Body == nil {
		return
	}
	resp.Body = &openAICodexTelemetryBody{ReadCloser: resp.Body, attempt: a}
}

// finish 仅一次地提交终止事件和本轮指标。
func (a *openAICodexTelemetryAttempt) finish(status string, terminal []byte) {
	if a == nil {
		return
	}
	a.done.Do(func() {
		result := openAICodexTelemetryTerminal{
			status: status, body: terminal,
			firstEvent: a.firstEvent, firstToken: a.firstToken,
		}
		openAICodexTelemetryGlobal.enqueueAnalytics(openAICodexTerminalEvents(a.profile, result))
		openAICodexTelemetryGlobal.recordTurnMetrics(a.profile, result)
	})
}

// bindUpstream 记录遥测发送使用的上游通道（与主请求同一 HTTPUpstream）。
func (m *openAICodexTelemetryManager) bindUpstream(upstream HTTPUpstream) {
	m.mu.Lock()
	if m.upstream == nil {
		m.upstream = upstream
	}
	m.mu.Unlock()
}

// start 按需启动发送 worker 与每分钟指标刷新循环。
func (m *openAICodexTelemetryManager) start() {
	m.once.Do(func() {
		go m.worker()
		go func() {
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			for now := range ticker.C {
				m.flushMetrics(now)
			}
		}()
	})
}

// worker 串行发送队列中的遥测任务。
func (m *openAICodexTelemetryManager) worker() {
	debug := os.Getenv("OPENAI_CODEX_TELEMETRY_DEBUG") == "1"
	for job := range m.queue {
		if err := m.send(job); err != nil {
			logger.LegacyPrintf("service.openai_codex_telemetry", "[Codex 遥测] 发送失败: %v", err)
		} else if debug {
			logger.LegacyPrintf("service.openai_codex_telemetry", "[Codex 遥测] 发送成功: %s (%d 字节)", job.url, len(job.body))
		}
	}
}

// enqueue 非阻塞入队，队列满时丢弃（遥测永远不许反压主链路）。
func (m *openAICodexTelemetryManager) enqueue(job openAICodexTelemetryJob) {
	m.start()
	select {
	case m.queue <- job:
	default:
		logger.LegacyPrintf("service.openai_codex_telemetry", "[Codex 遥测] 队列已满，丢弃本批数据")
	}
}

// markThread 记录账号观察到的 thread，并报告它是否首次出现。
func (m *openAICodexTelemetryManager) markThread(accountID int64, threadID string, now time.Time) bool {
	key := strconv.FormatInt(accountID, 10) + ":" + threadID
	m.mu.Lock()
	defer m.mu.Unlock()
	_, found := m.threads[key]
	if len(m.threads) >= 4096 {
		m.threads = make(map[string]time.Time)
		found = false
	}
	m.threads[key] = now
	return !found
}

// enqueueAnalytics 编码并排队发送一批分析事件。
func (m *openAICodexTelemetryManager) enqueueAnalytics(events []openAICodexAnalyticsEvent) {
	if len(events) == 0 {
		return
	}
	body, err := json.Marshal(map[string]any{"events": events})
	if err == nil {
		m.enqueue(openAICodexTelemetryJob{client: events[0].client, url: openAICodexTelemetryAnalyticsURL, body: body})
	}
}

// send 经主请求同款 HTTPUpstream 发送一批遥测（同代理出口、同并发计费、同 TLS 指纹）。
func (m *openAICodexTelemetryManager) send(job openAICodexTelemetryJob) error {
	m.mu.Lock()
	upstream := m.upstream
	m.mu.Unlock()
	if upstream == nil || job.client.account == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), openAICodexTelemetryTimeout)
	defer cancel()
	proxyURL, err := m.resolveProxyURL(ctx, job)
	if err != nil {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, job.url, bytes.NewReader(job.body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	if job.metrics {
		req.Header.Set("User-Agent", "OTel-OTLP-Exporter-Rust/0.31.0")
		req.Header.Set("statsig-api-key", openAICodexTelemetryStatsigKey)
	} else {
		req.Header.Set("Authorization", "Bearer "+job.client.accessToken)
		req.Header.Set("Chatgpt-Account-Id", job.client.accountID)
		req.Header.Set("User-Agent", job.client.userAgent)
		req.Header.Set("Originator", job.client.originator)
	}
	resp, err := upstream.DoWithTLS(req, proxyURL, job.client.account.ID, job.client.account.Concurrency, job.client.tlsProfile)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &openAICodexTelemetryHTTPError{status: resp.StatusCode}
	}
	return nil
}

type openAICodexTelemetryHTTPError struct{ status int }

func (e *openAICodexTelemetryHTTPError) Error() string {
	return "telemetry upstream: " + http.StatusText(e.status)
}

// openAICodexTelemetryBody 响应体观测器：旁路解析 SSE，不改动下游字节流。
type openAICodexTelemetryBody struct {
	io.ReadCloser
	attempt  *openAICodexTelemetryAttempt
	pending  []byte
	event    []byte
	dropping bool
}

func (b *openAICodexTelemetryBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.observe(p[:n])
	}
	if err == io.EOF {
		b.flushJSON()
		b.attempt.finish("interrupted", nil)
	}
	return n, err
}

func (b *openAICodexTelemetryBody) Close() error {
	b.attempt.finish("interrupted", nil)
	return b.ReadCloser.Close()
}

func (b *openAICodexTelemetryBody) observe(data []byte) {
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		part := data
		if i >= 0 {
			part = data[:i]
		}
		if !b.dropping && len(b.pending)+len(part) <= openAICodexTelemetryMaxEventBytes {
			b.pending = append(b.pending, part...)
		} else {
			b.pending, b.event, b.dropping = nil, nil, true
		}
		if i < 0 {
			return
		}
		b.line(bytes.TrimSuffix(b.pending, []byte{'\r'}))
		b.pending = b.pending[:0]
		data = data[i+1:]
	}
}

func (b *openAICodexTelemetryBody) line(line []byte) {
	if len(line) == 0 {
		b.processEvent(b.event)
		b.event, b.dropping = b.event[:0], false
		return
	}
	if bytes.HasPrefix(line, []byte("data:")) && !b.dropping {
		part := bytes.TrimSpace(line[5:])
		if len(b.event)+len(part) <= openAICodexTelemetryMaxEventBytes {
			b.event = append(b.event, part...)
		}
	}
}

// processEvent 解析 SSE 事件并记录首包、首 token 与终态。
func (b *openAICodexTelemetryBody) processEvent(data []byte) {
	if !json.Valid(data) {
		return
	}
	now := time.Now()
	if b.attempt.firstEvent.IsZero() {
		b.attempt.firstEvent = now
	}
	typ := gjson.GetBytes(data, "type").String()
	if b.attempt.firstToken.IsZero() && strings.HasSuffix(typ, ".delta") {
		b.attempt.firstToken = now
	}
	switch typ {
	case "response.completed":
		b.attempt.finish("completed", data)
	case "response.failed", "error":
		b.attempt.finish("failed", data)
	case "response.incomplete":
		b.attempt.finish("interrupted", data)
	}
}

// flushJSON 在非流式响应结束时解析最终状态。
func (b *openAICodexTelemetryBody) flushJSON() {
	if len(b.event) > 0 {
		b.processEvent(b.event)
		return
	}
	if json.Valid(b.pending) {
		switch gjson.GetBytes(b.pending, "status").String() {
		case "completed":
			b.attempt.finish("completed", b.pending)
		case "failed":
			b.attempt.finish("failed", b.pending)
		}
	}
}

// openAICodexTurnMetadata 读取请求体 client_metadata 或终态头中的 turn metadata。
func openAICodexTurnMetadata(body []byte, headers http.Header) gjson.Result {
	raw := gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata")
	if raw.Type == gjson.String && gjson.Valid(raw.String()) {
		return gjson.Parse(raw.String())
	}
	if value := strings.TrimSpace(headers.Get("x-codex-turn-metadata")); gjson.Valid(value) {
		return gjson.Parse(value)
	}
	return gjson.Result{}
}

func firstNonEmptyOpenAIString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// openAICodexMaxInt64 / openAICodexMaxFloat64 避免使用内建 max：
// 测试文件在包级声明了 max(int,int)，vet 含测试编译时会遮蔽内建。
func openAICodexMaxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func openAICodexMaxFloat64(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
