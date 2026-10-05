package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestMigrationFilenameAliasesMatchPublishedFiles(t *testing.T) {
	require.Len(t, migrationFilenameAliases, 9)
	for name, alias := range migrationFilenameAliases {
		t.Run(name, func(t *testing.T) {
			content, err := migrations.FS.ReadFile(name)
			require.NoError(t, err)
			require.Equal(t, alias.checksum, migrationChecksum(string(content)))
			require.NotEqual(t, name, alias.filename)
			_, broadCompatibility := migrationChecksumCompatibilityRules[name]
			require.False(t, broadCompatibility, "alias validation must not be bypassed by a checksum compatibility shortcut")
		})
	}
}

func TestApplyMigrationsFSFilenameAlias(t *testing.T) {
	const name = "238_scheduler_account_revisions.sql"
	alias := migrationFilenameAliases[name]
	content, err := migrations.FS.ReadFile(name)
	require.NoError(t, err)
	checksum := migrationChecksum(string(content))
	for _, tc := range []struct {
		name, canonical, legacy, wantErr string
		canonicalErr, aliasErr           error
		changeFile, apply                bool
	}{
		{name: "legacy only", legacy: checksum},
		{name: "canonical only", canonical: checksum},
		{name: "both matching", canonical: checksum, legacy: checksum},
		{name: "fresh database", apply: true},
		{name: "bad alias", legacy: "unknown", wantErr: "alias r3_0238_scheduler_account_revisions.sql checksum mismatch"},
		{name: "bad alias cannot hide behind canonical", canonical: checksum, legacy: "unknown", wantErr: "alias r3_0238_scheduler_account_revisions.sql checksum mismatch"},
		{name: "bad canonical cannot hide behind alias", canonical: "unknown", legacy: checksum, wantErr: "checksum mismatch"},
		{name: "changed file cannot reuse alias", legacy: checksum, changeFile: true, wantErr: "checksum mismatch"},
		{name: "alias query failure", aliasErr: errors.New("alias unavailable"), wantErr: "alias unavailable"},
		{name: "canonical query failure", canonicalErr: errors.New("canonical unavailable"), wantErr: "canonical unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			prepareMigrationsBootstrapExpectations(mock)
			query := mock.ExpectQuery("SELECT checksum FROM schema_migrations WHERE filename = \\$1").WithArgs(name)
			if tc.canonicalErr != nil {
				query.WillReturnError(tc.canonicalErr)
			} else {
				rows := sqlmock.NewRows([]string{"checksum"})
				if tc.canonical != "" {
					rows.AddRow(tc.canonical)
				}
				query.WillReturnRows(rows)
			}
			if tc.canonicalErr == nil && (tc.canonical == "" || tc.canonical == checksum) {
				query := mock.ExpectQuery("SELECT checksum FROM schema_migrations WHERE filename = \\$1").WithArgs(alias.filename)
				if tc.aliasErr != nil {
					query.WillReturnError(tc.aliasErr)
				} else {
					rows := sqlmock.NewRows([]string{"checksum"})
					if tc.legacy != "" {
						rows.AddRow(tc.legacy)
					}
					query.WillReturnRows(rows)
				}
			}
			if tc.apply {
				mock.ExpectBegin()
				mock.ExpectExec(regexp.QuoteMeta(strings.TrimSpace(string(content)))).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectExec("INSERT INTO schema_migrations").WithArgs(name, checksum).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectCommit()
			}
			mock.ExpectExec("SELECT pg_advisory_unlock\\(\\$1\\)").WithArgs(migrationsAdvisoryLockID).WillReturnResult(sqlmock.NewResult(0, 1))
			file := append([]byte(nil), content...)
			if tc.changeFile {
				file = append(file, []byte("\n-- unverified change")...)
			}
			err = applyMigrationsFS(context.Background(), db, fstest.MapFS{name: {Data: file}})
			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.wantErr)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestMigrationFilenameAliasesPostgresUpgrade(t *testing.T) {
	for _, history := range []string{"canonical", "legacy", "mixed", "conflicting"} {
		t.Run(history, func(t *testing.T) {
			db, pending := newProbeUpgradePostgres(t)
			ctx := t.Context()
			require.NoError(t, applyMigrationsFS(ctx, db, pending))
			for name := range pending {
				if history == "legacy" || history == "mixed" && strings.HasPrefix(name, "238_") {
					_, err := db.ExecContext(ctx, "UPDATE schema_migrations SET filename=$1 WHERE filename=$2", migrationFilenameAliases[name].filename, name)
					require.NoError(t, err)
				}
			}
			if history == "conflicting" {
				_, err := db.ExecContext(ctx, "INSERT INTO schema_migrations(filename,checksum) VALUES($1,$2)", migrationFilenameAliases["238_scheduler_account_revisions.sql"].filename, "unknown")
				require.NoError(t, err)
			}
			var before, after string
			const ledger = "SELECT json_agg(m ORDER BY filename)::text FROM schema_migrations m"
			require.NoError(t, db.QueryRowContext(ctx, ledger).Scan(&before))
			beforeState := probePostgresSnapshot(t, db)
			for range 2 {
				err := applyMigrationsFS(ctx, db, pending)
				if history == "conflicting" {
					require.ErrorContains(t, err, "checksum mismatch")
				} else {
					require.NoError(t, err)
				}
			}
			require.NoError(t, db.QueryRowContext(ctx, ledger).Scan(&after))
			require.Equal(t, before, after, "migration history must not be rewritten")
			require.Equal(t, beforeState, probePostgresSnapshot(t, db), "backfills must not run twice")
			var triggers int
			require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM pg_trigger WHERE tgrelid='accounts'::regclass AND tgname='accounts_scheduler_revision'").Scan(&triggers))
			require.Equal(t, 1, triggers)
		})
	}
}

func TestMigrationProductionSchemaCloneUpgrade(t *testing.T) {
	if os.Getenv("SUB2API_MIGRATION_SCHEMA_CLONE") != "1" {
		t.Skip("requires an explicitly restored disposable production schema clone")
	}
	socket := os.Getenv("SUB2API_PROBE_TEST_SOCKET")
	require.True(t, filepath.IsAbs(socket))
	require.Equal(t, "probe-pg-sock", filepath.Base(socket))
	require.NotContains(t, socket, "'")
	db, err := sql.Open("postgres", fmt.Sprintf("host='%s' port=%s dbname=production_schema_clone sslmode=disable", socket, os.Getenv("SUB2API_PROBE_TEST_PORT")))
	require.NoError(t, err)
	defer db.Close()
	var accounts int
	require.NoError(t, db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM accounts").Scan(&accounts))
	require.Zero(t, accounts, "this test only accepts a schema-only clone without customer data")
	for range 2 {
		require.NoError(t, ApplyMigrations(t.Context(), db))
	}
	for name, alias := range migrationFilenameAliases {
		var checksum string
		require.NoError(t, db.QueryRowContext(t.Context(), "SELECT checksum FROM schema_migrations WHERE filename=$1", alias.filename).Scan(&checksum))
		require.Equal(t, alias.checksum, checksum)
		require.ErrorIs(t, db.QueryRowContext(t.Context(), "SELECT checksum FROM schema_migrations WHERE filename=$1", name).Scan(&checksum), sql.ErrNoRows)
	}
	var ticketTableGone bool
	require.NoError(t, db.QueryRowContext(t.Context(), "SELECT to_regclass('openai_codex_tickets') IS NULL").Scan(&ticketTableGone))
	require.True(t, ticketTableGone, "248 must be evaluated, not silently skipped")
	var constraint string
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT pg_get_constraintdef(oid)
		FROM pg_constraint WHERE conrelid='openai_downgrade_probe_results'::regclass
		AND conname='openai_downgrade_probe_results_verdict_check'`).Scan(&constraint))
	require.Contains(t, constraint, "transport_ok IS TRUE")
	require.NotContains(t, constraint, "answer_correct IS NOT NULL", "249 must allow an unknown completed answer")
}

func TestMigrationFreshPostgresUpgrade(t *testing.T) {
	admin := newProbePostgresWithMigrations(t, nil)
	// Historical migrations explicitly inspect public, so use a disposable
	// database rather than changing their behavior to fit a private schema.
	name := fmt.Sprintf("migration_fresh_%d_%d", os.Getpid(), probePostgresSchemaSequence.Add(1))
	_, err := admin.ExecContext(t.Context(), "CREATE DATABASE "+pq.QuoteIdentifier(name))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := admin.Exec("DROP DATABASE " + pq.QuoteIdentifier(name))
		require.NoError(t, err)
	})
	db, err := sql.Open("postgres", fmt.Sprintf("host='%s' port=%s dbname=%s sslmode=disable",
		os.Getenv("SUB2API_PROBE_TEST_SOCKET"), os.Getenv("SUB2API_PROBE_TEST_PORT"), name))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	for range 2 {
		require.NoError(t, ApplyMigrations(t.Context(), db))
	}
	var triggers int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM pg_trigger
		WHERE tgrelid='accounts'::regclass AND tgname='accounts_scheduler_revision'`).Scan(&triggers))
	require.Equal(t, 1, triggers)
}
