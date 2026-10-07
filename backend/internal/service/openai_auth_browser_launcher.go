package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
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
const openAIAuthBrowserStateLength = 64
const openAIAuthBrowserCodeVerifierLength = 128
const maxAuthBrowserLauncherOutput = 8 * 1024

var (
	ErrOpenAIAuthBrowserInvalidRequest     = errors.New("auth browser launch request is invalid")
	ErrOpenAIAuthBrowserSessionNotFound    = errors.New("authorization session not found")
	ErrOpenAIAuthBrowserSessionExpired     = errors.New("authorization session expired")
	ErrOpenAIAuthBrowserSessionInvalid     = errors.New("authorization session is invalid")
	ErrOpenAIAuthBrowserProxyUnavailable   = errors.New("authorization proxy bucket is unavailable")
	ErrOpenAIAuthBrowserRouteChanged       = errors.New("authorization proxy configuration changed")
	ErrOpenAIAuthBrowserIngressUnavailable = errors.New("authorization proxy has no Chrome-usable local ingress")
	ErrOpenAIAuthBrowserLauncherTimeout    = errors.New("auth browser launcher timed out")
)

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
//   3) exec launch.sh <state指纹> <auth_url> <本机入口> → Chrome 弹窗
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
	accountTOTP             *AccountTOTPService
	launcherPath            string
	sessionStore            OpenAIOAuthSessionStore
	proxyRepo               ProxyRepository
	validateReauthorization func(context.Context, *OpenAIOAuthSession, bool) error
	now                     func() time.Time
	newCommand              func(context.Context, string, ...string) *exec.Cmd
	timeout                 time.Duration
	automationTimeout       time.Duration

	mu               sync.Mutex
	inFlight         map[string]struct{}
	inFlightAccounts map[int64]string
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
		newCommand:   newOpenAIAuthBrowserCommand,
		timeout:      defaultAuthBrowserLauncherTimeout,
		inFlight:     make(map[string]struct{}),
	}
}

// openAIAuthBrowserLocalIngress 把业务桶映射到本机免认证入口。
// 静态 ISP 桶的业务口要认证（老 autossh socks5h://127.0.0.1:17911-17914、
// r17ar 起 mihomo 原生 17921-17924 且监听带 users），Chrome 无法携带代理
// 凭据——mihomo-buckets 为四桶另配免认证 Chrome 专用监听口 17931-17934
// （同上游、同出口、同桶序，仅本机 127.0.0.1）。两代业务口按桶序映射到
// 该系列；桶行自带凭据不影响映射（凭据留给网关业务路径用）。其余桶
// （novproxy 等）本就免认证，原样返回。
func openAIAuthBrowserLocalIngress(p *Proxy) string {
	if p == nil {
		return ""
	}
	protocol := strings.ToLower(strings.TrimSpace(p.Protocol))
	host := strings.TrimSpace(p.Host)
	if protocol == "socks5h" && host == "127.0.0.1" {
		switch {
		case p.Port >= 17911 && p.Port <= 17914:
			return fmt.Sprintf("http://127.0.0.1:%d", p.Port+20)
		case p.Port >= 17921 && p.Port <= 17924:
			return fmt.Sprintf("http://127.0.0.1:%d", p.Port+10)
		}
	}
	// 带凭据的代理一律不行（无法安全传给 Chrome）；本地免认证口原样。
	if p.Username != "" || p.Password != "" {
		return ""
	}
	switch protocol {
	case "http", "https", "socks5", "socks5h":
	default:
		return ""
	}
	if host == "" || p.Port < 1 || p.Port > 65535 {
		return ""
	}
	if strings.ContainsAny(host, " \t\r\n") {
		return ""
	}
	if strings.HasPrefix(host, "[") || strings.HasSuffix(host, "]") {
		if !strings.HasPrefix(host, "[") || !strings.HasSuffix(host, "]") {
			return ""
		}
		host = host[1 : len(host)-1]
		if net.ParseIP(host) == nil {
			return ""
		}
	}
	if strings.Contains(host, ":") && net.ParseIP(host) == nil {
		return ""
	}
	return protocol + "://" + net.JoinHostPort(host, strconv.Itoa(p.Port))
}

