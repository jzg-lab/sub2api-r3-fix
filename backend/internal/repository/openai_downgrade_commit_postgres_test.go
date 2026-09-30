package repository

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

var probePostgresSchemaSequence atomic.Uint64

// Opt in to a disposable Unix-socket cluster. Never take a production DSN.
func newProbePostgres(t *testing.T) *sql.DB {
	t.Helper()
	return newProbePostgresWithOwnership(t, true)
}

func newProbePostgresWithOwnership(t *testing.T, ownership bool) *sql.DB {
	t.Helper()
	migrations := []string{
		"036_scheduler_outbox.sql", "152_scheduler_outbox_dedup_key.sql",
		"153_scheduler_outbox_pending_dedup_key_index_notx.sql", "237_openai_downgrade_probe.sql",
	}
	if ownership {
		migrations = append(migrations, "239_openai_probe_ownership.sql")
	}
	migrations = append(migrations,
		"240_openai_probe_rate_limit_streak.sql",
		"241_openai_probe_turn_state_len.sql",
		"244_openai_probe_harvest_mode.sql",
		"246_openai_probe_nullable_verdict.sql",
		// r17aq：RecordOpenAIDowngradeProbe 落账后同步聚合 proxy_outcome_stats；
		// Save 写 auth_consecutive_failures 列。
		"227_proxy_outcome_stats.sql",
		"228_probe_auth_strikes.sql",
	)
	return newProbePostgresWithMigrations(t, migrations)
}

func newProbePostgresWithMigrations(t *testing.T, migrations []string) *sql.DB {
	t.Helper()
	socket := os.Getenv("SUB2API_PROBE_TEST_SOCKET")
	if socket == "" {
		t.Skip("requires a disposable PostgreSQL Unix socket")
	}
	require.True(t, filepath.IsAbs(socket))
	require.Equal(t, "probe-pg-sock", filepath.Base(socket))
	require.NotContains(t, socket, "'")
	require.NotContains(t, socket, "\\")
	port, err := strconv.Atoi(os.Getenv("SUB2API_PROBE_TEST_PORT"))
	require.NoError(t, err)
	require.GreaterOrEqual(t, port, 1024)
	require.LessOrEqual(t, port, 65535)
	dsn := fmt.Sprintf("host='%s' port=%d dbname=postgres sslmode=disable connect_timeout=3", socket, port)
	admin, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, admin.PingContext(ctx))
	schema := fmt.Sprintf("probe_test_%d_%d", os.Getpid(), probePostgresSchemaSequence.Add(1))
	_, err = admin.ExecContext(ctx, "CREATE SCHEMA "+pq.QuoteIdentifier(schema))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := admin.Exec("DROP SCHEMA " + pq.QuoteIdentifier(schema) + " CASCADE")
		require.NoError(t, cleanupErr)
	})
	db, err := sql.Open("postgres", dsn+" search_path="+schema)
	require.NoError(t, err)
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = db.Close() })
	// Base columns mirror the production types; the probe/outbox schemas below
	// are executed from the actual migrations, not reimplemented test schemas.
	_, err = db.Exec(`
			CREATE TABLE proxies (
				id BIGINT PRIMARY KEY, status VARCHAR(20) NOT NULL DEFAULT 'active',
				deleted_at TIMESTAMPTZ, expires_at TIMESTAMPTZ, exit_ip TEXT
			);
			CREATE TABLE accounts (
				id BIGINT PRIMARY KEY, platform VARCHAR(50) NOT NULL DEFAULT 'openai',
				type VARCHAR(50) NOT NULL DEFAULT 'oauth',
				credentials JSONB NOT NULL DEFAULT '{}',
				proxy_id BIGINT REFERENCES proxies(id),
				parent_account_id BIGINT, status VARCHAR(20) NOT NULL DEFAULT 'active',
			schedulable BOOLEAN NOT NULL DEFAULT TRUE,
			extra JSONB NOT NULL DEFAULT '{}', error_message TEXT,
			updated_at TIMESTAMPTZ NOT NULL, deleted_at TIMESTAMPTZ,
			auto_pause_on_expired BOOLEAN NOT NULL DEFAULT TRUE,
			expires_at TIMESTAMPTZ, rate_limited_at TIMESTAMPTZ,
			rate_limit_reset_at TIMESTAMPTZ
		);
		CREATE TABLE usage_logs (
			id BIGSERIAL PRIMARY KEY,
			account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			actual_cost DECIMAL(20, 10) NOT NULL DEFAULT 0,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
	`)
	require.NoError(t, err)
	for _, name := range migrations {
		migration, err := os.ReadFile(filepath.Join("..", "..", "migrations", name))
		require.NoError(t, err)
		_, err = db.Exec(string(migration))
		require.NoError(t, err, name)
	}
	return db
}

