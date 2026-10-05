package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func newRescueGraduationPostgres(t *testing.T) (*accountRepository, *sql.DB, *service.Account, int64) {
	t.Helper()
	repo, db, account := newOAuthReauthorizationPostgres(t)
	migration, err := os.ReadFile(filepath.Join("..", "..", "migrations", "237_openai_downgrade_probe.sql"))
	require.NoError(t, err)
	_, err = db.Exec(string(migration))
	require.NoError(t, err)
	for _, name := range []string{"239_openai_probe_ownership.sql", "241_openai_probe_turn_state_len.sql", "247_probe_auth_strikes.sql"} {
		migration, err := os.ReadFile(filepath.Join("..", "..", "migrations", name))
		require.NoError(t, err)
		_, err = db.Exec(string(migration))
		require.NoError(t, err)
	}
	origin, err := repo.client.Group.Create().SetName("original").SetPlatform(service.PlatformOpenAI).Save(t.Context())
	require.NoError(t, err)
	rescue, err := repo.client.Group.Create().SetName("rescue").SetPlatform(service.PlatformOpenAI).Save(t.Context())
	require.NoError(t, err)
	extra := map[string]any{"openai_rescue_lane": map[string]any{
		"entered_at": time.Now().UTC().Format(time.RFC3339Nano), "orig_group_ids": []int64{origin.ID},
	}, "custom": "before"}
	_, err = repo.client.Account.UpdateOneID(account.ID).SetPlatform(service.PlatformOpenAI).
		SetStatus(service.StatusActive).SetExtra(extra).SetSchedulable(true).
		SetCredentials(map[string]any{"email": "rescue@example.test"}).Save(t.Context())
	require.NoError(t, err)
	_, err = repo.client.AccountGroup.Create().SetAccountID(account.ID).SetGroupID(rescue.ID).SetPriority(1).Save(t.Context())
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO openai_downgrade_probe_states(account_id) VALUES($1)`, account.ID)
	require.NoError(t, err)
	account, err = repo.GetByID(t.Context(), account.ID)
	require.NoError(t, err)
	return repo, db, account, origin.ID
}

func verifiedRescueLane(t *testing.T, repo *accountRepository, db *sql.DB, id int64) *service.OpenAIRescueLane {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	_, err := db.Exec(`UPDATE openai_downgrade_probe_states SET state='on_duty',probe_mode='normal' WHERE account_id=$1`, id)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO openai_downgrade_probe_results
		(account_id,mode,transport_ok,answer_correct,reasoning_tokens,http_status,created_at)
		VALUES($1,'qualification',true,true,1400,200,$2)`, id, now)
	require.NoError(t, err)
	lane := service.NewOpenAIRescueLane(repo, nil, nil, nil)
	lane.SetProbeStateSource(func(ctx context.Context, ids []int64) (map[int64]service.OpenAIProbeHealthSnapshot, error) {
		snapshots, err := (&openAIDowngradeProbeRepository{db: db}).ListOpenAIProbeHealthSnapshots(ctx, ids)
		out := make(map[int64]service.OpenAIProbeHealthSnapshot)
		for _, snapshot := range snapshots {
			out[snapshot.AccountID] = snapshot
		}
		return out, err
	})
	lane.SetBridgeSource(func(context.Context) *service.PluginBridgeStatus {
		return &service.PluginBridgeStatus{Running: true, Healthy: true, StatusJSON: fmt.Sprintf(
			`{"prober":{"enabled":true,"accounts":[{"account_id":%d,"consecutive_passes":6,"last_verdict":"pass","last_probe_at":%q,"previous_pass_at":%q}]}}`,
			id, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))}
	})
	return lane
}

func rescueGraduationUpdates() map[string]any {
	return map[string]any{"openai_rescue_lane": nil, "openai_rescue_suspected": false,
		"openai_rescue_rescued_at": time.Now().UTC().Format(time.RFC3339), "openai_rescue_rescue_count": 1}
}

