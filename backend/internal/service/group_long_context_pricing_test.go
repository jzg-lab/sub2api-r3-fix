//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGroupLongContextPricingValidation(t *testing.T) {
	for _, config := range []GroupLongContextPricing{
		{ThresholdTokens: -1000}, {ThresholdTokens: 128001}, {ThresholdTokens: 2147484000},
		{ModelThresholds: map[string]int{"": 128000}}, {ModelThresholds: map[string]int{"model*": 128000}},
		{ModelThresholds: map[string]int{"model": 0}}, {ModelThresholds: map[string]int{"model": 1001}},
		{ModelThresholds: map[string]int{"Model": 128000, "model": 200000}},
	} {
		_, err := normalizeGroupLongContextPricing(config)
		require.Error(t, err, "%+v", config)
	}
	config := GroupLongContextPricing{ThresholdTokens: 128000, ModelThresholds: map[string]int{" Claude-Sonnet-4.6 ": 256000}}
	normalized, err := normalizeGroupLongContextPricing(config)
	require.NoError(t, err)
	require.Equal(t, 256000, normalized.ModelThresholds["claude-sonnet-4-6"])
	normalized.ModelThresholds["claude-sonnet-4-6"] = 1000
	require.Equal(t, 256000, config.ModelThresholds[" Claude-Sonnet-4.6 "])
	_, err = normalizeGroupLongContextPricing(GroupLongContextPricing{})
	require.NoError(t, err)
}

func TestGroupLongContextPricingBoundariesAndExemptions(t *testing.T) {
	bs := newTestBillingServiceForResolver()
	base := bs.fallbackPrices["claude-sonnet-4"]
	base.LongContextInputThreshold, base.LongContextInputMultiplier, base.LongContextOutputMultiplier = 200000, 2, 1.5
	r := NewModelPricingResolver(nil, bs)
	for _, tc := range []struct {
		name                                  string
		config                                GroupLongContextPricing
		tokens                                UsageTokens
		disabled, accountOptOut, exempt, want bool
	}{
		{name: "old config", tokens: UsageTokens{InputTokens: 129000}},
		{name: "old threshold still applies", tokens: UsageTokens{InputTokens: 200001}, want: true},
		{name: "before", config: GroupLongContextPricing{ThresholdTokens: 128000}, tokens: UsageTokens{InputTokens: 127999}},
		{name: "equal", config: GroupLongContextPricing{ThresholdTokens: 128000}, tokens: UsageTokens{InputTokens: 128000, OutputTokens: 50000}},
		{name: "after", config: GroupLongContextPricing{ThresholdTokens: 128000}, tokens: UsageTokens{InputTokens: 128001}, want: true},
		{name: "cache counts", config: GroupLongContextPricing{ThresholdTokens: 128000}, tokens: UsageTokens{InputTokens: 100000, CacheReadTokens: 20000, CacheCreationTokens: 8001, OutputTokens: 1000}, want: true},
		{name: "model override", config: GroupLongContextPricing{ThresholdTokens: 128000, ModelThresholds: map[string]int{"claude-sonnet-4": 256000}}, tokens: UsageTokens{InputTokens: 200001}},
		{name: "model only", config: GroupLongContextPricing{ModelThresholds: map[string]int{"claude-sonnet-4": 128000}}, tokens: UsageTokens{InputTokens: 128001}, want: true},
		{name: "other model ignored", config: GroupLongContextPricing{ModelThresholds: map[string]int{"other-model": 128000}}, tokens: UsageTokens{InputTokens: 128001}},
		{name: "group disabled", config: GroupLongContextPricing{ThresholdTokens: 128000}, tokens: UsageTokens{InputTokens: 300000}, disabled: true},
		{name: "account opt out", config: GroupLongContextPricing{ThresholdTokens: 128000}, tokens: UsageTokens{InputTokens: 300000}, accountOptOut: true},
		{name: "subscription exempt", config: GroupLongContextPricing{ThresholdTokens: 128000}, tokens: UsageTokens{InputTokens: 300000}, exempt: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group := &Group{LongContextPricingEnabled: !tc.disabled, LongContextPricing: tc.config}
			input := CostInput{Ctx: context.Background(), Model: "claude-sonnet-4", Group: group, Tokens: tc.tokens, RateMultiplier: 2.5, Resolver: r, DisableModelLongContextPricing: tc.exempt}
			if tc.accountOptOut {
				flag := false
				input.LongContextBillingEnabled = &flag
			}
			cost, err := bs.CalculateCostUnified(input)
			require.NoError(t, err)
			require.Equal(t, tc.want, cost.LongContextBillingApplied)
			inMult, outMult := 1.0, 1.0
			if tc.want {
				inMult, outMult = 2, 1.5
			}
			require.InDelta(t, float64(tc.tokens.InputTokens)*3e-6*inMult, cost.InputCost, 1e-12)
			require.InDelta(t, float64(tc.tokens.OutputTokens)*15e-6*outMult, cost.OutputCost, 1e-12)
			require.InDelta(t, float64(tc.tokens.CacheReadTokens)*.3e-6*inMult, cost.CacheReadCost, 1e-12)
			require.InDelta(t, float64(tc.tokens.CacheCreationTokens)*3.75e-6*inMult, cost.CacheCreationCost, 1e-12)
			require.InDelta(t, cost.TotalCost*2.5, cost.ActualCost, 1e-12)
			require.Equal(t, 200000, base.LongContextInputThreshold)
		})
	}
	base.LongContextThresholdInclusive = true
	group := &Group{LongContextPricingEnabled: true, LongContextPricing: GroupLongContextPricing{ThresholdTokens: 128000}}
	cost, err := bs.CalculateCostUnified(CostInput{Model: "claude-sonnet-4", Group: group, Tokens: UsageTokens{InputTokens: 128000}, RateMultiplier: 1, Resolver: r})
	require.NoError(t, err)
	require.True(t, cost.LongContextBillingApplied, "inclusive model boundary is preserved")
}

