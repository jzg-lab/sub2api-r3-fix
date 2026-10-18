package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	hclog "github.com/hashicorp/go-hclog"
	hcplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var errPluginRuntimeUnavailable = errors.New("OpenAI OAuth 插件不可用")

type pluginRuntime struct {
	installation      *PluginInstallation
	client            *hcplugin.Client
	api               pluginv1.TransportPluginClient
	broker            *hcplugin.GRPCBroker
	capabilities      []string
	hostHTTP          *pluginHostHTTP
	hostHTTPServer    *grpc.Server
	inFlight          atomic.Int64
	draining          atomic.Bool
	healthUnavailable atomic.Bool
	// Only the serialized reconciler owns this timestamp.
	healthFailureSince     time.Time
	done                   chan struct{}
	doneOnce               sync.Once
	pausedAccountIDsSource func(context.Context) ([]int64, error)
	pausedAccountIDs       []int64
	probeBaseConfig        []byte
	probeAccountsMu        sync.Mutex
	probeAccounts          map[int64]time.Time
}

func startPluginRuntime(ctx context.Context, installation *PluginInstallation, startTimeout time.Duration, socketDir string, hostServices ...pluginv1.HostServiceServer) (*pluginRuntime, error) {
	if installation == nil {
		return nil, errors.New("插件安装记录为空")
	}
	checksum, err := hex.DecodeString(installation.BinarySHA256)
	if err != nil || len(checksum) != sha256.Size {
		return nil, errors.New("插件二进制哈希无效")
	}
	cmd := exec.CommandContext(context.WithoutCancel(ctx), installation.BinaryPath)
	if installation.PluginKey == "lyunlong.codex.lb-cookie-pin" {
		dir, err := pluginEvidenceDir(socketDir, installation.PluginKey)
		if err != nil {
			return nil, fmt.Errorf("prepare plugin evidence: %w", err)
		}
		cmd.Env = []string{"SUB2API_PLUGIN_STATE_DIR=" + dir}
	}
	observations := &pluginEvidenceLog{logger: slog.Default()}
	client := hcplugin.NewClient(&hcplugin.ClientConfig{
		HandshakeConfig:  pluginv1.HandshakeConfig,
		Plugins:          pluginv1.ClientPluginMap(),
		Cmd:              cmd,
		AllowedProtocols: []hcplugin.Protocol{hcplugin.ProtocolGRPC},
		StartTimeout:     startTimeout,
		SecureConfig: &hcplugin.SecureConfig{
			Checksum: checksum,
			Hash:     sha256.New(),
		},
		Logger:           hclog.NewNullLogger(),
		SyncStdout:       io.Discard,
		SyncStderr:       &pluginEvidenceLog{logger: slog.Default()},
		Stderr:           observations,
		UnixSocketConfig: &hcplugin.UnixSocketConfig{TempDir: socketDir},
		SkipHostEnv:      true,
	})
	rpcClient, err := client.Client()
	if err != nil {
		client.Kill()
		return nil, fmt.Errorf("启动插件进程: %w", err)
	}
	dispensed, err := rpcClient.Dispense(pluginv1.TransportPluginName)
	if err != nil {
		client.Kill()
		return nil, fmt.Errorf("获取插件传输能力: %w", err)
	}
	api, ok := dispensed.(pluginv1.TransportPluginClient)
	if !ok {
		client.Kill()
		return nil, errors.New("插件未实现传输 gRPC 客户端")
	}
	runtime := &pluginRuntime{
		installation: installation,
		client:       client,
		api:          api,
		done:         make(chan struct{}),
	}
	if transport, ok := dispensed.(*pluginv1.TransportClient); ok {
		runtime.broker = transport.Broker
	}
	infoCtx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	info, err := api.GetInfo(infoCtx, &pluginv1.GetInfoRequest{})
	if err != nil {
		runtime.kill()
		return nil, fmt.Errorf("读取插件信息: %w", err)
	}
	if info.PluginId != installation.PluginKey || info.PluginVersion != installation.Version ||
		info.ProtocolVersion != pluginv1.ProtocolVersion || info.TransportApiVersion != pluginv1.TransportAPIVersion {
		runtime.kill()
		return nil, errors.New("插件运行时信息与已校验清单不一致")
	}
	runtime.capabilities = append([]string(nil), info.Capabilities...)
	if err := runtime.checkHealth(infoCtx); err != nil {
		runtime.kill()
		return nil, err
	}
	// HostService is optional and additive. Older plugins return Unimplemented;
	// that must not disable the transport or the newer HostHTTP channel.
	var hostService pluginv1.HostServiceServer
	if len(hostServices) > 0 {
		hostService = hostServices[0]
	}
	offerPluginHostServices(ctx, installation, api, runtime.broker, hostService, startTimeout)
	return runtime, nil
}