func TestOpenAIRescuePostgresGraduationIsAtomic(t *testing.T) {
	for _, failure := range []string{"none", "group_insert", "extra_update", "outbox"} {
		t.Run(failure, func(t *testing.T) {
			repo, db, account, origin := newRescueGraduationPostgres(t)
			if failure != "none" {
				table, operation := "accounts", "UPDATE"
				if failure == "group_insert" {
					table, operation = "account_groups", "INSERT"
				}
				if failure == "outbox" {
					table, operation = "scheduler_outbox", "INSERT"
				}
				_, err := db.Exec(`CREATE FUNCTION reject_graduation() RETURNS trigger LANGUAGE plpgsql AS
					$$ BEGIN RAISE EXCEPTION 'injected graduation failure'; END $$;
					CREATE TRIGGER reject_graduation_write BEFORE ` + operation + ` ON ` + table + `
					FOR EACH ROW EXECUTE FUNCTION reject_graduation();`)
				require.NoError(t, err)
			}
			graduated, err := repo.GraduateOpenAIRescue(t.Context(), account.ID, account.Extra["openai_rescue_lane"], []int64{origin}, rescueGraduationUpdates())
			if failure != "none" {
				require.ErrorContains(t, err, "injected graduation failure")
				require.False(t, graduated)
			} else {
				require.NoError(t, err)
				require.True(t, graduated)
			}
			current, err := repo.GetByID(t.Context(), account.ID)
			require.NoError(t, err)
			if failure != "none" {
				require.Equal(t, account.GroupIDs, current.GroupIDs)
				require.Equal(t, account.Extra, current.Extra)
				_, err := db.Exec(`DROP TRIGGER reject_graduation_write ON ` + map[string]string{"group_insert": "account_groups", "extra_update": "accounts", "outbox": "scheduler_outbox"}[failure])
				require.NoError(t, err)
				lane := verifiedRescueLane(t, repo, db, account.ID)
				entered, healed, withdrawn, err := lane.RunReconcileSweep(t.Context())
				require.NoError(t, err)
				require.Equal(t, 0, entered)
				require.Equal(t, 1, healed)
				require.Equal(t, 0, withdrawn)
				current, err = repo.GetByID(t.Context(), account.ID)
				require.NoError(t, err)
			} else {
				require.True(t, graduated)
			}
			require.Equal(t, []int64{origin}, current.GroupIDs)
			require.Nil(t, service.GetOpenAIRescueLaneMarker(current))
			require.Equal(t, float64(1), current.Extra["openai_rescue_rescue_count"])
		})
	}
}

func TestOpenAIRescuePostgresStaleMarkerUpdateCannotReopenRescue(t *testing.T) {
	for _, change := range []string{"none", "graduated", "new_marker"} {
		t.Run(change, func(t *testing.T) {
			repo, db, account, origin := newRescueGraduationPostgres(t)
			expected := account.Extra["openai_rescue_lane"]
			payload, err := json.Marshal(expected)
			require.NoError(t, err)
			var delayed map[string]any
			require.NoError(t, json.Unmarshal(payload, &delayed))
			delayed["seed_ok"] = true
			if change == "graduated" {
				committed, err := repo.GraduateOpenAIRescue(t.Context(), account.ID, expected, []int64{origin}, rescueGraduationUpdates())
				require.NoError(t, err)
				require.True(t, committed)
			} else if change == "new_marker" {
				_, err := db.Exec(`UPDATE accounts SET extra = jsonb_set(extra, '{openai_rescue_lane,entered_at}', '"2099-01-01T00:00:00Z"') WHERE id = $1`, account.ID)
				require.NoError(t, err)
			} else {
				account.UpdatedAt = time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
				_, err := db.Exec(`UPDATE accounts SET updated_at = $2 WHERE id = $1`, account.ID, account.UpdatedAt)
				require.NoError(t, err)
			}
			updated, err := repo.UpdateOpenAIRescueMarker(t.Context(), account.ID, expected, delayed)
			require.NoError(t, err)
			require.Equal(t, change == "none", updated)
			current, err := repo.GetByID(t.Context(), account.ID)
			require.NoError(t, err)
			switch change {
			case "none":
				require.Equal(t, true, current.Extra["openai_rescue_lane"].(map[string]any)["seed_ok"])
				require.True(t, current.UpdatedAt.After(account.UpdatedAt), "marker updates must not regress the account revision")
			case "graduated":
				require.Nil(t, service.GetOpenAIRescueLaneMarker(current))
				require.Equal(t, float64(1), current.Extra["openai_rescue_rescue_count"])
			case "new_marker":
				require.Equal(t, "2099-01-01T00:00:00Z", current.Extra["openai_rescue_lane"].(map[string]any)["entered_at"])
				require.NotContains(t, current.Extra["openai_rescue_lane"].(map[string]any), "seed_ok")
			}
		})
	}
}

