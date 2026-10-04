package service

import (
	"maps"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func openAIOAuthAccountEditFixture() *Account {
	return &Account{
		ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		UpdatedAt: time.Now().UTC(),
		Credentials: map[string]any{
			"access_token": "fixture-before", "email": "user@example.com",
			"client_id": "fixture-client", "chatgpt_account_id": "fixture-account",
			"model_mapping": map[string]any{"fixture-model": "fixture-model"},
		},
	}
}

func TestOpenAIOAuthAccountEditPreservesOmittedIdentity(t *testing.T) {
	account := openAIOAuthAccountEditFixture()
	incoming := map[string]any{"model_mapping": map[string]any{"next-model": "next-model"}}
	preserveOpenAIOAuthEditIdentity(account, incoming)
	require.NoError(t, validateOpenAIOAuthAccountEdit(account, "", incoming, nil))
	for _, key := range openAIReauthorizationCredentialKeys {
		require.Equal(t, account.Credentials[key], incoming[key])
	}
	require.NotEqual(t, account.Credentials["model_mapping"], incoming["model_mapping"])
	require.NoError(t, validateOpenAIOAuthAccountEdit(account, "", nil, nil))
}

func TestOpenAIOAuthAccountEditRejectsUnboundIdentityReplacement(t *testing.T) {
	for _, key := range openAIReauthorizationCredentialKeys {
		t.Run(key, func(t *testing.T) {
			account := openAIOAuthAccountEditFixture()
			incoming := maps.Clone(account.Credentials)
			incoming[key] = "fixture-replaced"
			require.ErrorIs(t, validateOpenAIOAuthAccountEdit(account, "", incoming, nil),
				ErrOpenAIOAuthReauthorizationProofRequired)
		})
	}
	account := openAIOAuthAccountEditFixture()
	require.ErrorIs(t, validateOpenAIOAuthAccountEdit(account, AccountTypeAPIKey, nil, nil),
		ErrOpenAIOAuthReauthorizationProofRequired)
}

func TestOpenAIOAuthAccountEditRefreshBinding(t *testing.T) {
	for _, mutation := range []string{"valid", "stale", "wrong account", "tampered", "mode changed"} {
		t.Run(mutation, func(t *testing.T) {
			account := openAIOAuthAccountEditFixture()
			incoming := maps.Clone(account.Credentials)
			incoming["access_token"] = "fixture-after"
			hash, err := openAIReauthorizationCredentialsHash(incoming)
			require.NoError(t, err)
			binding := &openAIAccountRefreshBinding{
				accountID: account.ID, revision: account.UpdatedAt, hash: hash,
			}
			switch mutation {
			case "stale":
				account.UpdatedAt = account.UpdatedAt.Add(time.Microsecond)
			case "wrong account":
				account.ID++
			case "tampered":
				incoming["email"] = "other@example.com"
			case "mode changed":
				incoming["auth_mode"] = "personalAccessToken"
			}
			input := (&OpenAITokenInfo{accountRefresh: binding}).AccountRefreshUpdate(incoming)
			err = validateOpenAIOAuthAccountEdit(account, "", input.Credentials, input.openAIRefresh)
			if mutation == "valid" {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, ErrOpenAIOAuthReauthorizationProofRequired)
			}
		})
	}
}

func TestOpenAIOAuthAccountEditLeavesNonBrowserAccountsUnchanged(t *testing.T) {
	account := openAIOAuthAccountEditFixture()
	account.Type = AccountTypeAPIKey
	require.NoError(t, validateOpenAIOAuthAccountEdit(account, "", map[string]any{"api_key": "fixture"}, nil))
	account.Type = AccountTypeOAuth
	account.Credentials["auth_mode"] = "personalAccessToken"
	require.NoError(t, validateOpenAIOAuthAccountEdit(account, "", map[string]any{"access_token": "fixture"}, nil))
}
