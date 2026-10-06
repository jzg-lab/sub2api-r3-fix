//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

type oauthDefaultsCreateRepo struct {
	AccountRepository
	account *Account
	err     error
}

func (r *oauthDefaultsCreateRepo) Create(_ context.Context, account *Account) error {
	r.account = account
	return r.err
}

func TestAccountServiceCreatePreservesAuthorizationRoute(t *testing.T) {
	for _, mode := range []string{"browser", OpenAIAuthModePersonalAccessToken, OpenAIAuthModeAgentIdentity, "api-key"} {
		for _, templateProxy := range []int64{0, 12} {
			for _, assigned := range []int64{0, 7} {
				t.Run(fmt.Sprintf("%s/template=%d/assigned=%d", mode, templateProxy, assigned), func(t *testing.T) {
					store := &tlsDefaultsRepo{values: map[string]string{}}
					settings := &SettingService{settingRepo: store}
					cfg := DefaultOpenAIOperationsSettings()
					concurrency := 8
					cfg.NewAccountDefaults = &OpenAINewAccountDefaults{
						ProxyID: &templateProxy, Concurrency: &concurrency,
					}
					require.NoError(t, settings.SetOpenAIOperationsSettings(t.Context(), cfg))
					req := CreateAccountRequest{
						Name: "route-defaults", Platform: PlatformOpenAI, Type: AccountTypeOAuth,
						Concurrency: 3,
					}
					if assigned != 0 {
						req.ProxyID = &assigned
					}
					switch mode {
					case OpenAIAuthModePersonalAccessToken, OpenAIAuthModeAgentIdentity:
						req.Credentials = map[string]any{"auth_mode": mode}
					case "api-key":
						req.Type = AccountTypeAPIKey
					}
					// Capture the attempted write, but never commit or reach group binding.
					writeErr := errors.New("fixture stops before persistence")
					repo := &oauthDefaultsCreateRepo{err: writeErr}
					svc := &AccountService{accountRepo: repo, settings: settings}
					account, err := svc.Create(t.Context(), req)
					require.ErrorIs(t, err, writeErr)
					require.Nil(t, account)
					require.NotNil(t, repo.account)
					wantProxy := templateProxy
					if mode == "browser" {
						wantProxy = assigned
					}
					if wantProxy == 0 {
						require.Nil(t, repo.account.ProxyID)
					} else {
						require.NotNil(t, repo.account.ProxyID)
						require.Equal(t, wantProxy, *repo.account.ProxyID)
					}
					require.Equal(t, req.Concurrency, repo.account.Concurrency)
					if mode != "api-key" {
						require.True(t, repo.account.Schedulable)
						require.NotContains(t, repo.account.Extra, OpenAIDowngradeQualificationExtraKey)
					}
					require.Equal(t, 3, req.Concurrency)
					if assigned == 0 {
						require.Nil(t, req.ProxyID)
					} else {
						require.Equal(t, assigned, *req.ProxyID)
					}
				})
			}
		}
	}
}
