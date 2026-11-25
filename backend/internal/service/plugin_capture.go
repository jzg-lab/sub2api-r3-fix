package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"google.golang.org/grpc/metadata"
)

const captureRecordMaxBytes = 60 << 20
const captureQueueMaxBytes = 128 << 20
const captureDownloadTimeout = 30 * time.Second

// captureSession lives only for one forwarding attempt; queued records own their bytes.
type captureSession struct {
	ctx            context.Context
	budgetCtx      context.Context
	releaseBudget  func()
	mu             sync.Mutex
	manager        *PluginManager
	runtime        *pluginRuntime
	accountID      int64
	groupID        int64
	requestedModel string
	provider       string
	request        []byte
	response       *captureReadCloser
	finished       bool
	clientSecrets  []string
}
type captureContextKey struct{}

type captureReadCloser struct {
	budgetCtx context.Context
	io.ReadCloser
	mu             sync.Mutex
	buffer         bytes.Buffer
	complete       bool
	readFailed     bool
	tooLarge       bool
	contentType    string
	stopped        bool
	statusCode     int
	transport      string
	requestSecrets []string
	attemptError   bool
}

func (r *captureReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return n, err
	}
	if !r.tooLarge && n > 0 {
		required := r.buffer.Len() + n
		if required > captureRecordMaxBytes {
			r.tooLarge = true
		} else if required > r.buffer.Cap() {
			capacity := min(captureRecordMaxBytes, max(required, max(4096, 2*r.buffer.Cap())))
			if r.budgetCtx != nil && pkghttputil.ReserveRequestBodyMemory(r.budgetCtx, int64(capacity)) != nil {
				r.tooLarge = true
			} else {
				oldCapacity := r.buffer.Cap()
				data := make([]byte, r.buffer.Len(), capacity)
				copy(data, r.buffer.Bytes())
				r.buffer = *bytes.NewBuffer(data)
				if r.budgetCtx != nil {
					pkghttputil.ReleaseRequestBodyMemory(r.budgetCtx, int64(oldCapacity))
				}
			}
		}
		if r.tooLarge {
			if r.budgetCtx != nil {
				pkghttputil.ReleaseRequestBodyMemory(r.budgetCtx, int64(r.buffer.Cap()))
			}
			r.buffer = bytes.Buffer{}
		} else {
			_, _ = r.buffer.Write(p[:n])
		}
	}
	if err == io.EOF {
		r.complete = true
	} else if err != nil {
		r.readFailed = true
	}
	return n, err
}

func (m *PluginManager) beginCapture(ctx context.Context, c *gin.Context, account *Account) (context.Context, func(bool)) {
	noop := func(bool) {}
	if m == nil || account == nil || (account.Platform != PlatformAnthropic && account.Platform != PlatformOpenAI) ||
		isCaptureProbeTraffic(ctx) || ctx.Value(captureContextKey{}) != nil {
		return ctx, noop
	}
	runtime := m.captureRuntime.Load()
	if runtime == nil || runtime.draining.Load() {
		return ctx, noop
	}
	select {
	case m.captureSlots <- struct{}{}:
	default:
		return ctx, noop
	}
	budgetCtx, releaseBudget := pkghttputil.WithRequestBodyMemoryBudget(ctx, m.captureMemory)
	session := &captureSession{ctx: budgetCtx, manager: m, runtime: runtime, accountID: account.ID, provider: account.Platform}
	// Snapshot routing and the client model before mappings or later forwarding attempts mutate them.
	session.groupID = getOpenAIGroupIDFromContext(c)
	if c != nil && c.Request != nil {
		session.requestedModel, _ = c.Request.Context().Value(ctxkey.Model).(string)
		session.clientSecrets = captureHeaderSecrets(c.Request.Header)
		if key, ok := c.Get("api_key"); ok {
			if apiKey, ok := key.(*APIKey); ok && apiKey != nil && apiKey.Key != "" {
				session.clientSecrets = append(session.clientSecrets, apiKey.Key)
			}
		}
	}
	return context.WithValue(ctx, captureContextKey{}, session), func(success bool) {
		defer func() {
			<-m.captureSlots
			releaseBudget()
		}()
		session.finish(success)
	}
}

// Only business forwarding installs a session. Probes, OAuth refresh and media do not.
type captureHTTPUpstream struct{ HTTPUpstream }

