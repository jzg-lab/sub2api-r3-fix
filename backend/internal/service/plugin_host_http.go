package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	hcplugin "github.com/hashicorp/go-plugin"
)

const (
	pluginHTTPGrantTTL     = 24 * time.Hour
	pluginHTTPProbeTimeout = 120 * time.Second
)

var errPluginHTTPBinding = errors.New("host HTTP binding unavailable or stale")

type pluginHTTPGrant struct {
	ctx         context.Context
	accountID   int64
	concurrency int
	method      string
	target      string
	host        string
	proxy       string
	identity    string
	profile     *tlsfingerprint.Profile
	probeToken  string
	expires     time.Time
	used        bool // guarded by pluginHostHTTP.mu
}

// Grants are process-local, bounded and never persisted. Business grants are
// single-use; background grants additionally recheck current account/control
// state before every dispatch. The plugin cannot choose another route/profile.
type pluginHostHTTP struct {
	pluginv1.UnimplementedTransportPluginServer
	mu           sync.Mutex
	brokerID     uint32
	business     map[string]*pluginHTTPGrant
	probes       map[int64]*pluginHTTPGrant
	probeRunning map[int64]bool
	upstream     HTTPUpstream
	resolve      func(*Account) *tlsfingerprint.Profile
	account      func(context.Context, int64) (*Account, error)
	controls     OpenAIProbeBatchControlStore
	runtime      *pluginRuntime
}

func (m *PluginManager) startHostHTTP(r *pluginRuntime) error {
	if r.installation.PluginKey != "lyunlong.codex.lb-cookie-pin" {
		return nil
	}
	supported := false
	for _, capability := range r.capabilities {
		supported = supported || capability == pluginv1.HostHTTPCapability
	}
	if !supported || r.broker == nil || m.httpUpstream == nil || m.tlsProfiles == nil ||
		m.rateLimitService == nil || m.rateLimitService.accountRepo == nil {
		return errors.New("plugin requires compatible host HTTP/TLS transport; no direct fallback")
	}
	host := &pluginHostHTTP{
		brokerID: r.broker.NextId(), business: make(map[string]*pluginHTTPGrant),
		probes: make(map[int64]*pluginHTTPGrant), probeRunning: make(map[int64]bool),
		upstream: m.httpUpstream, resolve: m.tlsProfiles.ResolveTLSProfile,
		account: m.rateLimitService.accountRepo.GetByID, controls: m.probeControls, runtime: r,
	}
	return host.start(r)
}

func (host *pluginHostHTTP) start(r *pluginRuntime) error {
	listener, err := r.broker.Accept(host.brokerID)
	if err != nil {
		return err
	}
	server := hcplugin.DefaultGRPCServer(nil)
	pluginv1.RegisterTransportPluginServer(server, host)
	r.hostHTTP, r.hostHTTPServer = host, server
	go func() { _ = server.Serve(listener) }()
	// Broker connection advertisements expire after five seconds. Establish
	// the persistent reverse connection now, not on the first business call.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	health, err := r.api.Health(pluginv1.WithHostHTTPBroker(pluginv1.WithLivenessHealth(ctx), host.brokerID), &pluginv1.HealthRequest{})
	if err != nil || health == nil || !health.Healthy {
		server.Stop()
		return errors.New("plugin host HTTP transport initialization failed")
	}
	return nil
}

func pluginHTTPToken() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func pluginHTTPProfileKey(profile *tlsfingerprint.Profile) string {
	if profile == nil {
		return "disabled"
	}
	return "enabled:" + profile.TransportKey()
}

