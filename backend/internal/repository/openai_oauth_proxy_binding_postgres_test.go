package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func insertPreparedOpenAIOAuthAccount(
	ctx context.Context,
	tx *sql.Tx,
	id int64,
	account *service.Account,
) error {
	credentials, err := json.Marshal(account.Credentials)
	if err != nil {
		return err
	}
	extra, err := json.Marshal(account.Extra)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO accounts(
			id, platform, type, credentials, extra, proxy_id,
			status, schedulable, updated_at
		)
		VALUES($1, $2, $3, $4::jsonb, $5::jsonb, $6, $7, $8, NOW())
	`, id, account.Platform, account.Type, credentials, extra, account.ProxyID,
		account.Status, account.Schedulable)
	return err
}

func TestPrepareOpenAIOAuthAccountCreatePostgresSerializesConcurrentIdentity(t *testing.T) {
	db := newProbePostgres(t)
	_, err := db.Exec("INSERT INTO proxies(id) VALUES(7)")
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	start := make(chan struct{})
	ready := make(chan struct{}, 2)
	results := make(chan error, 2)
	for _, id := range []int64{101, 102} {
		go func(id int64) {
			tx, beginErr := db.BeginTx(ctx, nil)
			if beginErr != nil {
				results <- beginErr
				return
			}
			defer func() { _ = tx.Rollback() }()
			ready <- struct{}{}
			<-start

			account := openAIOAuthCreateFixture()
			if prepareErr := prepareOpenAIOAuthAccountCreate(ctx, tx, account); prepareErr != nil {
				results <- prepareErr
				return
			}
			if insertErr := insertPreparedOpenAIOAuthAccount(ctx, tx, id, account); insertErr != nil {
				results <- insertErr
				return
			}
			results <- tx.Commit()
		}(id)
	}
	<-ready
	<-ready
	close(start)

	var successCount, duplicateCount int
	for range 2 {
		result := <-results
		switch {
		case result == nil:
			successCount++
		case errors.Is(result, service.ErrOpenAIOAuthIdentityExists):
			duplicateCount++
		default:
			require.NoError(t, result)
		}
	}
	require.Equal(t, 1, successCount)
	require.Equal(t, 1, duplicateCount)

	var accountCount int
	var schedulable, qualificationPending bool
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*), bool_and(schedulable),
			bool_and(COALESCE((extra ->> $1)::boolean, FALSE))
		FROM accounts
		WHERE lower(btrim(credentials ->> 'email')) = 'user@example.com'
	`, service.OpenAIDowngradeQualificationExtraKey).
		Scan(&accountCount, &schedulable, &qualificationPending))
	require.Equal(t, 1, accountCount)
	require.True(t, schedulable)
	require.False(t, qualificationPending)
}

func TestPrepareOpenAIOAuthAccountCreatePostgresAllowsDirectReimport(t *testing.T) {
	db := newProbePostgres(t)
	_, err := db.Exec("INSERT INTO proxies(id) VALUES(7)")
	require.NoError(t, err)

	first := openAIOAuthCreateFixture()
	firstTx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	require.NoError(t, prepareOpenAIOAuthAccountCreate(t.Context(), firstTx, first))
	require.NoError(t, insertPreparedOpenAIOAuthAccount(t.Context(), firstTx, 201, first))
	require.NoError(t, firstTx.Commit())

	_, err = db.Exec(`
		UPDATE accounts
		SET deleted_at = NOW(),
			schedulable = TRUE,
			extra = jsonb_build_object($1::text, proxy_id)
		WHERE id = 201
	`, service.OpenAIOAuthQualifiedProxyExtraKey)
	require.NoError(t, err)

	second := openAIOAuthCreateFixture()
	second.ProxyID = nil
	secondTx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	require.NoError(t, prepareOpenAIOAuthAccountCreate(t.Context(), secondTx, second))
	require.NoError(t, insertPreparedOpenAIOAuthAccount(t.Context(), secondTx, 202, second))
	require.NoError(t, secondTx.Commit())

	require.Nil(t, second.ProxyID)
	require.NotContains(t, second.Extra, service.OpenAIOAuthQualifiedProxyExtraKey)
	require.NotContains(t, second.Extra, service.OpenAIDowngradeQualificationExtraKey)
	require.True(t, second.Schedulable)

	var activeCount int
	var restoredProxyID sql.NullInt64
	var schedulable, qualificationPending bool
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*), max(proxy_id), bool_and(schedulable),
			bool_and(COALESCE((extra ->> $1)::boolean, FALSE))
		FROM accounts
		WHERE deleted_at IS NULL
			AND lower(btrim(credentials ->> 'email')) = 'user@example.com'
	`, service.OpenAIDowngradeQualificationExtraKey).
		Scan(&activeCount, &restoredProxyID, &schedulable, &qualificationPending))
	require.Equal(t, 1, activeCount)
	require.False(t, restoredProxyID.Valid)
	require.True(t, schedulable)
	require.False(t, qualificationPending)
}