func withCaptureUpstream(upstream HTTPUpstream) HTTPUpstream {
	if upstream == nil {
		return nil
	}
	if _, ok := upstream.(*captureHTTPUpstream); ok {
		return upstream
	}
	return &captureHTTPUpstream{HTTPUpstream: upstream}
}
func (u *captureHTTPUpstream) Do(req *http.Request, proxy string, id int64, concurrency int) (*http.Response, error) {
	resp, err := u.HTTPUpstream.Do(req, proxy, id, concurrency)
	captureHTTPResponse(req, resp, err)
	return resp, err
}
func (u *captureHTTPUpstream) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	resp, err := u.HTTPUpstream.DoWithTLS(req, proxy, id, concurrency, profile)
	captureHTTPResponse(req, resp, err)
	return resp, err
}
func captureHTTPResponse(req *http.Request, resp *http.Response, err error) {
	if req == nil {
		return
	}
	session, _ := req.Context().Value(captureContextKey{}).(*captureSession)
	if session == nil {
		return
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.finished {
		return
	}
	if session.response != nil {
		session.emitAttemptLocked(session.response)
	}
	session.response, session.request = nil, nil
	if session.releaseBudget != nil {
		session.releaseBudget()
		session.releaseBudget = nil
	}
	session.budgetCtx, session.releaseBudget = pkghttputil.WithRequestBodyMemoryScope(session.ctx)
	var request []byte
	if req.GetBody != nil {
		body, readErr := req.GetBody()
		if readErr == nil {
			requestReader := &captureReadCloser{ReadCloser: body, budgetCtx: session.budgetCtx}
			_, readErr = io.Copy(io.Discard, requestReader)
			_ = body.Close()
			if readErr == nil && !requestReader.tooLarge {
				request = requestReader.buffer.Bytes()
			}
		}
	}
	secrets := captureHeaderSecrets(req.Header)
	if resp == nil || resp.Body == nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		session.emitRawAttempt(request, nil, nil, "http", status, false, secrets)
		return
	}
	session.request = request
	session.response = &captureReadCloser{ReadCloser: resp.Body, budgetCtx: session.budgetCtx, contentType: resp.Header.Get("Content-Type"), statusCode: resp.StatusCode, transport: "http", requestSecrets: secrets, attemptError: err != nil}
	if strings.Contains(strings.ToLower(session.response.contentType), "text/event-stream") {
		session.response.transport = "sse"
	}
	resp.Body = session.response
}
func (s *captureSession) finish(_ bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finished = true
	defer func() {
		if s.releaseBudget != nil {
			s.releaseBudget()
		}
		s.budgetCtx = nil
	}()
	if s.response != nil {
		s.emitAttemptLocked(s.response)
	}
}

func (s *captureSession) emitAttemptLocked(r *captureReadCloser) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	defer func() { r.stopped = true; r.buffer = bytes.Buffer{}; s.request = nil }()
	if r.tooLarge {
		return
	}
	complete := r.complete && !r.readFailed && !r.attemptError
	if r.transport == "sse" {
		complete = !r.readFailed && !r.attemptError && captureResponseComplete(r.contentType, r.buffer.Bytes(), r.complete)
	}
	s.emitRawAttempt(s.request, r.buffer.Bytes(), nil, r.transport, r.statusCode, complete, r.requestSecrets)
}
func captureResponseComplete(contentType string, body []byte, eof bool) bool {
	if !strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		if !eof || !json.Valid(body) {
			return false
		}
		return !captureHasError(gjson.ParseBytes(body)) && !captureFailedStatus(gjson.GetBytes(body, "status").String())
	}
	terminal := false
	var event string
	var frame strings.Builder
	check := func() bool {
		data := frame.String()
		if event == "error" {
			return false
		}
		if strings.TrimSpace(data) == "[DONE]" {
			terminal = true
			return true
		}
		if data == "" {
			return true
		}
		if !json.Valid([]byte(data)) {
			return false
		}
		value := gjson.Parse(data)
		kind := value.Get("type").String()
		if kind == "error" || kind == "response.failed" || kind == "response.incomplete" || captureHasError(value) || captureHasError(value.Get("response")) ||
			captureFailedStatus(value.Get("response.status").String()) {
			return false
		}
		if kind == "message_stop" || kind == "response.completed" || event == "message_stop" || event == "response.completed" {
			terminal = true
		}
		return true
	}
	// Iterate without allocating a slice for every heartbeat/line.
	for len(body) > 0 {
		line, rest, _ := bytes.Cut(body, []byte{'\n'})
		body = rest
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if len(line) == 0 {
			if !check() {
				return false
			}
			event = ""
			frame.Reset()
			continue
		}
		if bytes.HasPrefix(line, []byte("event:")) {
			event = strings.TrimSpace(string(line[6:]))
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			if frame.Len() > 0 {
				_ = frame.WriteByte('\n')
			}
			_, _ = frame.Write(bytes.TrimPrefix(line[5:], []byte(" ")))
		}
	}
	// Incomplete trailing frames do not establish successful termination.
	if event != "" || frame.Len() > 0 {
		if !eof || !check() {
			return false
		}
	}
	return terminal
}
func captureFailedStatus(status string) bool {
	return status == "failed" || status == "incomplete" || status == "cancelled" || status == "in_progress" || status == "queued"
}

