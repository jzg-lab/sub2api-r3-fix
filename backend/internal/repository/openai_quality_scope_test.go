package repository

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOpenAIQualityScopePostgresManualScheduling(t *testing.T) {
	for _, mode := range []string{"legacy", "selected", "unselected", "mixed", "empty", "corrupt", "invalid_proxy"} {
		for _, bulk := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/bulk=%v", mode, bulk), func(t *testing.T) {
				repo, db, account, origin := newRescueTerminationPostgres(t)
				selected := []int64{origin}
				if mode == "unselected" || mode == "corrupt" || mode == "invalid_proxy" {
					selected = []int64{account.GroupIDs[0]}
				} else if mode == "empty" {
					selected = []int64{}
				}
				if mode != "legacy" {
					payload, err := json.Marshal(map[string]any{"quality_protected_group_ids": selected})
					require.NoError(t, err)
					_, err = db.Exec(`INSERT INTO settings(key,value) VALUES('openai_operations',$1)`, string(payload))
					require.NoError(t, err)
				}
				original := []int64{origin}
				if mode == "mixed" {
					group, err := repo.client.Group.Create().SetName("ordinary").SetPlatform(service.PlatformOpenAI).Save(t.Context())
					require.NoError(t, err)
					original = append(original, group.ID)
					payload, err := json.Marshal(original)
					require.NoError(t, err)
					_, err = db.Exec(`UPDATE accounts SET extra=jsonb_set(extra,'{openai_rescue_lane,orig_group_ids}',$2::jsonb) WHERE id=$1`, account.ID, string(payload))
					require.NoError(t, err)
				}
				if mode == "corrupt" {
					_, err := db.Exec(`UPDATE accounts SET extra=extra #- '{openai_rescue_lane,orig_group_ids}' WHERE id=$1`, account.ID)
					require.NoError(t, err)
				}
				if mode == "invalid_proxy" {
					proxy, err := repo.client.Proxy.Create().SetName("invalid").SetProtocol("http").SetHost("127.0.0.1").SetPort(1).SetStatus("inactive").Save(t.Context())
					require.NoError(t, err)
					_, err = db.Exec(`UPDATE accounts SET proxy_id=$2 WHERE id=$1`, account.ID, proxy.ID)
					require.NoError(t, err)
				}
				_, err := db.Exec(`UPDATE accounts SET schedulable=false WHERE id=$1`, account.ID)
				require.NoError(t, err)
				before, err := repo.GetByID(t.Context(), account.ID)
				require.NoError(t, err)
				enabled := true
				if bulk {
					_, err = repo.BulkUpdate(t.Context(), []int64{account.ID}, service.AccountBulkUpdate{Schedulable: &enabled, ManualScheduling: true})
				} else {
					err = repo.SetSchedulable(t.Context(), account.ID, true)
				}
				allowed := mode == "unselected" || mode == "mixed" || mode == "empty"
				if allowed {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
				current, err := repo.GetByID(t.Context(), account.ID)
				require.NoError(t, err)
				require.Equal(t, allowed, current.Schedulable)
				require.Equal(t, before.Priority, current.Priority)
				if !allowed {
					require.Equal(t, before.Extra, current.Extra)
					require.Equal(t, before.GroupIDs, current.GroupIDs)
					return
				}
				require.ElementsMatch(t, original, current.GroupIDs)
				require.Nil(t, current.Extra["openai_rescue_lane"])
				require.True(t, service.OpenAIRescueManuallyTerminated(current))
				require.Nil(t, current.Extra["openai_rescue_rescued_at"])
				lane := service.NewOpenAIRescueLane(repo, nil, func() service.OpenAIRescueLaneConfig {
					return service.OpenAIRescueLaneConfig{Enabled: true, GroupID: account.GroupIDs[0]}
				}, nil)
				_, _, _, err = lane.RunReconcileSweep(t.Context())
				require.NoError(t, err)
				current, err = repo.GetByID(t.Context(), account.ID)
				require.NoError(t, err)
				require.True(t, current.Schedulable, "sweep must not undo manual scheduling")
			})
		}
	}
}

func TestOpenAIQualityScopePostgresProbeAndAutoEntry(t *testing.T) {
	for _, selected := range []bool{false, true} {
		for _, authFailures := range []int{0, 1, 2} {
			t.Run(fmt.Sprintf("selected=%v/auth=%v", selected, authFailures), func(t *testing.T) {
				repo, db, account, origin := newRescueTerminationPostgres(t)
				stopped, err := repo.TerminateOpenAIRescue(t.Context(), account.ID, time.Now())
				require.NoError(t, err)
				require.True(t, stopped)
				_, err = db.Exec(`UPDATE accounts SET schedulable=true, extra=extra - 'openai_rescue_terminated_at' WHERE id=$1`, account.ID)
				require.NoError(t, err)
				ids := []int64{}
				if selected {
					ids = []int64{origin}
				}
				payload, err := json.Marshal(map[string]any{"quality_protected_group_ids": ids})
				require.NoError(t, err)
				_, err = db.Exec(`INSERT INTO settings(key,value) VALUES('openai_operations',$1)`, string(payload))
				require.NoError(t, err)
				current, err := repo.GetByID(t.Context(), account.ID)
				require.NoError(t, err)
				store := &openAIDowngradeProbeRepository{db: db}
				state, err := store.GetOpenAIDowngradeState(t.Context(), account.ID)
				require.NoError(t, err)
				paused := false
				mutation := &service.OpenAIDowngradeMutation{
					AccountID: account.ID, ExpectedAccountUpdatedAt: current.UpdatedAt,
					ExpectedStateUpdatedAt: state.UpdatedAt, ExpectedProxyID: current.ProxyID,
					ExpectedStatus: current.Status, ExpectedSchedulable: current.Schedulable,
					State: state, Schedulable: &paused,
					Results: []service.OpenAIDowngradeProbeResult{{AccountID: account.ID, Mode: "normal", TransportOK: true}},
				}
				if authFailures == 1 {
					mutation.State.AuthConsecutiveFailures = 1
				}
				if authFailures == 2 {
					message := "OpenAI probe authentication failed"
					mutation.ErrorMessage = &message
				}
				require.NoError(t, store.CommitOpenAIDowngradeMutation(t.Context(), mutation))
				current, err = repo.GetByID(t.Context(), account.ID)
				require.NoError(t, err)
				require.Equal(t, !selected && authFailures == 0, current.Schedulable)
				var results int
				require.NoError(t, db.QueryRow(`SELECT count(*) FROM openai_downgrade_probe_results WHERE account_id=$1`, account.ID).Scan(&results))
				require.Equal(t, 1, results, "quality evidence must remain")
				if authFailures == 0 {
					entered, err := repo.EnterOpenAIRescue(t.Context(), current, map[string]any{"entered_at": time.Now().UTC().Format(time.RFC3339Nano)}, account.GroupIDs[0], false)
					require.NoError(t, err)
					require.Equal(t, selected, entered)
				}
			})
		}
	}
}