func (h *pluginHostHTTP) bind(ctx context.Context, req *http.Request, proxy string, account *Account) (pluginv1.HostHTTPBinding, error) {
	if req == nil || req.URL == nil || account == nil || h.resolve == nil ||
		account.ID <= 0 || account.Platform != PlatformOpenAI || account.Type != AccountTypeOAuth ||
		validateOpenAIAccountProxyRoute(account, proxy) != nil || ctx.Err() != nil {
		return pluginv1.HostHTTPBinding{}, errPluginHTTPBinding
	}
	requestToken, err := pluginHTTPToken()
	if err != nil {
		return pluginv1.HostHTTPBinding{}, err
	}
	probeToken, err := pluginHTTPToken()
	if err != nil {
		return pluginv1.HostHTTPBinding{}, err
	}
	now := time.Now()
	grant := &pluginHTTPGrant{
		ctx: ctx, accountID: account.ID, concurrency: account.Concurrency,
		method: req.Method, target: req.URL.String(), host: req.Host, proxy: proxy,
		identity: pluginv1.ProbeIdentity(req.Header.Get("Authorization"), req.Header.Get("Chatgpt-Account-Id"), proxy),
		profile:  h.resolve(account).Clone(), probeToken: probeToken, expires: now.Add(pluginHTTPGrantTTL),
	}
	if grant.profile != nil {
		if err := grant.profile.Validate(); err != nil {
			return pluginv1.HostHTTPBinding{}, err
		}
	}
	h.mu.Lock()
	if len(h.business) >= 4096 {
		h.mu.Unlock()
		return pluginv1.HostHTTPBinding{}, errPluginHTTPBinding
	}
	if old := h.probes[account.ID]; old != nil && now.Before(old.expires) &&
		old.identity == grant.identity && pluginHTTPSameProbeTarget(old.target, grant.target) && old.host == grant.host &&
		old.concurrency == grant.concurrency && pluginHTTPProfileKey(old.profile) == pluginHTTPProfileKey(grant.profile) {
		grant.probeToken = old.probeToken
	}
	if _, exists := h.probes[account.ID]; !exists && len(h.probes) >= pluginv1.MaxProbePolicyAccounts {
		var oldestID int64
		oldest := now.Add(2 * pluginHTTPGrantTTL)
		for id, entry := range h.probes {
			if entry.expires.Before(oldest) {
				oldestID, oldest = id, entry.expires
			}
		}
		delete(h.probes, oldestID)
	}
	probeGrant := *grant
	// Background grants outlive the business call, but must not retain its
	// context values (request bodies, tracing and other per-request state).
	probeGrant.ctx = nil
	h.business[requestToken], h.probes[account.ID] = grant, &probeGrant
	h.mu.Unlock()
	context.AfterFunc(ctx, func() {
		h.mu.Lock()
		delete(h.business, requestToken)
		h.mu.Unlock()
	})
	return pluginv1.HostHTTPBinding{BrokerID: h.brokerID, Request: requestToken, Probe: grant.probeToken}, nil
}

func pluginHTTPProbeTarget(target string) string {
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return ""
	}
	if u.Path == "/backend-api/codex/responses/compact" {
		u.Path = "/backend-api/codex/responses"
		u.RawPath = ""
	}
	if u.Path != "/backend-api/codex/responses" {
		return ""
	}
	return u.String()
}

func pluginHTTPSameProbeTarget(a, b string) bool {
	if a == b {
		return true
	}
	target := pluginHTTPProbeTarget(a)
	return target != "" && target == pluginHTTPProbeTarget(b)
}