type boundedAuthBrowserOutput struct {
	buffer    bytes.Buffer
	mu        sync.Mutex
	limit     int
	truncated bool
}

func newBoundedAuthBrowserOutput() *boundedAuthBrowserOutput {
	return &boundedAuthBrowserOutput{limit: maxAuthBrowserLauncherOutput}
}

func (b *boundedAuthBrowserOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	total := len(p)
	remaining := b.limit - b.buffer.Len()
	if remaining <= 0 {
		b.truncated = b.truncated || total > 0
		return total, nil
	}
	if len(p) > remaining {
		_, _ = b.buffer.Write(p[:remaining])
		b.truncated = true
		return total, nil
	}
	_, _ = b.buffer.Write(p)
	return total, nil
}

func (b *boundedAuthBrowserOutput) text() string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	output := strings.TrimSpace(b.buffer.String())
	if b.truncated {
		if output != "" {
			output += "\n"
		}
		output += "[launcher output truncated]"
	}
	return output
}

// OpenAIAuthBrowserLaunchResult 启动结果（回显给前端确认弹窗文案）。
type OpenAIAuthBrowserLaunchResult struct {
	Launched       bool   `json:"launched"`
	AlreadyRunning bool   `json:"already_running"`
	ProfileTag     string `json:"profile_tag"`
	ProxyName      string `json:"proxy_name"`
	ExitIngress    string `json:"exit_ingress"`
	Output         string `json:"output"`
	Code           string `json:"code,omitempty"`
	State          string `json:"state,omitempty"`
}

// Transient input travels only over the child process's stdin, never argv,
// environment, disk or diagnostic output.
type OpenAIAuthBrowserLogin struct {
	Email         string `json:"email"`
	Password      string `json:"password"`
	TOTPSecret    string `json:"totp_secret"`
	UseStoredTOTP bool   `json:"use_stored_totp,omitempty"`
}

func validAuthBrowserLogin(login *OpenAIAuthBrowserLogin) bool {
	return login != nil && len(login.Email) <= 254 && strings.Contains(login.Email, "@") &&
		!strings.ContainsAny(login.Email, "\x00\r\n\t ") &&
		len(login.Password) > 0 && len(login.Password) <= 4096 &&
		!strings.ContainsRune(login.Password, '\x00') && len(login.TOTPSecret) <= 256
}

func (l *OpenAIAuthBrowserLauncher) claimLaunch(sessionID string, accountID int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inFlight == nil {
		l.inFlight = make(map[string]struct{})
	}
	if _, exists := l.inFlight[sessionID]; exists {
		return false
	}
	if accountID != 0 {
		if _, exists := l.inFlightAccounts[accountID]; exists {
			return false
		}
		if l.inFlightAccounts == nil {
			l.inFlightAccounts = make(map[int64]string)
		}
		l.inFlightAccounts[accountID] = sessionID
	}
	l.inFlight[sessionID] = struct{}{}
	return true
}

func (l *OpenAIAuthBrowserLauncher) releaseLaunch(sessionID string, accountID int64) {
	l.mu.Lock()
	delete(l.inFlight, sessionID)
	if owner, exists := l.inFlightAccounts[accountID]; exists && owner == sessionID {
		delete(l.inFlightAccounts, accountID)
	}
	l.mu.Unlock()
}

func validOpenAIAuthBrowserLowerHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func validOpenAIAuthBrowserState(state string) bool {
	return validOpenAIAuthBrowserLowerHex(state, openAIAuthBrowserStateLength)
}

func validOpenAIAuthBrowserCodeVerifier(verifier string) bool {
	return validOpenAIAuthBrowserLowerHex(verifier, openAIAuthBrowserCodeVerifierLength)
}

func openAIAuthBrowserProfileTag(state string) string {
	sum := sha256.Sum256([]byte(state))
	return fmt.Sprintf("auth-%x", sum)
}