func seedProbePostgres(t *testing.T, db *sql.DB) *service.OpenAIDowngradeMutation {
	t.Helper()
	mutation := probeCommitFixture()
	_, err := db.Exec(`
		INSERT INTO proxies(id) VALUES(3), (4);
	`)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO accounts(id, proxy_id, credentials, extra, updated_at)
		VALUES(
			$1, $2,
			jsonb_build_object('email', 'probe-account@example.test'),
			jsonb_build_object($3::text, $2::bigint),
			$4
		)
	`, mutation.AccountID, mutation.ExpectedProxyID, service.OpenAIOAuthQualifiedProxyExtraKey,
		mutation.ExpectedAccountUpdatedAt)
	require.NoError(t, err)
	repo := &openAIDowngradeProbeRepository{db: db}
	_, err = repo.EnsureOpenAIDowngradeState(context.Background(), mutation.AccountID,
		mutation.ExpectedProxyID, mutation.ExpectedStateUpdatedAt)
	require.NoError(t, err)
	return mutation
}

func probePostgresSnapshot(t *testing.T, db *sql.DB) string {
	t.Helper()
	var snapshot string
	err := db.QueryRow(`
		SELECT jsonb_build_object(
			'accounts', (SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM accounts a),
			'controls', (SELECT jsonb_agg(to_jsonb(c) ORDER BY account_id) FROM openai_downgrade_probe_controls c),
			'states', (SELECT jsonb_agg(to_jsonb(s) ORDER BY account_id) FROM openai_downgrade_probe_states s),
			'results', (SELECT COUNT(*) FROM openai_downgrade_probe_results),
			'events', (SELECT COUNT(*) FROM openai_downgrade_probe_events),
			'outbox', (SELECT COUNT(*) FROM scheduler_outbox)
		)::text
	`).Scan(&snapshot)
	require.NoError(t, err)
	return snapshot
}

func TestOpenAIProbeNullableVerdictMigrationAndWrites(t *testing.T) {
	db := newProbePostgresWithMigrations(t, []string{
		"237_openai_downgrade_probe.sql",
		"239_openai_probe_ownership.sql",
		"240_openai_probe_rate_limit_streak.sql",
		"241_openai_probe_turn_state_len.sql",
		"244_openai_probe_harvest_mode.sql",
	})
	seedProbePostgres(t, db)
	_, err := db.Exec(`
		INSERT INTO openai_downgrade_probe_results(
			account_id, proxy_id, transport_ok, answer_correct, http_status
		) VALUES(7, 3, FALSE, FALSE, 200)
	`)
	require.NoError(t, err)

	migration, err := os.ReadFile(filepath.Join("..", "..", "migrations",
		"246_openai_probe_nullable_verdict.sql"))
	require.NoError(t, err)
	_, err = db.Exec(string(migration))
	require.NoError(t, err)

	var verdict sql.NullBool
	require.NoError(t, db.QueryRow(`
		SELECT answer_correct
		FROM openai_downgrade_probe_results
		ORDER BY id DESC LIMIT 1
	`).Scan(&verdict))
	require.False(t, verdict.Valid, "historical interrupted probes must be backfilled to NULL")

	repo := &openAIDowngradeProbeRepository{db: db}
	proxyID := int64(3)
	require.NoError(t, repo.RecordOpenAIDowngradeProbe(context.Background(),
		&service.OpenAIDowngradeProbeResult{
			AccountID: 7, ProxyID: &proxyID, TransportOK: false,
			AnswerCorrect: false, HTTPStatus: 200, ErrorMessage: "stream interrupted",
		}))
	require.NoError(t, db.QueryRow(`
		SELECT answer_correct
		FROM openai_downgrade_probe_results
		ORDER BY id DESC LIMIT 1
	`).Scan(&verdict))
	require.False(t, verdict.Valid, "new interrupted probes must persist an unknown verdict")

	_, err = db.Exec(`
		INSERT INTO openai_downgrade_probe_results(
			account_id, proxy_id, transport_ok, answer_correct, http_status
		) VALUES(7, 3, FALSE, FALSE, 200)
	`)
	require.ErrorContains(t, err, "openai_downgrade_probe_results_verdict_check")
	_, err = db.Exec(string(migration))
	require.NoError(t, err, "the migration must remain idempotent")
}

func TestOpenAIProbePostgresRollbackAtEveryWriteBoundary(t *testing.T) {
	for _, table := range []string{
		"accounts", "openai_downgrade_probe_results", "openai_downgrade_probe_events",
		"openai_downgrade_probe_states", "scheduler_outbox",
	} {
		t.Run(table, func(t *testing.T) {
			db := newProbePostgres(t)
			mutation := seedProbePostgres(t, db)
			before := probePostgresSnapshot(t, db)
			stateBefore := *mutation.State
			_, err := db.Exec(`
				CREATE FUNCTION reject_probe_write() RETURNS trigger LANGUAGE plpgsql AS
				$$ BEGIN RAISE EXCEPTION 'injected probe write failure'; END $$;
				CREATE TRIGGER reject_write BEFORE INSERT OR UPDATE ON ` + pq.QuoteIdentifier(table) + `
				FOR EACH ROW EXECUTE FUNCTION reject_probe_write();
			`)
			require.NoError(t, err)
			repo := &openAIDowngradeProbeRepository{db: db}
			err = repo.CommitOpenAIDowngradeMutation(context.Background(), mutation)
			require.ErrorContains(t, err, "injected probe write failure")
			require.Equal(t, before, probePostgresSnapshot(t, db))
			require.Equal(t, stateBefore, *mutation.State)
		})
	}
}

func TestOpenAIProbePostgresRejectsInterveningAccountChanges(t *testing.T) {
	changes := map[string]string{
		"manual_pause": "schedulable = FALSE, updated_at = updated_at + INTERVAL '1 microsecond'",
		"disabled":     "status = 'disabled'",
		"other_error":  "status = 'error', error_message = 'unowned'",
		"new_proxy":    "proxy_id = 4",
		"credentials":  "extra = '{\"identity_generation\":2}', updated_at = updated_at + INTERVAL '1 microsecond'",
		"soft_delete":  "deleted_at = NOW()",
		"expired":      "expires_at = NOW() - INTERVAL '1 minute'",
		"shadow":       "parent_account_id = 99",
		"platform":     "platform = 'anthropic'",
		"account_type": "type = 'apikey'",
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			db := newProbePostgres(t)
			mutation := seedProbePostgres(t, db)
			_, err := db.Exec("UPDATE accounts SET " + change + " WHERE id = 7")
			require.NoError(t, err)
			before := probePostgresSnapshot(t, db)
			repo := &openAIDowngradeProbeRepository{db: db}
			require.ErrorIs(t, repo.CommitOpenAIDowngradeMutation(context.Background(), mutation), service.ErrOpenAIProbeStale)
			require.Equal(t, before, probePostgresSnapshot(t, db))
		})
	}
}

func TestOpenAIProbePostgresExactlyOneConcurrentCommit(t *testing.T) {
	db := newProbePostgres(t)
	first := seedProbePostgres(t, db)
	second := probeCommitFixture()
	repo := &openAIDowngradeProbeRepository{db: db}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, mutation := range []*service.OpenAIDowngradeMutation{first, second} {
		go func(mutation *service.OpenAIDowngradeMutation) {
			<-start
			results <- repo.CommitOpenAIDowngradeMutation(ctx, mutation)
		}(mutation)
	}
	close(start)
	firstErr, secondErr := <-results, <-results
	if firstErr == nil {
		require.ErrorIs(t, secondErr, service.ErrOpenAIProbeStale)
	} else {
		require.ErrorIs(t, firstErr, service.ErrOpenAIProbeStale)
		require.NoError(t, secondErr)
	}
	for _, table := range []string{"openai_downgrade_probe_results", "openai_downgrade_probe_events", "scheduler_outbox"} {
		var count int
		require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM "+pq.QuoteIdentifier(table)).Scan(&count))
		require.Equal(t, 1, count, table)
	}
	var schedulable bool
	require.NoError(t, db.QueryRow("SELECT schedulable FROM accounts WHERE id = 7").Scan(&schedulable))
	require.False(t, schedulable)
}

func TestOpenAIProbePostgresCompletesRestoredOAuthQualification(t *testing.T) {
	db := newProbePostgres(t)
	mutation := seedProbePostgres(t, db)
	_, err := db.Exec(`
		UPDATE accounts
		SET schedulable = FALSE,
			extra = jsonb_build_object($1::text, TRUE, $2::text, $3::bigint)
		WHERE id = $4
	`, service.OpenAIDowngradeQualificationExtraKey, service.OpenAIOAuthQualifiedProxyExtraKey,
		*mutation.ExpectedProxyID, mutation.AccountID)
	require.NoError(t, err)

	enabled := true
	reasoningTokens := 900
	mutation.ExpectedSchedulable = false
	mutation.Schedulable = &enabled
	mutation.CompleteQualification = true
	mutation.Events = nil
	mutation.Results = []service.OpenAIDowngradeProbeResult{{
		AccountID: mutation.AccountID, ProxyID: mutation.ExpectedProxyID,
		TransportOK: true, AnswerCorrect: true, HTTPStatus: 200,
		ReasoningTokens: &reasoningTokens,
	}}

	repo := &openAIDowngradeProbeRepository{db: db}
	require.NoError(t, repo.CommitOpenAIDowngradeMutation(context.Background(), mutation))

	var schedulable, qualificationPending bool
	var qualifiedProxyID int64
	require.NoError(t, db.QueryRow(`
		SELECT schedulable,
			COALESCE((extra ->> $1)::boolean, FALSE),
			(extra ->> $2)::bigint
		FROM accounts WHERE id = $3
	`, service.OpenAIDowngradeQualificationExtraKey, service.OpenAIOAuthQualifiedProxyExtraKey,
		mutation.AccountID).Scan(&schedulable, &qualificationPending, &qualifiedProxyID))
	require.True(t, schedulable)
	require.False(t, qualificationPending)
	require.Equal(t, *mutation.ExpectedProxyID, qualifiedProxyID)

	var outboxCount int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM scheduler_outbox").Scan(&outboxCount))
	require.Equal(t, 1, outboxCount)
}

func TestOpenAIProbePostgresStaleStateAndHardDeletion(t *testing.T) {
	for _, change := range []string{
		"UPDATE openai_downgrade_probe_states SET updated_at = updated_at + INTERVAL '1 microsecond'",
		"DELETE FROM accounts WHERE id = 7",
	} {
		t.Run(strings.Fields(change)[0], func(t *testing.T) {
			db := newProbePostgres(t)
			mutation := seedProbePostgres(t, db)
			_, err := db.Exec(change)
			require.NoError(t, err)
			before := probePostgresSnapshot(t, db)
			repo := &openAIDowngradeProbeRepository{db: db}
			require.ErrorIs(t, repo.CommitOpenAIDowngradeMutation(context.Background(), mutation), service.ErrOpenAIProbeStale)
			require.Equal(t, before, probePostgresSnapshot(t, db))
		})
	}
}
