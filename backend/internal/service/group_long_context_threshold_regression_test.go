//go:build unit

package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGroupLongContextThresholdJSONConfig(t *testing.T) {
	bs := newTestBillingServiceForResolver()
	base := bs.fallbackPrices["claude-sonnet-4"]
	base.LongContextInputThreshold = 200000
	base.LongContextInputMultiplier = 2
	base.LongContextOutputMultiplier = 1.5
	var group Group
	require.NoError(t, json.Unmarshal([]byte(`{"LongContextPricingEnabled":true,"LongContextPricing":{"threshold_tokens":128000}}`), &group))
	cost, err := bs.CalculateTokenCostForRequest(TokenCostRequest{
		Ctx: context.Background(), Model: "claude-sonnet-4", Group: &group,
		Tokens: UsageTokens{InputTokens: 129000, OutputTokens: 1000}, RateMultiplier: 1,
		Resolver: NewModelPricingResolver(nil, bs),
	})
	require.NoError(t, err)
	t.Logf("input=129000 output=1000 group_threshold=128000 model_threshold=200000 input_cost=%.6f output_cost=%.6f surcharge=%t", cost.InputCost, cost.OutputCost, cost.LongContextBillingApplied)
	require.True(t, cost.LongContextBillingApplied)
	require.InDelta(t, .774, cost.InputCost, 1e-12)
	require.InDelta(t, .0225, cost.OutputCost, 1e-12)
	require.Equal(t, 200000, base.LongContextInputThreshold, "shared catalog must remain unchanged")
}
