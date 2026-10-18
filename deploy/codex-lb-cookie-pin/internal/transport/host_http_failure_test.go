package transport

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	pluginv1 "github.com/liyunlong/sub2api-cookie-plugin/pkg/pluginapi/v1"
	"google.golang.org/grpc"
)

func TestHostHTTPFailureCannotGrantReplay(t *testing.T) {
	code := pluginv1.HostHTTPFailureCode(&net.DNSError{Err: "opaque"}, nil, false)
	for _, sent := range []bool{false, true} {
		err := newHostHTTPError(sent, code)
		class, phase := classifyUpstreamFailure(err, context.Canceled)
		if class != "dns_failure" || phase != "dns" {
			t.Fatalf("lost finite host error: %s/%s", class, phase)
		}
		if hostHTTPNotSent(err) == sent {
			t.Fatal("RequestSent was changed by diagnostic classification")
		}
		if (&upstreamFailureTrace{}).definitelyNotConnected(err) {
			t.Fatal("remote error classification was treated as local dispatch evidence")
		}
	}
}

func TestHostHTTPFailureDropsUnknownCode(t *testing.T) {
	err := newHostHTTPError(true, "opaque-untrusted-code")
	class, phase := classifyUpstreamFailure(err, nil)
	if err.code != "" || class != "connection_error" || phase != "unknown" ||
		hostHTTPNotSent(err) || strings.Contains(err.Error(), "opaque") {
		t.Fatal("unknown host code escaped or altered dispatch policy")
	}
}

type hostFailureTestStream struct {
	grpc.ClientStream
	frames []*pluginv1.ForwardResponse
	closed chan struct{}
}

func (s *hostFailureTestStream) Recv() (*pluginv1.ForwardResponse, error) {
	if len(s.frames) == 0 {
		return nil, io.EOF
	}
	frame := s.frames[0]
	s.frames = s.frames[1:]
	return frame, nil
}

func (s *hostFailureTestStream) Send(*pluginv1.ForwardRequest) error { return nil }

func (s *hostFailureTestStream) CloseSend() error {
	if s.closed != nil {
		close(s.closed)
	}
	return nil
}

type hostFailureTestAPI struct {
	pluginv1.TransportPluginClient
	stream pluginv1.TransportPlugin_ForwardClient
}

func (api hostFailureTestAPI) Forward(context.Context, ...grpc.CallOption) (pluginv1.TransportPlugin_ForwardClient, error) {
	return api.stream, nil
}

func TestHostHTTPClientPreservesFailureCodeAndDispatch(t *testing.T) {
	code := pluginv1.HostHTTPFailureCode(&net.OpError{Op: "dial", Err: context.DeadlineExceeded}, nil, false)
	for _, sent := range []bool{false, true} {
		stream := &hostFailureTestStream{
			closed: make(chan struct{}),
			frames: []*pluginv1.ForwardResponse{
				{Frame: &pluginv1.ForwardResponse_Error{Error: &pluginv1.ForwardResponseError{
					Code: code, Message: "opaque-untrusted-message", RequestSent: sent,
				}}},
			},
		}
		client := &hostHTTPClient{api: hostFailureTestAPI{stream: stream}}
		request, err := http.NewRequest(http.MethodPost, "https://fixture.invalid/", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		<-stream.closed
		class, phase := classifyUpstreamFailure(err, nil)
		if response != nil || err == nil || hostHTTPNotSent(err) == sent ||
			class != "network_timeout" || phase != "connect" ||
			strings.Contains(err.Error(), "opaque") {
			t.Fatal("host RPC failure lost diagnostics or altered dispatch certainty")
		}
	}
}

func TestHostHTTPBodyFailureKeepsPartialOutputBarrier(t *testing.T) {
	for _, code := range []string{
		pluginv1.HostHTTPFailureCode(io.ErrUnexpectedEOF, nil, true),
		"HOST_HTTP_STREAM", "opaque-untrusted-code",
	} {
		ctx, cancel := context.WithCancel(context.Background())
		body := &hostHTTPBody{
			cancel: cancel,
			stream: &hostFailureTestStream{frames: []*pluginv1.ForwardResponse{
				{Frame: &pluginv1.ForwardResponse_BodyChunk{BodyChunk: []byte("partial")}},
				{Frame: &pluginv1.ForwardResponse_Error{Error: &pluginv1.ForwardResponseError{
					Code: code, RequestSent: false,
				}}},
			}},
		}
		p := make([]byte, 32)
		n, err := body.Read(p)
		if err != nil || string(p[:n]) != "partial" {
			t.Fatal("already delivered body was lost")
		}
		n, err = body.Read(p)
		var host *hostHTTPError
		if n != 0 || !errors.As(err, &host) || !host.sent || hostHTTPNotSent(err) {
			t.Fatal("late false RequestSent permitted replay of partial output")
		}
		if ctx.Err() != context.Canceled {
			t.Fatal("interrupted body did not release its RPC")
		}
		if strings.Contains(err.Error(), "opaque") {
			t.Fatal("untrusted error code escaped")
		}
	}
}

func TestHostHTTPBodyNilErrorFrameFailsClosed(t *testing.T) {
	body := &hostHTTPBody{
		cancel: func() {},
		stream: &hostFailureTestStream{frames: []*pluginv1.ForwardResponse{
			{Frame: &pluginv1.ForwardResponse_Error{}},
		}},
	}
	n, err := body.Read(make([]byte, 1))
	class, phase := classifyUpstreamFailure(err, nil)
	if n != 0 || err == nil || hostHTTPNotSent(err) ||
		class != "host_rpc_failure" || phase != "host_rpc" {
		t.Fatal("malformed error frame was not retained as a sent protocol failure")
	}
}
