package repository

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func newJointRescueGraduationPostgres(t *testing.T) (*accountRepository, *sql.DB, service.OpenAIRescueGraduation) {
	t.Helper()
	db := newProbePostgres(t)
	seedProbePostgres(t, db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	_, err := db.ExecContext(t.Context(), `
		CREATE TABLE account_groups (
			account_id BIGINT NOT NULL REFERENCES accounts(id),
			group_id BIGINT NOT NULL,
			priority INT NOT NULL DEFAULT 50,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY(account_id, group_id)
		);
		CREATE TABLE groups (id BIGINT PRIMARY KEY, deleted_at TIMESTAMPTZ);
		INSERT INTO groups(id) VALUES(11), (12), (99);
		INSERT INTO account_groups(account_id, group_id) VALUES(7, 99);
		UPDATE openai_downgrade_probe_states SET state = 'on_duty', probe_mode = 'normal' WHERE account_id = 7;
	`)
	require.NoError(t, err)
	migration, err := os.ReadFile(filepath.Join("..", "..", "migrations", "238_scheduler_account_revisions.sql"))
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), string(migration))
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `
		UPDATE accounts SET schedulable = FALSE, status = 'active',
			extra = extra || jsonb_build_object(
				'openai_rescue_lane', jsonb_build_object(
					'entered_at', $1::timestamptz,
					'orig_group_ids', jsonb_build_array(11, 12)),
				'openai_rescue_suspected', TRUE),
			updated_at = $1
		WHERE id = 7
	`, now.Add(-time.Hour))
	require.NoError(t, err)
	m := service.OpenAIRescueGraduation{
		AccountID: 7, HostProbeAt: now.Add(-time.Minute),
		EvidenceExpiresAt: now.Add(time.Minute),
		OldGroupIDs:       []int64{99}, GroupIDs: []int64{11, 12},
		Extra: map[string]any{
			"openai_rescue_lane": nil, "openai_rescue_suspected": false,
			"openai_rescue_rescued_at": now.Format(time.RFC3339), "openai_rescue_rescue_count": 1,
		},
	}
	require.NoError(t, db.QueryRowContext(t.Context(),
		"SELECT updated_at FROM accounts WHERE id = 7").Scan(&m.ExpectedUpdatedAt))
	require.NoError(t, db.QueryRowContext(t.Context(), `
		INSERT INTO openai_downgrade_probe_results(
			account_id, proxy_id, mode, transport_ok, answer_correct, reasoning_tokens, http_status, created_at
		) VALUES(7, 3, 'qualification', TRUE, TRUE, 1400, 200, $1)
		RETURNING id
	`, m.HostProbeAt).Scan(&m.HostProbeID))
	// Discard only setup notifications in this disposable schema.
	_, err = db.ExecContext(t.Context(), "DELETE FROM scheduler_outbox")
	require.NoError(t, err)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	return &accountRepository{client: client, sql: db}, db, m
}

func TestR17bgGraduationRetainsLocalExitGuards(t *testing.T) {
	for _, change := range []string{"missing", "null", "invalid", "deleted_group", "terminated", "empty"} {
		t.Run(change, func(t *testing.T) {
			repo, db, m := newJointRescueGraduationPostgres(t)
			var query string
			switch change {
			case "missing":
				query = `UPDATE accounts SET extra=extra #- '{openai_rescue_lane,orig_group_ids}' WHERE id=7`
			case "null":
				query = `UPDATE accounts SET extra=jsonb_set(extra,'{openai_rescue_lane,orig_group_ids}','null') WHERE id=7`
			case "invalid":
				query = `UPDATE accounts SET extra=jsonb_set(extra,'{openai_rescue_lane,orig_group_ids}','[11,"bad"]') WHERE id=7`
			case "deleted_group":
				query = `UPDATE groups SET deleted_at=clock_timestamp() WHERE id=11`
			case "terminated":
				query = `UPDATE accounts SET extra=extra || '{"openai_rescue_terminated_at":"stopped"}' WHERE id=7`
			case "empty":
				query = `UPDATE accounts SET extra=jsonb_set(extra,'{openai_rescue_lane,orig_group_ids}','[]') WHERE id=7`
			}
			_, err := db.ExecContext(t.Context(), query)
			require.NoError(t, err)
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT updated_at FROM accounts WHERE id=7`).Scan(&m.ExpectedUpdatedAt))
			before := rescueGraduationSnapshot(t, db)
			err = repo.CommitOpenAIRescueGraduation(t.Context(), m)
			if change == "empty" {
				require.NoError(t, err)
				var count int
				require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM account_groups WHERE account_id=7`).Scan(&count))
				require.Zero(t, count, "restore the validated database snapshot, not caller-supplied groups")
			} else {
				require.Error(t, err)
				require.Equal(t, before, rescueGraduationSnapshot(t, db))
			}
		})
	}
}

