//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func fallbackTestContext(ctx context.Context, groupID int64, user *User) context.Context {
	return WithGatewayGroupAccess(ctx, &APIKey{
		ID: 10, UserID: user.ID, User: user, GroupID: &groupID,
		Group: &Group{ID: groupID, Status: StatusActive},
	})
}

func publicFallbackTestContext(ctx context.Context, groupID int64) context.Context {
	return fallbackTestContext(ctx, groupID, &User{ID: 7, Status: StatusActive})
}

func TestGatewayFallbackAccessPolicy(t *testing.T) {
	for _, tt := range []struct {
		name    string
		user    *User
		group   *Group
		allowed bool
	}{
		{"public", &User{ID: 7, Status: StatusActive}, &Group{ID: 2, Status: StatusActive}, true},
		{"exclusive_denied", &User{ID: 7, Status: StatusActive}, &Group{ID: 2, Status: StatusActive, IsExclusive: true}, false},
		{"exclusive_allowed", &User{ID: 7, Status: StatusActive, AllowedGroups: []int64{2}}, &Group{ID: 2, Status: StatusActive, IsExclusive: true}, true},
		{"restricted_public_denied", &User{ID: 7, Status: StatusActive, RestrictPublicGroups: true, AllowedGroups: []int64{1}}, &Group{ID: 2, Status: StatusActive}, false},
		{"restricted_public_allowed", &User{ID: 7, Status: StatusActive, RestrictPublicGroups: true, AllowedGroups: []int64{1, 2}}, &Group{ID: 2, Status: StatusActive}, true},
		{"inactive_user", &User{ID: 7, Status: "disabled", AllowedGroups: []int64{2}}, &Group{ID: 2, Status: StatusActive}, false},
		{"inactive_group", &User{ID: 7, Status: StatusActive}, &Group{ID: 2, Status: "disabled"}, false},
		{"missing_group", &User{ID: 7, Status: StatusActive}, nil, false},
		{"invalid_group", &User{ID: 7, Status: StatusActive}, &Group{Status: StatusActive}, false},
		{"subscription_not_inherited", &User{ID: 7, Status: StatusActive, AllowedGroups: []int64{2}}, &Group{ID: 2, Status: StatusActive, SubscriptionType: SubscriptionTypeSubscription}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := fallbackTestContext(context.Background(), 1, tt.user)
			require.Equal(t, tt.allowed, CanUseGatewayFallbackGroup(ctx, tt.group))
		})
	}
}

func TestGatewayFallbackAccessSnapshotCannotExpand(t *testing.T) {
	user := &User{ID: 7, Status: StatusActive, RestrictPublicGroups: true, AllowedGroups: []int64{2}}
	ctx := fallbackTestContext(context.Background(), 1, user)
	group2 := &Group{ID: 2, Status: StatusActive, IsExclusive: true}
	group3 := &Group{ID: 3, Status: StatusActive, IsExclusive: true}
	user.AllowedGroups[0] = 3
	require.True(t, CanUseGatewayFallbackGroup(ctx, group2))
	require.False(t, CanUseGatewayFallbackGroup(ctx, group3))
	rebound := fallbackTestContext(ctx, 2, user)
	require.False(t, CanUseGatewayFallbackGroup(rebound, group3))

	// A subsequent authenticated request uses the newly loaded grants.
	next := fallbackTestContext(context.Background(), 1, user)
	require.False(t, CanUseGatewayFallbackGroup(next, group2))
	require.True(t, CanUseGatewayFallbackGroup(next, group3))
}

func TestGatewayFallbackAccessRejectsIncompleteBinding(t *testing.T) {
	group := &Group{ID: 2, Status: StatusActive}
	for _, key := range []*APIKey{
		nil,
		{ID: 1},
		{ID: 1, UserID: 8, User: &User{ID: 7, Status: StatusActive}, GroupID: ptrInt64(1), Group: &Group{ID: 1, Status: StatusActive}},
		{ID: 1, UserID: 7, User: &User{ID: 7, Status: StatusActive}, GroupID: ptrInt64(1), Group: &Group{ID: 3, Status: StatusActive}},
	} {
		require.False(t, CanUseGatewayFallbackGroup(WithGatewayGroupAccess(context.Background(), key), group))
	}
	require.False(t, CanUseGatewayFallbackGroup(context.Background(), group))
	require.False(t, CanUseGatewayFallbackGroup(nil, group))
}

func TestOpenAIAccountPoolFallbackRequiresAuthenticatedAccess(t *testing.T) {
	groups := map[int64]*Group{
		1: {ID: 1, Platform: PlatformOpenAI, Status: StatusActive, FallbackGroupID: ptrInt64(2)},
		2: {ID: 2, Platform: PlatformOpenAI, Status: StatusActive, IsExclusive: true},
	}
	svc := newOpenAIAccountPoolFallbackService(groups, map[int64][]Account{
		2: {openAIAccountPoolTestAccount(902, 2)},
	}, nil)

	selection, _, err := svc.SelectAccountWithSchedulerForCapability(
		context.Background(), ptrInt64(1), "", "", "gpt-6-astra", nil,
		OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions,
		false, false, true,
	)
	require.Error(t, err)
	require.Nil(t, selection)
}

func TestGatewayClientFallbackRequiresAuthenticatedAccess(t *testing.T) {
	svc := &GatewayService{groupRepo: openAIAccountPoolFallbackGroupRepo{
		groups: map[int64]*Group{
			1: {ID: 1, Status: StatusActive, ClaudeCodeOnly: true, FallbackGroupID: ptrInt64(2)},
			2: {ID: 2, Status: StatusActive, IsExclusive: true},
		},
	}}
	group, id, err := svc.resolveGatewayGroup(context.Background(), ptrInt64(1))
	require.Error(t, err)
	require.Nil(t, group)
	require.Nil(t, id)
}