func openAIAuthBrowserEnvironment(environment []string) []string {
	result := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		switch name {
		case "PATH", "HOME", "USER", "LOGNAME", "TMPDIR", "TMP", "TEMP",
			"LANG", "LANGUAGE", "LC_ALL", "LC_CTYPE", "TZ",
			"SystemRoot", "SYSTEMROOT", "WINDIR", "windir", "ComSpec", "COMSPEC",
			"PATHEXT", "USERPROFILE", "APPDATA", "LOCALAPPDATA",
			"DISPLAY", "WAYLAND_DISPLAY", "XAUTHORITY", "XDG_RUNTIME_DIR",
			"XDG_CONFIG_HOME", "XDG_CACHE_HOME", "DBUS_SESSION_BUS_ADDRESS",
			"SUB2API_AUTH_BROWSER_PYTHON", "SUB2API_AUTH_BROWSER_CURL",
			"SUB2API_AUTH_BROWSER_CHROME", "SUB2API_AUTH_BROWSER_NODE",
			"SUB2API_AUTH_BROWSER_PROFILE_ROOT", "SUB2API_AUTH_BROWSER_LOG_FILE",
			"SUB2API_AUTH_BROWSER_PROFILE_MAX_AGE_HOURS",
			"SUB2API_AUTH_BROWSER_REQUIRE_STATIC_EXIT_CHECKS":
			result = append(result, entry)
		default:
			// Retain per-ingress IP pins, not arbitrary host or interpreter settings.
			if suffix, ok := strings.CutPrefix(name, "SUB2API_AUTH_BROWSER_EXPECTED_EXIT_"); ok {
				port, err := strconv.ParseUint(suffix, 10, 16)
				if err == nil && port > 0 && strconv.FormatUint(port, 10) == suffix {
					result = append(result, entry)
				}
			}
		}
	}
	return result
}

func newOpenAIAuthBrowserCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	// Browser processes must not inherit database credentials, host proxy
	// fallbacks, or interpreter injection settings from the service.
	cmd.Env = openAIAuthBrowserEnvironment(os.Environ())
	return cmd
}

// Launch 按授权会话直拉激活浏览器。sessionID 即生成链接时返回的
// session_id（前端手里有，不用重新解析 URL）。
func (l *OpenAIAuthBrowserLauncher) Launch(ctx context.Context, sessionID string) (*OpenAIAuthBrowserLaunchResult, error) {
	return l.launch(ctx, sessionID, nil)
}

func (l *OpenAIAuthBrowserLauncher) LaunchWithLogin(ctx context.Context, sessionID string, login *OpenAIAuthBrowserLogin) (*OpenAIAuthBrowserLaunchResult, error) {
	if !validAuthBrowserLogin(login) {
		return nil, ErrOpenAIAuthBrowserInvalidRequest
	}
	return l.launch(ctx, sessionID, login)
}