func TestGroupLongContextPricingResolverAndSchedule(t *testing.T) {
	bs := newTestBillingServiceForResolver()
	base := bs.fallbackPrices["claude-sonnet-4"]
	base.LongContextInputThreshold, base.LongContextInputMultiplier, base.LongContextOutputMultiplier = 200000, 2, 1.5
	r := NewModelPricingResolver(nil, bs)
	group := &Group{ID: 7, LongContextPricingEnabled: true, LongContextPricing: GroupLongContextPricing{ThresholdTokens: 128000},
		ModelPricing: []ChannelModelPricing{{Models: []string{"claude-sonnet-4"}, BillingMode: BillingModeToken, InputPrice: testPtrFloat64(1e-6)}}}
	schedule, err := bs.ResolveContextPricingSchedule(context.Background(), r, ContextPricingScheduleInput{Model: "claude-sonnet-4", Group: group})
	require.NoError(t, err)
	require.Len(t, schedule.Tiers, 2)
	require.Equal(t, 128000, *schedule.Tiers[0].MaxTokens)
	require.InDelta(t, 1e-6, *schedule.Tiers[0].Input, 1e-12)
	require.InDelta(t, 2e-6, *schedule.Tiers[1].Input, 1e-12)
	require.InDelta(t, 22.5e-6, *schedule.Tiers[1].Output, 1e-12)
	reference, err := bs.ResolveContextPricingSchedule(context.Background(), r, ContextPricingScheduleInput{Model: "claude-sonnet-4"})
	require.NoError(t, err)
	require.Equal(t, 200000, *reference.Tiers[0].MaxTokens, "reference catalog is not affected")

	for _, tc := range []struct {
		name      string
		pricing   ModelPricing
		intervals []PricingInterval
		mode      BillingMode
	}{
		{name: "no threshold", pricing: ModelPricing{LongContextInputMultiplier: 2}},
		{name: "no surcharge", pricing: ModelPricing{LongContextInputThreshold: 200000, LongContextInputMultiplier: 1, LongContextOutputMultiplier: 1}},
		{name: "channel tiers", pricing: *base, intervals: []PricingInterval{{MinTokens: 0, InputPrice: testPtrFloat64(5e-6)}}},
		{name: "per request", pricing: *base, mode: BillingModePerRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolved := &ResolvedPricing{BasePricing: &tc.pricing, Mode: tc.mode, Intervals: tc.intervals}
			applyGroupLongContextThreshold(group, "claude-sonnet-4", resolved)
			require.Equal(t, tc.pricing, *resolved.BasePricing)
		})
	}
	base.LongContextInputThreshold = 0
	group.ModelPricing = nil
	flat, err := bs.ResolveContextPricingSchedule(context.Background(), r, ContextPricingScheduleInput{Model: "claude-sonnet-4", Group: group})
	require.NoError(t, err)
	require.Len(t, flat.Tiers, 1, "flat-rate models do not gain a surcharge")
}

