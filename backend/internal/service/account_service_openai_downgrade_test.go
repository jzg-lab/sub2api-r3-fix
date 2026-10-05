package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type accountServiceCreateRepoStub struct {
	AccountRepository
	created *Account
}

func (s *accountServiceCreateRepoStub) Create(_ context.Context, account *Account) error {
	s.created = account
	account.ID = 42
	return nil
}

type accountServiceUpdateRepoStub struct {
	AccountRepository
	account *Account
}

func (s *accountServiceUpdateRepoStub) GetByID(_ context.Context, _ int64) (*Account, error) {
	return s.account, nil
}

func (s *accountServiceUpdateRepoStub) Update(_ context.Context, account *Account) error {
	s.account = account
	return nil
}

func TestAccountServiceUpdatePreservesModelRateLimits(t *testing.T) {
	// fork PreserveAccountProtection 移植回归（服务层部分）：model_rate_limits
	// 是网关 429 运行态，陈旧表单整包替换 extra 时不得被清空。sol_fallback /
	// qualification 的防线在 repo 行锁合并（lockAndMergeAccountProbeExtra），
	// 不在此层。
	limits := map[string]any{"gpt-5.6-sol": map[string]any{"rate_limit_reset_at": "2099-01-01T00:00:00Z"}}
	repo := &accountServiceUpdateRepoStub{account: &Account{
		ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
		Extra: map[string]any{modelRateLimitsKey: limits, "note": "user-key"},
	}}
	svc := NewAccountService(repo, nil)

	updated, err := svc.Update(context.Background(), 7, UpdateAccountRequest{
		Extra: &map[string]any{"note": "edited"}, // 陈旧表单：没带 model_rate_limits
	})
	require.NoError(t, err)
	require.Equal(t, "edited", updated.Extra["note"])
	require.Equal(t, limits, updated.Extra[modelRateLimitsKey],
		"stale form must not strip runtime model rate limits")
}

func TestAccountServiceCreateOpenAIOAuthIsImmediatelySchedulable(t *testing.T) {
	repo := &accountServiceCreateRepoStub{}
	service := NewAccountService(repo, nil)

	account, err := service.Create(context.Background(), CreateAccountRequest{
		Name:     "new-openai-oauth",
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Priority: 17,
	})

	require.NoError(t, err)
	require.Same(t, repo.created, account)
	require.True(t, account.Schedulable)
	require.Equal(t, 17, account.Priority)
	require.NotContains(t, account.Extra, openAIDowngradeQualificationExtraKey)
}