func (l *OpenAIAuthBrowserLauncher) launch(ctx context.Context, sessionID string, login *OpenAIAuthBrowserLogin) (*OpenAIAuthBrowserLaunchResult, error) {
	if l == nil {
		return nil, fmt.Errorf("auth browser launcher is not configured")
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, fmt.Errorf("%w: session_id is required", ErrOpenAIAuthBrowserInvalidRequest)
	}
	session, err := l.sessionStore.Get(ctx, sessionID)
	if err != nil {
		switch {
		case errors.Is(err, ErrPendingAuthSessionNotFound):
			return nil, ErrOpenAIAuthBrowserSessionNotFound
		case errors.Is(err, ErrPendingAuthSessionExpired),
			errors.Is(err, ErrPendingAuthSessionConsumed):
			return nil, ErrOpenAIAuthBrowserSessionExpired
		case errors.Is(err, ErrOpenAIOAuthSessionInvalid),
			errors.Is(err, ErrPendingAuthBrowserMismatch):
			return nil, fmt.Errorf("%w: %v", ErrOpenAIAuthBrowserSessionInvalid, err)
		default:
			return nil, fmt.Errorf("load authorization session: %w", err)
		}
	}
	if session == nil {
		return nil, ErrOpenAIAuthBrowserSessionNotFound
	}
	if l.now().After(session.CreatedAt.Add(openAIOAuthSessionTTL)) {
		return nil, ErrOpenAIAuthBrowserSessionExpired
	}
	if !validOpenAIAuthBrowserState(session.State) {
		return nil, fmt.Errorf("%w: state is invalid", ErrOpenAIAuthBrowserSessionInvalid)
	}
	if !validOpenAIAuthBrowserCodeVerifier(session.CodeVerifier) {
		return nil, fmt.Errorf("%w: PKCE verifier is invalid", ErrOpenAIAuthBrowserSessionInvalid)
	}
	if err := validateOpenAIOAuthReauthorizationBinding(session); err != nil {
		return nil, ErrOpenAIAuthBrowserSessionInvalid
	}
	if login != nil && session.ReauthorizationAccountID == 0 {
		return nil, ErrOpenAIAuthBrowserInvalidRequest
	}

	ingress, proxyName, proxyURL := "direct", "Direct", ""
	if session.ProxyID != 0 {
		proxy, err := l.proxyRepo.GetByID(ctx, session.ProxyID)
		if err != nil || proxy == nil || !proxy.IsActive() {
			return nil, ErrOpenAIAuthBrowserProxyUnavailable
		}
		proxyURL, err = openAIOAuthProxySnapshotURL(proxy, &session.ProxyID)
		if err != nil {
			return nil, ErrOpenAIAuthBrowserProxyUnavailable
		}
		ingress = openAIAuthBrowserLocalIngress(proxy)
		if ingress == "" {
			return nil, ErrOpenAIAuthBrowserIngressUnavailable
		}
		proxyName = proxy.Name
	}
	if session.ProxyRouteHash == "" || session.ProxyRouteHash != openAIOAuthProxyRouteHash(proxyURL) {
		return nil, ErrOpenAIAuthBrowserRouteChanged
	}

	if session.ReauthorizationAccountID != 0 {
		if l.validateReauthorization == nil {
			return nil, ErrOpenAIAuthBrowserSessionInvalid
		}
		if err := l.validateReauthorization(ctx, session, false); err != nil {
			return nil, err
		}
	}

	if login != nil {
		copy := *login
		login = &copy
		defer func() { login.Password, login.TOTPSecret = "", "" }()
		if login.UseStoredTOTP {
			if login.TOTPSecret != "" || l.accountTOTP == nil {
				return nil, ErrOpenAIAuthBrowserInvalidRequest
			}
			login.TOTPSecret, err = l.accountTOTP.Load(ctx, session, login.Email)
			if err != nil {
				return nil, err
			}
		} else if login.TOTPSecret != "" {
			login.TOTPSecret, err = NormalizeAccountTOTP(login.TOTPSecret)
			if err != nil {
				return nil, ErrOpenAIAuthBrowserInvalidRequest
			}
		}
	}

	// 浏览器配置目录名使用完整 state 的 SHA-256 指纹：保留完整碰撞强度，
	// 同时避免把 OAuth state 直接暴露到目录名、日志和启动结果中。
	profileTag := openAIAuthBrowserProfileTag(session.State)

	authURL := openai.BuildAuthorizationURLForPlatform(
		session.State,
		openai.GenerateCodeChallenge(session.CodeVerifier),
		session.RedirectURI,
		session.Platform,
	)

	result := &OpenAIAuthBrowserLaunchResult{
		ProfileTag:  profileTag,
		ProxyName:   proxyName,
		ExitIngress: ingress,
	}

	// Serialize account-bound launches across sessions as well as duplicate clicks.
	// A busy launch is not evidence that this session's browser completed.
	if !l.claimLaunch(sessionID, session.ReauthorizationAccountID) {
		result.AlreadyRunning = true
		result.Output = "launcher is already running for this account or authorization session"
		return result, nil
	}
	defer l.releaseLaunch(sessionID, session.ReauthorizationAccountID)

	// 启动脚本需要做代理预检，生命周期不能继承 HTTP 请求的取消信号：
	// 页面切换后，浏览器启动仍应继续。准备阶段使用请求 ctx 读取会话和代理；
	// 子进程使用独立、有界的后台 context，但响应必须等待其真实退出结果。
	timeout := l.timeout
	if timeout <= 0 {
		timeout = defaultAuthBrowserLauncherTimeout
	}
	parent := context.Background()
	if login != nil {
		parent = ctx
		timeout = l.automationTimeout
		if timeout <= 0 {
			timeout = 5 * time.Minute
		}
	}
	launchCtx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	commandContext := l.newCommand
	if commandContext == nil {
		commandContext = newOpenAIAuthBrowserCommand
	}
	cmd := commandContext(launchCtx, l.launcherPath, profileTag, authURL, ingress)
	output := newBoundedAuthBrowserOutput()
	cmd.Stdout = output
	if login == nil {
		cmd.Stderr = output
	} else {
		input, err := json.Marshal(login)
		if err != nil {
			return nil, ErrOpenAIAuthBrowserInvalidRequest
		}
		defer clear(input)
		cmd.Stdin = bytes.NewReader(input)
		cmd.Env = append(cmd.Env, "SUB2API_AUTH_BROWSER_MODE=automated")
		// Give the helper a short opportunity to terminate its isolated browser.
		cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
		cmd.WaitDelay = 3 * time.Second
		// Browser/helper stderr is not a public error channel.
	}
	if err := cmd.Start(); err != nil {
		return result, fmt.Errorf("start auth browser launcher: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		if login != nil {
			if launchCtx.Err() != nil {
				return result, ErrOpenAIAuthBrowserLauncherTimeout
			}
			return result, errors.New("automatic authorization did not complete; check the authorization window or use manual authorization")
		}
		detail := output.text()
		logger.LegacyPrintf(
			"service.openai_auth_browser",
			"Warning: auth browser launcher for session %s failed: %v",
			profileTag,
			err,
		)
		if errors.Is(launchCtx.Err(), context.DeadlineExceeded) {
			if detail != "" {
				return result, fmt.Errorf("%w after %s: %s", ErrOpenAIAuthBrowserLauncherTimeout, timeout, detail)
			}
			return result, fmt.Errorf("%w after %s", ErrOpenAIAuthBrowserLauncherTimeout, timeout)
		}
		if detail != "" {
			return result, fmt.Errorf("auth browser launcher exited unsuccessfully: %w: %s", err, detail)
		}
		return result, fmt.Errorf("auth browser launcher exited unsuccessfully: %w", err)
	}

	if login != nil {
		var callback struct {
			Code  string `json:"code"`
			State string `json:"state"`
		}
		if output.truncated || json.Unmarshal(output.buffer.Bytes(), &callback) != nil ||
			callback.State != session.State || len(callback.Code) == 0 || len(callback.Code) > 4096 ||
			strings.ContainsAny(callback.Code, " \t\r\n\x00") {
			return result, errors.New("automatic authorization callback is invalid")
		}
		// Recheck account identity and route after the interactive wait.
		if err := l.validateReauthorization(launchCtx, session, false); err != nil {
			return result, err
		}
		if redirect, err := url.Parse(session.RedirectURI); err != nil || redirect.Scheme != "http" {
			return result, ErrOpenAIAuthBrowserSessionInvalid
		}
		result.Code, result.State = callback.Code, callback.State
	}

	result.Launched = true
	result.Output = "launcher completed"
	return result, nil
}

func (l *OpenAIAuthBrowserLauncher) SetOAuthService(s *OpenAIOAuthService) {
	if l != nil && s != nil {
		l.validateReauthorization = s.validateReauthorizationSession
	}
}

func (l *OpenAIAuthBrowserLauncher) SetAccountTOTP(s *AccountTOTPService) { l.accountTOTP = s }
