package service

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
)

func TestOpenAIFixedEgressConfigurationRejectsInvalidOrAmbiguousRoutes(t *testing.T) {
	valid := config.AuthBrowserFixedEgressRoute{
		ProxyID: 7, ProxyRouteSHA256: strings.Repeat("a", 64),
		BrowserIngress: "http://127.0.0.1:17931", ExitIP: "198.51.100.25",
	}
	for _, name := range []string{
		"valid", "missing", "duplicate", "negative ID", "zero ID", "short hash", "uppercase hash",
		"invalid hash", "missing IP", "private IP", "noncanonical IP", "zone", "invalid ingress",
		"ingress credentials", "ingress path", "ingress query", "ingress fragment",
		"ingress whitespace", "invalid port", "missing port", "unsupported protocol",
	} {
		t.Run(name, func(t *testing.T) {
			pin := valid
			switch name {
			case "negative ID":
				pin.ProxyID = -1
			case "zero ID":
				pin.ProxyID = 0
			case "short hash":
				pin.ProxyRouteSHA256 = "a"
			case "uppercase hash":
				pin.ProxyRouteSHA256 = strings.Repeat("A", 64)
			case "invalid hash":
				pin.ProxyRouteSHA256 = strings.Repeat("z", 64)
			case "missing IP":
				pin.ExitIP = ""
			case "private IP":
				pin.ExitIP = "10.0.0.1"
			case "noncanonical IP":
				pin.ExitIP = "::ffff:198.51.100.25"
			case "zone":
				pin.ExitIP = "2001:db8::1%en0"
			case "invalid ingress":
				pin.BrowserIngress = "://"
			case "ingress credentials":
				pin.BrowserIngress = "http://fixture:fixture@127.0.0.1:17931"
			case "ingress path":
				pin.BrowserIngress += "/"
			case "ingress query":
				pin.BrowserIngress += "?"
			case "ingress fragment":
				pin.BrowserIngress += "#route"
			case "ingress whitespace":
				pin.BrowserIngress += " "
			case "invalid port":
				pin.BrowserIngress = "http://127.0.0.1:65536"
			case "missing port":
				pin.BrowserIngress = "http://127.0.0.1"
			case "unsupported protocol":
				pin.BrowserIngress = "ftp://127.0.0.1:17931"
			}
			routes := []config.AuthBrowserFixedEgressRoute{pin}
			if name == "duplicate" {
				routes = append(routes, pin)
			} else if name == "missing" {
				routes = nil
			} else if name != "valid" {
				// One invalid entry cannot leave a partially accepted route set.
				other := valid
				other.ProxyID = 8
				routes = append(routes, other)
			}
			compiled := compileOpenAIFixedEgressRoutes(routes)
			if name == "valid" {
				require.Equal(t, valid, compiled[7])
				routes[0].ExitIP = "198.51.100.99"
				require.Equal(t, valid, compiled[7], "startup pins must not alias the input slice")
			} else {
				require.Empty(t, compiled)
			}
		})
	}
}

