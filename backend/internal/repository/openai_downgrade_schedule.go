package repository

import (
	"context"
	"errors"
	"time"
)

// AccelerateOpenAIInterruptedProbeRechecks closes the feedback loop between
// probe evidence and successful real traffic. A later billable request proves
// that the account is serving again, but it does not prove answer quality, so
// this only shortens the next normal probe schedule.
func (r *openAIDowngradeProbeRepository) AccelerateOpenAIInterruptedProbeRechecks(
	ctx context.Context, now time.Time, delay time.Duration,
) (int64, error) {
	if now.IsZero() || delay <= 0 {
		return 0, errors.New("invalid OpenAI interrupted probe recheck delay")
	}
	result, err := r.db.ExecContext(ctx, `
		WITH candidates AS MATERIALIZED (
			SELECT s.account_id, s.updated_at, s.next_probe_at, s.last_probe_at,
				s.current_proxy_id, a.updated_at AS account_updated_at,
				latest.id AS probe_result_id, latest.created_at AS probe_at,
				traffic.created_at AS traffic_at,
				$1::timestamptz + $2 * INTERVAL '1 second' AS recheck_at
			FROM openai_downgrade_probe_states s
			JOIN accounts a ON a.id = s.account_id
			JOIN LATERAL (
				SELECT p.id, p.proxy_id, p.transport_ok, p.created_at
				FROM openai_downgrade_probe_results p
				WHERE p.account_id = s.account_id
				ORDER BY p.created_at DESC, p.id DESC
				LIMIT 1
			) latest ON TRUE
			JOIN LATERAL (
				SELECT ul.created_at
				FROM usage_logs ul
				WHERE ul.account_id = s.account_id
					AND ul.actual_cost > 0
					AND ul.created_at > latest.created_at
				ORDER BY ul.created_at DESC
				LIMIT 1
			) traffic ON TRUE
			WHERE latest.transport_ok IS FALSE
				AND s.last_probe_at IS NOT NULL
				AND latest.created_at >= s.last_probe_at
				AND s.next_probe_at > $1::timestamptz + $2 * INTERVAL '1 second'
				AND s.state = 'on_duty' AND s.probe_mode = 'normal'
				AND a.deleted_at IS NULL
				AND a.platform = 'openai' AND a.type = 'oauth'
				AND a.parent_account_id IS NULL
				AND a.status = 'active' AND a.schedulable IS TRUE
				AND (a.auto_pause_on_expired IS NOT TRUE
					OR a.expires_at IS NULL OR a.expires_at > $1)
				AND NOT EXISTS (
					SELECT 1 FROM openai_downgrade_probe_controls c
					WHERE c.account_id = a.id AND c.manual_paused
				)
				AND latest.proxy_id IS NOT DISTINCT FROM a.proxy_id
				AND s.current_proxy_id IS NOT DISTINCT FROM a.proxy_id
		), locked AS MATERIALIZED (
			SELECT c.*
			FROM candidates c
			JOIN accounts a ON a.id = c.account_id
				AND a.updated_at = c.account_updated_at
			FOR UPDATE OF a SKIP LOCKED
		), changed AS (
			UPDATE openai_downgrade_probe_states s
			SET next_probe_at = c.recheck_at,
				updated_at = GREATEST($1::timestamptz, s.updated_at + INTERVAL '1 microsecond')
			FROM locked c
			WHERE s.account_id = c.account_id
				AND s.updated_at = c.updated_at
				AND s.next_probe_at = c.next_probe_at
				AND s.last_probe_at = c.last_probe_at
				AND s.current_proxy_id IS NOT DISTINCT FROM c.current_proxy_id
			RETURNING s.account_id, s.current_proxy_id,
				c.probe_result_id, c.probe_at, c.traffic_at,
				c.next_probe_at AS previous_next_probe_at, s.next_probe_at
		)
		INSERT INTO openai_downgrade_probe_events(
			account_id, proxy_id, event_type, details, created_at
		)
		SELECT account_id, current_proxy_id, 'real_traffic_recheck_armed',
			jsonb_build_object(
				'probe_result_id', probe_result_id,
				'probe_at', probe_at,
				'traffic_at', traffic_at,
				'previous_next_probe_at', previous_next_probe_at,
				'next_probe_at', next_probe_at,
				'reason', 'successful_real_traffic_after_interrupted_probe'
			), $1
		FROM changed
	`, now, delay.Seconds())
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// Reconcile only legacy long 429 schedules. The account's actual cooldown is
// untouched; a later accepted probe still has to clear it through its CAS.
func (r *openAIDowngradeProbeRepository) ReconcileOpenAIRateLimitProbeSchedules(
	ctx context.Context, now time.Time, interval time.Duration,
) (int64, error) {
	if now.IsZero() || interval <= 0 {
		return 0, errors.New("invalid OpenAI rate limit reconciliation interval")
	}
	result, err := r.db.ExecContext(ctx, `
		WITH candidates AS MATERIALIZED (
			SELECT s.account_id, s.updated_at, s.next_probe_at, s.last_probe_at,
				s.current_proxy_id, a.updated_at AS account_updated_at,
				GREATEST(
					s.last_probe_at + ($2 * (1 + RANDOM() * 0.25)) * INTERVAL '1 second',
					$1::timestamptz + (15 + RANDOM() * 30) * INTERVAL '1 minute'
				) AS recheck_at
			FROM openai_downgrade_probe_states s
			JOIN accounts a ON a.id = s.account_id
			JOIN LATERAL (
				SELECT p.http_status, p.proxy_id, p.created_at
				FROM openai_downgrade_probe_results p
				WHERE p.account_id = s.account_id
				ORDER BY p.created_at DESC, p.id DESC LIMIT 1
			) latest ON TRUE
			WHERE s.last_probe_at IS NOT NULL
				AND s.next_probe_at > GREATEST(
					s.last_probe_at + ($2 * 1.25) * INTERVAL '1 second',
					$1::timestamptz + INTERVAL '45 minutes'
				)
				AND latest.http_status = 429
				AND latest.created_at >= s.last_probe_at
				AND latest.proxy_id IS NOT DISTINCT FROM a.proxy_id
				AND s.current_proxy_id IS NOT DISTINCT FROM a.proxy_id
				AND a.deleted_at IS NULL
				AND a.platform = 'openai' AND a.type = 'oauth'
				AND a.parent_account_id IS NULL
				AND (a.auto_pause_on_expired IS NOT TRUE
					OR a.expires_at IS NULL OR a.expires_at > $1)
				AND NOT EXISTS (
					SELECT 1 FROM openai_downgrade_probe_controls c
					WHERE c.account_id = a.id AND c.manual_paused
				)
				AND (a.status = 'active' OR (a.status = 'error' AND EXISTS (
					SELECT 1 FROM openai_downgrade_probe_controls c
					WHERE c.account_id = a.id AND c.owned_error = a.error_message
				)))
				AND (s.state <> 'on_duty' OR a.schedulable IS TRUE
					OR s.probe_mode = 'qualification' OR a.status = 'error')
		), locked AS MATERIALIZED (
			-- Match the probe commit's account-before-state lock order.
			-- Busy or changed accounts are reconsidered by the next scan.
			SELECT c.*
			FROM candidates c
			JOIN accounts a ON a.id = c.account_id
				AND a.updated_at = c.account_updated_at
			FOR UPDATE OF a SKIP LOCKED
		), changed AS (
			UPDATE openai_downgrade_probe_states s
			SET next_probe_at = c.recheck_at,
				updated_at = GREATEST($1::timestamptz, s.updated_at + INTERVAL '1 microsecond')
			FROM locked c
			WHERE s.account_id = c.account_id
				AND s.updated_at = c.updated_at AND s.next_probe_at = c.next_probe_at
				AND s.last_probe_at = c.last_probe_at
				AND s.current_proxy_id IS NOT DISTINCT FROM c.current_proxy_id
			RETURNING s.account_id, s.current_proxy_id, c.next_probe_at AS previous_next_probe_at,
				s.next_probe_at
		)
		INSERT INTO openai_downgrade_probe_events(account_id, proxy_id, event_type, details, created_at)
		SELECT account_id, current_proxy_id, 'rate_limit_schedule_reconciled',
			jsonb_build_object('previous_next_probe_at', previous_next_probe_at,
				'next_probe_at', next_probe_at, 'reason', 'sparse_recheck_adoption'), $1
		FROM changed
	`, now, interval.Seconds())
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