func rescueGraduationSnapshot(t *testing.T, db *sql.DB) string {
	t.Helper()
	var groups string
	require.NoError(t, db.QueryRowContext(t.Context(), `
		SELECT COALESCE(jsonb_agg(to_jsonb(g) ORDER BY account_id, group_id), '[]'::jsonb)::text
		FROM account_groups g
	`).Scan(&groups))
	return probePostgresSnapshot(t, db) + groups
}

func TestOpenAIRescueGraduationPostgresSuccessAndReplay(t *testing.T) {
	repo, db, m := newJointRescueGraduationPostgres(t)
	require.NoError(t, repo.CommitOpenAIRescueGraduation(t.Context(), m))
	var graduated bool
	require.NoError(t, db.QueryRowContext(t.Context(), `
		SELECT extra->'openai_rescue_lane' = 'null'::jsonb
			AND extra->>'openai_rescue_suspected' = 'false'
			AND extra->>'openai_rescue_rescue_count' = '1'
			AND schedulable AND updated_at > $1
		FROM accounts WHERE id = 7
	`, m.ExpectedUpdatedAt).Scan(&graduated))
	require.True(t, graduated)
	var groupIDs pq.Int64Array
	require.NoError(t, db.QueryRowContext(t.Context(),
		"SELECT array_agg(group_id ORDER BY priority) FROM account_groups WHERE account_id = 7").Scan(&groupIDs))
	require.Equal(t, pq.Int64Array{11, 12}, groupIDs)
	var notifications int
	require.NoError(t, db.QueryRowContext(t.Context(),
		"SELECT count(*) FROM scheduler_outbox WHERE account_id = 7").Scan(&notifications))
	require.Equal(t, 3, notifications)
	after := rescueGraduationSnapshot(t, db)
	require.ErrorIs(t, repo.CommitOpenAIRescueGraduation(t.Context(), m), service.ErrOpenAIProbeStale)
	require.Equal(t, after, rescueGraduationSnapshot(t, db))
}

