//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

const claudeAliasPricingCatalog = `{
	"claude-sonnet-4-20250514": {
		"input_cost_per_token": 0.000003,
		"output_cost_per_token": 0.000015
	},
	"claude-sonnet-4-6": {
		"input_cost_per_token": 0.000003,
		"output_cost_per_token": 0.000015,
		"input_cost_per_token_above_200k_tokens": 0.000006,
		"output_cost_per_token_above_200k_tokens": 0.0000225
	}
}`

func TestPricingLookupStability_ClaudeThinkingAlias(t *testing.T) {
	base := &LiteLLMModelPricing{InputCostPerToken: 3e-6}
	alias := &LiteLLMModelPricing{InputCostPerToken: 4e-6}
	other := &LiteLLMModelPricing{InputCostPerToken: 9e-6}
	for _, tc := range []struct {
		name    string
		model   string
		catalog map[string]*LiteLLMModelPricing
		want    *LiteLLMModelPricing
	}{
		{"base", "claude-sonnet-4-6-thinking", map[string]*LiteLLMModelPricing{
			"claude-sonnet-4-6": base, "claude-sonnet-4-20250514": other,
		}, base},
		{"models prefix", "models/claude-sonnet-4-6-thinking", map[string]*LiteLLMModelPricing{
			"claude-sonnet-4-6": base, "claude-sonnet-4-20250514": other,
		}, base},
		{"provider prefix", "anthropic/claude-sonnet-4-6-thinking", map[string]*LiteLLMModelPricing{
			"claude-sonnet-4-6": base, "claude-sonnet-4-20250514": other,
		}, base},
		{"dated base", "claude-sonnet-4-6-thinking", map[string]*LiteLLMModelPricing{
			"claude-sonnet-4-6-20260217": base, "claude-sonnet-4-20250514": other,
		}, base},
		{"explicit alias wins", "claude-sonnet-4-6-thinking", map[string]*LiteLLMModelPricing{
			"claude-sonnet-4-6": base, "claude-sonnet-4-6-thinking": alias,
		}, alias},
		{"provider alias wins", "anthropic/claude-sonnet-4-6-thinking", map[string]*LiteLLMModelPricing{
			"claude-sonnet-4-6": base, "anthropic/claude-sonnet-4-6-thinking": alias,
		}, alias},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &PricingService{pricingData: tc.catalog}
			for i := 0; i < 64; i++ {
				require.Same(t, tc.want, svc.GetModelPricing(tc.model))
				require.Same(t, tc.want, svc.GetIdentifiedModelPricing(tc.model))
			}
		})
	}
	svc := &PricingService{pricingData: map[string]*LiteLLMModelPricing{"claude-sonnet-4-6": base}}
	require.Nil(t, svc.GetIdentifiedModelPricing("claude-sonnet-unknown-thinking"))
	require.Nil(t, svc.GetIdentifiedModelPricing("claude-sonnet-4-6-thinking-forged"))
}

func TestPricingLookupStability_DateAndFamilyFallbacks(t *testing.T) {
	first := &LiteLLMModelPricing{InputCostPerToken: 3e-6}
	other := &LiteLLMModelPricing{InputCostPerToken: 9e-6}
	t.Run("date fallback", func(t *testing.T) {
		svc := &PricingService{pricingData: map[string]*LiteLLMModelPricing{
			"claude-sonnet-4-20250514": first,
			"claude-sonnet-4-20250515": other,
		}}
		for i := 0; i < 128; i++ {
			require.Same(t, first, svc.GetModelPricing("claude-sonnet-4"))
			require.Same(t, first, svc.GetIdentifiedModelPricing("claude-sonnet-4"))
		}
	})
	t.Run("family canonical before provider", func(t *testing.T) {
		svc := &PricingService{pricingData: map[string]*LiteLLMModelPricing{
			"claude-sonnet-4": first, "aaa/claude-sonnet-4": other,
		}}
		for i := 0; i < 128; i++ {
			require.Same(t, first, svc.GetModelPricing("claude-sonnet-4-preview"))
			require.Nil(t, svc.GetIdentifiedModelPricing("claude-sonnet-4-preview"))
		}
	})
	t.Run("family stable tie break", func(t *testing.T) {
		svc := &PricingService{pricingData: map[string]*LiteLLMModelPricing{
			"aaa/claude-sonnet-4": first, "bbb/claude-sonnet-4": other,
		}}
		for i := 0; i < 128; i++ {
			require.Same(t, first, svc.GetModelPricing("claude-sonnet-4-preview"))
		}
	})
}