// offerPluginHostServices exposes the per-plugin KV/account bridge over the
// go-plugin broker. Failure is best effort for compatibility with old plugins.
func offerPluginHostServices(
	ctx context.Context,
	installation *PluginInstallation,
	api pluginv1.TransportPluginClient,
	broker *hcplugin.GRPCBroker,
	hostServices pluginv1.HostServiceServer,
	startTimeout time.Duration,
) {
	if broker == nil || hostServices == nil || api == nil {
		return
	}
	brokerID := broker.NextId()
	go broker.AcceptAndServe(brokerID, func(opts []grpc.ServerOption) *grpc.Server {
		server := grpc.NewServer(opts...)
		pluginv1.RegisterHostServiceServer(server, hostServices)
		return server
	})
	initCtx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	resp, err := api.InitHostServices(initCtx, &pluginv1.InitHostServicesRequest{
		HostServiceId:         brokerID,
		HostServiceApiVersion: pluginv1.HostServiceAPIVersion,
	})
	pluginKey := ""
	if installation != nil {
		pluginKey = installation.PluginKey
	}
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			slog.Debug("plugin_host_services_unimplemented", "plugin", pluginKey)
		} else {
			slog.Warn("plugin_host_services_init_failed", "plugin", pluginKey, "error", err)
		}
		return
	}
	if resp != nil && !resp.Ready {
		slog.Debug("plugin_host_services_declined", "plugin", pluginKey, "message", resp.Message)
	}
}

func (r *pluginRuntime) validateAndApplyConfig(ctx context.Context, configJSON []byte) error {
	_, err := r.validateAndApplyNormalizedConfig(ctx, configJSON)
	return err
}

func (r *pluginRuntime) validateAndApplyNormalizedConfig(ctx context.Context, configJSON []byte) ([]byte, error) {
	pausedIDs, err := r.loadPausedAccountIDs(ctx)
	if err != nil {
		return nil, err
	}
	if r.pausedAccountIDsSource != nil {
		configJSON, err = withPluginProbePauses(configJSON, pausedIDs)
		if err != nil {
			return nil, err
		}
	}
	validation, err := r.api.ValidateConfig(ctx, &pluginv1.ValidateConfigRequest{ConfigJson: configJSON})
	if err != nil {
		return nil, fmt.Errorf("插件配置校验失败: %w", err)
	}
	if !validation.Valid {
		return nil, fmt.Errorf("插件配置无效: %s", validation.Message)
	}
	if len(validation.NormalizedConfigJson) > 0 {
		configJSON = validation.NormalizedConfigJson
	}
	if len(configJSON) == 0 || len(configJSON) > pluginConfigMaxBytes || !json.Valid(configJSON) {
		return nil, errors.New("插件返回的规范化配置不是有效且大小受限的 JSON")
	}
	var normalized any
	if err := json.Unmarshal(configJSON, &normalized); err != nil {
		return nil, fmt.Errorf("解析插件规范化配置: %w", err)
	}
	if normalized == nil {
		return nil, errors.New("插件返回的规范化配置根节点必须是对象")
	}
	if _, ok := normalized.(map[string]any); !ok {
		return nil, errors.New("插件返回的规范化配置根节点必须是对象")
	}
	configJSON, err = json.Marshal(normalized)
	if err != nil {
		return nil, fmt.Errorf("序列化插件规范化配置: %w", err)
	}
	if r.pausedAccountIDsSource != nil {
		configJSON, err = withPluginProbePauses(configJSON, pausedIDs)
		if err != nil {
			return nil, err
		}
	}
	applied, err := r.api.ApplyConfig(ctx, &pluginv1.ApplyConfigRequest{ConfigJson: configJSON})
	if err != nil {
		return nil, fmt.Errorf("应用插件配置失败: %w", err)
	}
	if !applied.Applied {
		return nil, fmt.Errorf("插件拒绝应用配置: %s", applied.Message)
	}
	if r.pausedAccountIDsSource != nil {
		configJSON, err = withPluginProbePauses(configJSON, nil)
		if err != nil {
			return nil, err
		}
		r.pausedAccountIDs = pausedIDs
		r.probeBaseConfig, err = withoutPluginDropAction(configJSON)
		if err != nil {
			return nil, err
		}
	}
	return configJSON, nil
}