func TestOpenAIRescueDuePostgresIsolationAndPause(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit string
		due  bool
	}{
		{"isolated_normal_rescue", "", true},
		{"manual_pause", `INSERT INTO openai_downgrade_probe_controls(account_id, manual_paused)
			VALUES(7, TRUE) ON CONFLICT(account_id) DO UPDATE SET manual_paused = TRUE`, false},
		{"exited", `UPDATE accounts SET extra = jsonb_set(extra, '{openai_rescue_lane,exit_reason}', '"auth_rejected"') WHERE id = 7`, false},
		{"marker_missing", `UPDATE accounts SET extra = extra - 'openai_rescue_lane' WHERE id = 7`, false},
		{"marker_null", `UPDATE accounts SET extra = jsonb_set(extra, '{openai_rescue_lane}', 'null') WHERE id = 7`, false},
		{"entry_missing", `UPDATE accounts SET extra = extra #- '{openai_rescue_lane,entered_at}' WHERE id = 7`, false},
		{"entry_malformed", `UPDATE accounts SET extra = jsonb_set(extra, '{openai_rescue_lane,entered_at}', '"not-a-time"') WHERE id = 7`, false},
		{"entry_impossible_date", `UPDATE accounts SET extra = jsonb_set(extra, '{openai_rescue_lane,entered_at}', '"2026-02-30T12:30:00Z"') WHERE id = 7`, false},
		{"entry_invalid_hour", `UPDATE accounts SET extra = jsonb_set(extra, '{openai_rescue_lane,entered_at}', '"2026-10-04T24:00:00Z"') WHERE id = 7`, false},
		{"entry_nanosecond_utc", `UPDATE accounts SET extra = jsonb_set(extra, '{openai_rescue_lane,entered_at}', '"2026-10-04T12:30:00.123456789Z"') WHERE id = 7`, true},
		{"entry_offset", `UPDATE accounts SET extra = jsonb_set(extra, '{openai_rescue_lane,entered_at}', '"2026-10-04T12:30:00+08:00"') WHERE id = 7`, true},
		{"pending_replace", `UPDATE openai_downgrade_probe_states SET state = 'pending_replace' WHERE account_id = 7`, false},
		{"disabled", `UPDATE accounts SET status = 'disabled' WHERE id = 7`, false},
		{"expired", `UPDATE accounts SET expires_at = NOW() - INTERVAL '1 minute' WHERE id = 7`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, db, _ := newJointRescueGraduationPostgres(t)
			now := time.Now().UTC()
			_, err := db.ExecContext(t.Context(), `
				UPDATE openai_downgrade_probe_states SET next_probe_at = $1 WHERE account_id = 7;
			`, now.Add(-time.Second))
			require.NoError(t, err)
			// Isolate the eligibility check from the separately retained IP throttle.
			_, err = db.ExecContext(t.Context(), "DELETE FROM openai_downgrade_probe_results")
			require.NoError(t, err)
			if tc.edit != "" {
				_, err = db.ExecContext(t.Context(), tc.edit)
				require.NoError(t, err)
			}
			due, err := (&openAIDowngradeProbeRepository{db: db}).ListDueOpenAIDowngradeStates(t.Context(), now, 10)
			require.NoError(t, err)
			require.Equal(t, tc.due, len(due) == 1)
			if tc.due {
				require.Equal(t, int64(7), due[0].AccountID)
			}
		})
	}
}

func TestOpenAIRescueGraduationPostgresConcurrentSingleCommit(t *testing.T) {
	repo, db, m := newJointRescueGraduationPostgres(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	const callers = 8
	results := make(chan error, callers)
	start := make(chan struct{})
	for range callers {
		go func() {
			<-start
			results <- repo.CommitOpenAIRescueGraduation(ctx, m)
		}()
	}
	close(start)
	successes, conflicts := 0, 0
	for range callers {
		err := <-results
		switch {
		case err == nil:
			successes++
		case errors.Is(err, service.ErrOpenAIProbeStale):
			conflicts++
		default:
			require.NoError(t, err)
		}
	}
	require.Equal(t, 1, successes)
	require.Equal(t, callers-1, conflicts)
	var notifications int
	require.NoError(t, db.QueryRowContext(t.Context(),
		"SELECT count(*) FROM scheduler_outbox WHERE account_id = 7").Scan(&notifications))
	require.Equal(t, 3, notifications)
}

func TestOpenAIRescueGraduationPostgresRejectsStaleEvidence(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
		err  error
	}{
		{"new_account_generation", "UPDATE accounts SET updated_at = clock_timestamp() WHERE id = 7", service.ErrOpenAIProbeStale},
		{"manual_pause", "UPDATE accounts SET schedulable = FALSE WHERE id = 7; INSERT INTO openai_downgrade_probe_controls(account_id, manual_paused) VALUES(7, TRUE) ON CONFLICT(account_id) DO UPDATE SET manual_paused = TRUE", service.ErrOpenAIProbeStale},
		{"paused_account", "UPDATE accounts SET schedulable = FALSE WHERE id = 7", service.ErrOpenAIProbeStale},
		{"expired_account", "UPDATE accounts SET expires_at = NOW() - INTERVAL '1 second' WHERE id = 7", service.ErrOpenAIProbeStale},
		{"deleted_account", "UPDATE accounts SET deleted_at = NOW() WHERE id = 7", service.ErrOpenAIProbeStale},
		{"error_account", "UPDATE accounts SET status = 'error' WHERE id = 7", service.ErrOpenAIProbeStale},
		{"marker_removed", "UPDATE accounts SET extra = extra - 'openai_rescue_lane' WHERE id = 7", service.ErrOpenAIProbeStale},
		{"rescue_exited", `UPDATE accounts SET extra = jsonb_set(extra, '{openai_rescue_lane,exit_reason}', '"auth_rejected"') WHERE id = 7`, service.ErrOpenAIProbeStale},
		{"requalifying", "UPDATE openai_downgrade_probe_states SET probe_mode = 'qualification' WHERE account_id = 7", service.ErrOpenAIProbeStale},
		{"host_result_missing", "DELETE FROM openai_downgrade_probe_results WHERE account_id = 7", service.ErrRescueRecoveryUnverified},
		{"host_failed", "UPDATE openai_downgrade_probe_results SET answer_correct = FALSE WHERE account_id = 7", service.ErrRescueRecoveryUnverified},
		{"host_unfinished", "UPDATE openai_downgrade_probe_results SET transport_ok = FALSE, answer_correct = NULL WHERE account_id = 7", service.ErrRescueRecoveryUnverified},
		{"host_wrong_mode", "UPDATE openai_downgrade_probe_results SET mode = 'normal' WHERE account_id = 7", service.ErrRescueRecoveryUnverified},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, db, m := newJointRescueGraduationPostgres(t)
			_, err := db.ExecContext(t.Context(), tc.sql)
			require.NoError(t, err)
			before := rescueGraduationSnapshot(t, db)
			require.ErrorIs(t, repo.CommitOpenAIRescueGraduation(t.Context(), m), tc.err)
			require.Equal(t, before, rescueGraduationSnapshot(t, db))
		})
	}
}

