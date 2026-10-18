package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestOpenAIScheduler_OAuthCacheAffinity(t *testing.T) {
	for _, tc := range []struct {
		name        string
		accountType string
		slow        bool
		failing     bool
		full        bool
		excluded    bool
		wantSticky  bool
	}{
		{name: "oauth_slow_healthy", accountType: AccountTypeOAuth, slow: true, wantSticky: true},
		{name: "setup_token_slow_healthy", accountType: AccountTypeSetupToken, slow: true, wantSticky: true},
		{name: "oauth_fast_healthy", accountType: AccountTypeOAuth, wantSticky: true},
		{name: "oauth_slow_failing", accountType: AccountTypeOAuth, slow: true, failing: true, wantSticky: false},
		{name: "setup_token_slow_failing", accountType: AccountTypeSetupToken, slow: true, failing: true, wantSticky: false},
		{name: "oauth_failing", accountType: AccountTypeOAuth, failing: true, wantSticky: false},
		{name: "oauth_slow_full", accountType: AccountTypeOAuth, slow: true, full: true, wantSticky: false},
		{name: "setup_token_slow_full", accountType: AccountTypeSetupToken, slow: true, full: true, wantSticky: false},
		{name: "oauth_slow_excluded", accountType: AccountTypeOAuth, slow: true, excluded: true},
		{name: "api_key_slow_healthy", accountType: AccountTypeAPIKey, slow: true, wantSticky: true},
		{name: "api_key_other_conversation_failures", accountType: AccountTypeAPIKey, failing: true, wantSticky: true},
		{name: "api_key_fast_legacy_sticky", accountType: AccountTypeAPIKey, wantSticky: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const stickyID, otherID = int64(21701), int64(21702)
			groupID := int64(10107)
			accounts := []Account{
				{ID: stickyID, Platform: PlatformOpenAI, Type: tc.accountType, Status: StatusActive,
					Schedulable: true, Concurrency: 1, Priority: 0, GroupIDs: []int64{groupID}},
				{ID: otherID, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive,
					Schedulable: true, Concurrency: 1, Priority: 1, GroupIDs: []int64{groupID}},
			}
			cache := &schedulerTestGatewayCache{
				sessionBindings: map[string]int64{"openai:cache_affinity": stickyID},
			}
			cfg := &config.Config{}
			cfg.Gateway.OpenAIScheduler.StickyEscapeEnabled = true
			cfg.Gateway.OpenAIScheduler.StickyEscapeTTFTMs = 15000
			cfg.Gateway.OpenAIScheduler.StickyEscapeErrorRate = 0.5
			var acquiredIDs []int64
			svc := &OpenAIGatewayService{
				accountRepo:      schedulerTestOpenAIAccountRepo{accounts: accounts},
				cache:            cache,
				cfg:              cfg,
				rateLimitService: newOpenAIAdvancedSchedulerRateLimitService("true"),
				concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{
					acquireResults: map[int64]bool{stickyID: !tc.full, otherID: true},
					acquiredIDs:    &acquiredIDs,
				}),
				openaiAccountStats: newOpenAIAccountRuntimeStats(),
			}
			ttft := 5000
			if tc.slow {
				ttft = 45000
			}
			for i := 0; i < 10; i++ {
				svc.openaiAccountStats.report(stickyID, !tc.failing, &ttft)
			}
			var excluded map[int64]struct{}
			if tc.excluded {
				excluded = map[int64]struct{}{stickyID: {}}
			}
			selection, decision, err := svc.SelectAccountWithScheduler(
				context.Background(), &groupID, "", "cache_affinity", "gpt-6-astra",
				excluded, OpenAIUpstreamTransportAny, false,
			)
			require.NoError(t, err)
			require.NotNil(t, selection)
			require.NotNil(t, selection.Account)
			if selection.ReleaseFunc != nil {
				defer selection.ReleaseFunc()
			}
			require.Equal(t, tc.wantSticky, decision.StickySessionHit)
			if tc.wantSticky {
				require.Equal(t, stickyID, selection.Account.ID)
				require.Equal(t, openAIAccountScheduleLayerSessionSticky, decision.Layer)
			} else {
				require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
			}
			if tc.excluded {
				require.Equal(t, otherID, selection.Account.ID)
				require.NotContains(t, acquiredIDs, stickyID)
			} else {
				// Temporary escape preserves the original cache/session owner.
				require.Equal(t, stickyID, cache.sessionBindings["openai:cache_affinity"])
			}
			if tc.wantSticky {
				require.Equal(t, []int64{stickyID}, acquiredIDs)
			}
		})
	}
}

