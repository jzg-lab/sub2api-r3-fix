package transport

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/liyunlong/sub2api-cookie-plugin/internal/cookiestore"
	pluginv1 "github.com/liyunlong/sub2api-cookie-plugin/pkg/pluginapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
)

type hostHTTPRPCFixture struct {
	pluginv1.UnimplementedTransportPluginServer
	forward func(pluginv1.TransportPlugin_ForwardServer) error
}

func (s hostHTTPRPCFixture) Forward(stream pluginv1.TransportPlugin_ForwardServer) error {
	return s.forward(stream)
}

func fixtureHostHTTPClient(t *testing.T, forward func(pluginv1.TransportPlugin_ForwardServer) error) *hostHTTPClient {
	t.Helper()
	listener := bufconn.Listen(64 * 1024)
	server := grpc.NewServer()
	pluginv1.RegisterTransportPluginServer(server, hostHTTPRPCFixture{forward: forward})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); _ = listener.Close() })
	conn, err := grpc.NewClient("passthrough:///host-http-fixture",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &hostHTTPClient{
		api:       pluginv1.NewTransportPluginClient(conn),
		binding:   pluginv1.HostHTTPBinding{BrokerID: 7, Request: strings.Repeat("a", 64), Probe: strings.Repeat("b", 64)},
		accountID: 42, concurrency: 3, proxy: "socks5://127.0.0.1:19001",
	}
}

func readHostHTTPFixtureRequest(stream pluginv1.TransportPlugin_ForwardServer) (*pluginv1.ForwardRequestStart, []byte, error) {
	first, err := stream.Recv()
	if err != nil {
		return nil, nil, err
	}
	if first.GetStart() == nil {
		return nil, nil, errors.New("missing start")
	}
	var body []byte
	for {
		frame, err := stream.Recv()
		if err != nil {
			return nil, nil, err
		}
		if frame.GetBodyEnd() {
			return first.GetStart(), body, nil
		}
		chunk, ok := frame.Frame.(*pluginv1.ForwardRequest_BodyChunk)
		if !ok {
			return nil, nil, errors.New("unexpected request frame")
		}
		body = append(body, chunk.BodyChunk...)
	}
}

func TestHostHTTPClientFramingAndExplicitCompletion(t *testing.T) {
	for _, mode := range []string{"complete", "truncated", "wrong_count", "rejected", "ambiguous"} {
		t.Run(mode, func(t *testing.T) {
			client := fixtureHostHTTPClient(t, func(stream pluginv1.TransportPlugin_ForwardServer) error {
				binding, ok := pluginv1.ReadHostHTTPBinding(stream.Context())
				if !ok || binding.BrokerID != 7 || binding.IsProbe {
					return errors.New("binding not preserved")
				}
				start, body, err := readHostHTTPFixtureRequest(stream)
				if err != nil {
					return err
				}
				if start.AccountId != 42 || start.AccountConcurrency != 3 ||
					start.ProxyUrl != "socks5://127.0.0.1:19001" ||
					start.Url != "https://fixture.invalid/backend-api/codex/responses" || string(body) != `{"fixture":true}` {
					return errors.New("request was changed")
				}
				if mode == "rejected" || mode == "ambiguous" {
					return stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_Error{
						Error: &pluginv1.ForwardResponseError{Code: "fixture", RequestSent: mode == "ambiguous"},
					}})
				}
				if err := stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_Start{
					Start: &pluginv1.ForwardResponseStart{StatusCode: 200, Headers: headersToPlugin(http.Header{"Content-Type": {"text/plain"}})},
				}}); err != nil {
					return err
				}
				if err := stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_BodyChunk{BodyChunk: []byte("fixture")}}); err != nil {
					return err
				}
				if mode == "complete" {
					return stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_End{End: &pluginv1.ForwardResponseEnd{BytesReceived: 7}}})
				}
				if mode == "wrong_count" {
					return stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_End{End: &pluginv1.ForwardResponseEnd{BytesReceived: 8}}})
				}
				return nil
			})
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://fixture.invalid/backend-api/codex/responses",
				strings.NewReader(`{"fixture":true}`))
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.Do(req)
			if mode == "rejected" || mode == "ambiguous" {
				if err == nil || hostHTTPNotSent(err) != (mode == "rejected") {
					t.Fatalf("wrong dispatch classification: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if string(body) != "fixture" {
				t.Fatalf("body=%q", body)
			}
			if mode == "complete" && err != nil {
				t.Fatal(err)
			}
			if (mode == "truncated" || mode == "wrong_count") && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("truncated RPC treated as completed: %v", err)
			}
		})
	}
}

