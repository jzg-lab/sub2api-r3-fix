package service

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/stretchr/testify/require"
)

func TestGemini36FlashPromotionExpiresAtConfiguredDate(t *testing.T) {
	svc := NewBillingService(&config.Config{}, nil)
	loc := timezone.Location()

	before, err := svc.getModelPricingAt("gemini-3.6-flash-high", time.Date(2026, 12, 31, 23, 59, 59, 0, loc))
	require.NoError(t, err)
	require.InDelta(t, gemini36FlashPromoInput, before.InputPricePerToken, 1e-15)
	require.InDelta(t, gemini36FlashPromoOutput, before.OutputPricePerToken, 1e-15)
	require.InDelta(t, gemini36FlashPromoCacheRead, before.CacheReadPricePerToken, 1e-15)

	atEnd, err := svc.getModelPricingAt("gemini-3.6-flash", time.Date(2027, 1, 1, 0, 0, 0, 0, loc))
	require.NoError(t, err)
	require.InDelta(t, gemini36FlashStandardInput, atEnd.InputPricePerToken, 1e-15)
	require.InDelta(t, gemini36FlashStandardOutput, atEnd.OutputPricePerToken, 1e-15)
	require.InDelta(t, gemini36FlashStandardCacheRead, atEnd.CacheReadPricePerToken, 1e-15)
}

func TestGemini4ArgonPromotionExpiresAtConfiguredDate(t *testing.T) {
	svc := NewBillingService(&config.Config{}, nil)
	loc := timezone.Location()

	before, err := svc.getModelPricingAt("gemini-4-argon-high", time.Date(2026, 11, 30, 23, 59, 59, 0, loc))
	require.NoError(t, err)
	require.InDelta(t, 2e-6, before.InputPricePerToken, 1e-15)
	require.InDelta(t, 10e-6, before.OutputPricePerToken, 1e-15)
	require.InDelta(t, 0.1e-6, before.CacheReadPricePerToken, 1e-15)

	atEnd, err := svc.getModelPricingAt("gemini-4-argon", time.Date(2026, 12, 1, 0, 0, 0, 0, loc))
	require.NoError(t, err)
	require.InDelta(t, 4e-6, atEnd.InputPricePerToken, 1e-15)
	require.InDelta(t, 20e-6, atEnd.OutputPricePerToken, 1e-15)
	require.InDelta(t, 0.2e-6, atEnd.CacheReadPricePerToken, 1e-15)
}

func TestGemini36FlashPromotionAppliesToCatalogAndKeepsCustomPricing(t *testing.T) {
	standard := &LiteLLMModelPricing{
		InputCostPerToken:               gemini36FlashStandardInput,
		OutputCostPerToken:              gemini36FlashStandardOutput,
		CacheReadInputTokenCost:         gemini36FlashStandardCacheRead,
		InputCostPerTokenPriority:       gemini36FlashStandardInput * gemini36FlashPriorityRatio,
		OutputCostPerTokenPriority:      gemini36FlashStandardOutput * gemini36FlashPriorityRatio,
		CacheReadInputTokenCostPriority: gemini36FlashStandardCacheRead * gemini36FlashPriorityRatio,
	}
	pricingSvc := &PricingService{pricingData: map[string]*LiteLLMModelPricing{"gemini-3.6-flash": standard}}
	svc := NewBillingService(&config.Config{}, pricingSvc)
	loc := timezone.Location()

	promo, err := svc.getModelPricingAt("gemini-3.6-flash", time.Date(2026, 12, 1, 0, 0, 0, 0, loc))
	require.NoError(t, err)
	require.InDelta(t, gemini36FlashPromoInput, promo.InputPricePerToken, 1e-15)
	require.InDelta(t, gemini36FlashPromoInput*gemini36FlashPriorityRatio, promo.InputPricePerTokenPriority, 1e-15)

	custom := &ModelPricing{InputPricePerToken: 9e-6, OutputPricePerToken: 10e-6, CacheReadPricePerToken: 1e-6}
	got := svc.applyModelSpecificPricingPolicyEx("gemini-3.6-flash", custom, false, time.Date(2027, 1, 1, 0, 0, 0, 0, loc))
	require.Same(t, custom, got)
}
