package repository

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestHTTPUpstreamProfilesDoNotRetireEachOthersConnections(t *testing.T) {
	for _, tc := range []struct {
		name          string
		fingerprinted bool
		kind          service.HTTPUpstreamProfile
	}{
		{"ordinary_plugin", false, service.HTTPUpstreamProfileOpenAIPlugin},
		{"fingerprinted_plugin", true, service.HTTPUpstreamProfileOpenAIPlugin},
		{"ordinary_openai", false, service.HTTPUpstreamProfileOpenAI},
		{"fingerprinted_openai", true, service.HTTPUpstreamProfileOpenAI},
		{"ordinary_grok", false, service.HTTPUpstreamProfileGrok},
		{"fingerprinted_grok", true, service.HTTPUpstreamProfileGrok},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Gateway.ConnectionPoolIsolation = config.ConnectionPoolIsolationAccountProxy
			cfg.Gateway.ResponseHeaderTimeout = 10
			svc := NewHTTPUpstream(cfg).(*httpUpstreamService)
			profile := &tlsfingerprint.Profile{ALPNProtocols: []string{}}
			entry := func(kind service.HTTPUpstreamProfile, concurrency int) *upstreamClientEntry {
				t.Helper()
				var result *upstreamClientEntry
				var err error
				if tc.fingerprinted {
					result, err = svc.getClientEntryWithTLS("", 42, concurrency, profile, kind, false, true)
				} else {
					result, err = svc.getClientEntry("", 42, concurrency, kind, false, true)
				}
				require.NoError(t, err)
				t.Cleanup(result.client.CloseIdleConnections)
				return result
			}
			var connections atomic.Int64
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "complete")
			}))
			server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateNew {
					connections.Add(1)
				}
			}
			server.Start()
			defer server.Close()
			send := func(client *http.Client) {
				t.Helper()
				// Exercise real pool reuse; TLS wire behavior has separate coverage.
				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
				require.NoError(t, err)
				resp, err := client.Do(req)
				require.NoError(t, err)
				_, err = io.Copy(io.Discard, resp.Body)
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())
			}
			business := entry(tc.kind, 2)
			telemetry := entry(service.HTTPUpstreamProfileDefault, 2)
			for i := 0; i < 6; i++ {
				send(entry(tc.kind, 2).client)
				send(entry(service.HTTPUpstreamProfileDefault, 2).client)
			}
			require.Equal(t, int64(2), connections.Load(),
				"alternating business and telemetry must retain one connection per policy")
			require.Same(t, business, entry(tc.kind, 2))
			require.Same(t, telemetry, entry(service.HTTPUpstreamProfileDefault, 2))
			changed := entry(tc.kind, 3)
			require.NotSame(t, business, changed, "real settings changes must still retire the old pool")
			require.Same(t, telemetry, entry(service.HTTPUpstreamProfileDefault, 2),
				"business settings changes must not retire the telemetry pool")
		})
	}
}
