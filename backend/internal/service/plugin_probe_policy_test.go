package service

import (
	"context"
	"errors"
	"testing"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"
)

type probePolicyAccounts struct {
	AccountRepository
	accounts []*Account
	err      error
	wait     bool
}

func (s *probePolicyAccounts) GetByIDs(ctx context.Context, _ []int64) ([]*Account, error) {
	if s.wait {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return s.accounts, s.err
}

func TestPluginProbePolicyDBTimeoutPreservesHealthContext(t *testing.T) {
	m := &PluginManager{
		rateLimitService: &RateLimitService{accountRepo: &probePolicyAccounts{wait: true}},
		probeControls:    &probePolicyControls{},
	}
	r := &pluginRuntime{installation: &PluginInstallation{PluginKey: "lyunlong.codex.lb-cookie-pin"}}
	r.trackProbeAccount(42, time.Now())
	ctx := m.probePolicyContext(t.Context(), r)
	require.NoError(t, ctx.Err(), "DB timeout must not cancel the later Health RPC")
	md, _ := metadata.FromOutgoingContext(ctx)
	policy, ok := pluginv1.ReadProbePolicy(metadata.NewIncomingContext(t.Context(), md), time.Now())
	require.True(t, ok)
	require.True(t, policy.Suspended)
	require.Empty(t, policy.Accounts)
}

type probePolicyControls struct {
	allowed map[int64]bool
	err     error
}

func (s *probePolicyControls) AllowedOpenAIProbeAccounts(context.Context, []int64) (map[int64]bool, error) {
	return s.allowed, s.err
}

func TestPluginProbePolicyAuthoritativeControls(t *testing.T) {
	now := time.Now()
	reset := now.Add(time.Hour)
	account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusError, Schedulable: false, RateLimitResetAt: &reset,
		Credentials: map[string]any{"access_token": "fixture", "chatgpt_account_id": "fixture-subject"}}
	repo := &probePolicyAccounts{accounts: []*Account{account}}
	controls := &probePolicyControls{allowed: map[int64]bool{42: true}}
	m := &PluginManager{rateLimitService: &RateLimitService{accountRepo: repo}, probeControls: controls}
	r := &pluginRuntime{installation: &PluginInstallation{PluginKey: "lyunlong.codex.lb-cookie-pin"}}
	r.trackProbeAccount(42, now)
	read := func() pluginv1.ProbePolicy {
		ctx := m.probePolicyContext(t.Context(), r)
		md, _ := metadata.FromOutgoingContext(ctx)
		policy, ok := pluginv1.ReadProbePolicy(metadata.NewIncomingContext(t.Context(), md), time.Now())
		require.True(t, ok)
		return policy
	}
	policy := read()
	require.Len(t, policy.Accounts, 1, "owned rescue circuit must remain recoverable")
	require.Equal(t, reset.UnixMilli(), policy.Accounts[0].NotBefore)
	require.Equal(t, pluginv1.ProbeIdentity("Bearer fixture", "fixture-subject", ""), policy.Accounts[0].Identity)
	controls.allowed[42] = false
	require.Empty(t, read().Accounts, "manual disable must revoke background permission")
	controls.allowed[42] = true
	repo.accounts = nil
	require.Empty(t, read().Accounts, "deleted account cannot retain a permission")
	repo.accounts = []*Account{account}
	repo.err = errors.New("fixture unavailable")
	policy = read()
	require.Empty(t, policy.Accounts, "DB failure cannot renew cached permission")
	require.True(t, policy.Suspended, "DB failure is not an authoritative account deletion")
	repo.err = nil
	controls.err = errors.New("fixture unavailable")
	policy = read()
	require.Empty(t, policy.Accounts)
	require.True(t, policy.Suspended)
}

func TestPluginProbePolicyTrackingBounded(t *testing.T) {
	r := &pluginRuntime{}
	now := time.Now()
	for id := int64(1); id <= pluginv1.MaxProbePolicyAccounts; id++ {
		r.trackProbeAccount(id, now.Add(time.Duration(id)*time.Millisecond))
	}
	r.trackProbeAccount(2, now.Add(time.Hour))
	require.Len(t, r.probeAccounts, pluginv1.MaxProbePolicyAccounts)
	r.trackProbeAccount(pluginv1.MaxProbePolicyAccounts+1, now.Add(time.Hour))
	require.Len(t, r.probeAccounts, pluginv1.MaxProbePolicyAccounts)
	require.NotContains(t, r.probeAccounts, int64(1))
	require.Contains(t, r.probeAccounts, int64(2))
}
