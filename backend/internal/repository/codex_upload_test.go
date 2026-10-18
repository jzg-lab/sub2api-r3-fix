package repository

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/stretchr/testify/require"
)

func TestCodexUploadIdleLifecycle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		original := codexFixtureRequest(t, t.Context(), strings.Repeat("x", 512*1024))
		req, guard := guardCodexUpload(original, time.Second)
		defer guard.close()
		require.Nil(t, req.GetBody)
		hooks := httptrace.ContextClientTrace(req.Context())
		time.Sleep(3 * time.Second)
		require.NoError(t, req.Context().Err(), "queue/dial time is not upload time")
		hooks.GotConn(httptrace.GotConnInfo{})
		buffer := make([]byte, 128*1024)
		for range 6 {
			time.Sleep(750 * time.Millisecond)
			n, err := req.Body.Read(buffer)
			require.NoError(t, err)
			require.Equal(t, 32*1024, n)
			require.NoError(t, req.Context().Err(), "sustained progress can exceed the idle budget")
		}
		hooks.WroteRequest(httptrace.WroteRequestInfo{})
		time.Sleep(3 * time.Second)
		require.NoError(t, req.Context().Err(), "response wait and body are not upload time")
		require.NoError(t, guard.result(nil))
		require.NoError(t, original.Context().Err())
	})
}

func TestCodexUploadStallIsFiniteWriteFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		req, guard := guardCodexUpload(codexFixtureRequest(t, t.Context(), "{}"), time.Second)
		defer guard.close()
		httptrace.ContextClientTrace(req.Context()).GotConn(httptrace.GotConnInfo{})
		_, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		// Reading EOF is not proof that the transport flushed all body bytes.
		time.Sleep(2 * time.Second)
		require.ErrorIs(t, req.Context().Err(), context.Canceled)
		result := guard.result(req.Context().Err())
		require.Same(t, errCodexUploadIdle, result)
		require.Equal(t, "HOST_HTTP_TRANSPORT/network_timeout/write",
			pluginv1.HostHTTPFailureCode(result, nil, false))
		readBytes, idleMS, expired := guard.observation()
		require.EqualValues(t, 2, readBytes, "body reads do not prove socket delivery")
		require.EqualValues(t, 1000, idleMS)
		require.True(t, expired)
	})
}

func TestCodexUploadObservationFreezesBeforeHeaderWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		req, guard := guardCodexUpload(codexFixtureRequest(t, t.Context(), "fixture"), time.Second)
		defer guard.close()
		hooks := httptrace.ContextClientTrace(req.Context())
		readBytes, idleMS, expired := guard.observation()
		require.Zero(t, readBytes)
		require.EqualValues(t, -1, idleMS)
		require.False(t, expired)
		hooks.GotConn(httptrace.GotConnInfo{})
		time.Sleep(200 * time.Millisecond)
		_, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		time.Sleep(50 * time.Millisecond)
		hooks.WroteRequest(httptrace.WroteRequestInfo{})
		time.Sleep(20 * time.Second)
		require.NoError(t, guard.result(nil))
		readBytes, idleMS, expired = guard.observation()
		require.EqualValues(t, 7, readBytes)
		require.EqualValues(t, 50, idleMS, "response waiting must not be reported as upload idle")
		require.False(t, expired)
		require.NoError(t, req.Context().Err())
	})
}

func TestCodexUploadParentCancellationAndOtherRequests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent, cancel := context.WithCancel(t.Context())
		first, stalled := guardCodexUpload(codexFixtureRequest(t, parent, "{}"), time.Second)
		second, healthy := guardCodexUpload(codexFixtureRequest(t, t.Context(), "{}"), time.Second)
		defer stalled.close()
		defer healthy.close()
		httptrace.ContextClientTrace(first.Context()).GotConn(httptrace.GotConnInfo{})
		httptrace.ContextClientTrace(second.Context()).GotConn(httptrace.GotConnInfo{})
		httptrace.ContextClientTrace(second.Context()).WroteRequest(httptrace.WroteRequestInfo{})
		cancel()
		time.Sleep(2 * time.Second)
		require.ErrorIs(t, stalled.result(first.Context().Err()), context.Canceled)
		_, _, expired := stalled.observation()
		require.False(t, expired, "parent cancellation is not the local upload guard")
		require.NoError(t, second.Context().Err())
		require.NoError(t, healthy.result(nil))
	})
}

type codexStallingTransport struct {
	calls atomic.Int32
}

