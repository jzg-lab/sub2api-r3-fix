package service

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

const defaultAuthBrowserLauncherTimeout = 45 * time.Second

// =============================================================================
// 授权浏览器直拉（方案A，2026-09-22 用户批准）
//
// 痛点：授权流程三处人肉搬运（复制链接→开 applet→手选 IP）全是出错点
// （9/22 实录：裸无痕窗口从日本轮换 IPv6 授权、美国桶使用 → 1149/1150
// 授权地跳变降智）。而「生成授权链接」那一刻后端已存了 proxy_id
// （pending_auth_sessions），信息没丢，只是没人消费。
//
// 语义：管理端 POST /openai/launch-auth-browser {session_id} →
//   1) sessionStore.Get 读回该次授权绑定的 proxy_id（授权 IP 唯一事实源）
//   2) proxyRepo.GetByID 拿桶 → 映射本机免认证入口（带认证的 socks5h
//      autossh 口 Chrome 用不了——Chrome --proxy-server 不支持 SOCKS5
//      认证，统一走 mihomo 免认证监听口）
//   3) exec launch.sh <state前12位> <auth_url> <本机入口> → Chrome 弹窗
//      （已带桶代理+授权链接+隔离配置+纽约时区）
//
// 安全边界：
//   - SUB2API_AUTH_BROWSER_LAUNCHER 环境变量指到 launch.sh 才启用
//    （默认关：不绑死本机路径，误配不炸）。
//   - 只 exec 该脚本本身；参数全部经 shell-free 的 exec.Command argv
//     传递，绝不过 shell。
//   - session 过期/已消费 → 拒绝（Consume 在 code 交换时才发生，Get 幂等）。
// =============================================================================

// openAIAuthBrowserLauncher 授权浏览器直拉服务。
type OpenAIAuthBrowserLauncher struct {
	launcherPath string
	sessionStore OpenAIOAuthSessionStore
	proxyRepo    ProxyRepository
	now          func() time.Time
	newCommand   func(context.Context, string, ...string) *exec.Cmd
	timeout      time.Duration

	mu       sync.Mutex
	inFlight map[string]struct{}
}

func NewOpenAIAuthBrowserLauncher(
	cfg *config.Config,
	sessionStore OpenAIOAuthSessionStore,
	proxyRepo ProxyRepository,
) *OpenAIAuthBrowserLauncher {
	path := ""
	if cfg != nil {
		path = strings.TrimSpace(cfg.Gateway.AuthBrowserLauncher)
	}
	if path == "" {
		return nil
	}
	if sessionStore == nil || proxyRepo == nil {
		return nil
	}
	return &OpenAIAuthBrowserLauncher{
		launcherPath: path,
		sessionStore: sessionStore,
		proxyRepo:    proxyRepo,
		now:          time.Now,
		newCommand:   exec.CommandContext,
		timeout:      defaultAuthBrowserLauncherTimeout,
		inFlight:     make(map[string]struct{}),
	}
}

// openAIAuthBrowserLocalIngress 把业务桶映射到本机免认证入口。
// 静态 ISP 桶（socks5h://127.0.0.1:17911-17914）远端要认证，Chrome 不支持
// SOCKS5 认证——mihomo-buckets 已为四桶加了免认证监听口 17921-17924
// （同 autossh 隧道同出口，只加认证终结层）。其余桶（novproxy 等）本就
// 免认证，原样返回。
func openAIAuthBrowserLocalIngress(p *Proxy) string {
	if p == nil {
		return ""
	}
	if strings.EqualFold(p.Protocol, "socks5h") &&
		p.Host == "127.0.0.1" && p.Port >= 17911 && p.Port <= 17914 {
		return fmt.Sprintf("http://127.0.0.1:%d", p.Port+10)
	}
	// 带凭据的代理一律不行（无法安全传给 Chrome）；本地免认证口原样。
	if p.Username != "" || p.Password != "" {
		return ""
	}
	return p.Protocol + "://" + net_JoinHostPort(p.Host, strconv.Itoa(p.Port))
}

// net_JoinHostPort 避免 import net 只为一个 helper。
func net_JoinHostPort(host, port string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + port
	}
	return host + ":" + port
}

// OpenAIAuthBrowserLaunchResult 启动结果（回显给前端确认弹窗文案）。
type OpenAIAuthBrowserLaunchResult struct {
	Launched       bool   `json:"launched"`
	AlreadyRunning bool   `json:"already_running"`
	ProfileTag     string `json:"profile_tag"`
	ProxyName      string `json:"proxy_name"`
	ExitIngress    string `json:"exit_ingress"`
	AuthURL        string `json:"auth_url"`
	Output         string `json:"output"`
}