func TestOpenAIRescueGraduationPostgresLatestProbeHasStableIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		offset time.Duration
		pass   bool
	}{
		{"later_failure", time.Microsecond, false},
		{"same_timestamp_failure", 0, false},
		{"same_timestamp_different_pass", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, db, m := newJointRescueGraduationPostgres(t)
			var latestID int64
			require.NoError(t, db.QueryRowContext(t.Context(), `
				INSERT INTO openai_downgrade_probe_results(
					account_id, proxy_id, mode, transport_ok, answer_correct, reasoning_tokens, http_status, created_at
				) VALUES(7, 3, 'qualification', TRUE, $1, 1400, 200, $2) RETURNING id
			`, tc.pass, m.HostProbeAt.Add(tc.offset)).Scan(&latestID))
			snapshots, err := (&openAIDowngradeProbeRepository{db: db}).ListOpenAIProbeHealthSnapshots(t.Context(), []int64{7})
			require.NoError(t, err)
			require.Len(t, snapshots, 1)
			require.Equal(t, latestID, snapshots[0].LastProbe.ID)
			before := rescueGraduationSnapshot(t, db)
			require.ErrorIs(t, repo.CommitOpenAIRescueGraduation(t.Context(), m), service.ErrRescueRecoveryUnverified)
			require.Equal(t, before, rescueGraduationSnapshot(t, db))
		})
	}
}

func TestOpenAIRescueGraduationPostgresRollbackAtEveryWriteBoundary(t *testing.T) {
	for _, tc := range []struct {
		table     string
		operation string
	}{
		{"account_groups", "DELETE"}, {"account_groups", "INSERT"},
		{"accounts", "UPDATE"}, {"scheduler_outbox", "INSERT"},
	} {
		t.Run(tc.table+"_"+tc.operation, func(t *testing.T) {
			repo, db, m := newJointRescueGraduationPostgres(t)
			before := rescueGraduationSnapshot(t, db)
			_, err := db.ExecContext(t.Context(), `
				CREATE FUNCTION reject_graduation_write() RETURNS trigger LANGUAGE plpgsql AS
				$$ BEGIN RAISE EXCEPTION 'injected graduation write failure'; END $$;
				CREATE TRIGGER reject_write BEFORE `+tc.operation+` ON `+pq.QuoteIdentifier(tc.table)+`
				FOR EACH ROW EXECUTE FUNCTION reject_graduation_write();
			`)
			require.NoError(t, err)
			require.ErrorContains(t, repo.CommitOpenAIRescueGraduation(t.Context(), m), "injected graduation write failure")
			require.Equal(t, before, rescueGraduationSnapshot(t, db))
		})
	}
}

