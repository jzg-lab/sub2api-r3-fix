package service

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/stretchr/testify/require"
)

type settlingPluginStream struct {
	lifecycleStream
	endSent chan struct{}
	settled chan struct{}
	tailErr error
}

func (s *settlingPluginStream) Recv() (*pluginv1.ForwardResponse, error) {
	if len(s.frames) > 0 {
		frame, err := s.lifecycleStream.Recv()
		if frame.GetEnd() != nil {
			close(s.endSent)
		}
		return frame, err
	}
	select {
	case <-s.settled:
		return nil, s.tailErr
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
}

func TestPluginResponseWaitsForServerSettlement(t *testing.T) {
	for _, tailErr := range []error{io.EOF, errors.New("fixture settlement failure")} {
		t.Run(tailErr.Error(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			stream := &settlingPluginStream{
				lifecycleStream: lifecycleStream{ctx: ctx, frames: []*pluginv1.ForwardResponse{
					{Frame: &pluginv1.ForwardResponse_BodyChunk{BodyChunk: []byte("completed response")}},
					{Frame: &pluginv1.ForwardResponse_End{End: &pluginv1.ForwardResponseEnd{}}},
				}},
				endSent: make(chan struct{}), settled: make(chan struct{}), tailErr: tailErr,
			}
			reader, writer := io.Pipe()
			body := &pluginResponseBody{reader: reader, cancel: cancel, done: func() {}}
			defer body.Close()
			go receivePluginResponseBody(stream, writer, make(chan error), cancel)
			readDone := make(chan error, 1)
			go func() {
				_, err := io.ReadAll(body)
				readDone <- err
			}()
			<-stream.endSent
			var early bool
			select {
			case <-readDone:
				early = true
			case <-time.After(30 * time.Millisecond):
			}
			require.NoError(t, ctx.Err(), "host must not cancel while the plugin settles its evidence")
			close(stream.settled)
			require.False(t, early, "transport End is not the server RPC terminal")
			if tailErr == io.EOF {
				require.NoError(t, <-readDone)
			} else {
				var failure *PluginTransportError
				require.ErrorAs(t, <-readDone, &failure)
				require.True(t, failure.RequestSent, "late settlement failure must not authorize replay")
			}
		})
	}
}

func TestPluginResponseRejectsFramesAfterEnd(t *testing.T) {
	for name, frame := range map[string]*pluginv1.ForwardResponse{
		"body": {Frame: &pluginv1.ForwardResponse_BodyChunk{BodyChunk: []byte("late output")}},
		"end":  {Frame: &pluginv1.ForwardResponse_End{End: &pluginv1.ForwardResponseEnd{}}},
		"error": {Frame: &pluginv1.ForwardResponse_Error{Error: &pluginv1.ForwardResponseError{
			Code: "SETTLEMENT_FAILED", RequestSent: false,
		}}},
	} {
		t.Run(name, func(t *testing.T) {
			reader, writer := io.Pipe()
			defer reader.Close()
			stream := &lifecycleStream{ctx: t.Context(), frames: []*pluginv1.ForwardResponse{
				{Frame: &pluginv1.ForwardResponse_End{End: &pluginv1.ForwardResponseEnd{}}},
				frame,
			}}
			go receivePluginResponseBody(stream, writer, nil, func() {})
			body, err := io.ReadAll(reader)
			require.Empty(t, body)
			var failure *PluginTransportError
			require.ErrorAs(t, err, &failure)
			require.True(t, failure.RequestSent)
			if name == "error" {
				require.Equal(t, "SETTLEMENT_FAILED", failure.Code)
			} else {
				require.Equal(t, "PLUGIN_INVALID_RESPONSE", failure.Code)
			}
		})
	}
}

func TestPluginResponseSettlementCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stream := &settlingPluginStream{
		lifecycleStream: lifecycleStream{ctx: ctx, frames: []*pluginv1.ForwardResponse{
			{Frame: &pluginv1.ForwardResponse_End{End: &pluginv1.ForwardResponseEnd{}}},
		}},
		endSent: make(chan struct{}), settled: make(chan struct{}),
	}
	reader, writer := io.Pipe()
	defer reader.Close()
	done := make(chan error, 1)
	go receivePluginResponseBody(stream, writer, nil, cancel)
	go func() { _, err := io.ReadAll(reader); done <- err }()
	<-stream.endSent
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("cancellation did not release settlement wait")
	}
}

func TestPluginResponseSettlementHasDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stream := &settlingPluginStream{
		lifecycleStream: lifecycleStream{ctx: ctx, frames: []*pluginv1.ForwardResponse{
			{Frame: &pluginv1.ForwardResponse_End{End: &pluginv1.ForwardResponseEnd{}}},
		}},
		endSent: make(chan struct{}), settled: make(chan struct{}),
	}
	reader, writer := io.Pipe()
	defer reader.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		receivePluginResponseBody(stream, writer, nil, cancel)
	}()
	_, err := io.ReadAll(reader)
	var failure *PluginTransportError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, "PLUGIN_SETTLEMENT_TIMEOUT", failure.Code)
	require.True(t, failure.RequestSent)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("settlement timeout leaked the receiver")
	}
}
