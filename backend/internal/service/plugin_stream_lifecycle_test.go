package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type lifecycleStream struct {
	grpc.ClientStream
	ctx       context.Context
	frames    []*pluginv1.ForwardResponse
	sendError error
}

func (s *lifecycleStream) Context() context.Context { return s.ctx }
func (s *lifecycleStream) Send(*pluginv1.ForwardRequest) error {
	if s.sendError != nil {
		return s.sendError
	}
	return s.ctx.Err()
}
func (s *lifecycleStream) CloseSend() error { return s.ctx.Err() }
func (s *lifecycleStream) Recv() (*pluginv1.ForwardResponse, error) {
	if len(s.frames) == 0 {
		return nil, io.EOF
	}
	frame := s.frames[0]
	s.frames = s.frames[1:]
	return frame, nil
}

type lifecyclePluginClient struct {
	pluginv1.TransportPluginClient
	stream       *lifecycleStream
	forwardError error
}

func (c lifecyclePluginClient) Forward(ctx context.Context, _ ...grpc.CallOption) (pluginv1.TransportPlugin_ForwardClient, error) {
	if c.forwardError != nil {
		return nil, c.forwardError
	}
	c.stream.ctx = ctx
	return c.stream, nil
}

type lifecycleRequestBody struct {
	io.Reader
	closed atomic.Int32
}

func (b *lifecycleRequestBody) Close() error {
	b.closed.Add(1)
	return nil
}

func TestPluginEarlyFailureClosesRequestBody(t *testing.T) {
	failure := errors.New("fixture stream failure")
	for _, createFailed := range []bool{true, false} {
		t.Run(map[bool]string{true: "stream_creation", false: "metadata_send"}[createFailed], func(t *testing.T) {
			client := lifecyclePluginClient{stream: &lifecycleStream{sendError: failure}}
			if createFailed {
				client.forwardError = failure
			}
			runtime := &pluginRuntime{api: client, done: make(chan struct{})}
			body := &lifecycleRequestBody{Reader: strings.NewReader("fixture")}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://fixture.invalid/", body)
			require.NoError(t, err)
			response, err := runtime.roundTrip(t.Context(), request, "", &Account{ID: 42})
			require.Nil(t, response)
			var transportError *PluginTransportError
			require.ErrorAs(t, err, &transportError)
			require.Equal(t, !createFailed, transportError.RequestSent)
			require.EqualValues(t, 1, body.closed.Load())
		})
	}
}

func TestPluginHostBindingFailureClosesBodyWithoutDispatch(t *testing.T) {
	host, account, _, _, _ := hostHTTPFixture(t)
	body := &lifecycleRequestBody{Reader: strings.NewReader("fixture")}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://fixture.invalid/", body)
	require.NoError(t, err)
	// The account requires its assigned proxy; an empty route must fail before
	// the nil plugin client can be called.
	runtime := &pluginRuntime{hostHTTP: host, done: make(chan struct{})}
	response, err := runtime.roundTrip(t.Context(), request, "", account)
	require.Nil(t, response)
	require.ErrorIs(t, err, errPluginRuntimeUnavailable)
	require.ErrorIs(t, err, errPluginHTTPBinding)
	require.EqualValues(t, 1, body.closed.Load())
}

func TestPluginResponseRequiresExplicitEnd(t *testing.T) {
	for _, explicitEnd := range []bool{false, true} {
		t.Run(map[bool]string{false: "truncated", true: "complete"}[explicitEnd], func(t *testing.T) {
			frames := []*pluginv1.ForwardResponse{{
				Frame: &pluginv1.ForwardResponse_BodyChunk{BodyChunk: []byte("partial output")},
			}}
			if explicitEnd {
				frames = append(frames, &pluginv1.ForwardResponse{
					Frame: &pluginv1.ForwardResponse_End{End: &pluginv1.ForwardResponseEnd{}},
				})
			}
			stream := &lifecycleStream{ctx: t.Context(), frames: frames}
			reader, writer := io.Pipe()
			defer reader.Close()
			done := make(chan struct{})
			go func() {
				defer close(done)
				receivePluginResponseBody(stream, writer, make(chan error), func() {})
			}()
			body, err := io.ReadAll(reader)
			require.Equal(t, "partial output", string(body))
			if explicitEnd {
				require.NoError(t, err)
			} else {
				var failure *PluginTransportError
				require.ErrorAs(t, err, &failure)
				require.Equal(t, "PLUGIN_STREAM_TRUNCATED", failure.Code)
				require.True(t, failure.RequestSent, "partial output must not gain replay permission")
			}
			<-done
		})
	}
}

