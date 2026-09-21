// Package httpclient 提供共享 HTTP 客户端池
//
// 性能优化说明：
// 原实现在多个服务中重复创建 http.Client：
// 1. proxy_probe_service.go: 每次探测创建新客户端
// 2. pricing_service.go: 每次请求创建新客户端
// 3. turnstile_service.go: 每次验证创建新客户端
// 4. github_release_service.go: 每次请求创建新客户端
// 5. claude_usage_service.go: 每次请求创建新客户端
//
// 新实现使用统一的客户端池：
// 1. 相同配置复用同一 http.Client 实例
// 2. 复用 Transport 连接池，减少 TCP/TLS 握手开销
// 3. 支持 HTTP/HTTPS/SOCKS5/SOCKS5H 代理
// 4. 代理配置失败时直接返回错误，不会回退到直连（避免 IP 关联风险）
package httpclient

import (
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyurl"
	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyutil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/servertiming"
	"github.com/Wei-Shaw/sub2api/internal/util/urlvalidator"
)

// Transport 连接池默认配置
const (
	defaultMaxIdleConns        = 100              // 最大空闲连接数
	defaultMaxIdleConnsPerHost = 10               // 每个主机最大空闲连接数
	defaultIdleConnTimeout     = 90 * time.Second // 空闲连接超时时间（建议小于上游 LB 超时）
	defaultDialTimeout         = 5 * time.Second  // TCP 连接超时（含代理握手），代理不通时快速失败
	defaultTLSHandshakeTimeout = 5 * time.Second  // TLS 握手超时
	validatedHostTTL           = 30 * time.Second // DNS Rebinding 校验缓存 TTL
)

// 共享客户端池上限与回收（r17x G 项）：键含 ProxyURL，代理编辑/轮换会持续产生
// 新键，旧实现只增不减。这里给出两条回收路径：超过上限按 LRU 驱逐最久未用；
// 空闲超过 TTL 的条目在 GetClient 时机会式清扫。驱逐只关闭 idle 连接并从池中
// 移除，调用方已持有的 *http.Client 仍可继续使用（in-flight 请求不受影响）。
const (
	maxSharedClients     = 256              // 池内客户端条目上限（LRU 驱逐）
	sharedClientIdleTTL  = 30 * time.Minute // 条目空闲回收阈值
	sharedClientSweepGap = 5 * time.Minute  // 机会式清扫的最小间隔
)

// Options 定义共享 HTTP 客户端的构建参数
type Options struct {
	ProxyURL              string        // 代理 URL（支持 http/https/socks5/socks5h）
	Timeout               time.Duration // 请求总超时时间
	ResponseHeaderTimeout time.Duration // 等待响应头超时时间
	InsecureSkipVerify    bool          // 是否跳过 TLS 证书验证（已禁用，不允许设置为 true）
	ValidateResolvedIP    bool          // 是否校验解析后的 IP（防止 DNS Rebinding）
	AllowPrivateHosts     bool          // 允许私有地址解析（与 ValidateResolvedIP 一起使用）

	// 可选的连接池参数（不设置则使用默认值）
	MaxIdleConns        int // 最大空闲连接总数（默认 100）
	MaxIdleConnsPerHost int // 每主机最大空闲连接（默认 10）
	MaxConnsPerHost     int // 每主机最大连接数（默认 0 无限制）
}

// sharedClientEntry 是池内条目：client 供复用，baseTransport 供驱逐时关闭
// idle 连接（servertiming 包装层不透传 CloseIdleConnections，必须留原始引用）。
type sharedClientEntry struct {
	client        *http.Client
	baseTransport *http.Transport
	lastUsed      atomic.Int64 // unix nano
}

// clientPool 带上限与空闲回收的共享客户端池。Get 为热路径，读写都走
// RWMutex；条目数与清扫用一把锁内的简单 map 实现，规模（上限 256）下
// 开销可忽略。
type clientPool struct {
	mu        sync.RWMutex
	entries   map[string]*sharedClientEntry
	lastSwept time.Time
}

func newClientPool() *clientPool {
	return &clientPool{entries: make(map[string]*sharedClientEntry)}
}

func (p *clientPool) get(key string) *http.Client {
	p.mu.RLock()
	entry, ok := p.entries[key]
	p.mu.RUnlock()
	if !ok {
		return nil
	}
	entry.lastUsed.Store(time.Now().UnixNano())
	return entry.client
}

func (p *clientPool) getOrBuild(key string, build func() (*http.Client, *http.Transport, error)) (*http.Client, error) {
	if client := p.get(key); client != nil {
		return client, nil
	}
	client, baseTransport, err := build()
	if err != nil {
		return nil, err
	}
	entry := &sharedClientEntry{client: client, baseTransport: baseTransport}
	entry.lastUsed.Store(time.Now().UnixNano())
	p.mu.Lock()
	p.entries[key] = entry
	overflow := len(p.entries) - maxSharedClients
	if overflow > 0 {
		p.evictLRULocked(overflow)
	}
	p.mu.Unlock()
	return client, nil
}

// evictLRULocked 收缩池到上限内。驱逐是尽力而为：in-flight 请求所在的
// client 仍被调用方持有，这里只关 idle 连接 + 移出池。sort.Slice 在锁内
// 执行，但只在溢出时发生（频率 = 键基数变化率，不是请求率）。
func (p *clientPool) evictLRULocked(n int) {
	keys := make([]string, 0, len(p.entries))
	for k := range p.entries {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return p.entries[keys[i]].lastUsed.Load() < p.entries[keys[j]].lastUsed.Load()
	})
	if n > len(keys) {
		n = len(keys)
	}
	for _, key := range keys[:n] {
		entry := p.entries[key]
		delete(p.entries, key)
		if entry != nil && entry.baseTransport != nil {
			entry.baseTransport.CloseIdleConnections()
		}
	}
}

