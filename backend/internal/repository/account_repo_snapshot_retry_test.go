package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
)

func TestRetryAccountSnapshotReadRecoversOnFreshGeneration(t *testing.T) {
	attempts := 0
	value, err := retryAccountSnapshotRead(context.Background(), func() (int, error) {
		attempts++
		if attempts < accountSnapshotReadMaxAttempts {
			return 0, errAccountSnapshotChanged
		}
		return 42, nil
	})

	require.NoError(t, err)
	require.Equal(t, 42, value)
	require.Equal(t, accountSnapshotReadMaxAttempts, attempts)
}

func TestRetryAccountSnapshotReadExhaustsBoundedAttempts(t *testing.T) {
	attempts := 0
	_, err := retryAccountSnapshotRead(context.Background(), func() (int, error) {
		attempts++
		return 0, errAccountSnapshotChanged
	})

	require.ErrorIs(t, err, errAccountSnapshotChanged)
	require.Equal(t, accountSnapshotReadMaxAttempts, attempts)
}

func TestRetryAccountSnapshotReadDoesNotRetryOtherErrors(t *testing.T) {
	expected := errors.New("query failed")
	attempts := 0
	_, err := retryAccountSnapshotRead(context.Background(), func() (int, error) {
		attempts++
		return 0, expected
	})

	require.ErrorIs(t, err, expected)
	require.Equal(t, 1, attempts)
}

func TestRetryAccountSnapshotReadStopsWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	_, err := retryAccountSnapshotRead(ctx, func() (int, error) {
		attempts++
		cancel()
		return 0, errAccountSnapshotChanged
	})

	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, attempts)
}

type captureOpsStatsQueryMatcher struct {
	actual *string
}

func (m captureOpsStatsQueryMatcher) Match(_, actual string) error {
	if m.actual == nil {
		return fmt.Errorf("query capture target is nil")
	}
	*m.actual = actual
	return nil
}

func TestListOpsAccountsForStatsSelectsSnapshotRevision(t *testing.T) {
	var capturedSQL string
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(captureOpsStatsQueryMatcher{actual: &capturedSQL}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	driver := entsql.OpenDB(dialect.Postgres, db)
	client := dbent.NewClient(dbent.Driver(driver))
	t.Cleanup(func() { _ = client.Close() })
	repo := newAccountRepositoryWithSQL(client, db, nil)

	mock.ExpectQuery("ops account projection").
		WillReturnRows(sqlmock.NewRows([]string{
			"id",
			"name",
			"platform",
			"concurrency",
			"load_factor",
			"status",
			"error_message",
			"schedulable",
			"rate_limit_reset_at",
			"overload_until",
			"temp_unschedulable_until",
			"updated_at",
		}))

	accounts, err := repo.ListOpsAccountsForStats(context.Background(), "", nil)
	require.NoError(t, err)
	require.Empty(t, accounts)
	require.NoError(t, mock.ExpectationsWereMet())

	normalized := normalizeSQLWhitespace(capturedSQL)
	selectClause, _, found := strings.Cut(normalized, " FROM ")
	require.True(t, found, "unexpected projection SQL: %s", normalized)
	require.Contains(t, selectClause, `"updated_at"`)
}
