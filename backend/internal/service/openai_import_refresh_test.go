package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenAITokenRefresherUnknownImportExpiry(t *testing.T) {
	refresher := NewOpenAITokenRefresher(nil, nil)
	for _, tc := range []struct {
		name   string
		expiry any
		want   bool
	}{
		{name: "absent", want: true},
		{name: "blank", expiry: "", want: true},
		{name: "malformed", expiry: "not-a-time", want: true},
		{name: "expired", expiry: time.Now().Add(-time.Hour).Format(time.RFC3339), want: true},
		{name: "fresh", expiry: time.Now().Add(time.Hour).Format(time.RFC3339), want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := &Account{
				Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
				Credentials: map[string]any{"refresh_token": "fixture-refresh", "expires_at": tc.expiry},
			}
			require.False(t, account.IsRateLimited())
			require.Equal(t, tc.want, refresher.NeedsRefresh(account, openAITokenRefreshSkew))
			delete(account.Credentials, "refresh_token")
			require.False(t, refresher.NeedsRefresh(account, openAITokenRefreshSkew))
		})
	}
}