// sweepIdleLocked 清扫空闲过期的条目（调用方持写锁）。驱逐语义同 LRU：
// 只关 idle 连接 + 移出池，调用方已持有的 client 继续可用。
func (p *clientPool) sweepIdleLocked(now time.Time) {
	for key, entry := range p.entries {
		if now.Sub(time.Unix(0, entry.lastUsed.Load())) > sharedClientIdleTTL {
			delete(p.entries, key)
			if entry.baseTransport != nil {
				entry.baseTransport.CloseIdleConnections()
			}
		}
	}
}

var sharedClients = newClientPool()

// 允许测试替换校验函数，生产默认指向真实实现。
var validateResolvedIP = urlvalidator.ValidateResolvedIP

// GetClient 返回共享的 HTTP 实例
// 性能优化：相同配置复用同一客户端，避免重复创建 Transport
// 安全说明：代理配置失败时直接返回错误，不会回退到直连，避免 IP 关联风险
func GetClient(opts Options) (*http.Client, error) {
	key := buildClientKey(opts)
	if client := sharedClients.get(key); client != nil {
		return client, nil
	}

	client, err := sharedClients.getOrBuild(key, func() (*http.Client, *http.Transport, error) {
		return buildClient(opts)
	})
	if err != nil {
		return nil, err
	}

	// 机会式清扫：距上次清扫超过间隔才做，写锁内全量扫描（池上限 256，开销可忽略）。
	sharedClients.mu.Lock()
	if now := time.Now(); now.Sub(sharedClients.lastSwept) > sharedClientSweepGap {
		sharedClients.sweepIdleLocked(now)
		sharedClients.lastSwept = now
	}
	sharedClients.mu.Unlock()

	return client, nil
}

func buildClient(opts Options) (*http.Client, *http.Transport, error) {
	transport, err := buildTransport(opts)
	if err != nil {
		return nil, nil, err
	}

	var rt http.RoundTripper = transport
	if opts.ValidateResolvedIP && !opts.AllowPrivateHosts {
		rt = newValidatedTransport(transport)
	}
	rt = servertiming.WrapRoundTripper(rt)
	return &http.Client{
		Transport: rt,
		Timeout:   opts.Timeout,
	}, transport, nil
}

func buildTransport(opts Options) (*http.Transport, error) {
	// 使用自定义值或默认值
	maxIdleConns := opts.MaxIdleConns
	if maxIdleConns <= 0 {
		maxIdleConns = defaultMaxIdleConns
	}
	maxIdleConnsPerHost := opts.MaxIdleConnsPerHost
	if maxIdleConnsPerHost <= 0 {
		maxIdleConnsPerHost = defaultMaxIdleConnsPerHost
	}

	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: defaultDialTimeout,
		}).DialContext,
		TLSHandshakeTimeout:   defaultTLSHandshakeTimeout,
		MaxIdleConns:          maxIdleConns,
		MaxIdleConnsPerHost:   maxIdleConnsPerHost,
		MaxConnsPerHost:       opts.MaxConnsPerHost, // 0 表示无限制
		IdleConnTimeout:       defaultIdleConnTimeout,
		ResponseHeaderTimeout: opts.ResponseHeaderTimeout,
	}

	if opts.InsecureSkipVerify {
		// 安全要求：禁止跳过证书验证，避免中间人攻击。
		return nil, fmt.Errorf("insecure_skip_verify is not allowed; install a trusted certificate instead")
	}

	_, parsed, err := proxyurl.Parse(opts.ProxyURL)
	if err != nil {
		return nil, err
	}
	if parsed == nil {
		return transport, nil
	}

	if err := proxyutil.ConfigureTransportProxy(transport, parsed); err != nil {
		return nil, err
	}

	return transport, nil
}

func buildClientKey(opts Options) string {
	return fmt.Sprintf("%s|%s|%s|%t|%t|%t|%d|%d|%d",
		strings.TrimSpace(opts.ProxyURL),
		opts.Timeout.String(),
		opts.ResponseHeaderTimeout.String(),
		opts.InsecureSkipVerify,
		opts.ValidateResolvedIP,
		opts.AllowPrivateHosts,
		opts.MaxIdleConns,
		opts.MaxIdleConnsPerHost,
		opts.MaxConnsPerHost,
	)
}

type validatedTransport struct {
	base           http.RoundTripper
	validatedHosts sync.Map // map[string]time.Time, value 为过期时间
	now            func() time.Time
}

func newValidatedTransport(base http.RoundTripper) *validatedTransport {
	return &validatedTransport{
		base: base,
		now:  time.Now,
	}
}

func (t *validatedTransport) isValidatedHost(host string, now time.Time) bool {
	if t == nil {
		return false
	}
	raw, ok := t.validatedHosts.Load(host)
	if !ok {
		return false
	}
	expireAt, ok := raw.(time.Time)
	if !ok {
		t.validatedHosts.Delete(host)
		return false
	}
	if now.Before(expireAt) {
		return true
	}
	t.validatedHosts.Delete(host)
	return false
}

func (t *validatedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req != nil && req.URL != nil {
		host := strings.ToLower(strings.TrimSpace(req.URL.Hostname()))
		if host != "" {
			now := time.Now()
			if t != nil && t.now != nil {
				now = t.now()
			}
			if !t.isValidatedHost(host, now) {
				if err := validateResolvedIP(host); err != nil {
					return nil, err
				}
				t.validatedHosts.Store(host, now.Add(validatedHostTTL))
			}
		}
	}
	if t == nil || t.base == nil {
		return nil, fmt.Errorf("validated transport base is nil")
	}
	return t.base.RoundTrip(req)
}