func (s *GatewayService) SetPluginManager(manager *PluginManager) {
	if s != nil {
		s.pluginManager = manager
		s.httpUpstream = withCaptureUpstream(s.httpUpstream)
	}
}

type queuedCapture struct {
	release         func()
	runtime         *pluginRuntime
	request         *pluginv1.CaptureRecordRequest
	filterModelHash [32]byte
}

func hasEnabledCaptureBinding(bindings []PluginBinding) bool {
	for _, binding := range bindings {
		if binding.Enabled && binding.Capability == PluginCapabilityCaptureRecords {
			return true
		}
	}
	return false
}

func (m *PluginManager) enqueueCapture(runtime *pluginRuntime, request *pluginv1.CaptureRecordRequest, release func(), filterModelHash [32]byte) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if runtime == nil || runtime.draining.Load() || m.captureRuntime.Load() != runtime || len(request.GetRecordJson()) > captureRecordMaxBytes {
		return false
	}
	size := int64(len(request.GetRecordJson()))
	for {
		current := m.captureQueuedBytes.Load()
		if current+size > captureQueueMaxBytes {
			return false
		}
		if m.captureQueuedBytes.CompareAndSwap(current, current+size) {
			break
		}
	}
	// Ownership transfers to the queue; no request/context/account pointers survive.
	select {
	case m.captureQueue <- queuedCapture{runtime: runtime, request: request, release: release, filterModelHash: filterModelHash}:
		return true
	default:
		m.captureQueuedBytes.Add(-size)
		return false
	}
}

func (m *PluginManager) reconcileCaptureRuntime(ctx context.Context, installation *PluginInstallation) error {
	current := m.captureRuntime.Load()
	if installation == nil {
		if current != nil {
			m.captureRuntime.Store(nil)
			m.mu.Lock()
			delete(m.runtimes, current.installation.ID)
			m.mu.Unlock()
			current.drain(10 * time.Second)
		}
		return nil
	}
	if current != nil && current.installation.ID == installation.ID &&
		current.installation.BinarySHA256 == installation.BinarySHA256 &&
		current.installation.ConfigEncrypted == installation.ConfigEncrypted && !current.client.Exited() {
		healthCtx, cancel := context.WithTimeout(ctx, pluginHealthTimeout)
		err := current.checkHealth(healthCtx)
		cancel()
		if err == nil {
			return nil
		}
	}
	if current != nil {
		m.captureRuntime.Store(nil)
		m.mu.Lock()
		delete(m.runtimes, current.installation.ID)
		m.mu.Unlock()
		current.drain(10 * time.Second)
	}
	if installation.State == PluginStateStarting && !m.startingStateExpired(installation) {
		return nil
	}
	local, err := m.ensureLocalInstallation(ctx, installation)
	if err != nil {
		return err
	}
	runtime, err := m.prepareRuntime(ctx, local, true)
	if err != nil {
		return err
	}
	latest, err := m.repo.GetByID(ctx, installation.ID)
	if err != nil {
		runtime.kill()
		return err
	}
	if !hasEnabledCaptureBinding(latest.Bindings) || latest.BinarySHA256 != installation.BinarySHA256 || latest.ConfigEncrypted != installation.ConfigEncrypted {
		runtime.kill()
		return nil
	}
	if err := m.repo.MarkRuntimeHealthy(ctx, latest.ID, latest.BinarySHA256, latest.ConfigEncrypted); err != nil {
		runtime.kill()
		if errors.Is(err, ErrPluginStateChanged) {
			return nil
		}
		return err
	}
	m.mu.Lock()
	var archiveRuntime *pluginRuntime
	if current := m.archiveReadRuntime; current != nil && current.installation.ID == installation.ID {
		archiveRuntime = current
		m.archiveReadRuntime = nil
	}
	m.captureRuntime.Store(runtime)
	m.runtimes[installation.ID] = runtime
	m.mu.Unlock()
	if archiveRuntime != nil {
		archiveRuntime.drain(captureDownloadTimeout)
	}
	return nil
}

