//go:build unit

package service

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/stretchr/testify/require"
)

func quotaResetSignalHeaders(windowMinutes string) http.Header {
	h := make(http.Header)
	for _, slot := range []string{"primary", "secondary"} {
		h.Set("x-codex-"+slot+"-used-percent", "0")
		h.Set("x-codex-"+slot+"-reset-after-seconds", "0")
		h.Set("x-codex-"+slot+"-window-minutes", windowMinutes)
	}
	return h
}

func TestQuotaResetSignal_UnavailableWindows(t *testing.T) {
	t.Parallel()
	for _, duration := range []string{"0", "-1"} {
		t.Run(duration, func(t *testing.T) {
			headers := quotaResetSignalHeaders(duration)
			snapshot := ParseCodexRateLimitHeaders(headers)
			require.NotNil(t, snapshot)
			require.Nil(t, snapshot.Normalize())
			require.Empty(t, buildCodexUsageExtraUpdates(snapshot, time.Now()))
			require.Nil(t, calculateOpenAI429ResetTime(headers))
			updates, err := extractOpenAICodexProbeUpdates(&http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     headers,
			})
			require.Error(t, err)
			require.Empty(t, updates)
			require.NotNil(t, snapshot.PrimaryUsedPercent, "normalization must not mutate its source")
		})
	}
}

func TestQuotaResetSignal_MixedWindowsAndSuccessfulZero(t *testing.T) {
	t.Parallel()
	headers := quotaResetSignalHeaders("0")
	headers.Set("x-codex-secondary-window-minutes", "300")
	headers.Set("x-codex-secondary-reset-after-seconds", "731")
	updates, err := extractOpenAICodexProbeUpdates(&http.Response{
		StatusCode: http.StatusOK, Header: headers,
	})
	require.NoError(t, err)
	require.Equal(t, 0.0, updates["codex_5h_used_percent"])
	require.Equal(t, 731, updates["codex_5h_reset_after_seconds"])
	require.Nil(t, updates["codex_7d_used_percent"])
	require.Nil(t, updates["codex_7d_reset_at"])
	require.NotNil(t, buildCodexUsageProgressFromExtra(updates, "5h", time.Now()))
	account := &Account{Extra: map[string]any{
		"codex_7d_used_percent": 80.0,
		"codex_7d_reset_at":     time.Now().Add(time.Hour).Format(time.RFC3339),
	}}
	mergeAccountExtra(account, updates)
	require.Nil(t, buildCodexUsageProgressFromExtra(account.Extra, "7d", time.Now()),
		"the new snapshot must invalidate the unavailable peer rather than renew its old sample")
}

func TestQuotaResetSignal_UnrelatedFailureCannotRefreshQuota(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusBadRequest, http.StatusBadGateway} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			updates, err := extractOpenAICodexProbeUpdates(&http.Response{
				StatusCode: status, Header: quotaResetSignalHeaders("300"),
			})
			require.Error(t, err)
			require.Empty(t, updates)
		})
	}
}

func TestQuotaResetSignal_LegacyCacheAndBoundary(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 16, 14, 0, 0, 0, time.UTC)
	for _, window := range []string{"5h", "7d"} {
		t.Run(window, func(t *testing.T) {
			extra := map[string]any{
				"codex_" + window + "_used_percent":   0.0,
				"codex_" + window + "_window_minutes": 0,
				"codex_" + window + "_reset_at":       now.Add(time.Minute).Format(time.RFC3339),
			}
			require.Nil(t, buildCodexUsageProgressFromExtra(extra, window, now))
			delete(extra, "codex_"+window+"_window_minutes")
			valid := buildCodexUsageProgressFromExtra(extra, window, now)
			require.NotNil(t, valid, "missing duration retains legacy compatibility")
			require.Equal(t, 0.0, valid.Utilization)
			require.Equal(t, 60, valid.RemainingSeconds, "use the supplied observation time")
			require.Nil(t, buildCodexUsageProgressFromExtra(extra, window, now.Add(time.Minute)))
			extra["codex_"+window+"_used_percent"] = nil
			require.Nil(t, buildCodexUsageProgressFromExtra(extra, window, now))
		})
	}
}