func (r *pluginRuntime) checkHealth(ctx context.Context) error {
	if r == nil || r.api == nil || r.client == nil || r.client.Exited() {
		return errors.New("插件进程已退出")
	}
	health, err := r.api.Health(pluginv1.WithLivenessHealth(ctx), &pluginv1.HealthRequest{})
	if err != nil {
		return fmt.Errorf("插件健康检查失败: %w", err)
	}
	if health == nil || !health.Healthy {
		message := "插件报告不健康"
		if health != nil && strings.TrimSpace(health.Message) != "" {
			message = "插件不健康: " + health.Message
		}
		return errors.New(message)
	}
	return nil
}

func (r *pluginRuntime) beginRequest() bool {
	if r == nil || r.draining.Load() || r.healthUnavailable.Load() {
		return false
	}
	r.inFlight.Add(1)
	if r.draining.Load() || r.healthUnavailable.Load() {
		r.finishRequest()
		return false
	}
	return true
}

func (r *pluginRuntime) finishRequest() {
	if r.inFlight.Add(-1) == 0 && r.draining.Load() {
		r.doneOnce.Do(func() { close(r.done) })
	}
}

func (r *pluginRuntime) drain(timeout time.Duration) {
	if r == nil {
		return
	}
	r.draining.Store(true)
	if r.inFlight.Load() == 0 {
		r.doneOnce.Do(func() { close(r.done) })
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-r.done:
	case <-timer.C:
	}
	r.kill()
}

func (r *pluginRuntime) kill() {
	if r != nil && r.hostHTTPServer != nil {
		r.hostHTTPServer.Stop()
	}
	if r != nil && r.client != nil {
		r.client.Kill()
	}
}

