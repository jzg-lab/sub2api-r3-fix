package service

import (
	"net/url"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

var ErrOpenAIOAuthFixedEgressRequired = infraerrors.Conflict(
	"OPENAI_OAUTH_FIXED_EGRESS_REQUIRED",
	"reauthorization requires a reviewed fixed-egress route matching the original login IP",
)

// Only startup configuration can attest route semantics. Equal IP probes cannot
// qualify a rotating proxy, and account edits must not create these bindings.
func compileOpenAIFixedEgressRoutes(routes []config.AuthBrowserFixedEgressRoute) map[int64]config.AuthBrowserFixedEgressRoute {
	result := make(map[int64]config.AuthBrowserFixedEgressRoute, len(routes))
	for _, route := range routes {
		ip, err := normalizeOpenAIOAuthLoginIP(route.ExitIP)
		ingress, parseErr := url.Parse(route.BrowserIngress)
		if route.ProxyID <= 0 || err != nil || ip != route.ExitIP ||
			!validOpenAIAuthBrowserLowerHex(route.ProxyRouteSHA256, 64) ||
			parseErr != nil || ingress == nil || ingress.User != nil ||
			strings.TrimSpace(route.BrowserIngress) != route.BrowserIngress ||
			validateOpenAIOAuthProxyURL(route.BrowserIngress) != nil {
			return nil
		}
		if _, duplicate := result[route.ProxyID]; duplicate {
			return nil
		}
		result[route.ProxyID] = route
	}
	return result
}

func (s *OpenAIOAuthService) validateFixedReauthorizationEgress(session *OpenAIOAuthSession, route string) error {
	pin, ok := s.fixedEgressRoutes[session.ProxyID]
	if !ok || pin.ProxyRouteSHA256 != session.ProxyRouteHash ||
		pin.ProxyRouteSHA256 != openAIOAuthProxyRouteHash(route) ||
		pin.ExitIP != session.ReauthorizationExitIP {
		return ErrOpenAIOAuthFixedEgressRequired
	}
	return nil
}
