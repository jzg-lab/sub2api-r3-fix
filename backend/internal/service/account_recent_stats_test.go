package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type recentStatsTestCache struct {
	GatewayCache
	observations []AccountAttemptObservation
}

func (c *recentStatsTestCache) RecordAccountAttempt(_ context.Context, _ int64, o AccountAttemptObservation, _ time.Time) error {
	c.observations = append(c.observations, o)
	return nil
}
func (c *recentStatsTestCache) GetAccountRecentStats(context.Context, []int64, time.Time) (map[int64]AccountRecentStats, error) {
	return nil, nil
}

func TestRecentAccountReliabilityBalancesEvidenceAndRecovery(t *testing.T) {
	now := time.Now()
	one := AccountRecentStats{Successes: 1}
	many := AccountRecentStats{Successes: 99, Failures: 1}
	require.Less(t, one.Reliability(now), many.Reliability(now))
	many.ConsecutiveFailures, many.LastObservedAt = 3, now.Unix()
	require.Less(t, many.Reliability(now), one.Reliability(now))
	require.Greater(t, many.Reliability(now.Add(time.Minute)), one.Reliability(now))
	require.Nil(t, (AccountRecentStats{}).View().SuccessRate)
	require.Nil(t, one.View().CacheHitRate)
	require.InDelta(t, 1, *one.View().SuccessRate, 0.001)
}

func TestObserveRecentAccountAttemptsExcludeClientErrorsAndUseNormalizedTokens(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cache := &recentStatsTestCache{}
	s := &OpenAIGatewayService{cache: cache}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	account := &Account{ID: 1}
	for _, err := range []error{context.Canceled, errors.New("invalid request"), &UpstreamFailoverError{StatusCode: 400}, &UpstreamFailoverError{StatusCode: 503, RequestScopedTransient: true}} {
		s.ObserveAccountAttempt(c, account, nil, err)
	}
	s.ObserveAccountAttempt(c, account, &OpenAIForwardResult{ClientDisconnect: true}, nil)
	require.Empty(t, cache.observations)
	s.ObserveAccountAttempt(c, account, nil, &UpstreamFailoverError{StatusCode: 502})
	s.ObserveAccountAttempt(c, account, nil, io.ErrUnexpectedEOF)
	ttft := 1000
	s.ObserveAccountAttempt(c, account, &OpenAIForwardResult{Usage: OpenAIUsage{InputTokens: 100, CacheReadInputTokens: 60, CacheCreationInputTokens: 10}, FirstTokenMs: &ttft}, nil)
	require.Len(t, cache.observations, 3)
	require.False(t, cache.observations[0].Success)
	require.False(t, cache.observations[1].Success)
	require.True(t, cache.observations[2].Success)
	require.EqualValues(t, 100, cache.observations[2].InputTokens)
	require.EqualValues(t, 60, cache.observations[2].CacheReadTokens)
	c.Request = c.Request.WithContext(func() context.Context { ctx, cancel := context.WithCancel(context.Background()); cancel(); return ctx }())
	s.ObserveAccountAttempt(c, account, nil, &UpstreamFailoverError{StatusCode: 502})
	require.Len(t, cache.observations, 3)
}

func TestRecentAccountOrderingPreservesConfiguredPriority(t *testing.T) {
	now := time.Now()
	ctx := context.WithValue(context.Background(), recentAccountStatsKey{}, map[int64]AccountRecentStats{
		1: {Successes: 100, Failures: 5, ConsecutiveFailures: 3, LastObservedAt: now.Unix()},
		2: {Successes: 50},
	})
	accounts := []*Account{{ID: 1, Priority: 2}, {ID: 2, Priority: 2}, {ID: 3, Priority: 0}}
	sortByRecentReliability(ctx, accounts)
	require.Equal(t, []int64{3, 2, 1}, []int64{accounts[0].ID, accounts[1].ID, accounts[2].ID})
}

func TestRecentAccountCacheIgnoresBillingSubstitution(t *testing.T) {
	cache := &recentStatsTestCache{}
	svc := &GatewayService{cache: cache}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	result := &ForwardResult{Usage: ClaudeUsage{InputTokens: 10, CacheReadInputTokens: 90}}
	svc.ObserveAccountAttempt(c, &Account{ID: 1}, result, nil, false)
	svc.ObserveAccountAttempt(c, &Account{ID: 1}, result, nil, true)
	require.Len(t, cache.observations, 2)
	require.EqualValues(t, 100, cache.observations[0].InputTokens)
	require.EqualValues(t, 90, cache.observations[0].CacheReadTokens)
	require.True(t, cache.observations[1].Success)
	require.Zero(t, cache.observations[1].InputTokens)
	require.Zero(t, cache.observations[1].CacheReadTokens)
}

func TestRecentWSStatsDoNotReusePriorTurnFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, status := range []int{400, 429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			cache := &recentStatsTestCache{}
			svc := &OpenAIGatewayService{cache: cache}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("GET", "/v1/responses", nil)
			SetOpenAIClientTransport(c, OpenAIClientTransportWS)
			BeginOpsStreamTurn(c, 1)
			MarkOpsStreamFailure(c, "api_error", "upstream_error", "first turn failed", status)
			svc.ObserveAccountAttempt(c, &Account{ID: 1}, &OpenAIForwardResult{UpstreamTerminalEvent: "response.failed"}, nil)
			if status == 400 {
				require.Empty(t, cache.observations)
			} else {
				require.Len(t, cache.observations, 1)
				require.False(t, cache.observations[0].Success)
			}
			cache.observations = nil
			BeginOpsStreamTurn(c, 2)
			ttft := 1200
			svc.ObserveAccountAttempt(c, &Account{ID: 1}, &OpenAIForwardResult{UpstreamTerminalEvent: "response.completed", Usage: OpenAIUsage{InputTokens: 100, CacheReadInputTokens: 60}, FirstTokenMs: &ttft}, nil)
			require.Len(t, cache.observations, 1)
			require.True(t, cache.observations[0].Success, "successful turn 2 must not inherit turn 1 failure")
			require.EqualValues(t, 100, cache.observations[0].InputTokens)
			require.EqualValues(t, 60, cache.observations[0].CacheReadTokens)
			require.Equal(t, 1200, *cache.observations[0].FirstTokenMs)
			require.Len(t, GetOpsStreamErrors(c), 1, "historical failures must remain available to ops logging")
		})
	}
}
