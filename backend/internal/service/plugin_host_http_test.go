package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

func hostHTTPFixture(t *testing.T) (*pluginHostHTTP, *Account, *http.Request, pluginv1.HostHTTPBinding, *pluginv1.ForwardRequestStart) {
	t.Helper()
	account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 3,
		Status: StatusActive, Schedulable: true, ProxyID: openAITransportTestProxyID(), Proxy: openAITransportTestProxy(),
		Credentials: map[string]any{"access_token": "fixture", "chatgpt_account_id": "fixture-subject"}}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://fixture.invalid/backend-api/codex/responses", strings.NewReader("{}"))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer fixture")
	req.Header.Set("Chatgpt-Account-Id", "fixture-subject")
	profile := &tlsfingerprint.Profile{CipherSuites: []uint16{0x1301}}
	host := &pluginHostHTTP{
		brokerID: 7, business: make(map[string]*pluginHTTPGrant), probes: make(map[int64]*pluginHTTPGrant),
		probeRunning: make(map[int64]bool),
		resolve:      func(*Account) *tlsfingerprint.Profile { return profile },
		account:      func(context.Context, int64) (*Account, error) { return account, nil },
		controls:     &probePolicyControls{allowed: map[int64]bool{42: true}},
	}
	binding, err := host.bind(t.Context(), req, account.Proxy.URL(), account)
	require.NoError(t, err)
	start := &pluginv1.ForwardRequestStart{Method: req.Method, Url: req.URL.String(), Host: req.Host,
		AccountId: account.ID, AccountConcurrency: int32(account.Concurrency), ProxyUrl: account.Proxy.URL(),
		Platform: account.Platform, AccountType: account.Type,
		Headers: headersToPlugin(req.Header), HasBody: true, ContentLength: 2}
	return host, account, req, binding, start
}

