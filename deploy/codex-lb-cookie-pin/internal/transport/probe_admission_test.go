package transport

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/liyunlong/sub2api-cookie-plugin/internal/cookiestore"
	"github.com/liyunlong/sub2api-cookie-plugin/internal/prober"
	pluginv1 "github.com/liyunlong/sub2api-cookie-plugin/pkg/pluginapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
)

func incomingPolicy(t *testing.T, policy pluginv1.ProbePolicy) context.Context {
	t.Helper()
	ctx := pluginv1.WithProbePolicy(t.Context(), policy)
	md, _ := metadata.FromOutgoingContext(ctx)
	return metadata.NewIncomingContext(t.Context(), md)
}

func TestProbeHostPolicyLifecycleAndGeneration(t *testing.T) {
	s := stoppedProbeServer(t, cookiestore.New())
	s.RequireHostProbePolicy()
	now := time.Now()
	tmpl := &probeTemplate{Headers: http.Header{"Authorization": {"Bearer fixture"}},
		CreatedAt: now.Add(-time.Minute), Confirmed: true, Generation: 1}
	s.templates[42] = tmpl
	if s.hostProbeAllowedLocked(42, tmpl, now) {
		t.Fatal("missing host permission admitted a background probe")
	}
	policy := pluginv1.ProbePolicy{IssuedAt: now.UnixMilli(), ExpiresAt: now.Add(time.Minute).UnixMilli(),
		Accounts: []pluginv1.ProbeAccountPolicy{{AccountID: 42, Identity: templateProbeIdentity(tmpl)}}}
	s.applyHostProbePolicy(incomingPolicy(t, policy), now)
	if !s.hostProbeAllowedLocked(42, tmpl, now) ||
		s.hostProbeAllowedLocked(42, tmpl, now.Add(time.Minute)) {
		t.Fatal("permission lease boundary not enforced")
	}
	rotated := *tmpl
	rotated.Headers = http.Header{"Authorization": {"Bearer changed"}}
	if s.hostProbeAllowedLocked(42, &rotated, now) {
		t.Fatal("old permission admitted changed credentials")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s.probeRunning.Store(int64(42), cancel)
	policy.IssuedAt++
	policy.ExpiresAt++
	policy.Accounts = nil // Deleted, disabled or explicitly manually paused.
	s.applyHostProbePolicy(incomingPolicy(t, policy), now)
	if s.templates[42] != nil || ctx.Err() == nil {
		t.Fatal("revocation retained template or in-flight probe")
	}
	policy.IssuedAt--
	policy.Accounts = []pluginv1.ProbeAccountPolicy{{AccountID: 42, Identity: templateProbeIdentity(tmpl)}}
	s.applyHostProbePolicy(incomingPolicy(t, policy), now)
	if s.hostProbeAllowedLocked(42, tmpl, now) {
		t.Fatal("late policy resurrected revoked permission")
	}
}

func TestProbeBudgetGlobalProxyAndBusinessPriority(t *testing.T) {
	s := stoppedProbeServer(t, cookiestore.New())
	a, b, c := "socks5://127.0.0.1:18001", "socks5://127.0.0.1:18002", "socks5://127.0.0.1:18003"
	if !s.reserveProbeBudgetLocked(1, a) || s.reserveProbeBudgetLocked(2, a) ||
		!s.reserveProbeBudgetLocked(2, b) || s.reserveProbeBudgetLocked(3, c) {
		t.Fatal("global/per-proxy probe admission violated")
	}
	s.releaseProbeBudget(1)
	s.businessByBucket[probeBucket(a)] = 1
	if s.reserveProbeBudgetLocked(3, a) {
		t.Fatal("probe competed with business traffic")
	}
	s.finishBusinessBucket(probeBucket(a))
	if !s.reserveProbeBudgetLocked(3, a) || s.reserveProbeBudgetLocked(3, c) {
		t.Fatal("budget did not recover or admitted overlapping account operations")
	}
}

func TestProbeHostPolicySuspensionAndNoDispatch(t *testing.T) {
	s := stoppedProbeServer(t, cookiestore.New())
	s.RequireHostProbePolicy()
	now := time.Now()
	tmpl := &probeTemplate{Headers: http.Header{"Authorization": {"Bearer fixture"}},
		Model: "fixture", CreatedAt: now.Add(-time.Minute), Confirmed: true, Generation: 1}
	s.templates[42] = tmpl
	p := pluginv1.ProbePolicy{IssuedAt: now.UnixMilli(), ExpiresAt: now.Add(time.Minute).UnixMilli(),
		Suspended: true}
	s.applyHostProbePolicy(incomingPolicy(t, p), now)
	if s.templates[42] == nil || s.hostProbeAllowedLocked(42, tmpl, now) {
		t.Fatal("temporary control failure must pause rather than delete or authorize")
	}
	_, answer, _ := s.sendProbe(t.Context(), 42, tmpl, prober.QuestionFor(0), probeTestConfig())
	if answer != "blocked:host-policy" {
		t.Fatalf("no permission was not rejected before dispatch: %s", answer)
	}
	state, err := s.evidence.Read(42)
	if err != nil || state.Operation.ID != "" {
		t.Fatal("no-permission attempt created a durable operation")
	}
	p.IssuedAt++
	p.ExpiresAt++
	p.Suspended = false
	p.Accounts = []pluginv1.ProbeAccountPolicy{{AccountID: 42, Identity: templateProbeIdentity(tmpl)}}
	s.applyHostProbePolicy(incomingPolicy(t, p), now)
	if !s.hostProbeAllowedLocked(42, tmpl, now) {
		t.Fatal("fresh authoritative snapshot did not resume permission")
	}
}

func TestProbePolicyLivenessIsReadOnly(t *testing.T) {
	s := stoppedProbeServer(t, cookiestore.New())
	ctx := pluginv1.WithLivenessHealth(t.Context())
	md, _ := metadata.FromOutgoingContext(ctx)
	ctx = metadata.NewIncomingContext(t.Context(), md)
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, err := s.Health(ctx, &pluginv1.HealthRequest{})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("liveness waited for policy/status lock")
	}
}

