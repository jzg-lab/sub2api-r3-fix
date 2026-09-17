//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/imroc/req/v3"
	"github.com/stretchr/testify/require"
)

func TestAdminService_EnsureOpenAIPrivacy_RetriesNonSuccessModes(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{PrivacyModeFailed, PrivacyModeCFBlocked} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			privacyCalls := 0
			proxyID, proxyRepo := openAIPrivacyRetryProxyFixture()
			svc := &adminServiceImpl{
				accountRepo: &mockAccountRepoForGemini{},
				proxyRepo:   proxyRepo,
				privacyClientFactory: func(proxyURL string) (*req.Client, error) {
					privacyCalls++
					return nil, errors.New("factory failed")
				},
			}

			account := &Account{
				ID:       101,
				ProxyID:  &proxyID,
				Platform: PlatformOpenAI,
				Type:     AccountTypeOAuth,
				Credentials: map[string]any{
					"access_token": "token-1",
				},
				Extra: map[string]any{
					"privacy_mode": mode,
				},
			}

			got := svc.EnsureOpenAIPrivacy(context.Background(), account)

			require.Equal(t, PrivacyModeFailed, got)
			require.Equal(t, 1, privacyCalls)
		})
	}
}

func TestTokenRefreshService_ensureOpenAIPrivacy_RetriesNonSuccessModes(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		TokenRefresh: config.TokenRefreshConfig{
			MaxRetries:          1,
			RetryBackoffSeconds: 0,
		},
	}

	for _, mode := range []string{PrivacyModeFailed, PrivacyModeCFBlocked} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			service := NewTokenRefreshService(&tokenRefreshAccountRepo{}, nil, nil, nil, nil, nil, nil, cfg, nil)
			privacyCalls := 0
			proxyID, proxyRepo := openAIPrivacyRetryProxyFixture()
			service.SetPrivacyDeps(func(proxyURL string) (*req.Client, error) {
				privacyCalls++
				return nil, errors.New("factory failed")
			}, proxyRepo)

			account := &Account{
				ID:       202,
				ProxyID:  &proxyID,
				Platform: PlatformOpenAI,
				Type:     AccountTypeOAuth,
				Credentials: map[string]any{
					"access_token": "token-2",
				},
				Extra: map[string]any{
					"privacy_mode": mode,
				},
			}

			service.ensureOpenAIPrivacy(context.Background(), account)

			require.Equal(t, 1, privacyCalls)
		})
	}
}

func openAIPrivacyRetryProxyFixture() (int64, *mockProxyRepoForOAuth) {
	id := int64(7)
	return id, &mockProxyRepoForOAuth{
		getByIDFunc: func(context.Context, int64) (*Proxy, error) {
			return &Proxy{ID: id, Status: StatusActive, Protocol: "http", Host: "127.0.0.1", Port: 8080}, nil
		},
	}
}
