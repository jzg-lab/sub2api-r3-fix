package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (r *openAIDowngradeProbeRepository) CommitOpenAIDowngradeMutation(ctx context.Context, mutation *service.OpenAIDowngradeMutation) error {
	if mutation == nil || mutation.State == nil || mutation.AccountID <= 0 ||
		mutation.State.AccountID != mutation.AccountID {
		return errors.New("invalid OpenAI probe mutation")
	}
	for _, result := range mutation.Results {
		if result.AccountID != mutation.AccountID {
			return errors.New("OpenAI probe result account mismatch")
		}
	}
	if mutation.RateLimitClear != nil {
		accepted := false
		for _, result := range mutation.Results {
			if result.TransportOK && result.HTTPStatus >= 200 && result.HTTPStatus < 300 {
				accepted = true
			}
		}
		if !accepted || mutation.RateLimitResetAt != nil {
			return errors.New("invalid OpenAI probe rate-limit recovery")
		}
	}
	// Recovery must not publish on_duty for an error that this mechanism does
	// not own. Owned-error recovery is a separate, explicitly versioned action.
	if mutation.Schedulable != nil && *mutation.Schedulable &&
		((mutation.ExpectedStatus != service.StatusActive && !mutation.RecoverOwnedError) || mutation.ErrorMessage != nil) {
		return errors.New("OpenAI probe cannot enable an account with an unowned error")
	}
	if mutation.RecoverOwnedError && (mutation.ExpectedStatus != service.StatusError ||
		mutation.Schedulable == nil || !*mutation.Schedulable || mutation.ErrorMessage != nil) {
		return errors.New("invalid OpenAI probe owned-error recovery")
	}
	// 资格完成最终落账的合格代理:老号(代理未变)= 快照代理 ExpectedProxyID;
	// 新号工作流(r15b/r17b)同轮先自动分桶再打资格针,目标在 mutation.ProxyID
	// (此时 ExpectedProxyID 为 nil,解引用即 panic——9/20 生产崩溃循环根因)。
	// 行锁与合格戳都必须落在这个目标代理上,与 UPDATE 后的 proxy_id 一致。
	var qualificationProxyID int64
	if mutation.CompleteQualification {
		var ok bool
		qualificationProxyID, ok = openAIQualificationProxyTarget(mutation)
		accepted := false
		if ok {
			for _, result := range mutation.Results {
				if result.IsQualificationPass() &&
					((result.ProxyID == nil && qualificationProxyID == 0) ||
						(result.ProxyID != nil && *result.ProxyID == qualificationProxyID)) {
					accepted = true
					break
				}
			}
		}
		if !accepted || mutation.Schedulable == nil || !*mutation.Schedulable {
			return errors.New("invalid OpenAI OAuth qualification completion")
		}
	}
	beginner, ok := r.db.(interface {
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	})
	if !ok {
		return service.ErrOpenAIProbeAtomicStore
	}
	tx, err := beginner.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if mutation.CompleteQualification && qualificationProxyID > 0 {
		if err := lockValidOpenAIOAuthProxy(ctx, tx, qualificationProxyID); err != nil {
			return err
		}
	}

	var version time.Time
	err = scanSingleRow(ctx, tx, `
		SELECT updated_at FROM accounts
		WHERE id = $1 AND deleted_at IS NULL
			AND platform = 'openai' AND type = 'oauth'
			AND parent_account_id IS NULL
			AND updated_at = $2
			AND proxy_id IS NOT DISTINCT FROM $3::bigint
			AND status = $4 AND schedulable = $5
			AND NOT EXISTS (
				SELECT 1 FROM openai_downgrade_probe_controls c
				WHERE c.account_id = accounts.id AND c.manual_paused
			)
			AND (status <> 'error' OR EXISTS (
				SELECT 1 FROM openai_downgrade_probe_controls c
				WHERE c.account_id = accounts.id AND c.owned_error = accounts.error_message
			))
			AND (auto_pause_on_expired IS NOT TRUE OR expires_at IS NULL OR expires_at > NOW())
		FOR UPDATE
	`, []any{mutation.AccountID, mutation.ExpectedAccountUpdatedAt,
		mutation.ExpectedProxyID, mutation.ExpectedStatus, mutation.ExpectedSchedulable}, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return service.ErrOpenAIProbeStale
	}
	if err != nil {
		return err
	}
	err = scanSingleRow(ctx, tx, `
		SELECT updated_at FROM openai_downgrade_probe_states
		WHERE account_id = $1 AND updated_at = $2 FOR UPDATE
	`, []any{mutation.AccountID, mutation.ExpectedStateUpdatedAt}, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return service.ErrOpenAIProbeStale
	}
	if err != nil {
		return err
	}

	changed := mutation.ChangesAccount()
	if changed {
		extra := map[string]any{}
		if mutation.FallbackMode != nil {
			extra[service.OpenAIDowngradeSolFallbackExtraKey] = *mutation.FallbackMode
		}
		payload, err := json.Marshal(extra)
		if err != nil {
			return err
		}
		extraExpression := "COALESCE(extra, '{}'::jsonb) || $5::jsonb"
		if mutation.CompleteQualification {
			extraExpression = "(" + extraExpression + ") - '" + service.OpenAIDowngradeQualificationExtraKey + "' - '" + service.OpenAIOAuthQualifiedProxyExtraKey + "'"
		}
		// Merely naming status/error_message in UPDATE revokes error ownership,
		// even if their values are unchanged. Leave them out for ordinary probes.
		errorUpdate := ""
		var limitedAt, resetAt *time.Time
		if observation := mutation.RateLimitClear; observation != nil {
			limitedAt, resetAt = &observation.LimitedAt, &observation.ResetAt
		}
		args := []any{mutation.AccountID, mutation.ProxyChanged, mutation.ProxyID,
			mutation.Schedulable, string(payload), mutation.RateLimitResetAt, limitedAt, resetAt}
		if mutation.ErrorMessage != nil {
			errorUpdate = "status = 'error', error_message = $9::text,"
			args = append(args, *mutation.ErrorMessage)
		} else if mutation.RecoverOwnedError {
			errorUpdate = "status = 'active', error_message = '',"
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE accounts SET
				proxy_id = CASE WHEN $2 THEN $3::bigint ELSE proxy_id END,
				schedulable = COALESCE($4::boolean, schedulable),
				extra = `+extraExpression+`,
				rate_limited_at = CASE
					WHEN $7::timestamptz IS NOT NULL THEN NULL
					WHEN $6::timestamptz IS NOT NULL AND
						(rate_limit_reset_at IS NULL OR rate_limit_reset_at < $6) THEN NOW()
					ELSE rate_limited_at END,
				rate_limit_reset_at = CASE
					WHEN $7::timestamptz IS NOT NULL THEN NULL
					WHEN $6::timestamptz IS NOT NULL AND
						(rate_limit_reset_at IS NULL OR rate_limit_reset_at < $6) THEN $6
					ELSE rate_limit_reset_at END,
				`+errorUpdate+`
				updated_at = GREATEST(clock_timestamp(), updated_at + INTERVAL '1 microsecond')
			WHERE id = $1 AND deleted_at IS NULL
				AND ($7::timestamptz IS NULL OR
					(rate_limited_at = $7 AND rate_limit_reset_at = $8::timestamptz))
		`, args...)
		if err := requireOpenAIProbeUpdatedRow(result, err); err != nil {
			return err
		}
	}
	if mutation.ErrorMessage != nil {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO openai_downgrade_probe_controls(account_id, owned_error)
			VALUES ($1, $2)
			ON CONFLICT (account_id) DO UPDATE
			SET owned_error = EXCLUDED.owned_error, updated_at = clock_timestamp()
		`, mutation.AccountID, *mutation.ErrorMessage); err != nil {
			return err
		}
	}

	transactionRepo := &openAIDowngradeProbeRepository{db: tx}
	for i := range mutation.Results {
		if err := transactionRepo.RecordOpenAIDowngradeProbe(ctx, &mutation.Results[i]); err != nil {
			return err
		}
	}
	for _, event := range mutation.Events {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO openai_downgrade_probe_events(account_id, proxy_id, event_type, details, created_at)
			VALUES ($1, $2, $3, $4::jsonb, COALESCE($5::timestamptz, NOW()))
		`, mutation.AccountID, event.ProxyID, event.Type, string(event.Details), event.ObservedAt); err != nil {
			return err
		}
	}
	state := *mutation.State
	state.UpdatedAt = time.Now().UTC().Truncate(time.Microsecond)
	if !state.UpdatedAt.After(mutation.ExpectedStateUpdatedAt) {
		state.UpdatedAt = mutation.ExpectedStateUpdatedAt.Add(time.Microsecond)
	}
	if err := transactionRepo.SaveOpenAIDowngradeState(ctx, &state); err != nil {
		return err
	}
	if changed {
		if err := enqueueSchedulerOutbox(ctx, tx, service.SchedulerOutboxEventAccountChanged,
			&mutation.AccountID, nil, nil); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	*mutation.State = state
	return nil
}

// Zero identifies a direct route when retiring a legacy qualification marker.
func openAIQualificationProxyTarget(mutation *service.OpenAIDowngradeMutation) (int64, bool) {
	if !mutation.ProxyChanged {
		if mutation.ExpectedProxyID == nil {
			return 0, true
		}
		if mutation.ExpectedProxyID != nil && *mutation.ExpectedProxyID > 0 {
			return *mutation.ExpectedProxyID, true
		}
		return 0, false
	}
	if mutation.ProxyID == nil {
		return 0, true
	}
	if mutation.ProxyID != nil && *mutation.ProxyID > 0 {
		return *mutation.ProxyID, true
	}
	return 0, false
}

func requireOpenAIProbeUpdatedRow(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("%w: affected rows %d", service.ErrOpenAIProbeStale, count)
	}
	return nil
}
