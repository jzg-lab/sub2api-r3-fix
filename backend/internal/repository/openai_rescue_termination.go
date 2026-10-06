package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbaccountgroup "github.com/Wei-Shaw/sub2api/ent/accountgroup"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

var _ service.OpenAIRescueLifecycleRepository = (*accountRepository)(nil)
var _ service.OpenAIRescueTerminatedAccountLister = (*accountRepository)(nil)

func (r *accountRepository) ListOpenAIRescueTerminatedAccountIDs(ctx context.Context) ([]int64, error) {
	rows, err := r.sql.QueryContext(ctx, `SELECT id FROM accounts
		WHERE platform = 'openai' AND type = 'oauth' AND parent_account_id IS NULL
		AND extra->'openai_rescue_terminated_at' IS NOT NULL
		AND extra->'openai_rescue_terminated_at' <> 'null'::jsonb ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (r *accountRepository) EnterOpenAIRescue(ctx context.Context, account *service.Account, marker map[string]any, groupID int64, manual bool) (bool, error) {
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	txCtx := dbent.NewTxContext(ctx, tx)
	client := tx.Client()
	var raw []byte
	var revision time.Time
	var priority int
	err = scanSingleRow(txCtx, client, `SELECT extra, updated_at, priority FROM accounts
		WHERE id = $1 AND deleted_at IS NULL AND platform = 'openai' AND type = 'oauth'
		AND parent_account_id IS NULL
		AND (auto_pause_on_expired IS NOT TRUE OR expires_at IS NULL OR expires_at > NOW())
		FOR UPDATE`, []any{account.ID}, &raw, &revision, &priority)
	if errors.Is(err, sql.ErrNoRows) {
		return false, service.ErrRescueLaneIneligible
	}
	if err != nil {
		return false, err
	}
	var extra map[string]any
	if err := json.Unmarshal(raw, &extra); err != nil {
		return false, err
	}
	if extra["openai_rescue_lane"] != nil {
		return false, nil
	}
	stopped := extra[service.OpenAIRescueTerminatedAtExtraKey] != nil
	if stopped && !manual {
		return false, service.ErrOpenAIRescueTerminated
	}
	if !revision.Equal(account.UpdatedAt) {
		return false, service.ErrOpenAIProbeStale
	}
	if !manual {
		protected, err := openAIQualityProtectionApplies(txCtx, client, account.ID)
		if err != nil || !protected {
			return false, err
		}
	}
	groups, err := client.AccountGroup.Query().Where(dbaccountgroup.AccountIDEQ(account.ID)).All(txCtx)
	if err != nil {
		return false, err
	}
	originalIDs := make([]int64, 0, len(groups))
	for _, group := range groups {
		originalIDs = append(originalIDs, group.GroupID)
	}
	marker["orig_group_ids"], marker["orig_priority"] = originalIDs, priority
	if err := replaceAccountGroupsInTx(txCtx, client, account.ID, []int64{groupID}); err != nil {
		return false, err
	}
	if _, err := client.ExecContext(txCtx, `UPDATE accounts SET schedulable = false,
		updated_at = GREATEST(clock_timestamp(), updated_at + INTERVAL '1 microsecond') WHERE id = $1`, account.ID); err != nil {
		return false, err
	}
	if err := updateOpenAIRescueExtraInTx(txCtx, client, account.ID, map[string]any{
		"openai_rescue_lane": marker, "openai_rescue_auth_rejected_at": nil,
		service.OpenAIRescueTerminatedAtExtraKey: nil,
	}); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	r.syncSchedulerAccountSnapshotDetached(ctx, account.ID)
	return true, nil
}

func (r *accountRepository) TerminateOpenAIRescue(ctx context.Context, accountID int64, stoppedAt time.Time) (bool, error) {
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	txCtx := dbent.NewTxContext(ctx, tx)
	client := tx.Client()
	var raw []byte
	err = scanSingleRow(txCtx, client, `SELECT extra FROM accounts
		WHERE id = $1 AND deleted_at IS NULL AND platform = 'openai' AND type = 'oauth'
		AND parent_account_id IS NULL FOR UPDATE`, []any{accountID}, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, service.ErrAccountNotFound
	}
	if err != nil {
		return false, err
	}
	var extra map[string]any
	if err := json.Unmarshal(raw, &extra); err != nil {
		return false, err
	}
	if extra["openai_rescue_lane"] == nil {
		return false, nil
	}
	if err := terminateOpenAIRescueInTx(txCtx, client, accountID, extra, stoppedAt, "manual"); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	r.syncSchedulerAccountSnapshotDetached(ctx, accountID)
	return true, nil
}

func terminateOpenAIRescueInTx(txCtx context.Context, client *dbent.Client, accountID int64, extra map[string]any, stoppedAt time.Time, reason string) error {
	groupIDs, err := service.OpenAIRescueOriginalGroups(extra["openai_rescue_lane"])
	if err != nil {
		return err
	}
	if err := replaceAccountGroupsInTx(txCtx, client, accountID, groupIDs); err != nil {
		return err
	}
	if _, err := client.ExecContext(txCtx, `UPDATE accounts SET schedulable = false,
		updated_at = GREATEST(clock_timestamp(), updated_at + INTERVAL '1 microsecond') WHERE id = $1`, accountID); err != nil {
		return err
	}
	if _, err := client.ExecContext(txCtx, `INSERT INTO openai_downgrade_probe_states(account_id,state,probe_mode)
		VALUES($1,'pending_replace','normal') ON CONFLICT(account_id) DO UPDATE
		SET state = 'pending_replace', probe_mode = 'normal', consecutive_successes = 0,
			consecutive_failures = 0,
			updated_at = GREATEST(clock_timestamp(), openai_downgrade_probe_states.updated_at + INTERVAL '1 microsecond')`, accountID); err != nil {
		return err
	}
	if err := updateOpenAIRescueExtraInTx(txCtx, client, accountID, map[string]any{
		"openai_rescue_lane": nil, "openai_rescue_suspected": false,
		service.OpenAIRescueTerminatedAtExtraKey: stoppedAt.Format(time.RFC3339Nano),
	}); err != nil {
		return err
	}
	groupJSON, err := json.Marshal(groupIDs)
	if err != nil {
		return err
	}
	if _, err := client.ExecContext(txCtx, `INSERT INTO openai_downgrade_probe_events(account_id, proxy_id, event_type, details)
		SELECT id, proxy_id, 'rescue_terminated', jsonb_build_object('reason', $3::text, 'orig_group_ids', $2::jsonb)
		FROM accounts WHERE id = $1`, accountID, string(groupJSON), reason); err != nil {
		return err
	}
	extra["openai_rescue_lane"] = nil
	extra["openai_rescue_suspected"] = false
	extra[service.OpenAIRescueTerminatedAtExtraKey] = stoppedAt.Format(time.RFC3339Nano)
	return nil
}

func (r *accountRepository) MutateOpenAIRescue(ctx context.Context, accountID int64, expected any, groupIDs *[]int64, updates map[string]any, disable bool) (bool, error) {
	payload, err := json.Marshal(expected)
	if err != nil {
		return false, err
	}
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	txCtx := dbent.NewTxContext(ctx, tx)
	client := tx.Client()
	var matches bool
	err = scanSingleRow(txCtx, client, `SELECT COALESCE(extra -> 'openai_rescue_lane' = $2::jsonb, false)
		FROM accounts WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`, []any{accountID, string(payload)}, &matches)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !matches) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if groupIDs != nil {
		if err := replaceAccountGroupsInTx(txCtx, client, accountID, *groupIDs); err != nil {
			return false, err
		}
	}
	if disable {
		if _, err := client.ExecContext(txCtx, `UPDATE accounts SET schedulable = false,
			updated_at = GREATEST(clock_timestamp(), updated_at + INTERVAL '1 microsecond') WHERE id = $1`, accountID); err != nil {
			return false, err
		}
	}
	if err := updateOpenAIRescueExtraInTx(txCtx, client, accountID, updates); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	r.syncSchedulerAccountSnapshotDetached(ctx, accountID)
	return true, nil
}

func updateOpenAIRescueExtraInTx(ctx context.Context, client *dbent.Client, accountID int64, updates map[string]any) error {
	if updates == nil {
		updates = map[string]any{}
	}
	payload, err := json.Marshal(updates)
	if err != nil {
		return err
	}
	if _, err := client.ExecContext(ctx, `UPDATE accounts SET extra=COALESCE(extra,'{}'::jsonb) || $2::jsonb,
		updated_at=GREATEST(clock_timestamp(), updated_at + INTERVAL '1 microsecond')
		WHERE id=$1 AND deleted_at IS NULL`, accountID, string(payload)); err != nil {
		return err
	}
	return enqueueSchedulerOutbox(ctx, client, service.SchedulerOutboxEventAccountChanged, &accountID, nil, nil)
}