func TestQuotaResetSignal_ExplicitResetNotRoundedToCycle(t *testing.T) {
	t.Parallel()
	for _, seconds := range []int64{731, 14*24*60*60 + 19} {
		t.Run(strconv.FormatInt(seconds, 10), func(t *testing.T) {
			reset := time.Now().Unix() + seconds
			body := []byte(fmt.Sprintf(`{"error":{"type":"usage_limit_reached","resets_at":%d}}`, reset))
			headers := quotaResetSignalHeaders("300")
			headers.Set("x-codex-primary-reset-after-seconds", "604800")
			repo := &openAI429SnapshotRepo{}
			svc := NewRateLimitService(repo, nil, nil, nil, nil)
			account := &Account{ID: 1043, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
			svc.handle429(context.Background(), account, headers, body)
			require.Equal(t, reset, repo.rateLimitedUntil.Unix(), "non-exhausted headers cannot override the explicit body")
			probeReset := probeOpenAI429ResetTime(headers, body)
			require.NotNil(t, probeReset)
			require.Equal(t, reset, probeReset.Unix(), "probe consumer must use the same evidence")
		})
	}
}

func TestQuotaResetSignal_BothExhaustedUseActualLaterReset(t *testing.T) {
	t.Parallel()
	headers := quotaResetSignalHeaders("300")
	headers.Set("x-codex-primary-window-minutes", "10080")
	headers.Set("x-codex-primary-used-percent", "100")
	headers.Set("x-codex-primary-reset-after-seconds", "11")
	headers.Set("x-codex-secondary-used-percent", "100")
	headers.Set("x-codex-secondary-reset-after-seconds", "731")
	before := time.Now()
	got := calculateOpenAI429ResetTime(headers)
	require.NotNil(t, got)
	require.WithinRange(t, *got, before.Add(731*time.Second), time.Now().Add(731*time.Second))
}

func TestQuotaResetSignal_ExpiredOrUnavailableUsageDoesNotClear429(t *testing.T) {
	t.Parallel()
	now := time.Now()
	until := now.Add(14 * 24 * time.Hour)
	account := &Account{
		ID: 1045, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		RateLimitResetAt: &until,
		Extra: map[string]any{
			"codex_5h_used_percent":   100.0,
			"codex_5h_reset_at":       now.Add(-time.Second).Format(time.RFC3339),
			"codex_7d_used_percent":   0.0,
			"codex_7d_window_minutes": 0,
			"codex_7d_reset_at":       now.Format(time.RFC3339),
		},
	}
	statsRepo := &quotaResetSignalStatsRepo{}
	svc := &AccountUsageService{usageLogRepo: statsRepo}
	usage, err := svc.getOpenAIUsage(context.Background(), account, false)
	require.NoError(t, err)
	require.Nil(t, usage.FiveHour)
	require.Nil(t, usage.SevenDay)
	require.Equal(t, until, *account.RateLimitResetAt)
	require.Zero(t, statsRepo.calls, "local counts must not manufacture zero-percent quota")
}

type quotaResetSignalStatsRepo struct {
	UsageLogRepository
	calls int
}

func (r *quotaResetSignalStatsRepo) GetAccountWindowStats(context.Context, int64, time.Time) (*usagestats.AccountStats, error) {
	r.calls++
	return &usagestats.AccountStats{}, nil
}

func TestQuotaResetSignal_RefreshDropsExpiredInMemoryWindow(t *testing.T) {
	t.Parallel()
	now := time.Now()
	usage := &UsageInfo{
		FiveHour: &UsageProgress{Utilization: 0},
		SevenDay: &UsageProgress{Utilization: 0},
	}
	extra := map[string]any{
		"codex_5h_used_percent":   0.0,
		"codex_5h_reset_at":       now.Add(-time.Second).Format(time.RFC3339),
		"codex_7d_used_percent":   0.0,
		"codex_7d_window_minutes": 0,
	}
	applyExtraToUsage(usage, extra, now)
	require.Nil(t, usage.FiveHour)
	require.Nil(t, usage.SevenDay)
}
