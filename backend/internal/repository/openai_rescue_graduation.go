package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

func (r *accountRepository) CommitOpenAIRescueTransition(ctx context.Context, m service.OpenAIRescueTransitionUpdate) (time.Time, error) {
	if m.AccountID <= 0 || m.ExpectedUpdatedAt.IsZero() {
		return time.Time{}, service.ErrOpenAIProbeStale
	}
	switch m.Kind {
	case service.OpenAIRescueTransitionEnter, service.OpenAIRescueTransitionExit, service.OpenAIRescueTransitionRebind:
	default:
		return time.Time{}, service.ErrRescueLaneIneligible
	}
	extra := m.Extra
	if extra == nil {
		extra = map[string]any{}
	}
	payload, err := json.Marshal(extra)
	if err != nil {
		return time.Time{}, err
	}
	expectedMarker, err := json.Marshal(m.ExpectedMarker)
	if err != nil {
		return time.Time{}, err
	}
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return time.Time{}, err
	}
	defer func() { _ = tx.Rollback() }()
	exec, ok := any(tx.Client().Driver()).(sqlExecutor)
	if !ok {
		return time.Time{}, service.ErrOpenAIProbeAtomicStore
	}
	var updatedAt time.Time
	err = scanSingleRow(ctx, exec, `
		UPDATE accounts
		SET extra = COALESCE(extra, '{}'::jsonb) || $3::jsonb,
			schedulable = FALSE,
			updated_at = GREATEST(clock_timestamp(), updated_at + INTERVAL '1 microsecond')
		WHERE id = $1 AND updated_at = $2 AND deleted_at IS NULL
			AND platform = 'openai' AND type = 'oauth' AND parent_account_id IS NULL
			AND (($4 = 'enter' AND COALESCE(jsonb_typeof(extra->'openai_rescue_lane'), '') <> 'object')
				OR ($4 <> 'enter' AND jsonb_typeof(extra->'openai_rescue_lane') = 'object'))
			AND ($4 <> 'rebind' OR COALESCE(extra->'openai_rescue_lane'->>'exit_reason', '') = '')
			AND ($4 = 'enter' OR $5::jsonb = 'null'::jsonb OR extra->'openai_rescue_lane' = $5::jsonb)
		RETURNING updated_at
	`, []any{m.AccountID, m.ExpectedUpdatedAt, string(payload), string(m.Kind), string(expectedMarker)}, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, service.ErrOpenAIProbeStale
	}
	if err != nil {
		return time.Time{}, err
	}
	if err := replaceRescueGroups(ctx, exec, m.AccountID, m.GroupIDs); err != nil {
		return time.Time{}, err
	}
	// Related-row triggers advance the account revision after group writes.
	if err := scanSingleRow(ctx, exec, "SELECT updated_at FROM accounts WHERE id = $1",
		[]any{m.AccountID}, &updatedAt); err != nil {
		return time.Time{}, err
	}
	groups := buildSchedulerGroupPayload(mergeGroupIDs(m.OldGroupIDs, m.GroupIDs))
	if err := enqueueSchedulerOutbox(ctx, exec, service.SchedulerOutboxEventAccountGroupsChanged,
		&m.AccountID, nil, groups); err != nil {
		return time.Time{}, err
	}
	if err := tx.Commit(); err != nil {
		return time.Time{}, err
	}
	r.syncSchedulerAccountSnapshotDetached(ctx, m.AccountID)
	return updatedAt, nil
}

