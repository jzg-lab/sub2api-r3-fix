package service

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAdminCreateAccountRejectsUnprovenBrowserOAuth(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
	}{
		{
			name:    "raw browser OAuth without a proxy",
			payload: `{"Platform":"openai","Type":"oauth"}`,
		},
		{
			name:    "raw browser OAuth with a proxy",
			payload: `{"Platform":"openai","Type":"oauth","ProxyID":7}`,
		},
		{
			name: "caller supplied trust claims",
			payload: `{
				"Platform":"openai",
				"Type":"oauth",
				"ProxyID":7,
				"Credentials":{"oauth_verified":true,"authorization_verified":true},
				"Extra":{"oauth_verified":true,"authorization_verified":true},
				"oauthProvenance":{"verified":true,"proxy_id":7}
			}`,
		},
		{
			name:    "PAT mode without material",
			payload: `{"Platform":"openai","Type":"oauth","Credentials":{"auth_mode":"personalAccessToken"}}`,
		},
		{
			name:    "legacy PAT mode without material",
			payload: `{"Platform":"openai","Type":"oauth","Credentials":{"openai_auth_mode":"personal_access_token"}}`,
		},
		{
			name:    "PAT mode with whitespace material",
			payload: `{"Platform":"openai","Type":"oauth","Credentials":{"auth_mode":"personalAccessToken","access_token":" \t ","refresh_token":"\n"}}`,
		},
		{
			name:    "legacy PAT mode with non-string material",
			payload: `{"Platform":"openai","Type":"oauth","Credentials":{"openai_auth_mode":"personal_access_token","access_token":true,"refresh_token":17}}`,
		},
		{
			name:    "nested whitespace material",
			payload: `{"Platform":"openai","Type":"oauth","Credentials":{"tokens":{"access_token":"\n","refresh_token":" \t "}}}`,
		},
		{
			name:    "nested non-string material",
			payload: `{"Platform":"openai","Type":"oauth","Credentials":{"tokens":{"access_token":true,"refresh_token":17}}}`,
		},
		{
			name:    "invalid nested material container",
			payload: `{"Platform":"openai","Type":"oauth","Credentials":{"tokens":["invalid"]}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var input CreateAccountInput
			require.NoError(t, json.Unmarshal([]byte(tc.payload), &input))
			input.Name = "unproven-browser-oauth"
			input.SkipDefaultGroupBind = true
			input.SkipMixedChannelCheck = true

			// A failed writer prevents asynchronous privacy traffic even before
			// the provenance guard is implemented.
			writeErr := errors.New("unproven OAuth reached the account writer")
			repo := &atomicAccountCreateTestRepo{err: writeErr}
			svc := &adminServiceImpl{accountRepo: repo, accountDuplicateRepo: repo}
			account, err := svc.CreateAccount(t.Context(), &input)

			require.Zero(t, repo.calls, "unproven browser OAuth must fail before any account write")
			require.Zero(t, repo.legacyCalls)
			require.Nil(t, account)
			require.Error(t, err)
			require.NotErrorIs(t, err, writeErr)
			require.ErrorIs(t, err, ErrOpenAIOAuthProvenanceRequired)
		})
	}

	// These fixtures prove material compatibility, not trusted authorization.
	// The writer fails before any asynchronous privacy traffic can start.
	const fixtureMaterial = "test-only"
	for _, tc := range []struct {
		name        string
		credentials map[string]any
	}{
		{"access material", map[string]any{"access_token": fixtureMaterial}},
		{"refresh material", map[string]any{"refresh_token": fixtureMaterial}},
		{"nested access material", map[string]any{"tokens": map[string]any{"access_token": fixtureMaterial}}},
		{"nested refresh material", map[string]any{"tokens": map[string]any{"refresh_token": fixtureMaterial}}},
		{"PAT access material", map[string]any{"auth_mode": "personalAccessToken", "access_token": fixtureMaterial}},
		{"legacy PAT access material", map[string]any{"openai_auth_mode": "personal_access_token", "access_token": fixtureMaterial}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeErr := errors.New("fixture stops before persistence")
			repo := &atomicAccountCreateTestRepo{err: writeErr}
			svc := &adminServiceImpl{accountRepo: repo, accountDuplicateRepo: repo}
			account, err := svc.CreateAccount(t.Context(), &CreateAccountInput{
				Name:                  "credential-material-fixture",
				Platform:              PlatformOpenAI,
				Type:                  AccountTypeOAuth,
				Credentials:           tc.credentials,
				SkipDefaultGroupBind:  true,
				SkipMixedChannelCheck: true,
			})

			require.Nil(t, account)
			require.ErrorIs(t, err, writeErr)
			require.Equal(t, 1, repo.calls)
			require.Zero(t, repo.legacyCalls)
		})
	}
}