func TestOpenAIScheduler_CacheAffinityNeverMasksHealthFailure(t *testing.T) {
	stats := newOpenAIAccountRuntimeStats()
	const accountID = int64(21703)
	ttft := 60000
	stats.report(accountID, true, &ttft)
	scheduler := &defaultOpenAIAccountScheduler{stats: stats}
	cfg := openAIStickyEscapeConfig{
		enabled: true, ttftMs: 15000, errorRate: 0.5, preserveCacheAffinity: true,
	}

	reason, _, observedTTFT, escape := scheduler.shouldEscapeStickyAccount(accountID, cfg)
	require.False(t, escape)
	require.Empty(t, reason)
	require.Equal(t, float64(ttft), observedTTFT)
	for i := 0; i < 4; i++ {
		stats.report(accountID, false, nil)
	}
	reason, _, observedTTFT, escape = scheduler.shouldEscapeStickyAccount(accountID, cfg)
	require.True(t, escape)
	require.Equal(t, "error_rate", reason)
	require.Equal(t, float64(ttft), observedTTFT)

	cfg.enabled = false
	_, _, _, escape = scheduler.shouldEscapeStickyAccount(accountID, cfg)
	require.False(t, escape)
}

func TestOpenAIScheduler_OAuthStickyRetainsCacheWithFasterPeer(t *testing.T) {
	ctx := context.Background()
	const (
		stickyID = int64(21705)
		peerID   = int64(21706)
	)
	groupID := int64(21704)
	cache := &schedulerTestGatewayCache{
		sessionBindings: map[string]int64{"openai:peer_ttft": stickyID},
	}
	cfg := &config.Config{}
	cfg.Gateway.OpenAIScheduler.StickyEscapeEnabled = true
	cfg.Gateway.OpenAIScheduler.StickyEscapeTTFTMs = 15000
	cfg.Gateway.OpenAIScheduler.StickyEscapeErrorRate = 0.5
	svc := &OpenAIGatewayService{
		accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{
			{
				ID:          stickyID,
				Platform:    PlatformOpenAI,
				Type:        AccountTypeOAuth,
				Status:      StatusActive,
				Schedulable: true,
				Concurrency: 1,
				Priority:    0,
				GroupIDs:    []int64{groupID},
			},
			{
				ID:          peerID,
				Platform:    PlatformOpenAI,
				Type:        AccountTypeOAuth,
				Status:      StatusActive,
				Schedulable: true,
				Concurrency: 1,
				Priority:    1,
				GroupIDs:    []int64{groupID},
			},
		}},
		cache:            cache,
		cfg:              cfg,
		rateLimitService: newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{
			acquireResults: map[int64]bool{stickyID: true, peerID: true},
		}),
		openaiAccountStats: newOpenAIAccountRuntimeStats(),
	}
	stickyTTFT := 12000
	peerTTFT := 4000
	svc.openaiAccountStats.report(stickyID, true, &stickyTTFT)
	svc.openaiAccountStats.report(peerID, true, &peerTTFT)

	selection, decision, err := svc.SelectAccountWithScheduler(
		ctx, &groupID, "", "peer_ttft", "gpt-6-astra", nil,
		OpenAIUpstreamTransportAny, false,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, stickyID, selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerSessionSticky, decision.Layer)
	require.True(t, decision.StickySessionHit)
	require.Equal(t, stickyID, cache.sessionBindings["openai:peer_ttft"])
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestOpenAIScheduler_OAuthStickyRetainsWhenPeerIsNotClearlyFaster(t *testing.T) {
	ctx := context.Background()
	const (
		stickyID = int64(21707)
		peerID   = int64(21708)
	)
	groupID := int64(21709)
	cache := &schedulerTestGatewayCache{
		sessionBindings: map[string]int64{"openai:peer_ttft_boundary": stickyID},
	}
	cfg := &config.Config{}
	cfg.Gateway.OpenAIScheduler.StickyEscapeEnabled = true
	cfg.Gateway.OpenAIScheduler.StickyEscapeTTFTMs = 15000
	cfg.Gateway.OpenAIScheduler.StickyEscapeErrorRate = 0.5
	svc := &OpenAIGatewayService{
		accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{
			{
				ID:          stickyID,
				Platform:    PlatformOpenAI,
				Type:        AccountTypeOAuth,
				Status:      StatusActive,
				Schedulable: true,
				Concurrency: 1,
				Priority:    0,
				GroupIDs:    []int64{groupID},
			},
			{
				ID:          peerID,
				Platform:    PlatformOpenAI,
				Type:        AccountTypeOAuth,
				Status:      StatusActive,
				Schedulable: true,
				Concurrency: 1,
				Priority:    1,
				GroupIDs:    []int64{groupID},
			},
		}},
		cache:            cache,
		cfg:              cfg,
		rateLimitService: newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{
			acquireResults: map[int64]bool{stickyID: true, peerID: true},
		}),
		openaiAccountStats: newOpenAIAccountRuntimeStats(),
	}
	stickyTTFT := 12000
	peerTTFT := 9000
	svc.openaiAccountStats.report(stickyID, true, &stickyTTFT)
	svc.openaiAccountStats.report(peerID, true, &peerTTFT)

	selection, decision, err := svc.SelectAccountWithScheduler(
		ctx, &groupID, "", "peer_ttft_boundary", "gpt-6-astra", nil,
		OpenAIUpstreamTransportAny, false,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, stickyID, selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerSessionSticky, decision.Layer)
	require.True(t, decision.StickySessionHit)
	require.Equal(t, stickyID, cache.sessionBindings["openai:peer_ttft_boundary"])
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestOpenAIScheduler_OAuthStickyRetainsCacheAtPeerSpeedBoundary(t *testing.T) {
	ctx := context.Background()
	const (
		stickyID = int64(21710)
		peerID   = int64(21711)
	)
	groupID := int64(21712)
	cache := &schedulerTestGatewayCache{
		sessionBindings: map[string]int64{"openai:peer_ttft_exact": stickyID},
	}
	cfg := &config.Config{}
	cfg.Gateway.OpenAIScheduler.StickyEscapeEnabled = true
	cfg.Gateway.OpenAIScheduler.StickyEscapeTTFTMs = 15000
	cfg.Gateway.OpenAIScheduler.StickyEscapeErrorRate = 0.5
	svc := &OpenAIGatewayService{
		accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{
			{
				ID:          stickyID,
				Platform:    PlatformOpenAI,
				Type:        AccountTypeOAuth,
				Status:      StatusActive,
				Schedulable: true,
				Concurrency: 1,
				Priority:    0,
				GroupIDs:    []int64{groupID},
			},
			{
				ID:          peerID,
				Platform:    PlatformOpenAI,
				Type:        AccountTypeOAuth,
				Status:      StatusActive,
				Schedulable: true,
				Concurrency: 1,
				Priority:    1,
				GroupIDs:    []int64{groupID},
			},
		}},
		cache:            cache,
		cfg:              cfg,
		rateLimitService: newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{
			acquireResults: map[int64]bool{stickyID: true, peerID: true},
		}),
		openaiAccountStats: newOpenAIAccountRuntimeStats(),
	}
	stickyTTFT := 12000
	peerTTFT := 8000
	svc.openaiAccountStats.report(stickyID, true, &stickyTTFT)
	svc.openaiAccountStats.report(peerID, true, &peerTTFT)

	selection, decision, err := svc.SelectAccountWithScheduler(
		ctx, &groupID, "", "peer_ttft_exact", "gpt-6-astra", nil,
		OpenAIUpstreamTransportAny, false,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, stickyID, selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerSessionSticky, decision.Layer)
	require.Equal(t, stickyID, cache.sessionBindings["openai:peer_ttft_exact"])
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestOpenAIScheduler_OAuthStickyRetainsWithoutObservedPeer(t *testing.T) {
	ctx := context.Background()
	const stickyID = int64(21713)
	groupID := int64(21714)
	cache := &schedulerTestGatewayCache{
		sessionBindings: map[string]int64{"openai:peer_ttft_unknown": stickyID},
	}
	cfg := &config.Config{}
	cfg.Gateway.OpenAIScheduler.StickyEscapeEnabled = true
	cfg.Gateway.OpenAIScheduler.StickyEscapeTTFTMs = 15000
	cfg.Gateway.OpenAIScheduler.StickyEscapeErrorRate = 0.5
	svc := &OpenAIGatewayService{
		accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{{
			ID:          stickyID,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeOAuth,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    0,
			GroupIDs:    []int64{groupID},
		}}},
		cache:            cache,
		cfg:              cfg,
		rateLimitService: newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{
			acquireResults: map[int64]bool{stickyID: true},
		}),
		openaiAccountStats: newOpenAIAccountRuntimeStats(),
	}
	stickyTTFT := 12000
	svc.openaiAccountStats.report(stickyID, true, &stickyTTFT)

	selection, decision, err := svc.SelectAccountWithScheduler(
		ctx, &groupID, "", "peer_ttft_unknown", "gpt-6-astra", nil,
		OpenAIUpstreamTransportAny, false,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, stickyID, selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerSessionSticky, decision.Layer)
	require.True(t, decision.StickySessionHit)
	require.Equal(t, stickyID, cache.sessionBindings["openai:peer_ttft_unknown"])
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestOpenAIScheduler_PeerTTFTDoesNotMoveHealthyCachedSession(t *testing.T) {
	const stickyID, fastID, slowID = int64(21715), int64(21716), int64(21717)
	groupID := int64(21718)
	accounts := []Account{
		{ID: stickyID, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
			Schedulable: true, Concurrency: 1, Priority: 0, GroupIDs: []int64{groupID}},
		{ID: fastID, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
			Schedulable: true, Concurrency: 1, Priority: 100, GroupIDs: []int64{groupID}},
		{ID: slowID, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
			Schedulable: true, Concurrency: 1, Priority: -100, GroupIDs: []int64{groupID}},
	}
	cache := &schedulerTestGatewayCache{
		sessionBindings: map[string]int64{"openai:peer_ttft_three_accounts": stickyID},
	}
	cfg := &config.Config{}
	cfg.Gateway.OpenAIScheduler.StickyEscapeEnabled = true
	cfg.Gateway.OpenAIScheduler.StickyEscapeTTFTMs = 15000
	cfg.Gateway.OpenAIScheduler.StickyEscapeErrorRate = 0.5
	cfg.Gateway.OpenAIWS.LBTopK = 1
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Priority = 1
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              cache,
		cfg:                cfg,
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		openaiAccountStats: newOpenAIAccountRuntimeStats(),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{
			acquireResults: map[int64]bool{stickyID: true, fastID: true, slowID: true},
		}),
	}
	for id, ttft := range map[int64]int{stickyID: 12000, fastID: 4000, slowID: 30000} {
		svc.openaiAccountStats.report(id, true, &ttft)
	}
	selection, _, err := svc.SelectAccountWithScheduler(context.Background(), &groupID, "",
		"peer_ttft_three_accounts", "gpt-6-astra", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	if selection.ReleaseFunc != nil {
		defer selection.ReleaseFunc()
	}
	require.Equal(t, stickyID, selection.Account.ID)
	require.Equal(t, stickyID, cache.sessionBindings["openai:peer_ttft_three_accounts"])
}