func replaceRescueGroups(ctx context.Context, exec sqlExecutor, accountID int64, groupIDs []int64) error {
	if len(groupIDs) > 0 {
		var found int
		if err := scanSingleRow(ctx, exec, `SELECT COUNT(*) FROM (
			SELECT id FROM groups WHERE id = ANY($1) AND deleted_at IS NULL ORDER BY id FOR KEY SHARE
		) AS targets`, []any{pq.Array(groupIDs)}, &found); err != nil {
			return err
		}
		if found != len(groupIDs) {
			return service.ErrGroupNotFound
		}
	}
	if _, err := exec.ExecContext(ctx, "DELETE FROM account_groups WHERE account_id = $1", accountID); err != nil {
		return err
	}
	if len(groupIDs) > 0 {
		if _, err := exec.ExecContext(ctx, `
			INSERT INTO account_groups(account_id, group_id, priority, created_at)
			SELECT $1, group_id, priority, NOW()
			FROM unnest($2::bigint[]) WITH ORDINALITY AS groups(group_id, priority)
		`, accountID, pq.Array(groupIDs)); err != nil {
			return err
		}
	}
	return nil
}

func (r *accountRepository) CommitOpenAIRescueMarker(ctx context.Context, m service.OpenAIRescueMarkerUpdate) (time.Time, error) {
	if m.AccountID <= 0 || m.ExpectedUpdatedAt.IsZero() {
		return time.Time{}, service.ErrOpenAIProbeStale
	}
	updates := map[string]any{"openai_rescue_lane": m.Marker}
	if m.RawMarker != nil {
		updates["openai_rescue_lane"] = m.RawMarker
	}
	if m.Withdraw {
		updates["openai_rescue_suspected"] = true
	}
	payload, err := json.Marshal(updates)
	if err != nil {
		return time.Time{}, err
	}
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return time.Time{}, err
	}
	defer func() { _ = tx.Rollback() }()
	exec, ok := any(tx.Client().Driver()).(sqlExecutor)
	if !ok {
		return time.Time{}, service.ErrOpenAIProbeAtomicStore
	}
	var updatedAt time.Time
	err = scanSingleRow(ctx, exec, `
		UPDATE accounts
		SET extra = COALESCE(extra, '{}'::jsonb) || $3::jsonb,
			schedulable = CASE WHEN $4 THEN FALSE ELSE schedulable END,
			updated_at = GREATEST(clock_timestamp(), updated_at + INTERVAL '1 microsecond')
		WHERE id = $1 AND updated_at = $2 AND deleted_at IS NULL
			AND platform = 'openai' AND type = 'oauth' AND parent_account_id IS NULL
			AND jsonb_typeof(extra->'openai_rescue_lane') = 'object'
			AND COALESCE(extra->'openai_rescue_terminated_at', 'null'::jsonb) = 'null'::jsonb
		RETURNING updated_at
	`, []any{m.AccountID, m.ExpectedUpdatedAt, string(payload), m.Withdraw}, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, service.ErrOpenAIProbeStale
	}
	if err != nil {
		return time.Time{}, err
	}
	if err := enqueueSchedulerOutbox(ctx, exec, service.SchedulerOutboxEventAccountChanged,
		&m.AccountID, nil, nil); err != nil {
		return time.Time{}, err
	}
	if err := tx.Commit(); err != nil {
		return time.Time{}, err
	}
	r.syncSchedulerAccountSnapshotDetached(ctx, m.AccountID)
	return updatedAt, nil
}