func (tr *codexStallingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tr.calls.Add(1)
	defer req.Body.Close()
	requireTrace := httptrace.ContextClientTrace(req.Context())
	requireTrace.GotConn(httptrace.GotConnInfo{})
	_, err := io.Copy(io.Discard, req.Body)
	if err != nil {
		return nil, err
	}
	<-req.Context().Done()
	return nil, req.Context().Err()
}

func TestCodexUploadStallBothConsumersReleaseCapacity(t *testing.T) {
	for _, useTLS := range []bool{false, true} {
		t.Run(map[bool]string{false: "Do", true: "DoWithTLS"}[useTLS], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc := NewHTTPUpstream(nil).(*httpUpstreamService)
				profile := &tlsfingerprint.Profile{Name: "fixture"}
				var entry *upstreamClientEntry
				var err error
				if useTLS {
					entry, err = svc.getClientEntryWithTLS("", 42, 1, profile, service.HTTPUpstreamProfileOpenAIPlugin, false, false)
				} else {
					entry, err = svc.getClientEntry("", 42, 1, service.HTTPUpstreamProfileOpenAIPlugin, false, false)
				}
				require.NoError(t, err)
				transport := &codexStallingTransport{}
				entry.client.Transport = transport
				req := codexFixtureRequest(t, t.Context(), `{"input":"fixture"}`)
				req.Header.Set("Idempotency-Key", "fixture-do-not-replay")
				start := time.Now()
				var resp *http.Response
				if useTLS {
					resp, err = svc.DoWithTLS(req, "", 42, 1, profile)
				} else {
					resp, err = svc.Do(req, "", 42, 1)
				}
				require.Nil(t, resp)
				require.ErrorIs(t, err, errCodexUploadIdle)
				require.Equal(t, codexUploadIdleTimeout, time.Since(start))
				require.EqualValues(t, 1, transport.calls.Load(), "no replay after a dispatched request")
				require.Zero(t, atomic.LoadInt64(&entry.inFlight))
				require.NoError(t, req.Context().Err(), "request-scoped deadline must not cancel its caller")
			})
		})
	}
}

func TestCodexUploadFinishedAndQueuedTimers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		req, guard := guardCodexUpload(codexFixtureRequest(t, t.Context(), "{}"), time.Second)
		defer guard.close()
		hooks := httptrace.ContextClientTrace(req.Context())
		hooks.GotConn(httptrace.GotConnInfo{})
		time.Sleep(750 * time.Millisecond)
		_, err := req.Body.Read(make([]byte, 1))
		require.NoError(t, err)
		guard.expire() // A queued callback must not discard newer progress.
		time.Sleep(500 * time.Millisecond)
		require.NoError(t, req.Context().Err())
		hooks.WroteRequest(httptrace.WroteRequestInfo{Err: errors.New("fixture-write-error")})
		time.Sleep(2 * time.Second)
		require.NoError(t, req.Context().Err())
		require.EqualError(t, guard.result(errors.New("fixture-write-error")), "fixture-write-error")
	})
}

func TestCodexUploadBlockedSocketWrite(t *testing.T) {
	var dials atomic.Int32
	peers := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	stopPeers := func() { releaseOnce.Do(func() { close(release) }) }
	defer stopPeers()
	base := &http.Transport{
		ResponseHeaderTimeout: time.Second,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			dials.Add(1)
			client, server := net.Pipe()
			go func() {
				defer server.Close()
				defer func() { peers <- struct{}{} }()
				reader := bufio.NewReader(server)
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					if line == "\r\n" {
						break
					}
				}
				// Consume a byte so the application request is already sent.
				if _, err := reader.ReadByte(); err != nil {
					return
				}
				<-release
			}()
			return client, nil
		},
	}
	defer base.CloseIdleConnections()
	parent, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	original, err := http.NewRequestWithContext(parent, http.MethodPost,
		"http://fixture.invalid/responses", strings.NewReader(strings.Repeat("x", 2<<20)))
	require.NoError(t, err)
	req, guard := guardCodexUpload(original, 80*time.Millisecond)
	defer guard.close()
	start := time.Now()
	resp, err := (&http.Client{Transport: base}).Do(req)
	require.Nil(t, resp)
	require.Same(t, errCodexUploadIdle, guard.result(err))
	require.Less(t, time.Since(start), time.Second)
	require.NoError(t, parent.Err())
	require.EqualValues(t, 1, dials.Load(), "partially written POST must not be replayed")
	stopPeers()
	select {
	case <-peers:
	case <-time.After(time.Second):
		t.Fatal("timed-out upload retained its peer")
	}
}