func TestProbeHostPolicyNativeHealthAndLateResults(t *testing.T) {
	for _, renew := range []bool{false, true} {
		t.Run(fmt.Sprintf("renew=%t", renew), func(t *testing.T) {
			s := stoppedProbeServer(t, cookiestore.New())
			s.RequireHostProbePolicy()
			cfg := probeTestConfig()
			cfg.ProbeMinReasoningTokens = 0
			s.Store.SetConfig(cfg)
			listener := bufconn.Listen(1 << 20)
			server := grpc.NewServer()
			pluginv1.RegisterTransportPluginServer(server, s)
			done := make(chan error, 1)
			go func() { done <- server.Serve(listener) }()
			defer func() {
				server.Stop()
				listener.Close()
				<-done
			}()
			conn, err := grpc.NewClient("passthrough:///fixture",
				grpc.WithTransportCredentials(insecure.NewCredentials()),
				grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
					return listener.DialContext(ctx)
				}))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			client := pluginv1.NewTransportPluginClient(conn)
			q := prober.Question{ID: "fixture", Prompt: "fixture", Grade: func(answer string) bool {
				return answer == "42"
			}}
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				// Expiry or renewal happens after dispatch, before the terminal.
				s.probeMu.Lock()
				s.hostPolicyExpires = time.Now().Add(-time.Second)
				issued := s.hostPolicyIssued
				tmpl := s.templates[42]
				s.probeMu.Unlock()
				if renew {
					now := time.Now().Add(time.Millisecond)
					p := pluginv1.ProbePolicy{IssuedAt: max(now.UnixMilli(), issued+1),
						ExpiresAt: now.Add(time.Minute).UnixMilli(),
						Accounts:  []pluginv1.ProbeAccountPolicy{{AccountID: 42, Identity: templateProbeIdentity(tmpl)}}}
					ctx, cancel := context.WithTimeout(pluginv1.WithProbePolicy(t.Context(), p), time.Second)
					defer cancel()
					if _, err := client.Health(ctx, &pluginv1.HealthRequest{}); err != nil {
						t.Error(err)
					}
				}
				fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"42\"}\n\n")
				fmt.Fprint(w, completeFixture)
			}))
			defer up.Close()
			stashFrom(s, 42, up.URL+"/responses", time.Now().Add(-time.Second))
			tmpl := *s.templates[42]
			now := time.Now()
			p := pluginv1.ProbePolicy{IssuedAt: now.UnixMilli(), ExpiresAt: now.Add(time.Minute).UnixMilli(),
				Accounts: []pluginv1.ProbeAccountPolicy{{AccountID: 42, Identity: templateProbeIdentity(&tmpl)}}}
			ctx, cancel := context.WithTimeout(pluginv1.WithProbePolicy(t.Context(), p), time.Second)
			defer cancel()
			if _, err := client.Health(ctx, &pluginv1.HealthRequest{}); err != nil {
				t.Fatal(err)
			}
			verdict, answer, _ := s.sendProbe(t.Context(), 42, &tmpl, q, cfg)
			state, err := s.evidence.Read(42)
			if err != nil {
				t.Fatal(err)
			}
			if renew {
				if verdict != prober.VerdictPass || state.Operation.Phase != "ok" {
					t.Fatalf("renewed lease lost a complete result: %v %s phase=%s", verdict, answer, state.Operation.Phase)
				}
			} else if verdict != prober.VerdictError || state.ProbePassed != 0 {
				t.Fatalf("expired lease admitted late success: %v %s passed=%d", verdict, answer, state.ProbePassed)
			}
		})
	}
}
