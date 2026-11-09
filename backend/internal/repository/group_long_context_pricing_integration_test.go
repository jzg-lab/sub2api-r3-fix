//go:build integration

package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestGroupLongContextPricingPersistence(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newGroupRepositoryWithSQL(tx.Client(), tx)
	g := &service.Group{Name: "context-threshold", Platform: service.PlatformAnthropic, RateMultiplier: 1, Status: service.StatusActive,
		SubscriptionType: service.SubscriptionTypeStandard, LongContextPricingEnabled: true,
		LongContextPricing: service.GroupLongContextPricing{ThresholdTokens: 128000, ModelThresholds: map[string]int{"claude-sonnet-4": 256000}}}
	require.NoError(t, repo.Create(ctx, g))
	loaded, err := repo.GetByID(ctx, g.ID)
	require.NoError(t, err)
	require.Equal(t, g.LongContextPricing, loaded.LongContextPricing)
	lite, err := repo.GetByIDLite(ctx, g.ID)
	require.NoError(t, err)
	require.Equal(t, g.LongContextPricing, lite.LongContextPricing)
	user, err := tx.Client().User.Create().SetEmail("context-threshold@example.test").SetPasswordHash("test-only").Save(ctx)
	require.NoError(t, err)
	keys := newAPIKeyRepositoryWithSQL(tx.Client(), tx)
	key := &service.APIKey{UserID: user.ID, Key: "sk-context-threshold-test", Name: "threshold", Status: service.StatusActive, GroupID: &g.ID}
	require.NoError(t, keys.Create(ctx, key))
	auth, err := keys.GetByKeyForAuth(ctx, key.Key)
	require.NoError(t, err)
	require.NotNil(t, auth.Group)
	require.Equal(t, g.LongContextPricing, auth.Group.LongContextPricing, "fast auth projection must include the new configuration")
	g.LongContextPricing = service.GroupLongContextPricing{}
	require.NoError(t, repo.Update(ctx, g))
	loaded, err = repo.GetByID(ctx, g.ID)
	require.NoError(t, err)
	require.Empty(t, loaded.LongContextPricing.ModelThresholds)
	require.Zero(t, loaded.LongContextPricing.ThresholdTokens)
}