func TestPluginHostHTTPBindingSingleUseAndIsolation(t *testing.T) {
	for _, field := range []string{"account", "platform", "account_type", "proxy", "target", "method", "host", "identity", "capacity", "broker", "expired", "canceled", "rpc_canceled", "valid"} {
		t.Run(field, func(t *testing.T) {
			host, _, _, binding, start := hostHTTPFixture(t)
			ctx := t.Context()
			switch field {
			case "account":
				start.AccountId++
			case "platform":
				start.Platform = "other"
			case "account_type":
				start.AccountType = "apikey"
			case "proxy":
				start.ProxyUrl = ""
			case "target":
				start.Url += "/another"
			case "method":
				start.Method = http.MethodGet
			case "host":
				start.Host = "different.invalid"
			case "identity":
				start.Headers["Authorization"] = &pluginv1.HeaderValues{Values: []string{"Bearer replacement-fixture"}}
			case "capacity":
				start.AccountConcurrency++
			case "broker":
				binding.BrokerID++
			case "expired":
				host.business[binding.Request].expires = time.Now().Add(-time.Second)
			case "canceled":
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				host.business[binding.Request].ctx = ctx
			case "rpc_canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			_, release, err := host.authorize(ctx, binding, start)
			if field != "valid" {
				require.ErrorIs(t, err, errPluginHTTPBinding)
				require.Nil(t, release)
				return
			}
			require.NoError(t, err)
			release()
			_, _, err = host.authorize(t.Context(), binding, start)
			require.ErrorIs(t, err, errPluginHTTPBinding, "business grant must never replay")
		})
	}
}

func TestPluginHostHTTPProbeRechecksCurrentAuthority(t *testing.T) {
	for _, field := range []string{"deleted", "db", "controls", "disabled", "proxy", "identity", "profile", "capacity", "rate_limit", "overload", "temporary", "valid"} {
		t.Run(field, func(t *testing.T) {
			host, account, _, binding, start := hostHTTPFixture(t)
			binding.IsProbe = true
			future := time.Now().Add(time.Hour)
			switch field {
			case "deleted":
				host.account = func(context.Context, int64) (*Account, error) { return nil, nil }
			case "db":
				host.account = func(context.Context, int64) (*Account, error) { return nil, errors.New("fixture") }
			case "controls":
				host.controls = &probePolicyControls{err: errors.New("fixture")}
			case "disabled":
				host.controls = &probePolicyControls{allowed: map[int64]bool{42: false}}
			case "proxy":
				account.Proxy = nil
			case "identity":
				account.Credentials["access_token"] = "rotated-fixture"
			case "profile":
				host.resolve = func(*Account) *tlsfingerprint.Profile { return nil }
			case "capacity":
				account.Concurrency++
			case "rate_limit":
				account.RateLimitResetAt = &future
			case "overload":
				account.OverloadUntil = &future
			case "temporary":
				account.TempUnschedulableUntil = &future
			}
			_, release, err := host.authorize(t.Context(), binding, start)
			if field != "valid" {
				require.Error(t, err)
				require.Empty(t, host.probeRunning, "failed admission must release its reservation")
				return
			}
			require.NoError(t, err)
			_, _, err = host.authorize(t.Context(), binding, start)
			require.Error(t, err, "same account cannot run concurrent background probes")
			release()
			release()
			require.Empty(t, host.probeRunning)
		})
	}
}

func TestPluginHostHTTPProfileSnapshotAndProbeGeneration(t *testing.T) {
	host, account, req, first, _ := hostHTTPFixture(t)
	require.Nil(t, host.probes[account.ID].ctx, "background grant must not retain business context")
	require.NotNil(t, host.business[first.Request].ctx)
	original := host.business[first.Request].profile.TransportKey()
	profile := host.resolve(account)
	profile.CipherSuites[0] = 0x1302
	require.Equal(t, original, host.business[first.Request].profile.TransportKey())
	second, err := host.bind(t.Context(), req, account.Proxy.URL(), account)
	require.NoError(t, err)
	require.NotEqual(t, first.Probe, second.Probe, "TLS drift must revoke old background grants")
	req.URL.Path += "/compact"
	third, err := host.bind(t.Context(), req, account.Proxy.URL(), account)
	require.NoError(t, err)
	require.Equal(t, second.Probe, third.Probe, "compact and responses share one background target")
	require.NotEqual(t, second.Request, third.Request)
	req.URL.Path = "/unsupported-a"
	fourth, err := host.bind(t.Context(), req, account.Proxy.URL(), account)
	require.NoError(t, err)
	req.URL.Path = "/unsupported-b"
	fifth, err := host.bind(t.Context(), req, account.Proxy.URL(), account)
	require.NoError(t, err)
	require.NotEqual(t, fourth.Probe, fifth.Probe, "invalid canonical targets must not compare equal")
}

type hostHTTPFixtureStream struct {
	grpc.ServerStream
	ctx    context.Context
	input  []*pluginv1.ForwardRequest
	output []*pluginv1.ForwardResponse
}

func (s *hostHTTPFixtureStream) Context() context.Context { return s.ctx }
func (s *hostHTTPFixtureStream) Recv() (*pluginv1.ForwardRequest, error) {
	if len(s.input) == 0 {
		return nil, io.EOF
	}
	frame := s.input[0]
	s.input = s.input[1:]
	return frame, nil
}
func (s *hostHTTPFixtureStream) Send(frame *pluginv1.ForwardResponse) error {
	s.output = append(s.output, frame)
	return nil
}

type hostHTTPUpstreamFixture struct {
	HTTPUpstream
	call func(*http.Request, string, int64, int, *tlsfingerprint.Profile) (*http.Response, error)
}

func (u hostHTTPUpstreamFixture) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	return u.call(req, proxy, id, concurrency, profile)
}

func TestPluginHostHTTPForwardUsesExactHostTLSAndExplicitEnd(t *testing.T) {
	for _, tlsEnabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[tlsEnabled], func(t *testing.T) {
			host, account, req, binding, start := hostHTTPFixture(t)
			if !tlsEnabled {
				host.resolve = func(*Account) *tlsfingerprint.Profile { return nil }
				var err error
				binding, err = host.bind(t.Context(), req, account.Proxy.URL(), account)
				require.NoError(t, err)
			}
			expected := pluginHTTPProfileKey(host.business[binding.Request].profile)
			calls := 0
			host.upstream = hostHTTPUpstreamFixture{call: func(r *http.Request, proxy string, id int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
				calls++
				require.Equal(t, expected, pluginHTTPProfileKey(profile))
				require.Equal(t, account.Proxy.URL(), proxy)
				require.Equal(t, account.ID, id)
				require.Equal(t, account.Concurrency, concurrency)
				require.Equal(t, HTTPUpstreamProfileOpenAIPlugin, HTTPUpstreamProfileFromContext(r.Context()))
				require.True(t, HTTPUpstreamRedirectsDisabled(r.Context()))
				require.Nil(t, r.GetBody)
				body, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				require.Equal(t, "{}", string(body))
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("complete")), Header: http.Header{}}, nil
			}}
			md, _ := metadata.FromOutgoingContext(pluginv1.WithHostHTTPBinding(t.Context(), binding))
			stream := &hostHTTPFixtureStream{ctx: metadata.NewIncomingContext(t.Context(), md), input: []*pluginv1.ForwardRequest{
				{Frame: &pluginv1.ForwardRequest_Start{Start: start}},
				{Frame: &pluginv1.ForwardRequest_BodyChunk{BodyChunk: []byte("{}")}},
				{Frame: &pluginv1.ForwardRequest_BodyEnd{BodyEnd: true}},
			}}
			require.NoError(t, host.Forward(stream))
			require.Equal(t, 1, calls)
			require.Len(t, stream.output, 3)
			require.NotNil(t, stream.output[0].GetStart())
			require.NotNil(t, stream.output[2].GetEnd())
			require.EqualValues(t, 8, stream.output[2].GetEnd().BytesReceived)
		})
	}
}

