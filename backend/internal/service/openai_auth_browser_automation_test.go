package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The child is the test binary, not Chrome or a real authorization endpoint.
func TestAuthBrowserAutomationChild(t *testing.T) {
	mode := os.Getenv("SUB2API_TEST_AUTOMATION")
	if mode == "" {
		return
	}
	input, err := io.ReadAll(io.LimitReader(os.Stdin, 16<<10))
	if err != nil {
		os.Exit(2)
	}
	var login OpenAIAuthBrowserLogin
	if json.Unmarshal(input, &login) != nil || !validAuthBrowserLogin(&login) {
		os.Exit(3)
	}
	for _, value := range append(os.Args, os.Environ()...) {
		if strings.Contains(value, login.Password) ||
			(login.TOTPSecret != "" && strings.Contains(value, login.TOTPSecret)) {
			os.Exit(4)
		}
	}
	if os.Getenv("SUB2API_AUTH_BROWSER_MODE") != "automated" {
		os.Exit(5)
	}
	switch mode {
	case "wait":
		time.Sleep(30 * time.Second)
		os.Exit(6)
	case "failure":
		// Deliberately hostile child diagnostics must never reach a response.
		_, _ = os.Stdout.Write(input)
		_, _ = os.Stderr.Write(input)
		os.Exit(7)
	case "malformed":
		_, _ = os.Stdout.Write(input)
	case "overflow":
		_, _ = io.WriteString(os.Stdout, strings.Repeat("x", maxAuthBrowserLauncherOutput+1))
	default:
		code, state := "fixture-code", os.Getenv("SUB2API_TEST_CALLBACK_STATE")
		if mode == "wrong-state" {
			state = "foreign-state"
		} else if mode == "invalid-code" {
			code = "invalid\ncode"
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"code": code, "state": state})
	}
	os.Exit(0)
}

func automationFixture(t *testing.T, mode string) (*OpenAIAuthBrowserLauncher, *OpenAIOAuthSession, *int32, *int32) {
	t.Helper()
	svc, _, session, _, _ := reauthorizationIPFixture(t)
	launcher := &OpenAIAuthBrowserLauncher{
		launcherPath: "local-test-child",
		sessionStore: &launcherSessionStoreStub{session: session},
		proxyRepo:    svc.proxyRepo, fixedEgressRoutes: svc.fixedEgressRoutes,
		now: time.Now, automationTimeout: 10 * time.Second,
	}
	var checked, recorded int32
	launcher.validateReauthorization = func(ctx context.Context, s *OpenAIOAuthSession, consume bool) error {
		atomic.AddInt32(&checked, 1)
		return svc.validateReauthorizationSession(ctx, s, consume)
	}
	launcher.recordAuthorizationBrowser = func(context.Context, *OpenAIOAuthSession) error {
		atomic.AddInt32(&recorded, 1)
		return nil
	}
	launcher.newCommand = func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		require.Len(t, args, 4)
		require.Equal(t, launcher.fixedEgressRoutes[session.ProxyID].BrowserIngress, args[2])
		require.Equal(t, session.ReauthorizationExitIP, args[3])
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAuthBrowserAutomationChild$")
		cmd.Env = append(os.Environ(), "SUB2API_TEST_AUTOMATION="+mode,
			"SUB2API_TEST_CALLBACK_STATE="+session.State)
		return cmd
	}
	return launcher, session, &checked, &recorded
}

func automationTestLogin() *OpenAIAuthBrowserLogin {
	return &OpenAIAuthBrowserLogin{
		Email: "local-fixture@example.invalid", Password: "local-fixture-" + "not-a-real-password",
	}
}

func TestAutomaticAuthBrowserUsesStdinAndRechecksOriginalBinding(t *testing.T) {
	launcher, session, checked, recorded := automationFixture(t, "success")
	result, err := launcher.LaunchWithLogin(t.Context(), session.ID, automationTestLogin())
	require.NoError(t, err)
	require.True(t, result.Launched)
	require.Equal(t, "fixture-code", result.Code)
	require.Equal(t, session.State, result.State)
	require.EqualValues(t, 2, atomic.LoadInt32(checked))
	require.EqualValues(t, 1, atomic.LoadInt32(recorded))
	require.Equal(t, "launcher completed", result.Output)
	require.Empty(t, launcher.inFlight)
}