func TestHostHTTPResponseCloseCancelsHost(t *testing.T) {
	canceled := make(chan struct{})
	client := fixtureHostHTTPClient(t, func(stream pluginv1.TransportPlugin_ForwardServer) error {
		if _, _, err := readHostHTTPFixtureRequest(stream); err != nil {
			return err
		}
		if err := stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_Start{
			Start: &pluginv1.ForwardResponseStart{StatusCode: 200},
		}}); err != nil {
			return err
		}
		<-stream.Context().Done()
		close(canceled)
		return stream.Context().Err()
	})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://fixture.invalid/backend-api/codex/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	select {
	case <-canceled:
	case <-ctx.Done():
		t.Fatal("closing plugin response did not cancel host request")
	}
}

func TestRequiredHostHTTPNeverFallsBack(t *testing.T) {
	srv := stoppedProbeServer(t, cookiestore.New())
	srv.RequireHostHTTP()
	for _, binding := range []pluginv1.HostHTTPBinding{{}, {BrokerID: 7}} {
		if _, err := srv.outboundClient(binding, "", 42, 3); !errors.Is(err, errHostHTTPUnavailable) {
			t.Fatalf("missing host binding must reject without dialing: %v", err)
		}
	}
	client := fixtureHostHTTPClient(t, func(pluginv1.TransportPlugin_ForwardServer) error {
		t.Error("invalid binding reached host")
		return nil
	})
	srv.hostHTTP, srv.hostHTTPID = client.api, 7
	for _, binding := range []pluginv1.HostHTTPBinding{{}, {BrokerID: 8}} {
		if _, err := srv.outboundClient(binding, "", 42, 3); !errors.Is(err, errHostHTTPUnavailable) {
			t.Fatalf("host identity mismatch must reject: %v", err)
		}
	}
}

func TestRequiredHostHTTPReadinessDoesNotReplaceProcessLiveness(t *testing.T) {
	srv := stoppedProbeServer(t, cookiestore.New())
	srv.RequireHostHTTP()
	ctx := t.Context()
	health, err := srv.Health(ctx, &pluginv1.HealthRequest{})
	if err != nil || health.GetHealthy() {
		t.Fatalf("uninitialized host transport reported ready: %v %v", health, err)
	}
	md, _ := metadata.FromOutgoingContext(pluginv1.WithLivenessHealth(ctx))
	health, err = srv.Health(metadata.NewIncomingContext(ctx, md), &pluginv1.HealthRequest{})
	if err != nil || !health.GetHealthy() {
		t.Fatalf("pre-initialization process liveness failed: %v %v", health, err)
	}
	client := fixtureHostHTTPClient(t, func(pluginv1.TransportPlugin_ForwardServer) error {
		t.Error("health check dispatched an upstream request")
		return nil
	})
	srv.hostHTTP, srv.hostHTTPID = client.api, client.binding.BrokerID
	health, err = srv.Health(ctx, &pluginv1.HealthRequest{})
	if err != nil || !health.GetHealthy() {
		t.Fatalf("initialized host transport was not ready: %v %v", health, err)
	}
}

