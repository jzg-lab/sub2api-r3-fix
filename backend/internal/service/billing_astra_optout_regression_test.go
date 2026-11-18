package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestAstraLongContextFollowsGroupAcrossServiceTiers(t *testing.T) {
	billing := NewBillingService(&config.Config{}, nil)
	resolver := NewModelPricingResolver(nil, billing)
	for _, tier := range []string{"", "priority", "flex"} {
		for name, enabled := range map[string]bool{"disabled": false, "enabled": true} {
			t.Run(name+"/"+tier, func(t *testing.T) {
				tokens := UsageTokens{InputTokens: 815, CacheReadTokens: 302592, OutputTokens: 709}
				result, err := billing.CalculateCostUnified(CostInput{
					Ctx: context.Background(), Model: "gpt-6-astra", Tokens: tokens,
					RateMultiplier: 1, ServiceTier: tier, Resolver: resolver,
					Group: &Group{LongContextPricingEnabled: enabled},
				})
				require.NoError(t, err)
				expected, err := billing.calculateCostInternalWithPolicy(
					"gpt-6-astra", tokens, 1, tier, nil, enabled,
				)
				require.NoError(t, err)
				require.InDelta(t, expected.InputCost, result.InputCost, 1e-10)
				require.InDelta(t, expected.OutputCost, result.OutputCost, 1e-10)
				require.InDelta(t, expected.CacheReadCost, result.CacheReadCost, 1e-10)
				require.InDelta(t, expected.TotalCost, result.TotalCost, 1e-10)
				require.Equal(t, expected.LongContextBillingApplied, result.LongContextBillingApplied)
				if !result.LongContextBillingApplied && tier == "" {
					// Production row 194717 previously recorded $0.674659.
					require.InDelta(t, 0.346192, result.TotalCost, 1e-10)
				}
			})
		}
	}
}
