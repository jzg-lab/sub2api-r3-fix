package repository

import (
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOpenAIOAuthAccountEditRejectsConcurrentUpdateUnderLock(t *testing.T) {
	for _, mutation := range []string{"settings", "refresh", "missing revision"} {
		t.Run(mutation, func(t *testing.T) {
			repo, mock := atomicCreateRepository(t)
			stamp := time.Now().UTC().Truncate(time.Microsecond)
			account := openAIOAuthCreateFixture()
			account.ID = 71
			account.Schedulable = false
			account.UpdatedAt = stamp.Add(-time.Microsecond)
			if mutation == "missing revision" {
				account.UpdatedAt = time.Time{}
			}
			if mutation == "refresh" {
				account.Credentials["access_token"] = "fixture"
			}
			mock.ExpectBegin()
			mock.ExpectQuery(`(?s)SELECT id, platform, type, credentials, extra, proxy_id, parent_account_id,.*FOR NO KEY UPDATE`).
				WithArgs(account.ID).
				WillReturnRows(sqlmock.NewRows([]string{
					"id", "platform", "type", "credentials", "extra", "proxy_id", "parent_account_id",
					"status", "schedulable", "updated_at",
				}).AddRow(account.ID, service.PlatformOpenAI, service.AccountTypeOAuth,
					[]byte(`{"email":"user@example.com"}`), []byte(`{}`), int64(7), nil,
					service.StatusActive, false, stamp))
			mock.ExpectRollback()

			err := repo.Update(t.Context(), account)
			require.ErrorIs(t, err, service.ErrOAuthReauthorizationStale)
			// No UPDATE, outbox enqueue, or commit is permitted on this path.
		})
	}
}