func TestOpenAIRescuePostgresGraduationRestoresEmptyGroups(t *testing.T) {
	repo, db, account, _ := newRescueGraduationPostgres(t)
	_, err := db.Exec(`UPDATE accounts SET schedulable = false,
		extra = jsonb_set(extra, '{openai_rescue_lane,orig_group_ids}', '[]') WHERE id = $1`, account.ID)
	require.NoError(t, err)
	account, err = repo.GetByID(t.Context(), account.ID)
	require.NoError(t, err)
	lane := verifiedRescueLane(t, repo, db, account.ID)
	for range 2 {
		require.NoError(t, lane.GraduateRescue(t.Context(), account.ID, "test"))
	}
	current, err := repo.GetByID(t.Context(), account.ID)
	require.NoError(t, err)
	require.Empty(t, current.GroupIDs)
	require.True(t, current.Schedulable)
	require.Nil(t, service.GetOpenAIRescueLaneMarker(current))
	require.Equal(t, float64(1), current.Extra["openai_rescue_rescue_count"])
}

func TestOpenAIRescuePostgresGraduationRejectsStaleEvidence(t *testing.T) {
	for _, change := range []string{"new_marker", "qualification", "pending_replace", "disabled"} {
		t.Run(change, func(t *testing.T) {
			repo, db, account, origin := newRescueGraduationPostgres(t)
			switch change {
			case "new_marker":
				_, err := db.Exec(`UPDATE accounts SET extra = jsonb_set(extra, '{openai_rescue_lane,entered_at}', '"2099-01-01T00:00:00Z"') WHERE id = $1`, account.ID)
				require.NoError(t, err)
			case "disabled":
				_, err := db.Exec(`UPDATE accounts SET status = 'disabled' WHERE id = $1`, account.ID)
				require.NoError(t, err)
			default:
				state, mode := "on_duty", "qualification"
				if change == "pending_replace" {
					state, mode = "pending_replace", "normal"
				}
				_, err := db.Exec(`UPDATE openai_downgrade_probe_states SET state = $2, probe_mode = $3 WHERE account_id = $1`, account.ID, state, mode)
				require.NoError(t, err)
			}
			graduated, err := repo.GraduateOpenAIRescue(t.Context(), account.ID, account.Extra["openai_rescue_lane"], []int64{origin}, rescueGraduationUpdates())
			require.NoError(t, err)
			require.False(t, graduated)
			current, err := repo.GetByID(t.Context(), account.ID)
			require.NoError(t, err)
			require.Equal(t, account.GroupIDs, current.GroupIDs)
			require.NotNil(t, service.GetOpenAIRescueLaneMarker(current))
		})
	}
}

func TestOpenAIRescuePostgresConcurrentGraduationCommitsOnce(t *testing.T) {
	repo, _, account, origin := newRescueGraduationPostgres(t)
	type result struct {
		graduated bool
		err       error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			graduated, err := repo.GraduateOpenAIRescue(t.Context(), account.ID, account.Extra["openai_rescue_lane"], []int64{origin}, rescueGraduationUpdates())
			results <- result{graduated, err}
		}()
	}
	close(start)
	commits := 0
	for range 2 {
		select {
		case got := <-results:
			require.NoError(t, got.err)
			if got.graduated {
				commits++
			}
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent graduation did not finish")
		}
	}
	require.Equal(t, 1, commits)
	current, err := repo.GetByID(t.Context(), account.ID)
	require.NoError(t, err)
	require.Equal(t, []int64{origin}, current.GroupIDs)
	require.Equal(t, float64(1), current.Extra["openai_rescue_rescue_count"])
}