func TestOpenAIRescueGraduationPostgresEvidenceExpiresWhileWaitingForLock(t *testing.T) {
	repo, db, m := newJointRescueGraduationPostgres(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	lock, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = lock.Rollback() }()
	_, err = lock.ExecContext(ctx, "SELECT id FROM accounts WHERE id = 7 FOR UPDATE")
	require.NoError(t, err)
	m.EvidenceExpiresAt = time.Now().Add(100 * time.Millisecond)
	before := rescueGraduationSnapshot(t, db)
	result := make(chan error, 1)
	go func() { result <- repo.CommitOpenAIRescueGraduation(ctx, m) }()
	<-time.After(time.Until(m.EvidenceExpiresAt) + 10*time.Millisecond)
	require.NoError(t, lock.Rollback())
	require.ErrorIs(t, <-result, service.ErrRescueRecoveryUnverified)
	require.Equal(t, before, rescueGraduationSnapshot(t, db))
}

func rescueMarkerUpdate(m service.OpenAIRescueGraduation, withdraw bool) service.OpenAIRescueMarkerUpdate {
	return service.OpenAIRescueMarkerUpdate{
		AccountID: m.AccountID, ExpectedUpdatedAt: m.ExpectedUpdatedAt, Withdraw: withdraw,
		Marker: service.OpenAIRescueLaneMarker{
			EnteredAt: m.ExpectedUpdatedAt, OrigGroupIDs: []int64{11, 12}, SeedOK: true,
		},
	}
}

func TestOpenAIRescueMarkerPostgresAtomicWithdrawAndReplay(t *testing.T) {
	repo, db, graduation := newJointRescueGraduationPostgres(t)
	m := rescueMarkerUpdate(graduation, true)
	version, err := repo.CommitOpenAIRescueMarker(t.Context(), m)
	require.NoError(t, err)
	require.True(t, version.After(m.ExpectedUpdatedAt))
	var withdrawn bool
	require.NoError(t, db.QueryRowContext(t.Context(), `
		SELECT NOT schedulable AND extra->>'openai_rescue_suspected' = 'true'
			AND extra->'openai_rescue_lane'->>'seed_ok' = 'true' AND updated_at = $1
		FROM accounts WHERE id = 7
	`, version).Scan(&withdrawn))
	require.True(t, withdrawn)
	after := rescueGraduationSnapshot(t, db)
	_, err = repo.CommitOpenAIRescueMarker(t.Context(), m)
	require.ErrorIs(t, err, service.ErrOpenAIProbeStale)
	require.Equal(t, after, rescueGraduationSnapshot(t, db))
	m.ExpectedUpdatedAt, m.Withdraw = version, false
	m.Marker.SeedOK = false
	_, err = repo.CommitOpenAIRescueMarker(t.Context(), m)
	require.NoError(t, err)
	var stillWithdrawn bool
	require.NoError(t, db.QueryRowContext(t.Context(), `
		SELECT NOT schedulable AND extra->>'openai_rescue_suspected' = 'true'
			AND COALESCE(extra->'openai_rescue_lane'->>'seed_ok', 'false') = 'false' FROM accounts WHERE id = 7
	`).Scan(&stillWithdrawn))
	require.True(t, stillWithdrawn)
	var notifications int
	require.NoError(t, db.QueryRowContext(t.Context(),
		"SELECT count(*) FROM scheduler_outbox WHERE account_id = 7").Scan(&notifications))
	// Consecutive account_changed events are coalesced by the native outbox.
	require.Equal(t, 1, notifications)
}

