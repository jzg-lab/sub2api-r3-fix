package service

import (
	"context"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type openAICredentialSnapshotRepo struct {
	AccountRepository
	current *Account
	failure error
	writes  int
}

func (r *openAICredentialSnapshotRepo) UpdateCredentials(ctx context.Context, _ int64, credentials map[string]any) error {
	if err := ValidateOpenAIOAuthCredentialSnapshot(ctx, r.current); err != nil {
		return err
	}
	if r.failure != nil {
		return r.failure
	}
	r.current.Credentials = maps.Clone(credentials)
	r.writes++
	return nil
}

func TestOpenAIOAuthCredentialPersistenceDoesNotOverwriteNewGeneration(t *testing.T) {
	for _, mutation := range []string{"valid", "scheduler metadata", "new credentials",
		"changed mapping", "changed proxy", "changed platform", "changed type", "failed write"} {
		t.Run(mutation, func(t *testing.T) {
			account := openAIOAuthAccountEditFixture()
			proxyID := int64(7)
			account.ProxyID = &proxyID
			original := maps.Clone(account.Credentials)
			current := *account
			current.Credentials = maps.Clone(account.Credentials)
			repo := &openAICredentialSnapshotRepo{current: &current}
			switch mutation {
			case "scheduler metadata":
				current.UpdatedAt = current.UpdatedAt.Add(time.Second)
				current.Schedulable = false
			case "new credentials":
				current.Credentials["access_token"] = "fixture-concurrent"
			case "changed mapping":
				current.Credentials["model_mapping"] = map[string]any{"next": "next"}
			case "changed proxy":
				other := int64(8)
				current.ProxyID = &other
			case "changed platform":
				current.Platform = PlatformAnthropic
			case "changed type":
				current.Type = AccountTypeAPIKey
			case "failed write":
				repo.failure = errors.New("database unavailable")
			}
			latest := maps.Clone(current.Credentials)
			next := maps.Clone(account.Credentials)
			next["access_token"] = "fixture-refreshed"
			err := persistAccountCredentials(t.Context(), repo, account, next)
			if mutation == "valid" || mutation == "scheduler metadata" {
				require.NoError(t, err)
				require.Equal(t, 1, repo.writes)
				require.Equal(t, next, account.Credentials)
				require.Equal(t, next, current.Credentials)
			} else {
				require.Error(t, err)
				require.Zero(t, repo.writes)
				require.Equal(t, original, account.Credentials, "failed persistence must not mutate the caller snapshot")
				require.Equal(t, latest, current.Credentials, "newer durable state must survive")
			}
		})
	}
}

func TestOpenAIOAuthCredentialSnapshotAllowsLegacyWritesAndRejectsReboundRefresh(t *testing.T) {
	account := openAIOAuthAccountEditFixture()
	require.NoError(t, ValidateOpenAIOAuthCredentialSnapshot(t.Context(), account))
	ctx, err := bindOpenAIOAuthCredentialSnapshot(t.Context(), account)
	require.NoError(t, err)
	require.NoError(t, ValidateOpenAIOAuthCredentialSnapshot(ctx, account))
	account.ID++
	require.ErrorIs(t, ValidateOpenAIOAuthCredentialSnapshot(ctx, account), ErrOAuthReauthorizationStale)
}
