//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type plazaAccountRepository struct {
	AccountRepository
	accounts []Account
	err      error
	reads    int
}

func (r *plazaAccountRepository) ListActive(context.Context) ([]Account, error) {
	r.reads++
	return r.accounts, r.err
}

func plazaAccount(platform string, groupIDs []int64, mapping map[string]any) Account {
	return Account{Platform: platform, Type: AccountTypeAPIKey, Status: StatusActive,
		Schedulable: true, GroupIDs: groupIDs, Credentials: map[string]any{"model_mapping": mapping}}
}

func TestListPlazaGroups_AccountModelsWithoutChannelPrices(t *testing.T) {
	groups := []Group{{ID: 1, Platform: PlatformOpenAI}, {ID: 2, Platform: PlatformAnthropic},
		{ID: 3, Platform: PlatformKimi}, {ID: 4, Platform: PlatformMiniMax}, {ID: 5, Platform: PlatformGrok}}
	channels := []Channel{plazaPricedChannel(1, "partial", []int64{1}, PlatformOpenAI, "gpt-6-astra")}
	repo := &plazaAccountRepository{accounts: []Account{
		plazaAccount(PlatformOpenAI, []int64{1}, map[string]any{"gpt-6.1-sol": "gpt-6.1-sol"}),
		plazaAccount(PlatformAnthropic, []int64{2}, map[string]any{
			"claude-opus-5-5": "claude-opus-5-5", "claude-sonnet-5-5": "claude-sonnet-5-5", "claude-haiku-5-5": "claude-haiku-5-5",
		}),
		plazaAccount(PlatformKimi, []int64{3}, map[string]any{"kimi-k3": "kimi-k3"}),
		plazaAccount(PlatformMiniMax, []int64{4}, map[string]any{"minimax-m3": "minimax-m3"}),
		plazaAccount(PlatformGrok, []int64{5}, map[string]any{"grok-4.7": "grok-4.7"}),
	}}
	svc := newPlazaServiceWithBilling(channels, groups, map[int64]string{1: PlatformOpenAI}, nil)
	svc.accountRepo = repo
	out, err := svc.ListGroups(context.Background())
	require.NoError(t, err)
	require.Len(t, out, 5)
	byID := make(map[int64]map[string]PlazaModel)
	for _, g := range out {
		byID[g.ID] = plazaModelsByName(g.Models)
	}
	for id, prices := range map[int64]map[string]float64{
		1: {"gpt-6.1-sol": 2e-6},
		2: {"claude-opus-5-5": 4e-6, "claude-sonnet-5-5": 2e-6, "claude-haiku-5-5": 0.1e-6},
	} {
		for name, price := range prices {
			m, ok := byID[id][name]
			require.True(t, ok, name)
			require.NotNil(t, m.Pricing, name)
			require.NotNil(t, m.OfficialPricing, name)
			require.InDelta(t, price, *m.Pricing.InputPrice, 1e-12, name)
		}
	}
	for id, name := range map[int64]string{3: "kimi-k3", 4: "minimax-m3", 5: "grok-4.7"} {
		require.Contains(t, byID[id], name)
		require.NotNil(t, byID[id][name].Pricing, name)
	}
	require.NotContains(t, byID[3], "claude-opus-5-5")
	require.Equal(t, 1, repo.reads, "one account query, not one per group")
}