func TestPricingLookupStability_AliasScheduleAndBilling(t *testing.T) {
	billing, resolver := newTokenCostTestEnv(t, PlatformAnthropic, nil,
		newStubPricingServiceFromJSON(t, claudeAliasPricingCatalog))
	plaza := &ModelPlazaService{billingService: billing, resolver: resolver}
	const model = "claude-sonnet-4-6-thinking"
	for i := 0; i < 64; i++ {
		official := plaza.lookupOfficialPricing(context.Background(), model, make(map[string]*PlazaOfficialPricing))
		require.NotNil(t, official)
		requirePrice(t, testPtrFloat64(3e-6), official.InputPrice, "base input")
		requirePrice(t, testPtrFloat64(15e-6), official.OutputPrice, "base output")
		require.Len(t, official.Intervals, 2)
		require.Equal(t, 200000, official.Intervals[1].MinTokens)
		requirePrice(t, testPtrFloat64(6e-6), official.Intervals[1].InputPrice, "long input")
		requirePrice(t, testPtrFloat64(22.5e-6), official.Intervals[1].OutputPrice, "long output")

		for _, group := range []*Group{nil, enabledGroup(PlatformAnthropic), disabledGroup(PlatformAnthropic)} {
			for _, inputTokens := range []int{200000, 200001} {
				cost, err := billing.CalculateTokenCostForRequest(TokenCostRequest{
					Ctx: context.Background(), Model: model, Group: group, Resolver: resolver,
					Tokens: UsageTokens{InputTokens: inputTokens, OutputTokens: 1000}, RateMultiplier: 1,
				})
				require.NoError(t, err)
				input, output := 3e-6, 15e-6
				if inputTokens > 200000 && (group == nil || group.LongContextPricingEnabled) {
					input, output = 6e-6, 22.5e-6
				}
				require.InDelta(t, float64(inputTokens)*input+1000*output, cost.ActualCost, 1e-12)
			}
		}
	}
}

func TestPricingLookupStability_PreResolvedCatalogSurvivesRefresh(t *testing.T) {
	catalog := newStubPricingServiceFromJSON(t, claudeAliasPricingCatalog)
	billing, resolver := newTokenCostTestEnv(t, PlatformAnthropic, nil, catalog)
	const model = "claude-sonnet-4-6"
	resolved := resolver.Resolve(context.Background(), PricingInput{Model: model})
	replacement := newStubPricingServiceFromJSON(t, `{
		"claude-sonnet-4-6": {"input_cost_per_token": 0.000012, "output_cost_per_token": 0.000030}
	}`)
	catalog.mu.Lock()
	catalog.pricingData = replacement.pricingData
	catalog.mu.Unlock()

	req := TokenCostRequest{
		Model: model, Resolver: resolver, Resolved: resolved, RateMultiplier: 1,
		Tokens: UsageTokens{InputTokens: 250000, OutputTokens: 1000},
	}
	cost, err := billing.CalculateTokenCostForRequest(req)
	require.NoError(t, err)
	require.InDelta(t, 250000*6e-6+1000*22.5e-6, cost.ActualCost, 1e-12)

	req.Resolved = nil
	cost, err = billing.CalculateTokenCostForRequest(req)
	require.NoError(t, err)
	require.InDelta(t, 250000*12e-6+1000*30e-6, cost.ActualCost, 1e-12)
}

func TestPricingLookupStability_PreResolvedMissingPrice(t *testing.T) {
	billing, resolver := newTokenCostTestEnv(t, PlatformAnthropic, nil,
		newStubPricingServiceFromJSON(t, claudeAliasPricingCatalog))
	cost, err := billing.CalculateTokenCostForRequest(TokenCostRequest{
		Model: "claude-sonnet-4-6", Resolver: resolver,
		Resolved: &ResolvedPricing{Source: PricingSourceLiteLLM, Mode: BillingModeToken},
		Tokens:   UsageTokens{InputTokens: 1000}, RateMultiplier: 1,
	})
	require.ErrorIs(t, err, ErrModelPricingUnavailable)
	require.Nil(t, cost)
}