func TestAutomaticAuthBrowserRejectsUnsafeInputAndBindingBeforeExec(t *testing.T) {
	for _, name := range []string{"nil input", "invalid email", "empty password", "oversized input",
		"initial authorization", "expired session", "changed route", "missing fixed exit", "changed identity", "unknown IP"} {
		t.Run(name, func(t *testing.T) {
			launcher, session, _, recorded := automationFixture(t, "success")
			login := automationTestLogin()
			switch name {
			case "nil input":
				login = nil
			case "invalid email":
				login.Email = "invalid\n@example.invalid"
			case "empty password":
				login.Password = ""
			case "oversized input":
				login.Password = strings.Repeat("x", 4097)
			case "initial authorization":
				session.ReauthorizationAccountID = 0
			case "expired session":
				session.CreatedAt = time.Now().Add(-2 * openAIOAuthSessionTTL)
			case "changed route":
				session.ProxyRouteHash = strings.Repeat("a", 64)
			case "missing fixed exit":
				launcher.fixedEgressRoutes = nil
			case "changed identity":
				launcher.validateReauthorization = func(context.Context, *OpenAIOAuthSession, bool) error {
					return ErrOAuthReauthorizationStale
				}
			case "unknown IP":
				session.ReauthorizationExitIP = ""
			}
			starts := 0
			launcher.newCommand = func(context.Context, string, ...string) *exec.Cmd {
				starts++
				return nil
			}
			result, err := launcher.LaunchWithLogin(t.Context(), session.ID, login)
			require.Error(t, err)
			require.Nil(t, result)
			require.Zero(t, starts)
			require.Zero(t, atomic.LoadInt32(recorded))
		})
	}
}

func TestAutomaticAuthBrowserRejectsCallbackAndHidesChildDiagnostics(t *testing.T) {
	for _, mode := range []string{"failure", "malformed", "overflow", "wrong-state", "invalid-code"} {
		t.Run(mode, func(t *testing.T) {
			launcher, session, _, recorded := automationFixture(t, mode)
			login := automationTestLogin()
			result, err := launcher.LaunchWithLogin(t.Context(), session.ID, login)
			require.Error(t, err)
			require.NotContains(t, err.Error(), login.Password)
			require.NotContains(t, err.Error(), login.Email)
			require.NotNil(t, result)
			require.Empty(t, result.Code)
			require.Empty(t, result.Output)
			require.False(t, result.Launched)
			require.Zero(t, atomic.LoadInt32(recorded))
			require.Empty(t, launcher.inFlight)
		})
	}
}

func TestAutomaticAuthBrowserRejectsIdentityChangeDuringInteractiveWait(t *testing.T) {
	launcher, session, _, recorded := automationFixture(t, "success")
	checks := 0
	launcher.validateReauthorization = func(context.Context, *OpenAIOAuthSession, bool) error {
		checks++
		if checks == 2 {
			return ErrOAuthReauthorizationStale
		}
		return nil
	}
	result, err := launcher.LaunchWithLogin(t.Context(), session.ID, automationTestLogin())
	require.ErrorIs(t, err, ErrOAuthReauthorizationStale)
	require.False(t, result.Launched)
	require.Empty(t, result.Code)
	require.Zero(t, atomic.LoadInt32(recorded))
}