func TestPluginHostHTTPTraceIdentityFollowsOnlyBusinessGrant(t *testing.T) {
	host, account, req, _, start := hostHTTPFixture(t)
	ids := []string{"e2d5e88a-84cb-46ee-83e3-586131ca5745", "f72b2c65-75b0-48af-9be3-a705f4132418"}
	type privateContextKey struct{}
	bindings := make([]pluginv1.HostHTTPBinding, 2)
	for i, id := range ids {
		ctx := context.WithValue(t.Context(), privateContextKey{}, "fixture-private-marker")
		ctx = context.WithValue(ctx, ctxkey.RequestID, id)
		ctx = context.WithValue(ctx, ctxkey.ClientRequestID, ids[1-i])
		var err error
		bindings[i], err = host.bind(ctx, req, account.Proxy.URL(), account)
		require.NoError(t, err)
	}
	require.Nil(t, host.probes[account.ID].ctx)
	for _, i := range []int{1, 0, 2} {
		binding := bindings[i%2]
		binding.IsProbe = i == 2
		calls := 0
		host.upstream = hostHTTPUpstreamFixture{call: func(r *http.Request, proxy string, id int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
			calls++
			wantID, wantClientID := "", ""
			if !binding.IsProbe {
				wantID, wantClientID = ids[i], ids[1-i]
			}
			require.Equal(t, wantID, r.Context().Value(ctxkey.RequestID))
			require.Equal(t, wantClientID, r.Context().Value(ctxkey.ClientRequestID))
			require.Nil(t, r.Context().Value(privateContextKey{}))
			require.Empty(t, r.Header.Get("X-Request-ID"))
			require.Empty(t, r.Header.Get("X-Client-Request-ID"))
			require.Equal(t, account.Proxy.URL(), proxy)
			require.Equal(t, account.ID, id)
			_, err := io.Copy(io.Discard, r.Body)
			require.NoError(t, err)
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("complete")), Header: http.Header{}}, nil
		}}
		rpcCtx := context.WithValue(t.Context(), ctxkey.RequestID, ids[0])
		rpcCtx = context.WithValue(rpcCtx, ctxkey.ClientRequestID, ids[1])
		md, _ := metadata.FromOutgoingContext(pluginv1.WithHostHTTPBinding(rpcCtx, binding))
		stream := &hostHTTPFixtureStream{ctx: metadata.NewIncomingContext(rpcCtx, md), input: []*pluginv1.ForwardRequest{
			{Frame: &pluginv1.ForwardRequest_Start{Start: start}},
			{Frame: &pluginv1.ForwardRequest_BodyChunk{BodyChunk: []byte("{}")}},
			{Frame: &pluginv1.ForwardRequest_BodyEnd{BodyEnd: true}},
		}}
		require.NoError(t, host.Forward(stream))
		require.Equal(t, 1, calls)
	}
}