func TestListPlazaGroups_GroupCardsAndWildcardPricesRefresh(t *testing.T) {
	groups := []Group{{ID: 1, Platform: PlatformGemini, ModelPricing: []ChannelModelPricing{{
		Models: []string{"deepseek-v4.1-flash"}, BillingMode: BillingModePerRequest, PerRequestPrice: testPtrFloat64(0.08),
	}}}, {ID: 2, Platform: PlatformOpenAI}}
	channels := []Channel{{ID: 1, Status: StatusActive, GroupIDs: []int64{2}, ModelPricing: []ChannelModelPricing{{
		Platform: PlatformOpenAI, Models: []string{"private-image-*"}, BillingMode: BillingModeImage, PerRequestPrice: testPtrFloat64(0.25),
	}}}}
	repo := &plazaAccountRepository{accounts: []Account{plazaAccount(PlatformOpenAI, []int64{2}, map[string]any{"private-image-v1": "private-image-v1"})}}
	svc := newPlazaServiceWithBilling(channels, groups, map[int64]string{2: PlatformOpenAI}, nil)
	svc.accountRepo = repo
	for _, price := range []float64{0.08, 0.12} {
		groups[0].ModelPricing[0].PerRequestPrice = testPtrFloat64(price)
		out, err := svc.ListGroups(context.Background())
		require.NoError(t, err)
		require.Len(t, out, 2)
		for _, g := range out {
			require.Len(t, g.Models, 1)
			m := g.Models[0]
			require.NotContains(t, m.Name, "*")
			require.NotNil(t, m.Pricing)
			if g.ID == 1 {
				require.Equal(t, "deepseek-v4.1-flash", m.Name)
				require.Equal(t, PlatformGemini, m.Platform)
				require.Equal(t, price, *m.Pricing.PerRequestPrice)
			} else {
				require.Equal(t, "private-image-v1", m.Name)
				require.Equal(t, BillingModeImage, m.Pricing.BillingMode)
				require.Equal(t, 0.25, *m.Pricing.PerRequestPrice)
			}
		}
	}
	require.Equal(t, 2, repo.reads)
}

func TestListPlazaGroups_AccountCatalogRespectsConfiguration(t *testing.T) {
	groups := []Group{
		{ID: 1, Platform: PlatformOpenAI, ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: []string{"gpt-6.1-sol"}}},
		{ID: 2, Platform: PlatformAnthropic},
		{ID: 3, Platform: PlatformOpenAI},
		{ID: 4, Platform: PlatformComposite},
	}
	limited := plazaAccount(PlatformOpenAI, []int64{1, 3, 4}, nil)
	until := time.Now().Add(time.Hour)
	limited.RateLimitResetAt = &until
	limited.TempUnschedulableUntil = &until
	paused := plazaAccount(PlatformAnthropic, []int64{2}, nil)
	paused.Schedulable = false
	disabled := plazaAccount(PlatformAnthropic, []int64{2}, nil)
	disabled.Status = StatusDisabled
	channels := []Channel{plazaPricedChannel(1, "restricted", []int64{3}, PlatformOpenAI, "gpt-6-astra")}
	channels[0].RestrictModels = true
	svc := newPlazaService(channels, groups, nil)
	svc.accountRepo = &plazaAccountRepository{accounts: []Account{limited, paused, disabled,
		plazaAccount(PlatformOpenAI, []int64{2}, nil), // wrong platform for the group
		plazaAccount(PlatformAnthropic, []int64{4}, map[string]any{"claude-opus-5-5": "claude-opus-5-5"}),
	}}
	out, err := svc.ListGroups(context.Background())
	require.NoError(t, err)
	require.Len(t, out, 3)
	for _, g := range out {
		switch g.ID {
		case 1:
			require.Len(t, g.Models, 1)
			require.Equal(t, "gpt-6.1-sol", g.Models[0].Name)
		case 3:
			require.Len(t, g.Models, 1)
			require.Equal(t, "gpt-6-astra", g.Models[0].Name)
		case 4:
			models := plazaModelsByName(g.Models)
			require.Equal(t, PlatformOpenAI, models["gpt-6.1-sol"].Platform)
			require.Equal(t, PlatformAnthropic, models["claude-opus-5-5"].Platform)
		default:
			t.Fatalf("unexpected group %d", g.ID)
		}
	}
}