func TestOpenAIScheduler_HealthEscapeCannotReselectFailedStickyAccount(t *testing.T) {
	const stickyID, healthyID = int64(21719), int64(21720)
	groupID := int64(21721)
	cache := &schedulerTestGatewayCache{
		sessionBindings: map[string]int64{"openai:failed_sticky": stickyID},
	}
	cfg := &config.Config{}
	cfg.Gateway.OpenAIScheduler.StickyEscapeEnabled = true
	cfg.Gateway.OpenAIScheduler.StickyEscapeTTFTMs = 15000
	cfg.Gateway.OpenAIScheduler.StickyEscapeErrorRate = 0.5
	cfg.Gateway.OpenAIWS.LBTopK = 1
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Priority = 1
	var acquiredIDs []int64
	svc := &OpenAIGatewayService{
		accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{
			{ID: stickyID, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
				Schedulable: true, Concurrency: 1, Priority: 0, GroupIDs: []int64{groupID}},
			{ID: healthyID, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
				Schedulable: true, Concurrency: 1, Priority: 100, GroupIDs: []int64{groupID}},
		}},
		cache:              cache,
		cfg:                cfg,
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		openaiAccountStats: newOpenAIAccountRuntimeStats(),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{
			acquireResults: map[int64]bool{stickyID: true, healthyID: true},
			acquiredIDs:    &acquiredIDs,
		}),
	}
	for i := 0; i < 10; i++ {
		svc.openaiAccountStats.report(stickyID, false, nil)
	}
	excluded := map[int64]struct{}{999999: {}}
	selection, decision, err := svc.SelectAccountWithScheduler(context.Background(), &groupID, "",
		"failed_sticky", "gpt-6-astra", excluded, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	if selection.ReleaseFunc != nil {
		defer selection.ReleaseFunc()
	}
	require.Equal(t, healthyID, selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
	require.Equal(t, []int64{healthyID}, acquiredIDs)
	require.Equal(t, map[int64]struct{}{999999: {}}, excluded)
	require.Equal(t, stickyID, cache.sessionBindings["openai:failed_sticky"])
}
