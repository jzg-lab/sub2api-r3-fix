package transport

import (
	"net/http"
	"time"

	"github.com/liyunlong/sub2api-cookie-plugin/internal/prober"
	pluginv1 "github.com/liyunlong/sub2api-cookie-plugin/pkg/pluginapi/v1"
)

// Caller holds probeMu and has checked config/template generation. Business
// failures govern autonomous probes too; they are not quality or cookie evidence.
func (s *Server) observeForwardFailureLocked(accountID int64, tmpl *probeTemplate, status int, header http.Header, now time.Time) bool {
	authFailed := status == http.StatusUnauthorized || status == http.StatusForbidden
	policy := pluginv1.ProbeRateLimitPolicy{}
	if tmpl != nil {
		policy = tmpl.RateLimitPolicy
	}
	deadline := probeRetryDeadlineWithPolicy(status, header, now, policy)
	if !authFailed && deadline.IsZero() {
		return false
	}
	if tmpl == nil {
		return true
	}
	// Fence results already in flight, including transports that finish after
	// cancellation, before allowing them to record a pass or reroll a cookie.
	s.invalidateProbeResultsLocked(accountID)
	if authFailed {
		delete(s.templates, accountID)
		s.resetProbeStateLocked(accountID, now)
		return true
	}
	s.extendProbeCooldownLocked(accountID, deadline)
	return true
}

func (s *Server) extendProbeCooldownLocked(accountID int64, deadline time.Time) {
	state := s.states[accountID]
	if state == nil {
		state = prober.NewState(accountID)
		s.states[accountID] = state
	}
	state.RetryNotBefore = maxTime(state.RetryNotBefore, deadline)
	state.NextProbeAt = maxTime(state.NextProbeAt, state.RetryNotBefore)
}