func (r *pluginRuntime) roundTrip(ctx context.Context, request *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	if request == nil || request.URL == nil || account == nil {
		return nil, errors.New("插件出站请求参数不完整")
	}
	bodyDelegated := false
	defer func() {
		if !bodyDelegated && request.Body != nil {
			_ = request.Body.Close()
		}
	}()
	r.trackProbeAccount(account.ID, time.Now())
	streamCtx, cancel := context.WithCancel(ctx)
	if r.hostHTTP != nil {
		binding, err := r.hostHTTP.bind(streamCtx, request, proxyURL, account)
		if err != nil {
			cancel()
			return nil, errors.Join(errPluginRuntimeUnavailable, err)
		}
		streamCtx = pluginv1.WithHostHTTPBinding(streamCtx, binding)
	}
	stream, err := r.api.Forward(streamCtx)
	if err != nil {
		cancel()
		return nil, normalizePluginRPCError(ctx, "创建插件转发流", err, false)
	}
	requestID := strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.FormatInt(account.ID, 36)
	if err := stream.Send(&pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_Start{Start: &pluginv1.ForwardRequestStart{
		RequestId:          requestID,
		Method:             request.Method,
		Url:                request.URL.String(),
		Host:               request.Host,
		Headers:            headersToPlugin(request.Header),
		ProxyUrl:           proxyURL,
		AccountId:          account.ID,
		AccountConcurrency: int32(account.Concurrency),
		Platform:           account.Platform,
		AccountType:        account.Type,
		ContentLength:      request.ContentLength,
		HasBody:            request.Body != nil && request.Body != http.NoBody,
	}}}); err != nil {
		cancel()
		// gRPC Send 返回错误时无法证明服务端没有收到元数据，必须禁止自动重放。
		return nil, normalizePluginRPCError(ctx, "发送插件请求元数据", err, true)
	}
	sendErr := make(chan error, 1)
	bodyDelegated = true
	go func() {
		err := sendPluginRequestBody(stream, request.Body)
		sendErr <- err
		if err != nil {
			cancel()
		}
	}()

	first, err := stream.Recv()
	if err != nil {
		cancel()
		select {
		case bodyErr := <-sendErr:
			if bodyErr != nil {
				return nil, normalizePluginRPCError(ctx, "发送插件请求体", bodyErr, true)
			}
		default:
		}
		return nil, normalizePluginRPCError(ctx, "接收插件响应头", err, true)
	}
	if frameError := first.GetError(); frameError != nil {
		cancel()
		return nil, &PluginTransportError{Code: frameError.Code, Message: frameError.Message, RequestSent: frameError.RequestSent}
	}
	start := first.GetStart()
	if start == nil || start.StatusCode < 100 || start.StatusCode > 599 {
		cancel()
		return nil, &PluginTransportError{
			Code:        "PLUGIN_INVALID_RESPONSE",
			Message:     "插件未返回有效的 HTTP 响应头",
			RequestSent: true,
		}
	}
	pipeReader, pipeWriter := io.Pipe()
	done := sync.OnceFunc(r.finishRequest)
	stop := context.AfterFunc(streamCtx, func() {
		_ = pipeReader.CloseWithError(streamCtx.Err())
		done()
	})
	body := &pluginResponseBody{
		reader: pipeReader,
		cancel: func() { stop(); cancel() },
		done:   done,
	}
	go receivePluginResponseBody(stream, pipeWriter, sendErr, cancel)
	return &http.Response{
		Status:        start.Status,
		StatusCode:    int(start.StatusCode),
		Proto:         start.Protocol,
		ProtoMajor:    int(start.ProtocolMajor),
		ProtoMinor:    int(start.ProtocolMinor),
		Header:        headersFromPlugin(start.Headers),
		Body:          body,
		ContentLength: start.ContentLength,
		Request:       request,
	}, nil
}

type PluginTransportError struct {
	Code        string
	Message     string
	RequestSent bool
}

func (e *PluginTransportError) Is(target error) bool {
	return e != nil && target == errPluginRuntimeUnavailable && e.Code == "PLUGIN_HOST_TRANSPORT"
}

func (e *PluginTransportError) Error() string {
	if e == nil {
		return "插件传输失败"
	}
	code := strings.Map(func(value rune) rune {
		if value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9' || value == '_' || value == '-' || value == '.' {
			return value
		}
		return -1
	}, e.Code)
	if len(code) > 64 {
		code = code[:64]
	}
	return fmt.Sprintf("插件传输失败 [%s]: %s", code, sanitizeUpstreamErrorMessage(e.Message))
}

func normalizePluginRPCError(ctx context.Context, operation string, err error, requestMayHaveBeenSent bool) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return &PluginTransportError{
		Code:        "PLUGIN_RPC_ERROR",
		Message:     fmt.Sprintf("%s: %v", operation, err),
		RequestSent: requestMayHaveBeenSent,
	}
}

func sendPluginRequestBody(stream pluginv1.TransportPlugin_ForwardClient, body io.ReadCloser) error {
	if body != nil {
		closeBody := sync.OnceFunc(func() { _ = body.Close() })
		stop := context.AfterFunc(stream.Context(), closeBody)
		defer stop()
		defer closeBody()
		buffer := make([]byte, 32*1024)
		for {
			read, err := body.Read(buffer)
			if read > 0 {
				chunk := append([]byte(nil), buffer[:read]...)
				if sendErr := stream.Send(&pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_BodyChunk{BodyChunk: chunk}}); sendErr != nil {
					return sendErr
				}
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
		}
	}
	if err := stream.Send(&pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_BodyEnd{BodyEnd: true}}); err != nil {
		return err
	}
	return stream.CloseSend()
}