func TestPluginRequestBodyCancellationUnblocksRead(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	reader, writer := io.Pipe()
	defer writer.Close()
	done := make(chan error, 1)
	go func() { done <- sendPluginRequestBody(&lifecycleStream{ctx: ctx}, reader) }()
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("canceled upload left its request body reader blocked")
	}
}

func TestPluginLateErrorCannotGrantReplay(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(map[bool]string{false: "headers_only", true: "partial_body"}[partial], func(t *testing.T) {
			var frames []*pluginv1.ForwardResponse
			if partial {
				frames = append(frames, &pluginv1.ForwardResponse{
					Frame: &pluginv1.ForwardResponse_BodyChunk{BodyChunk: []byte("output")},
				})
			}
			frames = append(frames, &pluginv1.ForwardResponse{
				Frame: &pluginv1.ForwardResponse_Error{Error: &pluginv1.ForwardResponseError{
					Code: "UPSTREAM_BODY", RequestSent: false,
				}},
			})
			reader, writer := io.Pipe()
			defer reader.Close()
			done := make(chan struct{})
			go func() {
				defer close(done)
				receivePluginResponseBody(&lifecycleStream{ctx: t.Context(), frames: frames}, writer, nil, func() {})
			}()
			body, err := io.ReadAll(reader)
			var failure *PluginTransportError
			require.ErrorAs(t, err, &failure)
			require.True(t, failure.RequestSent)
			require.Equal(t, "UPSTREAM_BODY", failure.Code)
			if partial {
				require.Equal(t, "output", string(body))
			}
			<-done
		})
	}
}

func TestPluginResponseBodyReleasesAtTerminalRead(t *testing.T) {
	for _, failure := range []error{nil, errors.New("fixture stream error")} {
		reader, writer := io.Pipe()
		var canceled, finished atomic.Int32
		body := &pluginResponseBody{
			reader: reader, cancel: func() { canceled.Add(1) }, done: func() { finished.Add(1) },
		}
		require.NoError(t, writer.CloseWithError(failure))
		_, err := io.ReadAll(body)
		if failure == nil {
			require.NoError(t, err)
		} else {
			require.ErrorIs(t, err, failure)
		}
		require.NoError(t, body.Close())
		require.EqualValues(t, 1, canceled.Load())
		require.EqualValues(t, 1, finished.Load())
	}
}

func TestPluginCanceledUnreadResponseReleasesRuntime(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	stream := &lifecycleStream{frames: []*pluginv1.ForwardResponse{
		{Frame: &pluginv1.ForwardResponse_Start{Start: &pluginv1.ForwardResponseStart{StatusCode: 200}}},
		{Frame: &pluginv1.ForwardResponse_BodyChunk{BodyChunk: []byte("unread output")}},
	}}
	runtime := &pluginRuntime{api: lifecyclePluginClient{stream: stream}, done: make(chan struct{})}
	require.True(t, runtime.beginRequest())
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://fixture.invalid/", nil)
	require.NoError(t, err)
	response, err := runtime.roundTrip(ctx, request, "", &Account{ID: 42, Concurrency: 1})
	require.NoError(t, err)
	require.EqualValues(t, 1, runtime.inFlight.Load())
	cancel()
	require.Eventually(t, func() bool { return runtime.inFlight.Load() == 0 }, time.Second, time.Millisecond)
	require.NoError(t, response.Body.Close())
	require.Zero(t, runtime.inFlight.Load(), "cancel plus Close must release exactly once")
}