func (l *OpenAIAuthBrowserLauncher) claimLaunch(sessionID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inFlight == nil {
		l.inFlight = make(map[string]struct{})
	}
	if _, exists := l.inFlight[sessionID]; exists {
		return false
	}
	l.inFlight[sessionID] = struct{}{}
	return true
}

func (l *OpenAIAuthBrowserLauncher) releaseLaunch(sessionID string) {
	l.mu.Lock()
	delete(l.inFlight, sessionID)
	l.mu.Unlock()
}

// Launch 按授权会话直拉激活浏览器。sessionID 即生成链接时返回的
// session_id（前端手里有，不用重新解析 URL）。
func (l *OpenAIAuthBrowserLauncher) Launch(ctx context.Context, sessionID string) (*OpenAIAuthBrowserLaunchResult, error) {
	if l == nil {
		return nil, fmt.Errorf("auth browser launcher is not configured")
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}
	session, err := l.sessionStore.Get(ctx, sessionID)
	if err != nil || session == nil {
		return nil, fmt.Errorf("authorization session not found or expired")
	}
	if l.now().After(session.CreatedAt.Add(openAIOAuthSessionTTL)) {
		return nil, fmt.Errorf("authorization session expired")
	}
	if strings.TrimSpace(session.State) == "" {
		return nil, fmt.Errorf("authorization session state is invalid")
	}

	proxy, err := l.proxyRepo.GetByID(ctx, session.ProxyID)
	if err != nil || proxy == nil || !proxy.IsActive() {
		return nil, fmt.Errorf("authorization proxy bucket is unavailable")
	}
	ingress := openAIAuthBrowserLocalIngress(proxy)
	if ingress == "" {
		return nil, fmt.Errorf("proxy bucket %s has no Chrome-usable local ingress", proxy.Name)
	}

	// 浏览器配置目录名：state 前 12 位（一次授权一份隔离配置，合法字符集
	// 天然满足 launch.sh 的 [A-Za-z0-9._-] 校验——state 是 hex）。
	profileTag := "auth-" + session.State[:min(12, len(session.State))]

	authURL := openai.BuildAuthorizationURLForPlatform(
		session.State,
		openai.GenerateCodeChallenge(session.CodeVerifier),
		session.RedirectURI,
		session.Platform,
	)

	result := &OpenAIAuthBrowserLaunchResult{
		ProfileTag:  profileTag,
		ProxyName:   proxy.Name,
		ExitIngress: ingress,
		AuthURL:     authURL,
	}

	// 同一授权会话只允许一个启动脚本在途。前端重复点击或网络重试直接复用
	// 在途状态，不再重复打开 Chrome 配置目录。脚本尚未成功退出前不能声称
	// Launched=true，否则代理预检或 Chrome 启动失败会被重复请求误报为成功。
	if !l.claimLaunch(sessionID) {
		result.AlreadyRunning = true
		result.Output = "launcher is already running for this authorization session"
		return result, nil
	}
	defer l.releaseLaunch(sessionID)

	// 启动脚本需要做代理预检，生命周期不能继承 HTTP 请求的取消信号：
	// 页面切换后，浏览器启动仍应继续。准备阶段使用请求 ctx 读取会话和代理；
	// 子进程使用独立、有界的后台 context，但响应必须等待其真实退出结果。
	timeout := l.timeout
	if timeout <= 0 {
		timeout = defaultAuthBrowserLauncherTimeout
	}
	launchCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	commandContext := l.newCommand
	if commandContext == nil {
		commandContext = exec.CommandContext
	}
	cmd := commandContext(launchCtx, l.launcherPath, profileTag, authURL, ingress)
	if err := cmd.Start(); err != nil {
		return result, fmt.Errorf("start auth browser launcher: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		logger.LegacyPrintf(
			"service.openai_auth_browser",
			"Warning: auth browser launcher for session %s failed: %v",
			profileTag,
			err,
		)
		if errors.Is(launchCtx.Err(), context.DeadlineExceeded) {
			return result, fmt.Errorf("auth browser launcher timed out after %s", timeout)
		}
		return result, fmt.Errorf("auth browser launcher exited unsuccessfully: %w", err)
	}

	result.Launched = true
	result.Output = "launcher completed"
	return result, nil
}