const pluginResponseSettlementTimeout = 10 * time.Second

func drainPluginResponseSettlement(body io.ReadCloser, timeout time.Duration) error {
	timer := time.AfterFunc(timeout, func() { _ = body.Close() })
	defer timer.Stop()
	_, err := io.CopyN(io.Discard, body, 1<<20)
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return errors.New("plugin response settlement tail exceeds limit")
	}
	return err
}

func receivePluginResponseBody(stream pluginv1.TransportPlugin_ForwardClient, writer *io.PipeWriter, sendErr <-chan error, cancel context.CancelFunc) {
	defer func() { _ = writer.Close() }()
	ended := false
	var settlementTimer *time.Timer
	defer func() {
		if settlementTimer != nil {
			settlementTimer.Stop()
		}
	}()
	for {
		frame, err := stream.Recv()
		if err == io.EOF {
			if ended {
				select {
				case bodyErr := <-sendErr:
					if bodyErr != nil {
						_ = writer.CloseWithError(normalizePluginRPCError(stream.Context(), "发送插件请求体", bodyErr, true))
					}
				default:
				}
				return
			}
			_ = writer.CloseWithError(&PluginTransportError{
				Code: "PLUGIN_STREAM_TRUNCATED", Message: "插件流未返回完成帧", RequestSent: true,
			})
			return
		}
		if err != nil {
			_ = writer.CloseWithError(normalizePluginRPCError(stream.Context(), "接收插件响应体", err, true))
			return
		}
		if frameError := frame.GetError(); frameError != nil {
			// Response headers already prove dispatch; a late plugin error
			// cannot grant permission to replay the upstream request.
			_ = writer.CloseWithError(&PluginTransportError{Code: frameError.Code, Message: frameError.Message, RequestSent: true})
			return
		}
		if ended {
			_ = writer.CloseWithError(&PluginTransportError{
				Code: "PLUGIN_INVALID_RESPONSE", Message: "插件完成帧后返回了额外响应帧", RequestSent: true,
			})
			return
		}
		if chunk := frame.GetBodyChunk(); len(chunk) > 0 {
			if _, err := writer.Write(chunk); err != nil {
				return
			}
			continue
		}
		if frame.GetEnd() != nil {
			// The server may still be persisting its outcome after Send(End).
			// Exposing EOF now cancels the RPC through pluginResponseBody.Close.
			// Wait for the RPC terminal so settlement errors remain observable.
			ended = true
			settlementTimer = time.AfterFunc(pluginResponseSettlementTimeout, func() {
				_ = writer.CloseWithError(&PluginTransportError{
					Code: "PLUGIN_SETTLEMENT_TIMEOUT", Message: "插件完成确认超时", RequestSent: true,
				})
				cancel()
			})
		}
	}
}

type pluginResponseBody struct {
	reader *io.PipeReader
	cancel context.CancelFunc
	done   func()
	once   sync.Once
}

func (b *pluginResponseBody) Read(data []byte) (int, error) {
	n, err := b.reader.Read(data)
	if err != nil {
		_ = b.Close()
	}
	return n, err
}

func (b *pluginResponseBody) Close() error {
	var err error
	b.once.Do(func() {
		b.cancel()
		err = b.reader.Close()
		b.done()
	})
	return err
}

func headersToPlugin(headers http.Header) map[string]*pluginv1.HeaderValues {
	out := make(map[string]*pluginv1.HeaderValues, len(headers))
	for key, values := range headers {
		out[key] = &pluginv1.HeaderValues{Values: append([]string(nil), values...)}
	}
	return out
}

func headersFromPlugin(headers map[string]*pluginv1.HeaderValues) http.Header {
	out := make(http.Header, len(headers))
	for key, values := range headers {
		if values != nil {
			out[key] = append([]string(nil), values.Values...)
		}
	}
	return out
}
