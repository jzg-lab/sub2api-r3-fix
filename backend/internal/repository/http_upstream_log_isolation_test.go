package repository

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestHTTPUpstreamDiagnosticsDoNotExposeProxyCredentials(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	cfg := &config.Config{}
	cfg.Gateway.ConnectionPoolIsolation = config.ConnectionPoolIsolationAccountProxy
	cfg.Gateway.OpenAIHTTP2 = config.GatewayOpenAIHTTP2Config{
		Enabled: true, AllowProxyFallbackToHTTP1: true, FallbackErrorThreshold: 1,
	}
	svc := NewHTTPUpstream(cfg).(*httpUpstreamService)
	profile := &tlsfingerprint.Profile{ALPNProtocols: []string{}}
	proxy := "http://fixture-proxy-user:fixture-proxy-password@proxy.invalid:8080"
	for _, concurrency := range []int{1, 1, 2} {
		entry, err := svc.getClientEntryWithTLS(proxy, 42, concurrency, profile,
			service.HTTPUpstreamProfileOpenAIPlugin, false, true)
		require.NoError(t, err)
		t.Cleanup(entry.client.CloseIdleConnections)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://example.com/?token=fixture-request-private", nil)
	require.NoError(t, err)
	_, err = svc.DoWithTLS(req, proxy, 42, 2, profile)
	require.Error(t, err)
	svc.recordOpenAIHTTP2Failure(service.HTTPUpstreamProfileOpenAI, upstreamProtocolModeOpenAIH2,
		proxy, errors.New("http2: protocol error"))
	require.True(t, svc.isOpenAIHTTP2FallbackActive(proxy))
	logs := output.String()
	for _, secret := range []string{"fixture-proxy-user", "fixture-proxy-password", "fixture-request-private"} {
		require.NotContains(t, logs, secret)
	}
	for _, event := range []string{"creating_new_client", "reusing_client", "evicting_stale_client",
		"fingerprint_enabled", "fingerprint_request_failed", "proxy_fallback_activated"} {
		require.Contains(t, logs, event)
	}
	require.NotContains(t, logs, `"cache_key"`)
	require.Contains(t, logs, `"client_id"`)
}

func TestCodexTransportPoolsSeparateAccountsRoutesAndProxyCredentials(t *testing.T) {
	for _, fingerprinted := range []bool{false, true} {
		for _, globalMode := range []string{config.ConnectionPoolIsolationProxy, config.ConnectionPoolIsolationAccount} {
			t.Run(globalMode+map[bool]string{false: "/plain", true: "/tls"}[fingerprinted], func(t *testing.T) {
				cfg := &config.Config{}
				cfg.Gateway.ConnectionPoolIsolation = globalMode
				svc := NewHTTPUpstream(cfg).(*httpUpstreamService)
				profile := &tlsfingerprint.Profile{ALPNProtocols: []string{}}
				entry := func(proxy string, id int64) *upstreamClientEntry {
					t.Helper()
					var result *upstreamClientEntry
					var err error
					if fingerprinted {
						result, err = svc.getClientEntryWithTLS(proxy, id, 2, profile,
							service.HTTPUpstreamProfileOpenAIPlugin, false, true)
					} else {
						result, err = svc.getClientEntry(proxy, id, 2,
							service.HTTPUpstreamProfileOpenAIPlugin, false, true)
					}
					require.NoError(t, err)
					t.Cleanup(result.client.CloseIdleConnections)
					return result
				}
				proxy := "socks5h://fixture-user:fixture-password@proxy.invalid:1080"
				first := entry(proxy, 42)
				require.Same(t, first, entry(proxy, 42))
				require.NotSame(t, first, entry(proxy, 43), "accounts must not share a connection pool")
				require.NotSame(t, first, entry(strings.ReplaceAll(proxy, "proxy.invalid", "other.invalid"), 42))
				require.NotSame(t, first, entry(strings.ReplaceAll(proxy, "fixture-user", "other-user"), 42))
				require.NotSame(t, first, entry(strings.ReplaceAll(proxy, "fixture-password", "other-password"), 42))
				require.Same(t, first, entry(proxy, 42), "other identities must not retire this pool")
				require.NotSame(t, entry("", 42), entry("", 43))
			})
		}
	}
}
