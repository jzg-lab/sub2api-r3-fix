package transport

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"

	pluginv1 "github.com/liyunlong/sub2api-cookie-plugin/pkg/pluginapi/v1"
	"google.golang.org/grpc/connectivity"
)

var errHostHTTPUnavailable = errors.New("host HTTP transport unavailable; direct fallback disabled")

type hostHTTPError struct {
	sent bool
	code string
}

func (e *hostHTTPError) Error() string { return "host HTTP transport rejected or interrupted" }

func newHostHTTPError(sent bool, code string) *hostHTTPError {
	if _, _, ok := pluginv1.DecodeHostHTTPFailureCode(code); !ok {
		code = ""
	}
	return &hostHTTPError{sent: sent, code: code}
}

func hostHTTPNotSent(err error) bool {
	var failure *hostHTTPError
	return errors.As(err, &failure) && !failure.sent
}

func (s *Server) RequireHostHTTP() {
	s.mu.Lock()
	s.hostHTTPRequired = true
	s.mu.Unlock()
}

func (s *Server) hostHTTPReady() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hostHTTPRequired && s.hostHTTP == nil {
		return true
	}
	return s.hostHTTP != nil && s.hostHTTPID != 0 &&
		(s.hostHTTPConn == nil || s.hostHTTPConn.GetState() != connectivity.Shutdown)
}

func (s *Server) connectHostHTTP(id uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hostHTTP != nil {
		if s.hostHTTPID == id && (s.hostHTTPConn == nil || s.hostHTTPConn.GetState() != connectivity.Shutdown) {
			return nil
		}
		return errHostHTTPUnavailable
	}
	if id == 0 || s.broker == nil {
		return errHostHTTPUnavailable
	}
	conn, err := s.broker.Dial(id)
	if err != nil {
		return errHostHTTPUnavailable
	}
	s.hostHTTP = pluginv1.NewTransportPluginClient(conn)
	s.hostHTTPConn, s.hostHTTPID = conn, id
	return nil
}

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// The standalone client remains for isolated test/embedded hosts only. A
// production process never retries a broken host binding through its own TLS.
func (s *Server) outboundClient(binding pluginv1.HostHTTPBinding, proxy string, accountID int64, concurrency int32) (httpDoer, error) {
	s.mu.Lock()
	required, client, id := s.hostHTTPRequired, s.hostHTTP, s.hostHTTPID
	s.mu.Unlock()
	if binding.BrokerID != 0 {
		if client == nil || id != binding.BrokerID {
			return nil, errHostHTTPUnavailable
		}
		return &hostHTTPClient{api: client, binding: binding, proxy: proxy, accountID: accountID, concurrency: concurrency}, nil
	}
	if required || client != nil {
		return nil, errHostHTTPUnavailable
	}
	return s.clientFor(proxy, accountID, concurrency)
}

type hostHTTPClient struct {
	api         pluginv1.TransportPluginClient
	binding     pluginv1.HostHTTPBinding
	proxy       string
	accountID   int64
	concurrency int32
}

func (c *hostHTTPClient) Do(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(req.Context())
	stream, err := c.api.Forward(pluginv1.WithHostHTTPBinding(ctx, c.binding))
	if err != nil {
		cancel()
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, newHostHTTPError(false, "HOST_HTTP_RPC_OPEN")
	}
	start := &pluginv1.ForwardRequestStart{
		Method: req.Method, Url: req.URL.String(), Host: req.Host,
		Headers: headersToPlugin(req.Header), ProxyUrl: c.proxy,
		AccountId: c.accountID, AccountConcurrency: c.concurrency,
		Platform: "openai", AccountType: "oauth", ContentLength: req.ContentLength,
		HasBody: req.Body != nil && req.Body != http.NoBody,
	}
	if err := stream.Send(&pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_Start{Start: start}}); err != nil {
		cancel()
		if req.Body != nil {
			_ = req.Body.Close()
		}
		// Send errors are ambiguous: the other side may have consumed the frame.
		return nil, newHostHTTPError(true, "HOST_HTTP_RPC_SEND")
	}
	go func() {
		if req.Body != nil {
			stop := context.AfterFunc(ctx, func() { _ = req.Body.Close() })
			defer stop()
			defer req.Body.Close()
		}
		buffer := make([]byte, 32*1024)
		if start.HasBody {
			for {
				n, readErr := req.Body.Read(buffer)
				if n > 0 {
					if err := stream.Send(&pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_BodyChunk{BodyChunk: append([]byte(nil), buffer[:n]...)}}); err != nil {
						cancel()
						return
					}
				}
				if readErr == io.EOF {
					break
				}
				if readErr != nil {
					cancel()
					return
				}
			}
		}
		if err := stream.Send(&pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_BodyEnd{BodyEnd: true}}); err != nil {
			cancel()
			return
		}
		_ = stream.CloseSend()
	}()
	frame, err := stream.Recv()
	if err != nil {
		cancel()
		return nil, newHostHTTPError(true, "HOST_HTTP_RPC_RECV")
	}
	if failure := frame.GetError(); failure != nil {
		cancel()
		return nil, newHostHTTPError(failure.RequestSent, failure.Code)
	}
	headers := frame.GetStart()
	if headers == nil || headers.StatusCode < 100 || headers.StatusCode > 599 {
		cancel()
		return nil, newHostHTTPError(true, "HOST_HTTP_PROTOCOL")
	}
	return &http.Response{
		StatusCode: int(headers.StatusCode), Status: headers.Status,
		Proto: headers.Protocol, ProtoMajor: int(headers.ProtocolMajor), ProtoMinor: int(headers.ProtocolMinor),
		Header: headersFromPlugin(headers.Headers), ContentLength: headers.ContentLength, Request: req,
		Body: &hostHTTPBody{stream: stream, cancel: cancel},
	}, nil
}

type hostHTTPBody struct {
	stream   pluginv1.TransportPlugin_ForwardClient
	cancel   context.CancelFunc
	once     sync.Once
	chunk    []byte
	done     bool
	received int64
}

func (b *hostHTTPBody) Close() error {
	b.once.Do(b.cancel)
	return nil
}

func (b *hostHTTPBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(b.chunk) == 0 && !b.done {
		frame, err := b.stream.Recv()
		if err != nil {
			b.done = true
			_ = b.Close()
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return 0, err
		}
		switch value := frame.Frame.(type) {
		case *pluginv1.ForwardResponse_BodyChunk:
			b.chunk = value.BodyChunk
			b.received += int64(len(value.BodyChunk))
		case *pluginv1.ForwardResponse_End:
			b.done = true
			_ = b.Close()
			if value.End == nil || value.End.BytesReceived != b.received {
				return 0, io.ErrUnexpectedEOF
			}
		case *pluginv1.ForwardResponse_Error:
			b.done = true
			_ = b.Close()
			if value.Error == nil {
				return 0, newHostHTTPError(true, "HOST_HTTP_PROTOCOL")
			}
			// Response bytes already started; never trust a late unsent flag.
			return 0, newHostHTTPError(true, value.Error.Code)
		default:
			b.done = true
			_ = b.Close()
			return 0, newHostHTTPError(true, "HOST_HTTP_PROTOCOL")
		}
	}
	if len(b.chunk) == 0 && b.done {
		return 0, io.EOF
	}
	n := copy(p, b.chunk)
	b.chunk = b.chunk[n:]
	return n, nil
}