func TestGatewayFallbackResolversCheckEveryDestination(t *testing.T) {
	for _, restrictedPublic := range []bool{false, true} {
		for _, allowed := range []bool{false, true} {
			groups := map[int64]*Group{
				1: {ID: 1, Status: StatusActive, ClaudeCodeOnly: true, FallbackGroupID: ptrInt64(2)},
				2: {ID: 2, Status: StatusActive, ClaudeCodeOnly: true, FallbackGroupID: ptrInt64(3)},
				3: {ID: 3, Status: StatusActive, IsExclusive: !restrictedPublic},
			}
			svc := &GatewayService{groupRepo: openAIAccountPoolFallbackGroupRepo{groups: groups}}
			user := &User{ID: 7, Status: StatusActive, RestrictPublicGroups: restrictedPublic, AllowedGroups: []int64{1, 2}}
			if allowed {
				user.AllowedGroups = append(user.AllowedGroups, 3)
			}
			ctx := fallbackTestContext(context.Background(), 1, user)
			group, id, err := svc.resolveGatewayGroup(ctx, ptrInt64(1))
			direct, directErr := svc.ResolveFallbackGroupByID(ctx, 3)
			if allowed {
				require.NoError(t, err)
				require.NoError(t, directErr)
				require.Equal(t, groups[3], group)
				require.Equal(t, ptrInt64(3), id)
				require.Equal(t, groups[3], direct)
			} else {
				require.ErrorIs(t, err, ErrGroupNotAllowed)
				require.ErrorIs(t, directErr, ErrGroupNotAllowed)
				require.Nil(t, group)
				require.Nil(t, direct)
				require.Nil(t, id)
			}
		}
	}
}

func TestOpenAIFallbackChecksEveryHopAndEntryPoint(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		groups := map[int64]*Group{
			1: {ID: 1, Platform: PlatformOpenAI, Status: StatusActive, FallbackGroupID: ptrInt64(2)},
			2: {ID: 2, Platform: PlatformOpenAI, Status: StatusActive, IsExclusive: true, FallbackGroupID: ptrInt64(3)},
			3: {ID: 3, Platform: PlatformOpenAI, Status: StatusActive, IsExclusive: true},
		}
		user := &User{ID: 7, Status: StatusActive, RestrictPublicGroups: true, AllowedGroups: []int64{1, 2}}
		if allowed {
			user.AllowedGroups = append(user.AllowedGroups, 3)
		}
		ctx := fallbackTestContext(context.Background(), 1, user)
		for name, selectAccount := range map[string]func(*OpenAIGatewayService) (*Account, error){
			"scheduler": func(s *OpenAIGatewayService) (*Account, error) {
				result, _, err := s.SelectAccountWithSchedulerForCapability(ctx, ptrInt64(1), "", "", "gpt-6-astra", nil,
					OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions, false, false, true)
				if result == nil {
					return nil, err
				}
				return result.Account, err
			},
			"legacy": func(s *OpenAIGatewayService) (*Account, error) {
				return s.SelectAccountForModelWithExclusions(ctx, ptrInt64(1), "", "gpt-6-astra", nil)
			},
			"load_aware": func(s *OpenAIGatewayService) (*Account, error) {
				result, err := s.SelectAccountWithLoadAwareness(ctx, ptrInt64(1), "", "gpt-6-astra", nil)
				if result == nil {
					return nil, err
				}
				return result.Account, err
			},
		} {
			t.Run(name+map[bool]string{true: "_allowed", false: "_denied"}[allowed], func(t *testing.T) {
				svc := newOpenAIAccountPoolFallbackService(groups, map[int64][]Account{
					3: {openAIAccountPoolTestAccount(903, 3)},
				}, nil)
				account, err := selectAccount(svc)
				if allowed {
					require.NoError(t, err)
					require.NotNil(t, account)
					require.Equal(t, int64(903), account.ID)
				} else {
					require.Error(t, err)
					require.Nil(t, account)
				}
			})
		}
	}
}

func TestOpenAIAvailabilityDoesNotWaitOnUnauthorizedFallback(t *testing.T) {
	groups := map[int64]*Group{
		1: {ID: 1, Platform: PlatformOpenAI, Status: StatusActive, FallbackGroupID: ptrInt64(2)},
		2: {ID: 2, Platform: PlatformOpenAI, Status: StatusActive, IsExclusive: true},
	}
	account := openAIAccountPoolTestAccount(902, 2)
	releaseAt := time.Now().Add(time.Minute)
	account.RateLimitResetAt = &releaseAt
	svc := newOpenAIAccountPoolFallbackService(groups, map[int64][]Account{2: {account}}, nil)
	for _, allowed := range []bool{false, true} {
		user := &User{ID: 7, Status: StatusActive}
		if allowed {
			user.AllowedGroups = []int64{2}
		}
		ctx := fallbackTestContext(context.Background(), 1, user)
		at, found, err := svc.nextOpenAITransientAvailability(ctx, ptrInt64(1), "gpt-6-astra",
			OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions, "", false)
		require.NoError(t, err)
		require.Equal(t, allowed, found)
		if allowed {
			require.Equal(t, releaseAt, at)
		} else {
			require.True(t, at.IsZero())
		}
	}
}