func (r *accountRepository) CommitOpenAIRescueGraduation(ctx context.Context, m service.OpenAIRescueGraduation) error {
	if m.AccountID <= 0 || m.ExpectedUpdatedAt.IsZero() || m.HostProbeID <= 0 ||
		m.HostProbeAt.IsZero() || m.EvidenceExpiresAt.IsZero() {
		return service.ErrRescueRecoveryUnverified
	}
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	exec, ok := any(tx.Client().Driver()).(sqlExecutor)
	if !ok {
		return service.ErrOpenAIProbeAtomicStore
	}
	var version time.Time
	var rawMarker []byte
	err = scanSingleRow(ctx, exec, `
		SELECT a.updated_at, a.extra->'openai_rescue_lane'
		FROM accounts a
		WHERE a.id = $1 AND a.updated_at = $2
			AND a.deleted_at IS NULL AND a.platform = 'openai' AND a.type = 'oauth'
			AND a.parent_account_id IS NULL AND a.status = 'active'
			AND jsonb_typeof(a.extra->'openai_rescue_lane') = 'object'
			AND COALESCE(a.extra->'openai_rescue_lane'->>'exit_reason', '') = ''
			AND COALESCE(a.extra->'openai_rescue_terminated_at', 'null'::jsonb) = 'null'::jsonb
			AND (a.auto_pause_on_expired IS NOT TRUE OR a.expires_at IS NULL OR a.expires_at > clock_timestamp())
			AND NOT EXISTS (
				SELECT 1 FROM openai_downgrade_probe_controls c
				WHERE c.account_id = a.id AND c.manual_paused
			)
		FOR UPDATE OF a
	`, []any{m.AccountID, m.ExpectedUpdatedAt}, &version, &rawMarker)
	if errors.Is(err, sql.ErrNoRows) {
		return service.ErrOpenAIProbeStale
	}
	if err != nil {
		return err
	}
	var marker map[string]any
	if err := json.Unmarshal(rawMarker, &marker); err != nil {
		return err
	}
	m.GroupIDs, err = service.OpenAIRescueOriginalGroups(marker)
	if err != nil {
		return err
	}
	// Match the probe writer's account -> state lock order.
	err = scanSingleRow(ctx, exec, `
		SELECT updated_at FROM openai_downgrade_probe_states
		WHERE account_id = $1 AND state = 'on_duty' AND probe_mode = 'normal'
		FOR UPDATE
	`, []any{m.AccountID}, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return service.ErrOpenAIProbeStale
	}
	if err != nil {
		return err
	}
	var evidenceFresh bool
	if err := scanSingleRow(ctx, exec, `SELECT $1::timestamptz > clock_timestamp()`,
		[]any{m.EvidenceExpiresAt}, &evidenceFresh); err != nil {
		return err
	}
	if !evidenceFresh {
		return service.ErrRescueRecoveryUnverified
	}
	// Hold the account/state rows while checking that no later host result
	// replaced the signature read by the orchestrator.
	snapshots, err := (&openAIDowngradeProbeRepository{db: exec}).ListOpenAIProbeHealthSnapshots(ctx, []int64{m.AccountID})
	if err != nil {
		return err
	}
	if len(snapshots) != 1 || snapshots[0].LastProbe == nil {
		return service.ErrRescueRecoveryUnverified
	}
	probe := snapshots[0].LastProbe
	result := service.OpenAIDowngradeProbeResult{
		TransportOK: probe.TransportOK, HTTPStatus: probe.HTTPStatus,
		AnswerCorrect:   probe.AnswerCorrect != nil && *probe.AnswerCorrect,
		ReasoningTokens: probe.ReasoningTokens,
	}
	if probe.ID != m.HostProbeID || !probe.At.Equal(m.HostProbeAt) ||
		probe.Mode != "qualification" || !result.IsQualificationPass() {
		return service.ErrRescueRecoveryUnverified
	}
	payload, err := json.Marshal(m.Extra)
	if err != nil {
		return err
	}
	if err := replaceRescueGroups(ctx, exec, m.AccountID, m.GroupIDs); err != nil {
		return err
	}
	if _, err := exec.ExecContext(ctx, `
		UPDATE accounts
		SET extra = COALESCE(extra, '{}'::jsonb) || $2::jsonb,
			schedulable = TRUE,
			updated_at = GREATEST(clock_timestamp(), updated_at + INTERVAL '1 microsecond')
		WHERE id = $1
	`, m.AccountID, string(payload)); err != nil {
		return err
	}
	groups := buildSchedulerGroupPayload(mergeGroupIDs(m.OldGroupIDs, m.GroupIDs))
	if err := enqueueSchedulerOutbox(ctx, exec, service.SchedulerOutboxEventAccountGroupsChanged,
		&m.AccountID, nil, groups); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	r.syncSchedulerAccountSnapshotDetached(ctx, m.AccountID)
	return nil
}