func TestOpenAIFixedEgressDeniesBeforeProbeAndTokenExchange(t *testing.T) {
	for _, mutation := range []string{"missing", "ID", "route hash", "exit IP", "credentials"} {
		t.Run(mutation, func(t *testing.T) {
			svc, account, session, proxy, client := reauthorizationIPFixture(t)
			pin := svc.fixedEgressRoutes[proxy.ID]
			switch mutation {
			case "missing":
				delete(svc.fixedEgressRoutes, proxy.ID)
			case "ID":
				delete(svc.fixedEgressRoutes, proxy.ID)
				svc.fixedEgressRoutes[proxy.ID+1] = pin
			case "route hash":
				pin.ProxyRouteSHA256 = strings.Repeat("a", 64)
				svc.fixedEgressRoutes[proxy.ID] = pin
			case "exit IP":
				pin.ExitIP = "198.51.100.99"
				svc.fixedEgressRoutes[proxy.ID] = pin
			case "credentials":
				proxy.Username, proxy.Password = "fixture", "changed"
			}
			probes := 0
			svc.observeReauthorizationExitIP = func(context.Context, string) (string, error) {
				probes++
				return session.ReauthorizationExitIP, nil
			}
			store := svc.sessionStore.(*testOpenAIOAuthSessionStore)
			before := len(store.sessions)
			result, err := svc.GenerateReauthorizationAuthURL(t.Context(), account.ID,
				account.UpdatedAt.Format(time.RFC3339Nano), account.ProxyID, "", PlatformOpenAI, "")
			require.ErrorIs(t, err, ErrOpenAIOAuthFixedEgressRequired)
			require.Nil(t, result)
			require.Len(t, store.sessions, before)
			token, err := svc.ExchangeCode(t.Context(), &OpenAIExchangeCodeInput{
				SessionID: session.ID, State: session.State, Code: "fixture",
			})
			require.Error(t, err)
			require.Nil(t, token)
			require.Zero(t, probes, "a matching live IP must not qualify a route")
			require.Zero(t, atomic.LoadInt32(&client.exchangeCalled))
		})
	}
}

func TestOpenAIFixedEgressStartupWiring(t *testing.T) {
	for _, ingressChanged := range []bool{false, true} {
		t.Run(map[bool]string{false: "bound", true: "browser ingress changed"}[ingressChanged], func(t *testing.T) {
			svc, _, session, proxy, _ := reauthorizationIPFixture(t)
			settings := viper.New()
			settings.SetConfigType("yaml")
			require.NoError(t, settings.ReadConfig(strings.NewReader(`
gateway:
  auth_browser_launcher: /usr/bin/true
  auth_browser_fixed_egress_routes:
    - proxy_id: 7
      proxy_route_sha256: "`+openAIOAuthProxyRouteHash(proxy.URL())+`"
      browser_ingress: http://127.0.0.1:8080
      exit_ip: 198.51.100.25
`)))
			var cfg config.Config
			require.NoError(t, settings.Unmarshal(&cfg))
			if ingressChanged {
				cfg.Gateway.AuthBrowserFixedEgressRoutes[0].BrowserIngress = "http://127.0.0.1:8081"
			}
			launcher := NewOpenAIAuthBrowserLauncher(&cfg, svc.sessionStore, svc.proxyRepo)
			require.NotNil(t, launcher)
			// Exercise constructor -> injection -> generation/launch validation,
			// not only direct writes to service internals.
			svc.fixedEgressRoutes = nil
			launcher.SetReauthorizationService(svc)
			require.NoError(t, svc.validateReauthorizationSession(t.Context(), session, false))
			result, err := launcher.Launch(t.Context(), session.ID)
			if ingressChanged {
				require.ErrorIs(t, err, ErrOpenAIOAuthFixedEgressRequired)
				require.Nil(t, result)
			} else {
				require.NoError(t, err)
				require.True(t, result.Launched)
			}
		})
	}
}

func TestOpenAIFixedEgressMissingConfigurationDoesNotBlockInitialAuthorization(t *testing.T) {
	svc, session, _, client := oauthRouteFixture(t)
	cfg := &config.Config{Gateway: config.GatewayConfig{AuthBrowserLauncher: "/usr/bin/true"}}
	launcher := NewOpenAIAuthBrowserLauncher(cfg, svc.sessionStore, svc.proxyRepo)
	launcher.SetReauthorizationService(svc)
	result, err := launcher.Launch(t.Context(), session.ID)
	require.NoError(t, err)
	require.True(t, result.Launched)
	token, err := svc.ExchangeCode(t.Context(), &OpenAIExchangeCodeInput{
		SessionID: session.ID, State: session.State, Code: "fixture",
	})
	require.NoError(t, err)
	require.NotNil(t, token)
	require.EqualValues(t, 1, atomic.LoadInt32(&client.exchangeCalled))
}