func (m *PluginManager) captureLoop(ctx context.Context, done chan struct{}) {
	defer close(done)
	defer func() {
		for {
			select {
			case item := <-m.captureQueue:
				m.captureQueuedBytes.Add(-int64(len(item.request.RecordJson)))
				item.release()
			default:
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-m.captureQueue:
			runtime, request := item.runtime, item.request
			size := int64(len(request.RecordJson))
			if ctx.Err() != nil || m.captureRuntime.Load() != runtime || !runtime.beginRequest() {
				m.captureQueuedBytes.Add(-size)
				item.release()
				continue
			}
			rpcCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			rpcCtx = metadata.AppendToOutgoingContext(rpcCtx, "capture-filter-model-bin", string(item.filterModelHash[:]))
			response, err := runtime.api.CaptureRecord(rpcCtx, request)
			cancel()
			runtime.finishRequest()
			if err != nil || response == nil || !response.GetAccepted() {
				slog.Warn("plugin_capture_record_dropped", "plugin_id", runtime.installation.ID)
			}
			m.captureQueuedBytes.Add(-size)
			item.release()
		}
	}
}

func (m *PluginManager) captureReadRuntime(ctx context.Context, pluginID int64) (*pluginRuntime, error) {
	if runtime := m.captureRuntime.Load(); runtime != nil && runtime.installation.ID == pluginID && !runtime.client.Exited() {
		return runtime, nil
	}
	m.operationMu.Lock()
	defer m.operationMu.Unlock()
	if runtime := m.captureRuntime.Load(); runtime != nil && runtime.installation.ID == pluginID && !runtime.client.Exited() {
		return runtime, nil
	}
	installation, err := m.repo.GetByID(ctx, pluginID)
	if err != nil {
		return nil, err
	}
	if !installation.Manifest.HasCapability(PluginCapabilityCaptureRecords) {
		return nil, errors.New("插件没有档案读取能力")
	}
	m.mu.Lock()
	runtime := m.archiveReadRuntime
	m.mu.Unlock()
	if runtime != nil && runtime.installation.ID == pluginID &&
		runtime.installation.BinarySHA256 == installation.BinarySHA256 &&
		runtime.installation.ConfigEncrypted == installation.ConfigEncrypted && !runtime.client.Exited() {
		return runtime, nil
	}
	if runtime != nil {
		m.mu.Lock()
		if m.archiveReadRuntime == runtime {
			m.archiveReadRuntime = nil
		}
		m.mu.Unlock()
		runtime.drain(captureDownloadTimeout)
	}
	local, err := m.ensureLocalInstallation(ctx, installation)
	if err != nil {
		return nil, err
	}
	runtime, err = m.prepareRuntime(ctx, local, true)
	if err != nil {
		return nil, err
	}
	latest, err := m.repo.GetByID(ctx, pluginID)
	if err != nil || latest.BinarySHA256 != installation.BinarySHA256 || latest.ConfigEncrypted != installation.ConfigEncrypted {
		runtime.kill()
		if err != nil {
			return nil, err
		}
		return nil, ErrPluginStateChanged
	}
	m.mu.Lock()
	m.archiveReadRuntime = runtime
	m.mu.Unlock()
	return runtime, nil
}

func (m *PluginManager) ListCaptureRecords(ctx context.Context, pluginID int64, date string) ([]*pluginv1.CaptureRecordInfo, error) {
	runtime, err := m.captureReadRuntime(ctx, pluginID)
	if err != nil {
		return nil, err
	}
	if !runtime.beginRequest() {
		return nil, errors.New("档案读取进程正在停止")
	}
	defer runtime.finishRequest()
	rpcCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	response, err := runtime.api.ListCaptureRecords(rpcCtx, &pluginv1.ListCaptureRecordsRequest{Date: date})
	if err != nil {
		return nil, err
	}
	return response.GetRecords(), nil
}

func (m *PluginManager) DownloadCaptureRecord(ctx context.Context, pluginID int64, date, instanceID, recordID string) ([]byte, error) {
	runtime, err := m.captureReadRuntime(ctx, pluginID)
	if err != nil {
		return nil, err
	}
	if !runtime.beginRequest() {
		return nil, errors.New("档案读取进程正在停止")
	}
	defer runtime.finishRequest()
	rpcCtx, cancel := context.WithTimeout(ctx, captureDownloadTimeout)
	defer cancel()
	response, err := runtime.api.DownloadCaptureRecord(rpcCtx, &pluginv1.DownloadCaptureRecordRequest{
		Date: date, InstanceId: instanceID, RecordId: recordID,
	})
	if err != nil {
		return nil, err
	}
	return response.GetRecordJson(), nil
}

func isCaptureProbeTraffic(ctx context.Context) bool {
	if pluginv1.IsProbeTraffic(ctx) {
		return true
	}
	outgoing, _ := metadata.FromOutgoingContext(ctx)
	return pluginv1.IsProbeTraffic(metadata.NewIncomingContext(ctx, outgoing))
}

func captureHasError(value gjson.Result) bool {
	err := value.Get("error")
	return err.Exists() && err.Type != gjson.Null
}
