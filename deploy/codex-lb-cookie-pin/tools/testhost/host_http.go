package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	hcplugin "github.com/hashicorp/go-plugin"
	pluginv1 "github.com/liyunlong/sub2api-cookie-plugin/pkg/pluginapi/v1"
	"google.golang.org/grpc"
)

// This fixture is intentionally not a production host/TLS implementation.
// Its only permitted consumer is the single loopback mock created by runMock.
type fixtureHostHTTP struct {
	pluginv1.UnimplementedTransportPluginServer
	mu      sync.Mutex
	server  *grpc.Server
	binding pluginv1.HostHTTPBinding
	target  string
	account int64
}

func startFixtureHostHTTP(ctx context.Context, transport *pluginv1.TransportClient) (*fixtureHostHTTP, error) {
	if transport.Broker == nil {
		return nil, errors.New("test broker unavailable")
	}
	host := &fixtureHostHTTP{binding: pluginv1.HostHTTPBinding{
		BrokerID: transport.Broker.NextId(), Request: strings.Repeat("a", 64), Probe: strings.Repeat("b", 64),
	}}
	listener, err := transport.Broker.Accept(host.binding.BrokerID)
	if err != nil {
		return nil, err
	}
	host.server = hcplugin.DefaultGRPCServer(nil)
	pluginv1.RegisterTransportPluginServer(host.server, host)
	go func() { _ = host.server.Serve(listener) }()
	initCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	health, err := transport.Health(pluginv1.WithHostHTTPBroker(pluginv1.WithLivenessHealth(initCtx), host.binding.BrokerID), &pluginv1.HealthRequest{})
	if err != nil || health == nil || !health.Healthy {
		host.server.Stop()
		return nil, errors.New("loopback fixture host initialization failed")
	}
	return host, nil
}

func (h *fixtureHostHTTP) Forward(stream pluginv1.TransportPlugin_ForwardServer) error {
	binding, ok := pluginv1.ReadHostHTTPBinding(stream.Context())
	frame, err := stream.Recv()
	if err != nil {
		return err
	}
	start := frame.GetStart()
	h.mu.Lock()
	target, account := h.target, h.account
	h.mu.Unlock()
	parsed, parseErr := url.Parse(target)
	if !ok || binding != h.binding || start == nil || target == "" || parseErr != nil ||
		parsed.Scheme != "http" || !net.ParseIP(parsed.Hostname()).IsLoopback() ||
		start.Url != target || start.AccountId != account || start.Method != http.MethodPost ||
		start.Host != parsed.Host || start.ProxyUrl != "" {
		return errors.New("mock host rejects non-fixture route")
	}
	var body bytes.Buffer
	for {
		frame, err := stream.Recv()
		if err != nil {
			return err
		}
		if frame.GetBodyEnd() {
			break
		}
		chunk, ok := frame.Frame.(*pluginv1.ForwardRequest_BodyChunk)
		if !ok || body.Len()+len(chunk.BodyChunk) > 1024*1024 {
			return errors.New("invalid mock body")
		}
		_, _ = body.Write(chunk.BodyChunk)
	}
	req, err := http.NewRequestWithContext(stream.Context(), start.Method, target, &body)
	if err != nil {
		return err
	}
	req.Host = start.Host
	for name, values := range start.Headers {
		req.Header[name] = append([]string(nil), values.GetValues()...)
	}
	localTransport := &http.Transport{Proxy: nil}
	defer localTransport.CloseIdleConnections()
	client := &http.Client{Transport: localTransport, Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	headers := make(map[string]*pluginv1.HeaderValues)
	for name, values := range response.Header {
		headers[name] = &pluginv1.HeaderValues{Values: values}
	}
	if err := stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_Start{
		Start: &pluginv1.ForwardResponseStart{StatusCode: int32(response.StatusCode), Headers: headers},
	}}); err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
	if err != nil || len(data) > 1024*1024 {
		return errors.New("invalid mock response")
	}
	if len(data) > 0 {
		if err := stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_BodyChunk{BodyChunk: data}}); err != nil {
			return err
		}
	}
	return stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_End{
		End: &pluginv1.ForwardResponseEnd{BytesReceived: int64(len(data))},
	}})
}
