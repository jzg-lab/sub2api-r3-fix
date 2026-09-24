package service

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 授权会话桩。
type launcherSessionStoreStub struct {
	session *OpenAIOAuthSession
	err     error
}

func (s *launcherSessionStoreStub) Create(ctx context.Context, session *OpenAIOAuthSession) error {
	return nil
}
func (s *launcherSessionStoreStub) Get(ctx context.Context, sessionID string) (*OpenAIOAuthSession, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.session, nil
}
func (s *launcherSessionStoreStub) Consume(ctx context.Context, sessionID string) (*OpenAIOAuthSession, error) {
	return s.session, nil
}

// 桶桩：嵌入接口满足 ProxyRepository，只覆写 GetByID——未覆写方法被误调
// 即 panic，测试立刻暴露。自包含，不依赖带 //go:build unit 的共享桩。
type launcherProxyRepoStub struct {
	ProxyRepository
	proxy *Proxy
	err   error
}

func (r *launcherProxyRepoStub) GetByID(ctx context.Context, id int64) (*Proxy, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.proxy, nil
}

func TestOpenAIAuthBrowserLocalIngress(t *testing.T) {
	cases := []struct {
		name  string
		proxy *Proxy
		want  string
	}{
		{"static isp 13", &Proxy{ID: 13, Protocol: "socks5h", Host: "127.0.0.1", Port: 17911}, "http://127.0.0.1:17921"},
		{"static isp 16", &Proxy{ID: 16, Protocol: "socks5h", Host: "127.0.0.1", Port: 17914}, "http://127.0.0.1:17924"},
		{"first upstream with unrelated id", &Proxy{ID: 901, Protocol: "socks5h", Host: "127.0.0.1", Port: 17911}, "http://127.0.0.1:17921"},
		{"second upstream with unrelated id", &Proxy{ID: 42, Protocol: "SOCKS5H", Host: "127.0.0.1", Port: 17912}, "http://127.0.0.1:17922"},
		{"third upstream with unrelated id", &Proxy{ID: 3, Protocol: "socks5h", Host: "127.0.0.1", Port: 17913}, "http://127.0.0.1:17923"},
		{"last upstream with unrelated id", &Proxy{ID: 0, Protocol: "socks5h", Host: "127.0.0.1", Port: 17914}, "http://127.0.0.1:17924"},
		{"mapped upstream credentials stay local", &Proxy{ID: 99, Protocol: "socks5h", Host: "127.0.0.1", Port: 17912, Username: "u", Password: "p"}, "http://127.0.0.1:17922"},
		{"below mapping range", &Proxy{ID: 13, Protocol: "socks5h", Host: "127.0.0.1", Port: 17910}, "socks5h://127.0.0.1:17910"},
		{"above mapping range", &Proxy{ID: 16, Protocol: "socks5h", Host: "127.0.0.1", Port: 17915}, "socks5h://127.0.0.1:17915"},
		{"other host is not mapped", &Proxy{ID: 13, Protocol: "socks5h", Host: "192.0.2.1", Port: 17911}, "socks5h://192.0.2.1:17911"},
		{"other protocol is not mapped", &Proxy{ID: 13, Protocol: "http", Host: "127.0.0.1", Port: 17911}, "http://127.0.0.1:17911"},
		{"credentialed unmapped upstream rejected", &Proxy{ID: 13, Protocol: "socks5h", Host: "127.0.0.1", Port: 17910, Username: "u"}, ""},
		{"novproxy plain", &Proxy{ID: 11, Protocol: "http", Host: "127.0.0.1", Port: 17906}, "http://127.0.0.1:17906"},
		{"credentialed rejected", &Proxy{ID: 99, Protocol: "http", Host: "1.2.3.4", Port: 8080, Username: "u", Password: "p"}, ""},
		{"nil", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := openAIAuthBrowserLocalIngress(tc.proxy); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

// TestLauncherLaunchRejectsMissingSessionAndExpired：会话不存在/过期拒绝。
func TestLauncherLaunchRejectsMissingSessionAndExpired(t *testing.T) {
	store := &launcherSessionStoreStub{err: errors.New("not found")}
	repo := &launcherProxyRepoStub{proxy: &Proxy{ID: 13, Protocol: "socks5h", Host: "127.0.0.1", Port: 17911, Status: StatusActive}}
	l := &OpenAIAuthBrowserLauncher{
		launcherPath: "/bin/echo",
		sessionStore: store,
		proxyRepo:    repo,
		now:          func() time.Time { return time.Now() },
	}
	if _, err := l.Launch(context.Background(), "missing"); err == nil {
		t.Fatal("missing session must be rejected")
	}

	old := time.Now().Add(-3 * time.Hour)
	store.err = nil
	store.session = &OpenAIOAuthSession{ID: "s", State: strings.Repeat("a", 64), ProxyID: 13, CreatedAt: old}
	if _, err := l.Launch(context.Background(), "s"); err == nil {
		t.Fatal("expired session must be rejected")
	}
}

// TestLauncherLaunchExecsScriptWithCorrectArgv：成功路径——argv 依次为
// state 前 12 位目录名、重建的授权 URL（PKCE challenge 可派生）、本机入口。
func TestLauncherLaunchExecsScriptWithCorrectArgv(t *testing.T) {
	now := time.Now()
	verifier := "test-verifier-0123456789"
	state := "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	store := &launcherSessionStoreStub{session: &OpenAIOAuthSession{
		ID: "sess-1", State: state, CodeVerifier: verifier,
		RedirectURI: "https://chatgpt.com/api/auth/callback/login-web",
		ProxyID:     13, Platform: "openai", CreatedAt: now,
	}}
	repo := &launcherProxyRepoStub{proxy: &Proxy{ID: 13, Name: "static-isp-x", Protocol: "socks5h", Host: "127.0.0.1", Port: 17911, Status: StatusActive}}

	l := &OpenAIAuthBrowserLauncher{
		launcherPath: "/usr/bin/true", // 恒成功；argv 正确性靠结果字段反推
		sessionStore: store,
		proxyRepo:    repo,
		now:          func() time.Time { return now },
	}
	result, err := l.Launch(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("launch must succeed, got %v", err)
	}
	if !result.Launched {
		t.Fatal("result must report launched")
	}
	if result.Output != "launcher completed" {
		t.Fatalf("output = %q, want completed launch", result.Output)
	}
	if result.ProfileTag != "auth-"+state[:12] {
		t.Fatalf("profile tag = %q", result.ProfileTag)
	}
	if result.ExitIngress != "http://127.0.0.1:17921" {
		t.Fatalf("ingress = %q", result.ExitIngress)
	}
	if !strings.Contains(result.AuthURL, "state="+state) {
		t.Fatalf("auth url must carry state: %q", result.AuthURL)
	}
	// PKCE challenge 必须在 URL 里（S256 of verifier）。
	if !strings.Contains(result.AuthURL, "code_challenge=") {
		t.Fatal("auth url must carry pkce code_challenge")
	}
}

func TestOpenAIAuthBrowserLauncherHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_AUTH_BROWSER_LAUNCHER_HELPER") != "1" {
		return
	}
	marker := os.Getenv("AUTH_BROWSER_LAUNCHER_MARKER")
	if marker != "" {
		f, err := os.OpenFile(marker, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			os.Exit(2)
		}
		if _, err := f.WriteString("started\n"); err != nil {
			os.Exit(2)
		}
		if err := f.Close(); err != nil {
			os.Exit(2)
		}
	}
	if release := os.Getenv("AUTH_BROWSER_LAUNCHER_RELEASE"); release != "" {
		for {
			if _, err := os.Stat(release); err == nil {
				break
			} else if !errors.Is(err, os.ErrNotExist) {
				os.Exit(2)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	code, err := strconv.Atoi(os.Getenv("AUTH_BROWSER_LAUNCHER_EXIT_CODE"))
	if err != nil {
		os.Exit(2)
	}
	os.Exit(code)
}

func launcherTestHelperCommand(ctx context.Context, marker, release string, exitCode int) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOpenAIAuthBrowserLauncherHelperProcess$")
	cmd.Env = append(os.Environ(),
		"GO_WANT_AUTH_BROWSER_LAUNCHER_HELPER=1",
		"AUTH_BROWSER_LAUNCHER_MARKER="+marker,
		"AUTH_BROWSER_LAUNCHER_RELEASE="+release,
		"AUTH_BROWSER_LAUNCHER_EXIT_CODE="+strconv.Itoa(exitCode),
	)
	return cmd
}

func launcherTestService() *OpenAIAuthBrowserLauncher {
	now := time.Now()
	state := strings.Repeat("b", 64)
	store := &launcherSessionStoreStub{session: &OpenAIOAuthSession{
		ID: "sess-detached", State: state, CodeVerifier: "detached-verifier",
		RedirectURI: "https://chatgpt.com/api/auth/callback/login-web",
		ProxyID:     13, Platform: "openai", CreatedAt: now,
	}}
	repo := &launcherProxyRepoStub{proxy: &Proxy{
		ID: 13, Name: "static-isp-x", Protocol: "socks5h",
		Host: "127.0.0.1", Port: 17911, Status: StatusActive,
	}}
	return &OpenAIAuthBrowserLauncher{
		launcherPath: "ignored",
		sessionStore: store,
		proxyRepo:    repo,
		now:          func() time.Time { return now },
		timeout:      5 * time.Second,
	}
}

func TestLauncherLaunchWaitsForExitDetachedAndDeduplicated(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "launches")
	release := filepath.Join(dir, "release")
	l := launcherTestService()
	contexts := make(chan context.Context, 2)
	l.newCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		contexts <- ctx
		return launcherTestHelperCommand(ctx, marker, release, 0)
	}
	type outcome struct {
		result *OpenAIAuthBrowserLaunchResult
		err    error
	}
	done := make(chan outcome, 1)
	finished := make(chan struct{})
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	go func() {
		defer close(finished)
		result, err := l.Launch(requestCtx, "sess-detached")
		done <- outcome{result, err}
	}()
	t.Cleanup(func() {
		_ = os.WriteFile(release, nil, 0o600)
		select {
		case <-finished:
		case <-time.After(6 * time.Second):
			t.Error("launcher did not terminate during cleanup")
		}
	})

	deadline := time.Now().Add(3 * time.Second)
	for {
		if data, err := os.ReadFile(marker); err == nil && string(data) == "started\n" {
			break
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read marker: %v", err)
		}
		select {
		case first := <-done:
			t.Fatalf("launch returned before helper exit: %+v", first)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("launcher helper did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancelRequest()
	launchCtx := <-contexts
	if err := launchCtx.Err(); err != nil {
		t.Fatalf("request cancellation reached launcher: %v", err)
	}
	select {
	case first := <-done:
		t.Fatalf("launch returned before helper exit: %+v", first)
	default:
	}
	second, err := l.Launch(context.Background(), "sess-detached")
	if err != nil {
		t.Fatalf("duplicate launch: %v", err)
	}
	if second.Launched || !second.AlreadyRunning {
		t.Fatalf("duplicate launch must report in-flight without claiming readiness: %+v", second)
	}

	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case first := <-done:
		if first.err != nil {
			t.Fatalf("first launch: %v", first.err)
		}
		if !first.result.Launched || first.result.AlreadyRunning || first.result.Output != "launcher completed" {
			t.Fatalf("unexpected completed result: %+v", first.result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("launcher did not finish after helper was released")
	}
	if !errors.Is(launchCtx.Err(), context.Canceled) {
		t.Fatalf("completed launch context not released: %v", launchCtx.Err())
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "started\n" {
		t.Fatalf("duplicate launch spawned another helper: %q, %v", data, err)
	}

	// Wait releases the in-flight key, so a later explicit retry can start again.
	third, err := l.Launch(context.Background(), "sess-detached")
	if err != nil {
		t.Fatalf("launch after completion: %v", err)
	}
	if !third.Launched || third.AlreadyRunning {
		t.Fatalf("completed launch must be retryable: %+v", third)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "started\nstarted\n" {
		t.Fatalf("retry did not run exactly one helper: %q, %v", data, err)
	}
}

func TestLauncherLaunchFailureReleasesInFlight(t *testing.T) {
	for _, failure := range []string{"start", "exit", "timeout"} {
		t.Run(failure, func(t *testing.T) {
			l := launcherTestService()
			dir := t.TempDir()
			var launchCtx context.Context
			l.newCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
				launchCtx = ctx
				switch failure {
				case "start":
					return exec.CommandContext(ctx, filepath.Join(dir, "missing"))
				case "exit":
					return launcherTestHelperCommand(ctx, "", "", 23)
				default:
					return launcherTestHelperCommand(ctx, "", filepath.Join(dir, "never-released"), 0)
				}
			}
			if failure == "timeout" {
				l.timeout = 100 * time.Millisecond
			}
			result, err := l.Launch(context.Background(), "sess-detached")
			if err == nil {
				t.Fatal("launcher failure must be returned")
			}
			if result == nil || result.Launched || result.AlreadyRunning {
				t.Fatalf("failed launcher reported success: %+v", result)
			}
			if failure == "exit" {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != 23 {
					t.Fatalf("lost launcher exit code: %v", err)
				}
			}
			if failure == "timeout" {
				if !errors.Is(launchCtx.Err(), context.DeadlineExceeded) {
					t.Fatalf("launcher timeout was not enforced: %v", launchCtx.Err())
				}
				if !strings.Contains(err.Error(), "timed out") {
					t.Fatalf("launcher timeout was not observable: %v", err)
				}
			}
			if launchCtx.Err() == nil {
				t.Fatal("failed launch context was not released")
			}
			l.timeout = 5 * time.Second
			l.newCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
				return exec.CommandContext(ctx, "/usr/bin/true")
			}
			retry, err := l.Launch(context.Background(), "sess-detached")
			if err != nil || !retry.Launched || retry.AlreadyRunning {
				t.Fatalf("failed launch retained in-flight claim: %+v, %v", retry, err)
			}
		})
	}
}