func TestPluginHostHTTPForwardTerminalDeadlineCancelsOnlyItsRequest(t *testing.T) {
	host, account, _, binding, start := hostHTTPFixture(t)
	parent, cancelParent := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancelParent()
	calls := 0
	host.upstream = hostHTTPUpstreamFixture{call: func(r *http.Request, proxy string, id int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
		calls++
		require.Equal(t, account.ID, id)
		require.Equal(t, account.Proxy.URL(), proxy)
		_, err := io.Copy(io.Discard, r.Body)
		require.NoError(t, err)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body: io.NopCloser(io.MultiReader(
				strings.NewReader(pluginHTTPCompletedFixture),
				&pluginHTTPCancelOnlyBody{ctx: r.Context()},
			)),
		}, nil
	}}
	md, _ := metadata.FromOutgoingContext(pluginv1.WithHostHTTPBinding(parent, binding))
	stream := &hostHTTPFixtureStream{ctx: metadata.NewIncomingContext(parent, md), input: []*pluginv1.ForwardRequest{
		{Frame: &pluginv1.ForwardRequest_Start{Start: start}},
		{Frame: &pluginv1.ForwardRequest_BodyChunk{BodyChunk: []byte("{}")}},
		{Frame: &pluginv1.ForwardRequest_BodyEnd{BodyEnd: true}},
	}}
	started := time.Now()
	require.NoError(t, host.Forward(stream))
	require.Less(t, time.Since(started), time.Second)
	require.NoError(t, parent.Err(), "the RPC and sibling requests must remain live")
	require.NoError(t, t.Context().Err(), "the original grant must remain live")
	require.Equal(t, 1, calls, "never replay a completed upstream request")
	require.Len(t, stream.output, 3)
	require.NotNil(t, stream.output[0].GetStart())
	require.Equal(t, pluginHTTPCompletedFixture, string(stream.output[1].GetBodyChunk()))
	require.NotNil(t, stream.output[2].GetError(), "a deadline must not be recorded as HTTP EOF")
	require.Nil(t, stream.output[2].GetEnd())
}

func TestPluginHostHTTPRequestRequiresExplicitEnd(t *testing.T) {
	body := newPluginHTTPRequestBody(t.Context(), &hostHTTPFixtureStream{input: []*pluginv1.ForwardRequest{
		{Frame: &pluginv1.ForwardRequest_BodyChunk{BodyChunk: []byte("partial")}},
	}})
	defer body.Close()
	data, err := io.ReadAll(body)
	require.Equal(t, "partial", string(data))
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
}

type hostHTTPStalledUpload struct {
	pluginv1.TransportPlugin_ForwardServer
	ctx     context.Context
	entered chan struct{}
	exited  chan struct{}
}

func (s *hostHTTPStalledUpload) Recv() (*pluginv1.ForwardRequest, error) {
	close(s.entered)
	defer close(s.exited)
	<-s.ctx.Done()
	return nil, s.ctx.Err()
}

func TestPluginHostHTTPRequestCancellationUnblocksStalledUpload(t *testing.T) {
	for _, closeBody := range []bool{false, true} {
		t.Run(map[bool]string{false: "request_canceled", true: "body_closed"}[closeBody], func(t *testing.T) {
			rpcCtx, finishRPC := context.WithCancel(t.Context())
			defer finishRPC()
			requestCtx, cancel := context.WithCancel(t.Context())
			defer cancel()
			stream := &hostHTTPStalledUpload{
				ctx: rpcCtx, entered: make(chan struct{}), exited: make(chan struct{}),
			}
			body := newPluginHTTPRequestBody(requestCtx, stream)
			defer body.Close()
			readDone := make(chan error, 1)
			go func() {
				_, err := body.Read(make([]byte, 1))
				readDone <- err
			}()
			select {
			case <-stream.entered:
			case <-time.After(time.Second):
				t.Fatal("upload reader did not start")
			}
			if closeBody {
				require.NoError(t, body.Close())
			} else {
				cancel()
			}
			select {
			case err := <-readDone:
				if closeBody {
					require.ErrorIs(t, err, io.ErrClosedPipe)
				} else {
					require.ErrorIs(t, err, context.Canceled)
				}
			case <-time.After(time.Second):
				t.Fatal("HTTP upload remained blocked on RPC receive")
			}
			// The handler can now return; gRPC then ends its receive context.
			finishRPC()
			select {
			case <-stream.exited:
			case <-time.After(time.Second):
				t.Fatal("RPC upload receiver did not exit")
			}
		})
	}
}

func TestPluginHostHTTPRequestRejectsMalformedFrames(t *testing.T) {
	for name, frame := range map[string]*pluginv1.ForwardRequest{
		"missing":   nil,
		"false_end": {Frame: &pluginv1.ForwardRequest_BodyEnd{BodyEnd: false}},
		"start":     {Frame: &pluginv1.ForwardRequest_Start{Start: &pluginv1.ForwardRequestStart{}}},
	} {
		t.Run(name, func(t *testing.T) {
			body := newPluginHTTPRequestBody(t.Context(), &hostHTTPFixtureStream{
				input: []*pluginv1.ForwardRequest{frame},
			})
			defer body.Close()
			_, err := io.ReadAll(body)
			require.Error(t, err)
		})
	}
}