func TestOpenAIRescuePostgresStaleEditPreservesCurrentState(t *testing.T) {
	for _, graduated := range []bool{false, true} {
		t.Run(map[bool]string{false: "new_state_after_load", true: "cleared_state_after_load"}[graduated], func(t *testing.T) {
			repo, db, account, _ := newRescueGraduationPostgres(t)
			if graduated {
				_, err := db.Exec(`UPDATE accounts SET extra = extra - 'openai_rescue_lane' WHERE id = $1`, account.ID)
				require.NoError(t, err)
			} else {
				account.Extra = map[string]any{"custom": "edited"}
			}
			tx, err := repo.client.Tx(t.Context())
			require.NoError(t, err)
			defer func() { _ = tx.Rollback() }()
			extra, err := lockAndMergeAccountProbeExtra(t.Context(), tx.Client(), account, nil, nil)
			require.NoError(t, err)
			_, err = tx.Client().Account.UpdateOneID(account.ID).SetExtra(extra).Save(t.Context())
			require.NoError(t, err)
			require.NoError(t, tx.Commit())
			var raw []byte
			require.NoError(t, db.QueryRow(`SELECT extra FROM accounts WHERE id = $1`, account.ID).Scan(&raw))
			var got map[string]any
			require.NoError(t, json.Unmarshal(raw, &got))
			if graduated {
				require.NotContains(t, got, "openai_rescue_lane")
			} else {
				require.Contains(t, got, "openai_rescue_lane")
			}
		})
	}
}

func TestOpenAIRescuePostgresEditWaitsForCurrentRescueState(t *testing.T) {
	for _, clearMarker := range []bool{false, true} {
		t.Run(map[bool]string{false: "rescue_update", true: "graduation"}[clearMarker], func(t *testing.T) {
			repo, db, account, _ := newRescueGraduationPostgres(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			writer, err := db.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer func() { _ = writer.Rollback() }()
			var writerPID int
			require.NoError(t, writer.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&writerPID))
			if clearMarker {
				_, err = writer.ExecContext(ctx, `UPDATE accounts SET extra = extra - 'openai_rescue_lane' WHERE id = $1`, account.ID)
			} else {
				_, err = writer.ExecContext(ctx, `UPDATE accounts SET extra = jsonb_set(extra, '{openai_rescue_lane,seed_ok}', 'true') WHERE id = $1`, account.ID)
				account.Extra = map[string]any{"custom": "edited"}
			}
			require.NoError(t, err)
			finished := make(chan error, 1)
			go func() {
				tx, err := repo.client.Tx(ctx)
				if err != nil {
					finished <- err
					return
				}
				defer func() { _ = tx.Rollback() }()
				extra, err := lockAndMergeAccountProbeExtra(ctx, tx.Client(), account, nil, nil)
				if err == nil {
					_, err = tx.Client().Account.UpdateOneID(account.ID).SetExtra(extra).Save(ctx)
				}
				if err == nil {
					err = tx.Commit()
				}
				finished <- err
			}()
			require.Eventually(t, func() bool {
				var waiting bool
				err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid)))`, writerPID).Scan(&waiting)
				return err == nil && waiting
			}, 3*time.Second, 10*time.Millisecond, "account edit must wait on the rescue writer's row lock")
			require.NoError(t, writer.Commit())
			select {
			case err := <-finished:
				require.NoError(t, err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			current, err := repo.GetByID(ctx, account.ID)
			require.NoError(t, err)
			if clearMarker {
				require.NotContains(t, current.Extra, "openai_rescue_lane")
			} else {
				require.Equal(t, true, current.Extra["openai_rescue_lane"].(map[string]any)["seed_ok"])
				require.Equal(t, "edited", current.Extra["custom"])
			}
		})
	}
}
