//go:build unit

package service

import (
	"context"
	"maps"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type openAIAdminEditRepo struct {
	AccountRepository
	current *Account
	before  func()
	writes  int
}

func (r *openAIAdminEditRepo) GetByID(context.Context, int64) (*Account, error) {
	copy := *r.current
	copy.Credentials = maps.Clone(r.current.Credentials)
	copy.Extra = maps.Clone(r.current.Extra)
	return &copy, nil
}

func (r *openAIAdminEditRepo) Update(_ context.Context, account *Account) error {
	if r.before != nil {
		r.before()
	}
	if !r.current.UpdatedAt.Equal(account.UpdatedAt) {
		return ErrOAuthReauthorizationStale
	}
	copy := *account
	r.current = &copy
	r.writes++
	return nil
}

func TestOpenAIOAuthAdminEditAndRefresh(t *testing.T) {
	for _, mutation := range []string{"settings", "refresh", "unbound replacement", "stale refresh", "concurrent edit"} {
		t.Run(mutation, func(t *testing.T) {
			account := openAIOAuthAccountEditFixture()
			account.Extra = map[string]any{}
			original := maps.Clone(account.Credentials)
			repo := &openAIAdminEditRepo{current: account}
			svc := &adminServiceImpl{accountRepo: repo}
			input := &UpdateAccountInput{Name: "updated"}
			if mutation != "settings" {
				next := maps.Clone(account.Credentials)
				next["access_token"] = "fixture-refreshed"
				hash, err := openAIReauthorizationCredentialsHash(next)
				require.NoError(t, err)
				result := &OpenAITokenInfo{accountRefresh: &openAIAccountRefreshBinding{
					accountID: account.ID, revision: account.UpdatedAt, hash: hash,
				}}
				input = result.AccountRefreshUpdate(next)
			}
			switch mutation {
			case "unbound replacement":
				input.openAIRefresh = nil
			case "stale refresh":
				account.UpdatedAt = account.UpdatedAt.Add(time.Second)
			case "concurrent edit":
				repo.before = func() { repo.current.UpdatedAt = account.UpdatedAt.Add(time.Second) }
			}
			updated, err := svc.UpdateAccount(t.Context(), account.ID, input)
			if mutation == "settings" || mutation == "refresh" {
				require.NoError(t, err)
				require.NotNil(t, updated)
				require.Equal(t, 1, repo.writes)
				if mutation == "settings" {
					require.Equal(t, original, updated.Credentials)
				} else {
					require.Equal(t, "fixture-refreshed", updated.Credentials["access_token"])
				}
			} else {
				require.Error(t, err)
				require.Nil(t, updated)
				require.Zero(t, repo.writes)
				require.Equal(t, original, repo.current.Credentials)
			}
		})
	}
}