func TestAdminGroupLongContextPricingLifecycle(t *testing.T) {
	repo := &groupRepoStubForAdmin{createID: 30}
	svc := &adminServiceImpl{groupRepo: repo}
	config := GroupLongContextPricing{ThresholdTokens: 128000, ModelThresholds: map[string]int{" CLAUDE-SONNET-4 ": 256000}}
	group, err := svc.CreateGroup(context.Background(), &CreateGroupInput{Name: "threshold", Platform: PlatformAnthropic, RateMultiplier: 1, LongContextPricingEnabled: true, LongContextPricing: config})
	require.NoError(t, err)
	require.Equal(t, 256000, repo.created.LongContextPricing.ModelThresholds["claude-sonnet-4"])
	repo.getByID = group
	_, err = svc.UpdateGroup(context.Background(), group.ID, &UpdateGroupInput{Name: "unchanged-config"})
	require.NoError(t, err)
	require.Equal(t, 128000, repo.updated.LongContextPricing.ThresholdTokens)
	copy := cloneGroupForDuplicate(group, "copy")
	require.Equal(t, group.LongContextPricing, copy.LongContextPricing)
	require.True(t, copy.LongContextPricingEnabled)
	copy.LongContextPricing.ModelThresholds["claude-sonnet-4"] = 1000
	require.Equal(t, 256000, group.LongContextPricing.ModelThresholds["claude-sonnet-4"])
	_, err = svc.UpdateGroup(context.Background(), group.ID, &UpdateGroupInput{LongContextPricing: &GroupLongContextPricing{}})
	require.NoError(t, err)
	require.Zero(t, repo.updated.LongContextPricing.ThresholdTokens)
	require.Empty(t, repo.updated.LongContextPricing.ModelThresholds)
	invalid := GroupLongContextPricing{ThresholdTokens: -1}
	_, err = svc.CreateGroup(context.Background(), &CreateGroupInput{RateMultiplier: 1, LongContextPricing: invalid})
	require.Error(t, err)
	_, err = svc.UpdateGroup(context.Background(), group.ID, &UpdateGroupInput{LongContextPricing: &invalid})
	require.Error(t, err)
}

func TestGroupLongContextPricingChannelPrecedence(t *testing.T) {
	for _, withIntervals := range []bool{false, true} {
		card := ChannelModelPricing{Platform: PlatformAnthropic, Models: []string{"claude-sonnet-4"}, BillingMode: BillingModeToken, InputPrice: testPtrFloat64(1e-6)}
		if withIntervals {
			card.Intervals = []PricingInterval{{MinTokens: 0, MaxTokens: testPtrInt(256000), InputPrice: testPtrFloat64(4e-6)}, {MinTokens: 256000, InputPrice: testPtrFloat64(8e-6)}}
		}
		r := newResolverWithChannel(t, []ChannelModelPricing{card})
		base := r.billingService.fallbackPrices["claude-sonnet-4"]
		base.LongContextInputThreshold, base.LongContextInputMultiplier, base.LongContextOutputMultiplier = 200000, 2, 1.5
		g := &Group{ID: *groupIDPtr(), LongContextPricingEnabled: true, LongContextPricing: GroupLongContextPricing{ThresholdTokens: 128000}}
		cost, err := r.billingService.CalculateTokenCostForRequest(TokenCostRequest{Ctx: context.Background(), Model: "claude-sonnet-4", Group: g, Tokens: UsageTokens{InputTokens: 129000}, RateMultiplier: 1, Resolver: r})
		require.NoError(t, err)
		if withIntervals {
			require.InDelta(t, .516, cost.InputCost, 1e-12, "explicit channel interval is preserved without a second surcharge")
		} else {
			require.InDelta(t, .258, cost.InputCost, 1e-12, "flat channel rate uses the group's model threshold")
		}
	}
}