func TestListPlazaGroups_WildcardsMetadataAndPassthrough(t *testing.T) {
	catalog := &PricingService{pricingData: map[string]*LiteLLMModelPricing{
		"gpt-custom-new": {LiteLLMProvider: "openai"},
		"claude-custom":  {LiteLLMProvider: "anthropic"},
	}}
	a := plazaAccount(PlatformOpenAI, []int64{1}, map[string]any{"gpt-*": "gpt-*"})
	a.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{Models: map[string]UpstreamModelMetadata{"gpt-synced": {}}})
	b := plazaAccount(PlatformOpenAI, []int64{2}, map[string]any{"stale-alias": "retired"})
	b.Extra = map[string]any{"openai_passthrough": true}
	svc := newPlazaService(nil, []Group{{ID: 1, Platform: PlatformOpenAI}, {ID: 2, Platform: PlatformOpenAI}}, catalog)
	repo := &plazaAccountRepository{accounts: []Account{a, b}}
	svc.accountRepo = repo
	out, err := svc.ListGroups(context.Background())
	require.NoError(t, err)
	require.Len(t, out, 2)
	for _, g := range out {
		models := plazaModelsByName(g.Models)
		require.Contains(t, models, "gpt-6.1-sol")
		require.NotContains(t, models, "gpt-*")
		require.NotContains(t, models, "claude-custom")
		require.NotContains(t, models, "stale-alias")
		if g.ID == 1 {
			require.Contains(t, models, "gpt-custom-new")
			require.Contains(t, models, "gpt-synced")
		}
	}
	// The next poll sees account edits instead of a long-lived discovery cache.
	repo.accounts[0].Credentials["model_mapping"] = map[string]any{"only-new": "only-new"}
	out, err = svc.ListGroups(context.Background())
	require.NoError(t, err)
	for _, g := range out {
		if g.ID == 1 {
			require.Len(t, g.Models, 1)
			require.Equal(t, "only-new", g.Models[0].Name)
		}
	}
}

func TestListPlazaGroups_AccountReadFailureDoesNotReturnPartialCatalog(t *testing.T) {
	svc := newPlazaService(nil, []Group{{ID: 1, Platform: PlatformOpenAI}}, nil)
	sentinel := errors.New("account read failed")
	svc.accountRepo = &plazaAccountRepository{err: sentinel}
	out, err := svc.ListGroups(context.Background())
	require.ErrorIs(t, err, sentinel)
	require.Nil(t, out)
}

func TestListPlazaGroups_RestrictionsUseChannelBillingModel(t *testing.T) {
	ch := plazaPricedChannel(1, "mapped", []int64{1}, PlatformOpenAI, "gpt-6.1-sol")
	ch.RestrictModels = true
	ch.BillingModelSource = BillingModelSourceChannelMapped
	ch.ModelMapping = map[string]map[string]string{PlatformOpenAI: {"public-model": "gpt-6.1-sol"}}
	groups := []Group{{ID: 1, Platform: PlatformOpenAI, ModelAllowlist: GroupModelAllowlist{
		Enabled: true, Models: []string{"public-model"},
	}}}
	svc := newPlazaService([]Channel{ch}, groups, nil)
	svc.accountRepo = &plazaAccountRepository{accounts: []Account{plazaAccount(PlatformOpenAI, []int64{1}, nil)}}
	out, err := svc.ListGroups(context.Background())
	require.NoError(t, err)
	require.Len(t, out, 1)
	require.Len(t, out[0].Models, 1)
	require.Equal(t, "public-model", out[0].Models[0].Name)
}

func TestListPlazaGroups_RetiredOAuthModelsStayHidden(t *testing.T) {
	a := plazaAccount(PlatformOpenAI, []int64{1}, nil)
	a.Type = AccountTypeOAuth
	svc := newPlazaService(nil, []Group{{ID: 1, Platform: PlatformOpenAI}}, nil)
	svc.accountRepo = &plazaAccountRepository{accounts: []Account{a}}
	out, err := svc.ListGroups(context.Background())
	require.NoError(t, err)
	require.Len(t, out, 1)
	models := plazaModelsByName(out[0].Models)
	require.Contains(t, models, "gpt-6.1-sol")
	require.NotContains(t, models, "gpt-5.5")
}
