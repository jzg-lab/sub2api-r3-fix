package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (r *accountRepository) UpdateOpenAIRescueMarker(ctx context.Context, accountID int64, expectedMarker, marker any) (bool, error) {
	expectedJSON, err := json.Marshal(expectedMarker)
	if err != nil {
		return false, err
	}
	markerJSON, err := json.Marshal(marker)
	if err != nil {
		return false, err
	}
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	client := tx.Client()
	result, err := client.ExecContext(ctx, `UPDATE accounts
		SET extra = jsonb_set(extra, '{openai_rescue_lane}', $3::jsonb),
			updated_at = GREATEST(clock_timestamp(), updated_at + INTERVAL '1 microsecond')
		WHERE id = $1 AND deleted_at IS NULL AND extra -> 'openai_rescue_lane' = $2::jsonb`,
		accountID, string(expectedJSON), string(markerJSON))
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed == 0 {
		return false, err
	}
	if err := enqueueSchedulerOutbox(ctx, client, service.SchedulerOutboxEventAccountChanged, &accountID, nil, nil); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	r.syncSchedulerAccountSnapshotDetached(ctx, accountID)
	return true, nil
}

func (r *accountRepository) GraduateOpenAIRescue(ctx context.Context, accountID int64, expectedMarker any, groupIDs []int64, updates map[string]any) (bool, error) {
	markerJSON, err := json.Marshal(expectedMarker)
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

	// Match the probe commit's lock order: account, then probe state.
	var matches bool
	err = scanSingleRow(txCtx, client, `
		SELECT COALESCE(extra -> 'openai_rescue_lane' = $2::jsonb, false)
		FROM accounts WHERE id = $1 AND deleted_at IS NULL
			AND platform = 'openai' AND type = 'oauth' AND parent_account_id IS NULL
			AND status = 'active'
		FOR UPDATE
	`, []any{accountID, string(markerJSON)}, &matches)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !matches) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var state, mode string
	err = scanSingleRow(txCtx, client, `
		SELECT state, probe_mode FROM openai_downgrade_probe_states
		WHERE account_id = $1 FOR UPDATE
	`, []any{accountID}, &state, &mode)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (state != service.OpenAIDowngradeStateOnDuty || mode != "normal")) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	groupIDs, err = service.OpenAIRescueOriginalGroups(expectedMarker)
	if err != nil {
		return false, err
	}
	if err := replaceAccountGroupsInTx(txCtx, client, accountID, groupIDs); err != nil {
		return false, err
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
