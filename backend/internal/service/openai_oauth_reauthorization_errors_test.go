package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestReauthorizationAccountErrorsAtGenerationAndSessionBoundary(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*OpenAIOAuthService, *Account)
		want   error
		status int
	}{
		{"lookup missing", func(s *OpenAIOAuthService, _ *Account) {
			s.SetReauthorizationAccountLookup(nil)
		}, ErrOpenAIOAuthReauthorizationUnavailable, http.StatusServiceUnavailable},
		{"lookup failed", func(s *OpenAIOAuthService, _ *Account) {
			s.SetReauthorizationAccountLookup(func(context.Context, int64) (*Account, error) {
				return nil, errors.New("private-repository-diagnostic")
			})
		}, ErrOpenAIOAuthReauthorizationUnavailable, http.StatusServiceUnavailable},
		{"deleted", func(s *OpenAIOAuthService, _ *Account) {
			s.SetReauthorizationAccountLookup(func(context.Context, int64) (*Account, error) {
				return nil, fmt.Errorf("lookup: %w", ErrAccountNotFound)
			})
		}, ErrAccountNotFound, http.StatusNotFound},
		{"nil account", func(s *OpenAIOAuthService, _ *Account) {
			s.SetReauthorizationAccountLookup(func(context.Context, int64) (*Account, error) {
				return nil, nil
			})
		}, ErrAccountNotFound, http.StatusNotFound},
		{"wrong account", func(_ *OpenAIOAuthService, a *Account) {
			a.ID++
		}, ErrOpenAIOAuthReauthorizationUnavailable, http.StatusServiceUnavailable},
		{"non OAuth", func(_ *OpenAIOAuthService, a *Account) {
			a.Type = AccountTypeAPIKey
		}, ErrOpenAIOAuthReauthorizationUnsupported, http.StatusBadRequest},
		{"other platform", func(_ *OpenAIOAuthService, a *Account) {
			a.Platform = PlatformAnthropic
		}, ErrOpenAIOAuthReauthorizationUnsupported, http.StatusBadRequest},
		{"shadow", func(_ *OpenAIOAuthService, a *Account) {
			parent := int64(7)
			a.ParentAccountID = &parent
		}, ErrOpenAIOAuthReauthorizationUnsupported, http.StatusBadRequest},
		{"stale revision", func(_ *OpenAIOAuthService, a *Account) {
			a.UpdatedAt = a.UpdatedAt.Add(time.Nanosecond)
			a.Credentials = map[string]any{"access_token": "newer-fixture"}
		}, ErrOAuthReauthorizationStale, http.StatusConflict},
		{"proxy removed", func(_ *OpenAIOAuthService, a *Account) {
			a.ProxyID = nil
		}, ErrOpenAIOAuthProxyMismatch, http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			svc, account, session, _, client := reauthorizationIPFixture(t)
			id, revision, proxyID := account.ID, account.UpdatedAt.Format(time.RFC3339Nano), account.ProxyID
			test.mutate(svc, account)
			store := svc.sessionStore.(*testOpenAIOAuthSessionStore)
			before := len(store.sessions)
			result, err := svc.GenerateReauthorizationAuthURL(t.Context(), id, revision, proxyID, "", PlatformOpenAI, "")
			require.Nil(t, result)
			require.ErrorIs(t, err, test.want)
			require.Equal(t, test.status, infraerrors.Code(err))
			require.NotContains(t, err.Error(), "private-repository-diagnostic")
			require.Len(t, store.sessions, before)
			err = svc.validateReauthorizationSession(t.Context(), session, true)
			require.ErrorIs(t, err, test.want)
			require.Equal(t, test.status, infraerrors.Code(err))
			require.NotContains(t, err.Error(), "private-repository-diagnostic")
			require.Zero(t, client.exchangeCalled)
		})
	}
}

func TestReauthorizationCanceledLookupDoesNotReadAccount(t *testing.T) {
	svc, account, session, _, _ := reauthorizationIPFixture(t)
	svc.SetReauthorizationAccountLookup(func(context.Context, int64) (*Account, error) {
		t.Fatal("canceled request must not query account")
		return nil, nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := svc.GenerateReauthorizationAuthURL(ctx, account.ID,
		account.UpdatedAt.Format(time.RFC3339Nano), account.ProxyID, "", PlatformOpenAI, "")
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, svc.validateReauthorizationSession(ctx, session, false), context.Canceled)
}