func (h *pluginHostHTTP) authorize(ctx context.Context, binding pluginv1.HostHTTPBinding, start *pluginv1.ForwardRequestStart) (*pluginHTTPGrant, func(), error) {
	if ctx.Err() != nil || start == nil || start.Platform != PlatformOpenAI || start.AccountType != AccountTypeOAuth {
		return nil, nil, errPluginHTTPBinding
	}
	h.mu.Lock()
	var grant *pluginHTTPGrant
	if binding.BrokerID == h.brokerID && start != nil {
		if binding.IsProbe {
			grant = h.probes[start.AccountId]
			if grant != nil && (grant.probeToken != binding.Probe || h.probeRunning[start.AccountId] || len(h.probeRunning) >= 2) {
				grant = nil
			}
		} else {
			grant = h.business[binding.Request]
			if grant != nil && (grant.used || grant.ctx.Err() != nil) {
				grant = nil
			}
		}
	}
	if grant == nil {
		h.mu.Unlock()
		return nil, nil, errPluginHTTPBinding
	}
	target, method := grant.target, grant.method
	if binding.IsProbe {
		target, method = pluginHTTPProbeTarget(grant.target), http.MethodPost
	}
	headers := headersFromPlugin(start.Headers)
	if target == "" || start.Url != target || start.Method != method || start.Host != grant.host ||
		start.AccountId != grant.accountID || int(start.AccountConcurrency) != grant.concurrency ||
		start.ProxyUrl != grant.proxy || !time.Now().Before(grant.expires) ||
		pluginv1.ProbeIdentity(headers.Get("Authorization"), headers.Get("Chatgpt-Account-Id"), start.ProxyUrl) != grant.identity {
		h.mu.Unlock()
		return nil, nil, errPluginHTTPBinding
	}
	if !binding.IsProbe {
		grant.used = true
		h.mu.Unlock()
		return grant, func() {}, nil
	}
	h.probeRunning[start.AccountId] = true
	h.mu.Unlock()
	release := sync.OnceFunc(func() {
		h.mu.Lock()
		delete(h.probeRunning, start.AccountId)
		h.mu.Unlock()
	})
	controlCtx, cancel := context.WithTimeout(ctx, pluginProbePolicyTimeout)
	defer cancel()
	if h.account == nil || h.controls == nil {
		release()
		return nil, nil, errPluginHTTPBinding
	}
	account, err := h.account(controlCtx, grant.accountID)
	if err != nil || account == nil || account.Concurrency != grant.concurrency ||
		!isOpenAIDowngradeProbeAccountEligible(account, time.Now()) ||
		validateOpenAIAccountProxyRoute(account, grant.proxy) != nil ||
		pluginv1.ProbeIdentity("Bearer "+account.GetOpenAIAccessToken(), account.GetChatGPTAccountID(), grant.proxy) != grant.identity ||
		pluginHTTPProfileKey(h.resolve(account)) != pluginHTTPProfileKey(grant.profile) {
		release()
		return nil, nil, errPluginHTTPBinding
	}
	allowed, err := h.controls.AllowedOpenAIProbeAccounts(controlCtx, []int64{grant.accountID})
	if err != nil || !allowed[grant.accountID] {
		release()
		return nil, nil, errPluginHTTPBinding
	}
	for _, deadline := range []*time.Time{account.RateLimitResetAt, account.OverloadUntil, account.TempUnschedulableUntil} {
		if deadline != nil && deadline.After(time.Now()) {
			release()
			return nil, nil, errPluginHTTPBinding
		}
	}
	h.mu.Lock()
	current := h.probes[grant.accountID]
	valid := current != nil && current.probeToken == grant.probeToken
	h.mu.Unlock()
	if !valid || ctx.Err() != nil {
		release()
		return nil, nil, errPluginHTTPBinding
	}
	return grant, release, nil
}

