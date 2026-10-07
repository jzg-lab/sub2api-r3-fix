//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type monotonicOpenAI429Repo struct {
	openAI429SnapshotRepo
	extensions int
	err        error
}

func (r *monotonicOpenAI429Repo) SetRateLimitedIfLater(_ context.Context, _ int64, until time.Time) error {
	r.extensions++
	if r.err != nil {
		return r.err
	}
	if until.After(r.rateLimitedUntil) {
		r.rateLimitedUntil = until
	}
	return nil
}

func (r *monotonicOpenAI429Repo) UpdateSessionWindow(context.Context, int64, *time.Time, *time.Time, string) error {
	return nil
}

func TestOpenAI429DeadlinePersistenceNeverShortens(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header http.Header
		body   []byte
	}{
		{"retry_after", http.Header{"Retry-After": {"60"}}, nil},
		{"body", nil, []byte(`{"error":{"type":"usage_limit_reached","resets_in_seconds":60}}`)},
		{"fallback", nil, nil},
		{"legacy_header", http.Header{"Anthropic-Ratelimit-Unified-Reset": {fmt.Sprint(time.Now().Add(time.Minute).Unix())}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &monotonicOpenAI429Repo{}
			svc := NewRateLimitService(repo, nil, nil, nil, nil)
			account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
			svc.handle429(t.Context(), account, http.Header{"Retry-After": {"7200"}}, nil)
			later := repo.rateLimitedUntil
			require.True(t, later.After(time.Now().Add(time.Hour)))
			svc.handle429(t.Context(), account, tc.header, tc.body)
			require.Equal(t, later, repo.rateLimitedUntil, "a delayed shorter response must not release the account")
			require.Equal(t, 2, repo.extensions, "use the repository's atomic extension, not a stale account snapshot")
		})
	}
}

func TestOpenAI429DeadlinePersistenceFailureDoesNotOverwrite(t *testing.T) {
	repo := &monotonicOpenAI429Repo{err: errors.New("fixture persistence failure")}
	later := time.Now().Add(time.Hour)
	repo.rateLimitedUntil = later
	svc := NewRateLimitService(repo, nil, nil, nil, nil)
	svc.handle429(t.Context(), &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth}, http.Header{"Retry-After": {"60"}}, nil)
	require.Equal(t, 1, repo.extensions)
	require.Equal(t, later, repo.rateLimitedUntil, "do not fall back to a non-atomic write after an atomic write fails")
}

func TestOpenAI429DeadlineConsumers(t *testing.T) {
	for _, tc := range []struct {
		name       string
		retryAfter string
		window     int
		bodyDelay  int
		wantDelay  time.Duration
	}{
		{"seconds", "731", 0, 0, 731 * time.Second},
		{"http_date", "date", 0, 0, 731 * time.Second},
		{"window_longer", "731", 1800, 0, 1800 * time.Second},
		{"retry_longer", "1800", 731, 0, 1800 * time.Second},
		{"body_longer", "731", 0, 1800, 1800 * time.Second},
		{"retry_longer_than_body", "1800", 0, 731, 1800 * time.Second},
		{"body_longer_than_window", "", 731, 1800, 1800 * time.Second},
		{"invalid_retry_uses_window", "NaN", 731, 0, 731 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := time.Now()
			headers := make(http.Header)
			if tc.retryAfter == "date" {
				headers.Set("Retry-After", before.Add(tc.wantDelay).UTC().Format(http.TimeFormat))
			} else {
				headers.Set("Retry-After", tc.retryAfter)
			}
			if tc.window != 0 {
				headers.Set("x-codex-primary-used-percent", "100")
				headers.Set("x-codex-primary-reset-after-seconds", fmt.Sprint(tc.window))
				headers.Set("x-codex-primary-window-minutes", "300")
			}
			var body []byte
			if tc.bodyDelay != 0 {
				body = []byte(fmt.Sprintf(`{"error":{"type":"usage_limit_reached","resets_at":%d}}`, before.Unix()+int64(tc.bodyDelay)))
			}
			repo := &openAI429SnapshotRepo{}
			svc := NewRateLimitService(repo, nil, nil, nil, nil)
			svc.handle429(t.Context(), &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth}, headers, body)
			probe := probeOpenAI429ResetTime(headers, body)
			require.NotNil(t, probe)
			_, runtime := classifyOpenAIOAuth429(headers, body)
			require.NotNil(t, runtime)
			image := openAIImageRateLimitResetAt(headers, body)
			for _, deadline := range []time.Time{repo.rateLimitedUntil, *probe, *runtime, image} {
				require.WithinRange(t, deadline, before.Add(tc.wantDelay-time.Second), time.Now().Add(tc.wantDelay))
			}
		})
	}
}

func TestOpenAIImage429DeadlineKeepsLongestHint(t *testing.T) {
	now := time.Now()
	headers := http.Header{"Retry-After": {"60"}}
	body := []byte(`{"error":{"message":"Please try again in 120 seconds"}}`)
	require.WithinRange(t, openAIImageRateLimitResetAt(headers, body), now.Add(120*time.Second), time.Now().Add(120*time.Second))
	headers.Set("Retry-After", "180")
	require.WithinRange(t, openAIImageRateLimitResetAt(headers, body), now.Add(180*time.Second), time.Now().Add(180*time.Second))
}

func TestRetryAfterResetTimeInvalidAndOverflow(t *testing.T) {
	now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	for _, raw := range []string{"NaN", "+Inf", "-Inf", "-1", "garbage"} {
		require.Nil(t, parseRetryAfterResetTime(http.Header{"Retry-After": {raw}}, now), raw)
	}
	for _, raw := range []string{"18446744073709551615", "1e100"} {
		until := parseRetryAfterResetTime(http.Header{"Retry-After": {raw}}, now)
		require.NotNil(t, until, raw)
		require.True(t, until.After(now.Add(24*time.Hour)), "overflow must not permit early retry")
	}
}

func TestOpenAI429ResetOverflowDoesNotBecomeExpired(t *testing.T) {
	h := make(http.Header)
	h.Set("x-codex-primary-used-percent", "100")
	h.Set("x-codex-primary-window-minutes", "300")
	h.Set("x-codex-primary-reset-after-seconds", "9223372036854775807")
	until := probeOpenAI429ResetTime(h, nil)
	require.NotNil(t, until)
	require.True(t, until.After(time.Now().Add(24*time.Hour)))
	for _, body := range []string{
		`{"error":{"type":"usage_limit_reached","resets_in_seconds":9223372036854775807}}`,
		`{"error":{"type":"usage_limit_reached","resets_at":1e100}}`,
	} {
		until := probeOpenAI429ResetTime(nil, []byte(body))
		require.NotNil(t, until)
		require.True(t, until.After(time.Now().Add(24*time.Hour)))
	}
	require.Nil(t, probeOpenAI429ResetTime(nil, []byte(`{"error":{"type":"usage_limit_reached","resets_in_seconds":-3}}`)))
}
