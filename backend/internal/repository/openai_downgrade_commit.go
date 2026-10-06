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
			AND COALESCE(extra->'openai_rescue_terminated_at', 'null'::jsonb) = 'null'::jsonb
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

	if mutation.Schedulable != nil && !*mutation.Schedulable &&
		mutation.ErrorMessage == nil && mutation.State.AuthConsecutiveFailures == 0 {
		protected, err := openAIQualityProtectionApplies(ctx, tx, mutation.AccountID)
		if err != nil {
			return err
		}
		if !protected {
			// Keep evidence and normal recovery, but do not stop scheduling for
			// quality alone. Authentication strikes retain their existing pause.
			mutation.Schedulable = nil
		}
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

func (r *openAIDowngradeProbeRepository) CommitOpenAIAccountReenable(
	ctx context.Context,
	mutation *service.OpenAIAccountReenableMutation,
) (bool, error) {
	if mutation == nil || mutation.State == nil || mutation.AccountID <= 0 ||
		mutation.State.AccountID != mutation.AccountID ||
		mutation.State.State != service.OpenAIDowngradeStateOnDuty ||
		mutation.State.ProbeMode != "qualification" ||
		mutation.ReenabledAt.IsZero() {
		return false, errors.New("invalid OpenAI account reenable mutation")
	}
	beginner, ok := r.db.(interface {
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	})
	if !ok {
		return false, service.ErrOpenAIProbeAtomicStore
	}
	tx, err := beginner.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	var (
		accountUpdatedAt time.Time
		proxyID          sql.NullInt64
		status           string
		schedulable      bool
		platform         string
		accountType      string
		parentAccountID  sql.NullInt64
		notExpired       bool
		errorMessage     sql.NullString
	)
	err = scanSingleRow(ctx, tx, `
		SELECT updated_at, proxy_id, status, schedulable, platform, type,
			parent_account_id,
			(auto_pause_on_expired IS NOT TRUE OR expires_at IS NULL OR expires_at > NOW()),
			error_message
		FROM accounts
		WHERE id = $1 AND deleted_at IS NULL
			AND COALESCE(extra->'openai_rescue_terminated_at', 'null'::jsonb) = 'null'::jsonb
		FOR UPDATE
	`, []any{mutation.AccountID}, &accountUpdatedAt, &proxyID, &status, &schedulable,
		&platform, &accountType, &parentAccountID, &notExpired, &errorMessage)
	if errors.Is(err, sql.ErrNoRows) {
		return false, service.ErrOpenAIProbeStale
	}
	if err != nil {
		return false, err
	}
	if !accountUpdatedAt.Equal(mutation.ExpectedAccountUpdatedAt) ||
		!nullableInt64MatchesPointer(proxyID, mutation.ExpectedProxyID) ||
		status != mutation.ExpectedStatus ||
		schedulable != mutation.ExpectedSchedulable {
		return false, service.ErrOpenAIProbeStale
	}

	var (
		manualPaused bool
		ownedError   sql.NullString
	)
	err = scanSingleRow(ctx, tx, `
		SELECT manual_paused, owned_error
		FROM openai_downgrade_probe_controls
		WHERE account_id = $1
		FOR UPDATE
	`, []any{mutation.AccountID}, &manualPaused, &ownedError)
	if errors.Is(err, sql.ErrNoRows) {
		manualPaused = false
		ownedError = sql.NullString{}
	} else if err != nil {
		return false, err
	}

	ownedStatusError := status == service.StatusError &&
		ownedError.Valid && errorMessage.Valid && ownedError.String == errorMessage.String
	// schedulable 阻断项的救治区豁免（r17ba）：在区号入区即有意开调度
	//（救治组喂种子/测试流量），复活点击 = revived 标签 → reenable 考证，
	// 带 AllowSchedulable 放行；其余闸（平台/影子/过期/状态）不豁免。
	if platform != service.PlatformOpenAI || accountType != service.AccountTypeOAuth ||
		parentAccountID.Valid || !notExpired || (!mutation.AllowSchedulable && schedulable) ||
		(status != service.StatusActive && !ownedStatusError) {
		return false, service.ErrOpenAIReenableBlocked
	}
	if manualPaused && !mutation.Unpause {
		return false, service.ErrOpenAIReenablePaused
	}

	var (
		stateUpdatedAt time.Time
		stateName      string
	)
	err = scanSingleRow(ctx, tx, `
		SELECT updated_at, state
		FROM openai_downgrade_probe_states
		WHERE account_id = $1
		FOR UPDATE
	`, []any{mutation.AccountID}, &stateUpdatedAt, &stateName)
	if errors.Is(err, sql.ErrNoRows) {
		return false, service.ErrOpenAIProbeStale
	}
	if err != nil {
		return false, err
	}
	if !stateUpdatedAt.Equal(mutation.ExpectedStateUpdatedAt) {
		return false, service.ErrOpenAIProbeStale
	}
	if stateName != service.OpenAIDowngradeStatePendingReplace {
		return false, service.ErrOpenAIReenableNotDead
	}

	unpaused := manualPaused && mutation.Unpause
	if unpaused {
		result, err := tx.ExecContext(ctx, `
			UPDATE openai_downgrade_probe_controls
			SET manual_paused = FALSE, updated_at = clock_timestamp()
			WHERE account_id = $1 AND manual_paused IS TRUE
		`, mutation.AccountID)
		if err := requireOpenAIProbeUpdatedRow(result, err); err != nil {
			return false, err
		}
		if err := appendOpenAIReenableEvent(ctx, tx, mutation.AccountID,
			mutation.State.CurrentProxyID, "manual_unpause", map[string]any{
				"via": "reenable",
				"why": "dead-account rescue (r17an user ruling 2026-09-28)",
			}); err != nil {
			return false, err
		}
	}
	if err := appendOpenAIReenableEvent(ctx, tx, mutation.AccountID,
		mutation.State.CurrentProxyID, "manual_reenable", map[string]any{
			"from_state":   service.OpenAIDowngradeStatePendingReplace,
			"to_mode":      "qualification",
			"next_probeat": mutation.State.NextProbeAt.Format(time.RFC3339),
			"unpaused":     unpaused,
		}); err != nil {
		return false, err
	}

	state := *mutation.State
	state.UpdatedAt = time.Now().UTC().Truncate(time.Microsecond)
	if !state.UpdatedAt.After(mutation.ExpectedStateUpdatedAt) {
		state.UpdatedAt = mutation.ExpectedStateUpdatedAt.Add(time.Microsecond)
	}
	transactionRepo := &openAIDowngradeProbeRepository{db: tx}
	if err := transactionRepo.SaveOpenAIDowngradeState(ctx, &state); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	*mutation.State = state
	return unpaused, nil
}

func appendOpenAIReenableEvent(
	ctx context.Context,
	tx *sql.Tx,
	accountID int64,
	proxyID *int64,
	eventType string,
	details map[string]any,
) error {
	payload, err := json.Marshal(details)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO openai_downgrade_probe_events(account_id, proxy_id, event_type, details)
		VALUES ($1, $2, $3, $4::jsonb)
	`, accountID, proxyID, eventType, string(payload))
	return err
}

func nullableInt64MatchesPointer(value sql.NullInt64, expected *int64) bool {
	if expected == nil {
		return !value.Valid
	}
	return value.Valid && value.Int64 == *expected
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