func TestHostHTTPForwardPreservesDispatchRejection(t *testing.T) {
	for _, sent := range []bool{false, true} {
		t.Run(map[bool]string{false: "host_rejection", true: "ambiguous_upstream"}[sent], func(t *testing.T) {
			var calls atomic.Int32
			client := fixtureHostHTTPClient(t, func(stream pluginv1.TransportPlugin_ForwardServer) error {
				if _, _, err := readHostHTTPFixtureRequest(stream); err != nil {
					return err
				}
				calls.Add(1)
				return stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_Error{
					Error: &pluginv1.ForwardResponseError{Code: "HOST_HTTP", RequestSent: sent},
				}})
			})
			store := cookiestore.New()
			cfg := probeTestConfig()
			cfg.InjectScope, cfg.QualityProbeEnabled = "all", false
			store.SetConfig(cfg)
			srv := stoppedProbeServer(t, store)
			srv.RequireHostHTTP()
			srv.hostHTTP, srv.hostHTTPID = client.api, client.binding.BrokerID
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			md, _ := metadata.FromOutgoingContext(pluginv1.WithHostHTTPBinding(ctx, client.binding))
			stream := forwardFixture(metadata.NewIncomingContext(ctx, md),
				"https://fixture.invalid/backend-api/codex/responses", "fixture")
			stream.requests[0].GetStart().ProxyUrl = client.proxy
			if err := srv.Forward(stream); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 || len(stream.responses) != 1 {
				t.Fatalf("dispatch must not retry: calls=%d responses=%d", calls.Load(), len(stream.responses))
			}
			failure := stream.responses[0].GetError()
			wantCode := "PLUGIN_HOST_TRANSPORT"
			if sent {
				wantCode = "UPSTREAM"
			}
			if failure == nil || failure.Code != wantCode || failure.RequestSent != sent {
				t.Fatalf("host dispatch classification changed: %v", failure)
			}
		})
	}
}

func TestHostHTTPForwardRetainsCookieProtectionAndInvalidatesProbeGeneration(t *testing.T) {
	var calls atomic.Int64
	client := fixtureHostHTTPClient(t, func(stream pluginv1.TransportPlugin_ForwardServer) error {
		start, _, err := readHostHTTPFixtureRequest(stream)
		if err != nil {
			return err
		}
		call := calls.Add(1)
		if call > 1 && headersFromPlugin(start.Headers).Get("Cookie") != "__cflb=fixture" {
			return errors.New("host delegation lost cookie protection")
		}
		body := []byte(`{"object":"response","status":"completed","output":[]}`)
		headers := http.Header{"Set-Cookie": {"__cflb=fixture; Max-Age=3500"}, "Content-Type": {"application/json"}}
		if err := stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_Start{
			Start: &pluginv1.ForwardResponseStart{StatusCode: 200, Headers: headersToPlugin(headers)},
		}}); err != nil {
			return err
		}
		if err := stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_BodyChunk{BodyChunk: body}}); err != nil {
			return err
		}
		return stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_End{
			End: &pluginv1.ForwardResponseEnd{BytesReceived: int64(len(body))},
		}})
	})
	store := cookiestore.New()
	cfg := probeTestConfig()
	cfg.InjectScope, cfg.QualityProbeEnabled = "all", false
	store.SetConfig(cfg)
	srv := stoppedProbeServer(t, store)
	srv.RequireHostHTTP()
	srv.hostHTTP, srv.hostHTTPID = client.api, client.binding.BrokerID
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	send := func(binding pluginv1.HostHTTPBinding) {
		t.Helper()
		md, _ := metadata.FromOutgoingContext(pluginv1.WithHostHTTPBinding(ctx, binding))
		stream := forwardFixture(metadata.NewIncomingContext(ctx, md), "https://fixture.invalid/backend-api/codex/responses", "fixture")
		stream.requests[0].GetStart().ProxyUrl = client.proxy
		if err := srv.Forward(stream); err != nil {
			t.Fatal(err)
		}
		if len(stream.responses) == 0 || stream.responses[len(stream.responses)-1].GetEnd() == nil {
			t.Fatal("host-bound forward did not complete")
		}
	}
	send(client.binding)
	srv.probeMu.Lock()
	first := *srv.templates[42]
	srv.probeMu.Unlock()
	next := client.binding
	next.Request = strings.Repeat("c", 64)
	send(next)
	srv.probeMu.Lock()
	stable := *srv.templates[42]
	srv.probeMu.Unlock()
	if first.IdentityGeneration != stable.IdentityGeneration {
		t.Fatal("per-request grant unnecessarily invalidated the probe generation")
	}
	next.Probe = strings.Repeat("d", 64)
	send(next)
	srv.probeMu.Lock()
	changed := *srv.templates[42]
	srv.probeMu.Unlock()
	if changed.IdentityGeneration == stable.IdentityGeneration || calls.Load() != 3 {
		t.Fatal("TLS grant change did not invalidate the old probe generation")
	}
}