func TestOpenAIRescueMarkerPostgresRejectsStaleAccount(t *testing.T) {
	for _, withdraw := range []bool{false, true} {
		for _, change := range []string{"graduated", "reauthorized", "new_rescue", "marker_removed"} {
			t.Run(change+map[bool]string{false: "_observe", true: "_withdraw"}[withdraw], func(t *testing.T) {
				repo, db, graduation := newJointRescueGraduationPostgres(t)
				m := rescueMarkerUpdate(graduation, withdraw)
				switch change {
				case "graduated":
					require.NoError(t, repo.CommitOpenAIRescueGraduation(t.Context(), graduation))
				case "marker_removed":
					_, err := db.ExecContext(t.Context(), "UPDATE accounts SET extra = extra - 'openai_rescue_lane' WHERE id = 7")
					require.NoError(t, err)
				case "new_rescue":
					_, err := db.ExecContext(t.Context(), `
						UPDATE accounts SET updated_at = clock_timestamp(),
							extra = jsonb_set(extra, '{openai_rescue_lane,entered_at}', to_jsonb(clock_timestamp()))
						WHERE id = 7`)
					require.NoError(t, err)
				case "reauthorized":
					_, err := db.ExecContext(t.Context(), "UPDATE accounts SET updated_at = clock_timestamp() WHERE id = 7")
					require.NoError(t, err)
				}
				before := rescueGraduationSnapshot(t, db)
				_, err := repo.CommitOpenAIRescueMarker(t.Context(), m)
				require.ErrorIs(t, err, service.ErrOpenAIProbeStale)
				require.Equal(t, before, rescueGraduationSnapshot(t, db))
			})
		}
	}
}

func TestOpenAIRescueMarkerPostgresRollbackAtEveryWriteBoundary(t *testing.T) {
	for _, withdraw := range []bool{false, true} {
		for _, table := range []string{"accounts", "scheduler_outbox"} {
			t.Run(table+map[bool]string{false: "_observe", true: "_withdraw"}[withdraw], func(t *testing.T) {
				repo, db, graduation := newJointRescueGraduationPostgres(t)
				before := rescueGraduationSnapshot(t, db)
				operation := "UPDATE"
				if table == "scheduler_outbox" {
					operation = "INSERT"
				}
				_, err := db.ExecContext(t.Context(), `
					CREATE FUNCTION reject_marker_write() RETURNS trigger LANGUAGE plpgsql AS
					$$ BEGIN RAISE EXCEPTION 'injected marker write failure'; END $$;
					CREATE TRIGGER reject_write BEFORE `+operation+` ON `+pq.QuoteIdentifier(table)+`
					FOR EACH ROW EXECUTE FUNCTION reject_marker_write();
				`)
				require.NoError(t, err)
				_, err = repo.CommitOpenAIRescueMarker(t.Context(), rescueMarkerUpdate(graduation, withdraw))
				require.ErrorContains(t, err, "injected marker write failure")
				require.Equal(t, before, rescueGraduationSnapshot(t, db))
			})
		}
	}
}

func TestOpenAIRescueMarkerPostgresRacesGraduation(t *testing.T) {
	repo, db, graduation := newJointRescueGraduationPostgres(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	results := make(chan error, 2)
	start := make(chan struct{})
	go func() {
		<-start
		results <- repo.CommitOpenAIRescueGraduation(ctx, graduation)
	}()
	go func() {
		<-start
		_, err := repo.CommitOpenAIRescueMarker(ctx, rescueMarkerUpdate(graduation, true))
		results <- err
	}()
	close(start)
	successes, conflicts := 0, 0
	for range 2 {
		switch err := <-results; {
		case err == nil:
			successes++
		case errors.Is(err, service.ErrOpenAIProbeStale):
			conflicts++
		default:
			require.NoError(t, err)
		}
	}
	require.Equal(t, 1, successes)
	require.Equal(t, 1, conflicts)
	var coherent bool
	require.NoError(t, db.QueryRowContext(t.Context(), `
		SELECT (schedulable AND extra->'openai_rescue_lane' = 'null'::jsonb
				AND extra->>'openai_rescue_suspected' = 'false')
			OR (NOT schedulable AND jsonb_typeof(extra->'openai_rescue_lane') = 'object'
				AND extra->>'openai_rescue_suspected' = 'true')
		FROM accounts WHERE id = 7
	`).Scan(&coherent))
	require.True(t, coherent)
}