func TestAutomaticAuthBrowserCancellationAndDeduplication(t *testing.T) {
	for _, kind := range []string{"cancel", "timeout"} {
		t.Run(kind, func(t *testing.T) {
			launcher, session, _, recorded := automationFixture(t, "wait")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if kind == "timeout" {
				launcher.automationTimeout = 150 * time.Millisecond
			}
			started := make(chan struct{})
			command := launcher.newCommand
			launcher.newCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
				cmd := command(ctx, name, args...)
				close(started)
				return cmd
			}
			done := make(chan error, 1)
			go func() {
				_, err := launcher.LaunchWithLogin(ctx, session.ID, automationTestLogin())
				done <- err
			}()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("child launch was not reached")
			}
			if kind == "cancel" {
				result, err := launcher.LaunchWithLogin(t.Context(), session.ID, automationTestLogin())
				require.NoError(t, err)
				require.True(t, result.AlreadyRunning)
				require.False(t, result.Launched)
				cancel()
			}
			select {
			case err := <-done:
				require.Error(t, err)
				require.True(t, errors.Is(err, ErrOpenAIAuthBrowserLauncherTimeout) ||
					strings.Contains(err.Error(), "context canceled"))
			case <-time.After(6 * time.Second):
				t.Fatal("canceled child did not terminate")
			}
			require.Empty(t, launcher.inFlight)
			require.Zero(t, atomic.LoadInt32(recorded))
		})
	}
}

func TestAutomaticAuthBrowserDeduplicatesAccountAcrossSessions(t *testing.T) {
	launcher, session, _, recorded := automationFixture(t, "wait")
	store := newTestOpenAIOAuthSessionStore()
	require.NoError(t, store.Create(t.Context(), session))
	other := *session
	other.ID += "-other"
	require.NoError(t, store.Create(t.Context(), &other))
	launcher.sessionStore = store

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started := make(chan struct{})
	var commands int32
	command := launcher.newCommand
	launcher.newCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cmd := command(ctx, name, args...)
		if atomic.AddInt32(&commands, 1) == 1 {
			close(started)
		}
		return cmd
	}
	done := make(chan error, 1)
	go func() {
		_, err := launcher.LaunchWithLogin(ctx, session.ID, automationTestLogin())
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("child launch was not reached")
	}

	// Use a bounded request so the regression cannot leave another child running.
	secondCtx, secondCancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer secondCancel()
	result, err := launcher.LaunchWithLogin(secondCtx, other.ID, automationTestLogin())
	cancel()
	select {
	case childErr := <-done:
		require.Error(t, childErr)
	case <-time.After(6 * time.Second):
		t.Fatal("canceled child did not terminate")
	}
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.AlreadyRunning)
	require.False(t, result.Launched)
	require.EqualValues(t, 1, atomic.LoadInt32(&commands))
	require.Empty(t, launcher.inFlight)
	require.Zero(t, atomic.LoadInt32(recorded))
	require.Empty(t, launcher.inFlightAccounts)
	require.True(t, launcher.claimLaunch(other.ID, other.ReauthorizationAccountID))
	launcher.releaseLaunch(other.ID, other.ReauthorizationAccountID)
}

func TestAuthBrowserLaunchClaimsRemainIsolated(t *testing.T) {
	launcher := &OpenAIAuthBrowserLauncher{}
	require.True(t, launcher.claimLaunch("first", 101))
	require.False(t, launcher.claimLaunch("first", 102))
	require.False(t, launcher.claimLaunch("second", 101))
	require.True(t, launcher.claimLaunch("second", 102))
	require.True(t, launcher.claimLaunch("new-account-a", 0))
	require.True(t, launcher.claimLaunch("new-account-b", 0))
	require.False(t, launcher.claimLaunch("new-account-a", 0))

	launcher.releaseLaunch("not-owner", 101)
	require.False(t, launcher.claimLaunch("third", 101))
	launcher.releaseLaunch("first", 101)
	require.True(t, launcher.claimLaunch("third", 101))
	launcher.releaseLaunch("first", 101)
	require.False(t, launcher.claimLaunch("fourth", 101))
	launcher.releaseLaunch("third", 101)
	launcher.releaseLaunch("second", 102)
	launcher.releaseLaunch("new-account-a", 0)
	launcher.releaseLaunch("new-account-b", 0)
	require.Empty(t, launcher.inFlight)
	require.Empty(t, launcher.inFlightAccounts)
}