func (h *pluginHostHTTP) Forward(stream pluginv1.TransportPlugin_ForwardServer) error {
	reject := func(code string, sent bool) error {
		return stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_Error{Error: &pluginv1.ForwardResponseError{
			Code: code, Message: "host HTTP transport rejected or interrupted", RequestSent: sent,
		}}})
	}
	binding, ok := pluginv1.ReadHostHTTPBinding(stream.Context())
	if !ok {
		return reject("HOST_HTTP_BINDING", false)
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	grant, release, err := h.authorize(stream.Context(), binding, first.GetStart())
	if err != nil {
		return reject("HOST_HTTP_BINDING", false)
	}
	defer release()
	if h.runtime != nil {
		if !h.runtime.beginRequest() {
			return reject("HOST_HTTP_DRAINING", false)
		}
		defer h.runtime.finishRequest()
	}
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	if binding.IsProbe {
		var probeCancel context.CancelFunc
		ctx, probeCancel = context.WithTimeout(ctx, pluginHTTPProbeTimeout)
		defer probeCancel()
	} else {
		stop := context.AfterFunc(grant.ctx, cancel)
		defer stop()
	}
	// Copy only bounded IDs from the authorized business grant. Background
	// probes and RPC contexts must not inherit another request's identity.
	for _, key := range []ctxkey.Key{ctxkey.RequestID, ctxkey.ClientRequestID} {
		id := ""
		if !binding.IsProbe {
			id = ctxkey.TraceID(grant.ctx, key)
		}
		ctx = context.WithValue(ctx, key, id)
	}
	ctx = WithHTTPUpstreamRedirectsDisabled(WithHTTPUpstreamProfile(ctx, HTTPUpstreamProfileOpenAIPlugin))
	start := first.GetStart()
	var reader io.ReadCloser = http.NoBody
	if start.HasBody {
		reader = newPluginHTTPRequestBody(ctx, stream)
		defer reader.Close()
	}
	request, err := http.NewRequestWithContext(ctx, start.Method, start.Url, reader)
	if err != nil {
		return reject("HOST_HTTP_REQUEST", false)
	}
	request.Header, request.Host = headersFromPlugin(start.Headers), start.Host
	request.ContentLength = start.ContentLength
	request.GetBody = nil
	if !start.HasBody {
		request.Body = http.NoBody
		request.ContentLength = 0
	}
	upstreamStarted := time.Now()
	response, err := h.upstream.DoWithTLS(request, grant.proxy, grant.accountID, grant.concurrency, grant.profile.Clone())
	if err != nil {
		return reject(pluginv1.HostHTTPFailureCode(err, ctx.Err(), false), true)
	}
	if response == nil || response.Body == nil {
		return reject("HOST_HTTP_UPSTREAM", true)
	}
	defer func() {
		cancel()
		_ = response.Body.Close()
	}()
	if err := stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_Start{Start: &pluginv1.ForwardResponseStart{
		StatusCode: int32(response.StatusCode), Status: response.Status, Protocol: response.Proto,
		ProtocolMajor: int32(response.ProtoMajor), ProtocolMinor: int32(response.ProtoMinor),
		Headers: headersToPlugin(response.Header), ContentLength: response.ContentLength,
	}}}); err != nil {
		return err
	}
	var received int64
	buffer := make([]byte, 32*1024)
	terminal := newPluginHTTPTerminalObserver(request, response)
	terminal.bind(request, upstreamStarted)
	eof := false
	defer func() { terminal.record(grant.accountID, response, eof, received) }()
	for {
		n, readErr := response.Body.Read(buffer)
		var tail []byte
		if n > 0 && terminal.write(buffer[:n]) && readErr == nil {
			tail, readErr = readPluginHTTPTerminalTail(response.Body, pluginHTTPTerminalDrainTime, grant.accountID, cancel)
		}
		if n > 0 {
			received += int64(n)
			if err := stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_BodyChunk{BodyChunk: append([]byte(nil), buffer[:n]...)}}); err != nil {
				return err
			}
			terminal.forwarded()
		}
		if len(tail) > 0 {
			received += int64(len(tail))
			if err := stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_BodyChunk{BodyChunk: tail}}); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			eof = true
			return stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_End{End: &pluginv1.ForwardResponseEnd{BytesReceived: received}}})
		}
		if readErr != nil {
			return reject(pluginv1.HostHTTPFailureCode(readErr, ctx.Err(), true), true)
		}
	}
}

// A server Recv cannot be canceled independently of its RPC. Keep it off the
// HTTP body reader so cancellation/Close can unblock the transport immediately.
// Returning from Forward then cancels the RPC and releases any pending Recv.
func newPluginHTTPRequestBody(ctx context.Context, stream pluginv1.TransportPlugin_ForwardServer) io.ReadCloser {
	reader, writer := io.Pipe()
	stop := context.AfterFunc(ctx, func() { _ = writer.CloseWithError(ctx.Err()) })
	go func() {
		defer stop()
		_, err := io.Copy(writer, &pluginHTTPRequestFrames{stream: stream})
		_ = writer.CloseWithError(err)
	}()
	return reader
}

type pluginHTTPRequestFrames struct {
	stream pluginv1.TransportPlugin_ForwardServer
	chunk  []byte
	ended  bool
}

func (b *pluginHTTPRequestFrames) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(b.chunk) == 0 && !b.ended {
		frame, err := b.stream.Recv()
		if err == io.EOF {
			return 0, io.ErrUnexpectedEOF
		}
		if err != nil {
			return 0, err
		}
		if frame == nil {
			return 0, errors.New("missing request frame")
		}
		switch value := frame.Frame.(type) {
		case *pluginv1.ForwardRequest_BodyChunk:
			b.chunk = value.BodyChunk
		case *pluginv1.ForwardRequest_BodyEnd:
			if !value.BodyEnd {
				return 0, errors.New("invalid request end")
			}
			b.ended = true
		default:
			return 0, errors.New("unexpected request frame")
		}
	}
	if len(b.chunk) == 0 && b.ended {
		return 0, io.EOF
	}
	n := copy(p, b.chunk)
	b.chunk = b.chunk[n:]
	return n, nil
}
