package repository

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (r *accountRepository) ApplyOpenAIAuthStateIfUnchanged(
	ctx context.Context, before *service.Account, change service.OpenAIAuthStateUpdate,
) (bool, error) {
	if !service.IsOpenAIBrowserOAuthAccount(before) {
		return false, nil
	}
	changes := 0
	if change.ErrorMessage != nil {
		changes++
	}
	if change.CooldownUntil != nil {
		changes++
	}
	if changes != 1 {
		return false, errors.New("exactly one OpenAI authentication state change is required")
	}
	if r == nil || r.sql == nil {
		return false, errors.New("OpenAI authentication state repository unavailable")
	}
	credentials, err := json.Marshal(normalizeJSONMap(before.Credentials))
	if err != nil {
		return false, err
	}
	// Compare the complete credential document, not updated_at: normal usage
	// telemetry must not invalidate a result, but reauthorization must.
	result, err := r.sql.ExecContext(ctx, `WITH updated AS (
		UPDATE accounts AS a SET
			status = CASE WHEN $1::text IS NULL THEN a.status ELSE 'error' END,
			error_message = COALESCE($1::text, a.error_message),
			temp_unschedulable_until = CASE WHEN $2::timestamptz IS NULL
				THEN a.temp_unschedulable_until ELSE GREATEST(a.temp_unschedulable_until, $2) END,
			temp_unschedulable_reason = CASE WHEN $2::timestamptz IS NULL
				OR a.temp_unschedulable_until > $2 THEN a.temp_unschedulable_reason ELSE $3 END,
			updated_at = NOW()
		WHERE a.id = $4 AND a.deleted_at IS NULL AND a.parent_account_id IS NULL
			AND a.platform = 'openai' AND a.type = 'oauth'
			AND a.credentials = $5::jsonb
			AND a.proxy_id IS NOT DISTINCT FROM $6
			AND a.status = $7 AND a.schedulable = $8
		RETURNING a.id
	) INSERT INTO scheduler_outbox (event_type, account_id, group_id, payload)
	SELECT $9, id, NULL, NULL FROM updated`,
		change.ErrorMessage, change.CooldownUntil, change.CooldownReason,
		before.ID, string(credentials), before.ProxyID, before.Status, before.Schedulable,
		service.SchedulerOutboxEventAccountChanged,
	)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed == 0 {
		return false, err
	}
	r.syncSchedulerAccountSnapshotDetached(ctx, before.ID)
	return true, nil
}
