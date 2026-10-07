package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

type openAIDowngradeProbeRepository struct {
	db sqlExecutor
}

func NewOpenAIDowngradeProbeRepository(db *sql.DB) service.OpenAIDowngradeProbeStore {
	return &openAIDowngradeProbeRepository{db: db}
}

func (r *openAIDowngradeProbeRepository) EnsureOpenAIDowngradeState(
	ctx context.Context,
	accountID int64,
	proxyID *int64,
	now time.Time,
) (*service.OpenAIDowngradeProbeState, error) {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO openai_downgrade_probe_states
			(account_id, original_proxy_id, current_proxy_id, next_probe_at, updated_at)
		VALUES ($1, $2, $2, $3, $3)
		ON CONFLICT (account_id) DO NOTHING
	`, accountID, proxyID, now)
	if err != nil {
		return nil, err
	}
	return r.GetOpenAIDowngradeState(ctx, accountID)
}

func (r *openAIDowngradeProbeRepository) GetOpenAIDowngradeState(
	ctx context.Context,
	accountID int64,
) (*service.OpenAIDowngradeProbeState, error) {
	var out service.OpenAIDowngradeProbeState
	if err := scanSingleRow(ctx, r.db, `
		SELECT account_id, state, original_proxy_id, current_proxy_id,
			probe_mode,
			consecutive_failures, consecutive_successes, first_failure_at,
			circuit_opened_at, recovery_deadline, next_probe_at, swap_count_7d,
			last_swap_at, last_probe_at, auth_consecutive_failures,
			astra_consecutive_failures, astra_consecutive_successes, astra_next_probe_at,
			updated_at, consecutive_429s
		FROM openai_downgrade_probe_states
		WHERE account_id = $1
	`, []any{accountID},
		&out.AccountID, &out.State, &out.OriginalProxyID, &out.CurrentProxyID,
		&out.ProbeMode,
		&out.ConsecutiveFailures, &out.ConsecutiveSuccesses, &out.FirstFailureAt,
		&out.CircuitOpenedAt, &out.RecoveryDeadline, &out.NextProbeAt, &out.SwapCount7d,
		&out.LastSwapAt, &out.LastProbeAt, &out.AuthConsecutiveFailures,
		&out.AstraConsecutiveFailures, &out.AstraConsecutiveSuccesses, &out.AstraNextProbeAt,
		&out.UpdatedAt, &out.Consecutive429s); err != nil {
		return nil, err
	}
	return &out, nil
}

func (r *openAIDowngradeProbeRepository) ListDueOpenAIDowngradeStates(
	ctx context.Context,
	now time.Time,
	limit int,
) ([]service.OpenAIDowngradeProbeState, error) {
	if limit <= 0 {
		limit = 100
	}
	// First qualification is admission work, not routine polling. Retain an
	// exit-wide one-minute floor, including deleted accounts' history, and the
	// full ten-minute floor for 429s and all repeat probes.
	firstQualification := "(s.probe_mode = 'qualification' AND s.last_probe_at IS NULL)"
	recentInterval := `CASE WHEN ` + firstQualification + ` AND recent.http_status <> 429
		THEN INTERVAL '1 minute' ELSE INTERVAL '10 minutes' END`
	rows, err := r.db.QueryContext(ctx, `
		WITH due AS (
			SELECT
				s.account_id, s.state, s.original_proxy_id, s.current_proxy_id,
				s.probe_mode,
				s.consecutive_failures, s.consecutive_successes, s.first_failure_at,
				s.circuit_opened_at, s.recovery_deadline, s.next_probe_at, s.swap_count_7d,
				s.last_swap_at, s.last_probe_at, s.auth_consecutive_failures,
				s.astra_consecutive_failures, s.astra_consecutive_successes, s.astra_next_probe_at,
				s.updated_at, s.consecutive_429s,
				`+firstQualification+` AS first_qualification,
				CASE
					WHEN p.exit_ip IS NOT NULL AND TRIM(p.exit_ip) <> ''
						THEN 'ip:' || TRIM(p.exit_ip)
					WHEN a.proxy_id IS NOT NULL
						THEN 'proxy:' || a.proxy_id::text
					ELSE 'account:' || s.account_id::text
				END AS probe_key,
				ROW_NUMBER() OVER (
					PARTITION BY CASE
						WHEN p.exit_ip IS NOT NULL AND TRIM(p.exit_ip) <> ''
							THEN 'ip:' || TRIM(p.exit_ip)
						WHEN a.proxy_id IS NOT NULL
							THEN 'proxy:' || a.proxy_id::text
						ELSE 'account:' || s.account_id::text
					END
					ORDER BY `+firstQualification+` DESC, s.next_probe_at, s.account_id
				) AS probe_rank
			FROM openai_downgrade_probe_states s
			JOIN accounts a ON a.id = s.account_id
			LEFT JOIN proxies p ON p.id = a.proxy_id
			WHERE s.next_probe_at <= $1
				AND a.deleted_at IS NULL
				AND (a.rate_limit_reset_at IS NULL OR a.rate_limit_reset_at <= $1)
				AND a.platform = 'openai' AND a.type = 'oauth'
				AND a.parent_account_id IS NULL
				-- 判死即终态（r17x 用户裁定 2026-09-21，选项A）：
				-- pending_replace 不再自动排探针（复活唯一入口=手动启用/救治区）。
				AND s.state <> 'pending_replace' 
				AND NOT EXISTS (
					SELECT 1 FROM openai_downgrade_probe_controls c
					WHERE c.account_id = a.id AND c.manual_paused
				)
				AND (a.auto_pause_on_expired IS NOT TRUE
					OR a.expires_at IS NULL OR a.expires_at > $1)
				AND (
					a.status = 'active'
					OR (a.status = 'error' AND EXISTS (
						SELECT 1 FROM openai_downgrade_probe_controls c
						WHERE c.account_id = a.id AND c.owned_error = a.error_message
					))
				)
				AND (s.state <> 'on_duty' OR a.schedulable IS TRUE
					OR s.probe_mode = 'qualification' OR a.status = 'error'
					-- auth 一振暂停（r17aq）：schedulable=false 是探针落的，必须
					-- 继续被拾取才能洗白/毕业，否则暂停号永不再探=死锁。
					OR s.auth_consecutive_failures > 0
					-- Early host qualification does not release rescue isolation.
					OR (jsonb_typeof(a.extra->'openai_rescue_lane') = 'object'
						AND a.extra->'openai_rescue_lane'->>'entered_at' ~
							'^[0-9]{4}-[0-9]{2}-[0-9]{2}T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](\.[0-9]+)?(Z|[+-][0-9]{2}:[0-5][0-9])$'
						AND (`+ollamaCloudUsageParseRFC3339SQL("a.extra->'openai_rescue_lane'->>'entered_at'")+`) IS NOT NULL
						AND COALESCE(a.extra->'openai_rescue_lane'->>'exit_reason', '') = ''))
				AND (
					a.proxy_id IS NULL
					OR (
						p.exit_ip IS NOT NULL AND TRIM(p.exit_ip) <> ''
						AND NOT EXISTS (
							SELECT 1
							FROM openai_downgrade_probe_results recent
							JOIN proxies recent_proxy ON recent_proxy.id = recent.proxy_id
							WHERE recent.created_at > $1 - (`+recentInterval+`)
								AND recent_proxy.exit_ip IS NOT NULL
								AND TRIM(recent_proxy.exit_ip) = TRIM(p.exit_ip)
						)
					)
					OR (
						(p.exit_ip IS NULL OR TRIM(p.exit_ip) = '')
						AND NOT EXISTS (
							SELECT 1
							FROM openai_downgrade_probe_results recent
							WHERE recent.proxy_id = a.proxy_id
								AND recent.created_at > $1 - (`+recentInterval+`)
						)
					)
				)
		)
		SELECT account_id, state, original_proxy_id, current_proxy_id,
			probe_mode,
			consecutive_failures, consecutive_successes, first_failure_at,
			circuit_opened_at, recovery_deadline, next_probe_at, swap_count_7d,
			last_swap_at, last_probe_at, auth_consecutive_failures,
			astra_consecutive_failures, astra_consecutive_successes, astra_next_probe_at,
			updated_at, consecutive_429s
		FROM due
		WHERE probe_rank = 1
		ORDER BY first_qualification DESC, next_probe_at, account_id
		LIMIT $2
	`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []service.OpenAIDowngradeProbeState
	for rows.Next() {
		var item service.OpenAIDowngradeProbeState
		if err := rows.Scan(
			&item.AccountID, &item.State, &item.OriginalProxyID, &item.CurrentProxyID,
			&item.ProbeMode,
			&item.ConsecutiveFailures, &item.ConsecutiveSuccesses, &item.FirstFailureAt,
			&item.CircuitOpenedAt, &item.RecoveryDeadline, &item.NextProbeAt, &item.SwapCount7d,
			&item.LastSwapAt, &item.LastProbeAt, &item.AuthConsecutiveFailures,
			&item.AstraConsecutiveFailures, &item.AstraConsecutiveSuccesses, &item.AstraNextProbeAt,
			&item.UpdatedAt, &item.Consecutive429s,
		); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (r *openAIDowngradeProbeRepository) CanRunOpenAIDowngradeProbe(ctx context.Context, accountID int64) (bool, error) {
	var allowed bool
	err := scanSingleRow(ctx, r.db, `
		SELECT EXISTS (
			SELECT 1 FROM accounts a
			LEFT JOIN openai_downgrade_probe_controls c ON c.account_id = a.id
			WHERE a.id = $1 AND a.deleted_at IS NULL AND a.platform = 'openai' AND a.type = 'oauth'
				AND a.parent_account_id IS NULL AND COALESCE(c.manual_paused, FALSE) IS FALSE
				AND (a.status = 'active' OR (a.status = 'error' AND c.owned_error = a.error_message))
				AND (a.auto_pause_on_expired IS NOT TRUE OR a.expires_at IS NULL OR a.expires_at > NOW())
		)
	`, []any{accountID}, &allowed)
	return allowed, err
}

// RecentProbeOnExitIP 同出口近窗合成探针判定（路径B 手动诊断针的节流闸，
// 镜像 ListDue L128-146 两分支）：账号绑定的代理有 exit_ip → 查同 exit_ip
// 近窗任意针；无 exit_ip → 查同 proxy 桶近窗任意针。探针结果表不含
// account 维度过滤（同出口其它号的针也算节流信号）。
func (r *openAIDowngradeProbeRepository) RecentProbeOnExitIP(
	ctx context.Context,
	accountID int64,
	proxyID *int64,
	since time.Time,
) (bool, error) {
	if proxyID == nil {
		return false, nil
	}
	var hit bool
	// 先取账号绑定代理的 exit_ip（无则按 proxy 桶查）。
	var exitIP *string
	err := scanSingleRow(ctx, r.db, `
		SELECT p.exit_ip FROM proxies p WHERE p.id = $1
	`, []any{*proxyID}, &exitIP)
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}
	if exitIP != nil && strings.TrimSpace(*exitIP) != "" {
		err = scanSingleRow(ctx, r.db, `
			SELECT EXISTS (
				SELECT 1
				FROM openai_downgrade_probe_results recent
				JOIN proxies recent_proxy ON recent_proxy.id = recent.proxy_id
				WHERE recent.created_at > $1
					AND recent_proxy.exit_ip IS NOT NULL
					AND TRIM(recent_proxy.exit_ip) = TRIM($2)
			)
		`, []any{since, strings.TrimSpace(*exitIP)}, &hit)
		return hit, err
	}
	err = scanSingleRow(ctx, r.db, `
		SELECT EXISTS (
			SELECT 1
			FROM openai_downgrade_probe_results recent
			WHERE recent.proxy_id = $1 AND recent.created_at > $2
		)
	`, []any{*proxyID, since}, &hit)
	return hit, err
}

func (r *openAIDowngradeProbeRepository) SaveOpenAIDowngradeState(
	ctx context.Context,
	state *service.OpenAIDowngradeProbeState,
) error {
	if state == nil {
		return nil
	}
	result, err := r.db.ExecContext(ctx, `
		UPDATE openai_downgrade_probe_states
		SET state = $2, original_proxy_id = $3, current_proxy_id = $4,
			probe_mode = $5,
			consecutive_failures = $6, consecutive_successes = $7,
			first_failure_at = $8, circuit_opened_at = $9, recovery_deadline = $10,
			next_probe_at = $11, swap_count_7d = $12, last_swap_at = $13,
			last_probe_at = $14, auth_consecutive_failures = $15,
			astra_consecutive_failures = $16, astra_consecutive_successes = $17,
			astra_next_probe_at = $18, updated_at = $19, consecutive_429s = $20
		WHERE account_id = $1
	`, state.AccountID, state.State, state.OriginalProxyID, state.CurrentProxyID,
		state.ProbeMode, state.ConsecutiveFailures, state.ConsecutiveSuccesses, state.FirstFailureAt,
		state.CircuitOpenedAt, state.RecoveryDeadline, state.NextProbeAt, state.SwapCount7d,
		state.LastSwapAt, state.LastProbeAt, state.AuthConsecutiveFailures,
		state.AstraConsecutiveFailures, state.AstraConsecutiveSuccesses, state.AstraNextProbeAt,
		state.UpdatedAt, state.Consecutive429s)
	return requireOpenAIProbeUpdatedRow(result, err)
}

func (r *openAIDowngradeProbeRepository) RecordOpenAIDowngradeProbe(
	ctx context.Context,
	result *service.OpenAIDowngradeProbeResult,
) error {
	if result == nil {
		return nil
	}
	if beginner, ok := r.db.(interface {
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	}); ok {
		tx, err := beginner.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		transactionRepo := &openAIDowngradeProbeRepository{db: tx}
		if err := transactionRepo.recordOpenAIDowngradeProbe(ctx, result); err != nil {
			return err
		}
		return tx.Commit()
	}
	return r.recordOpenAIDowngradeProbe(ctx, result)
}

func (r *openAIDowngradeProbeRepository) recordOpenAIDowngradeProbe(
	ctx context.Context,
	result *service.OpenAIDowngradeProbeResult,
) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO openai_downgrade_probe_results
			(account_id, proxy_id, mode, probe, transport_ok, answer_correct,
			 reasoning_tokens, juice, latency_ms, http_status, error_message, turn_state_len)
		VALUES ($1, $2, COALESCE(NULLIF($3, ''), 'normal'), TRUE, $4,
			CASE WHEN $4::boolean THEN $5::boolean ELSE NULL::boolean END,
			$6, $7, $8, NULLIF($9, 0), NULLIF($10, ''), $11)
	`, result.AccountID, result.ProxyID, result.Mode, result.TransportOK,
		result.AnswerCorrect, result.ReasoningTokens, result.Juice,
		result.Latency.Milliseconds(), result.HTTPStatus, result.ErrorMessage,
		result.TurnStateLen)
	if err != nil {
		return err
	}
	// The raw result row is the source of truth; the per-proxy aggregate is
	// derived telemetry, so a missing proxy binding simply skips aggregation.
	if result.ProxyID == nil {
		return nil
	}
	return r.upsertProxyOutcomeStats(ctx, *result.ProxyID, result)
}

func (r *openAIDowngradeProbeRepository) AppendOpenAIDowngradeEvent(
	ctx context.Context,
	accountID int64,
	proxyID *int64,
	eventType string,
	details map[string]any,
) error {
	payload, err := json.Marshal(details)
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(ctx, `
		INSERT INTO openai_downgrade_probe_events(account_id, proxy_id, event_type, details)
		VALUES (NULLIF($1, 0), $2, $3, $4::jsonb)
	`, accountID, proxyID, eventType, string(payload))
	return err
}

func (r *openAIDowngradeProbeRepository) SetOpenAIAccountProxy(
	ctx context.Context,
	accountID int64,
	proxyID *int64,
) error {
	exec := r.db
	var tx *sql.Tx
	if beginner, ok := r.db.(interface {
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	}); ok {
		var err error
		tx, err = beginner.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		exec = tx
	}
	if err := validateOpenAIOAuthProxyMutation(ctx, exec, accountID, proxyID); err != nil {
		return err
	}
	result, err := exec.ExecContext(ctx, `
		UPDATE accounts
		SET proxy_id = $2, updated_at = NOW()
		WHERE id = $1 AND platform = 'openai' AND deleted_at IS NULL
	`, accountID, proxyID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return service.ErrAccountNotFound
	}
	if err := enqueueSchedulerOutbox(ctx, exec, service.SchedulerOutboxEventAccountChanged, &accountID, nil, nil); err != nil {
		return err
	}
	if tx != nil {
		return tx.Commit()
	}
	return nil
}

// openAIDowngradePerIPAccountCap 是每个出口 IP（按 exit_ip 聚合）上允许挂载的
// OpenAI 账号硬上限。用户政策：绝对不能超过 5 个；绑定优先级为先住宅
// （bucket_risk_score 低）后机房（risk 高），同类内选在挂账号最少的 IP 分散负载。
// bucket_capacity 字段不再参与判满（历史上全表为 1，会把每 proxy 误判为只能挂 1 号）。
const openAIDowngradePerIPAccountCap = 5

// IP 健康闸（r15c）：绑桶与逃生选择都绕开「疑似 IP 级限流」的出口。
// 判据 = 近窗内该出口 IP 的探针 ≥3 样本、429 占比 ≥50%、且 429 来自
// ≥2 个不同账号（SQL 内字面量 3 / *10>=*5 / 2）。跨账号条件是 aliyun 与
// 1023 的教训：单账号在任何 IP 都可能账号级限流（1023 在住宅 205.179 与
// linode 172.233 两个 IP 连续 429、aliyun 24/44 429 但全是单账号），只有
// 多账号同时 429 才算 IP 的账——生产数据实测：50%+双账号恰好只闸住宅
// 205.179（13/25、4 账号），其余 IP 全放行。窗口 = 用户政策「静默 48h
// 差不多」：窗口滑动、样本清零后 IP 自动回池，不永久弃用、无需人工复位。
const openAIDowngradeIPHealthWindowHours = 48

// scanProxyID 跑一条返回单个 proxy id 的查询；无行/空值统一归一为 nil。
func (r *openAIDowngradeProbeRepository) scanProxyID(
	ctx context.Context, query string, args []any,
) (*int64, error) {
	var out sql.NullInt64
	err := scanSingleRow(ctx, r.db, query, args, &out)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !out.Valid {
		return nil, nil
	}
	return &out.Int64, nil
}

func (r *openAIDowngradeProbeRepository) FindOpenAIDowngradeMainProxy(
	ctx context.Context,
	accountID int64,
) (*int64, error) {
	proxyID, err := r.scanProxyID(ctx, `
		SELECT p.id
		FROM proxies p
		JOIN (
			SELECT pr.exit_ip AS ip, COUNT(a.id) AS n
			FROM proxies pr
			LEFT JOIN accounts a ON a.proxy_id = pr.id
				AND a.platform = 'openai' AND a.deleted_at IS NULL
			WHERE pr.deleted_at IS NULL
				AND pr.exit_ip IS NOT NULL AND TRIM(pr.exit_ip) <> ''
			GROUP BY pr.exit_ip
			HAVING COUNT(a.id) < $1
		) room ON room.ip = p.exit_ip
		LEFT JOIN (
			SELECT pr.exit_ip AS ip,
				COUNT(*) AS samples,
				COUNT(*) FILTER (WHERE r.http_status = 429) AS limited,
				COUNT(DISTINCT r.account_id) FILTER (WHERE r.http_status = 429) AS limited_accounts
			FROM openai_downgrade_probe_results r
			JOIN proxies pr ON pr.id = r.proxy_id
			WHERE r.created_at >= NOW() - make_interval(hours => $2)
			GROUP BY pr.exit_ip
		) health ON health.ip = p.exit_ip
		WHERE p.deleted_at IS NULL AND p.status = 'active'
			AND p.bucket_enabled IS TRUE
			AND p.exit_ip IS NOT NULL AND TRIM(p.exit_ip) <> ''
			AND NOT (
				COALESCE(health.samples, 0) >= 3
				AND COALESCE(health.limited, 0) * 10 >= COALESCE(health.samples, 0) * 5
				AND COALESCE(health.limited_accounts, 0) >= 2
			)
		ORDER BY p.bucket_risk_score ASC, room.n ASC, p.id
		LIMIT 1
	`, []any{openAIDowngradePerIPAccountCap, openAIDowngradeIPHealthWindowHours})
	if err != nil {
		return nil, err
	}
	if proxyID != nil {
		return proxyID, nil
	}
	// 全部候选被健康闸排除时回退无闸选择：新号必须绑桶（不绑不能用），
	// 绑到疑似限流 IP 后仍可经探针状态机换桶逃生，不绑则完全不可用。
	return r.scanProxyID(ctx, `
		SELECT p.id
		FROM proxies p
		JOIN (
			SELECT pr.exit_ip AS ip, COUNT(a.id) AS n
			FROM proxies pr
			LEFT JOIN accounts a ON a.proxy_id = pr.id
				AND a.platform = 'openai' AND a.deleted_at IS NULL
			WHERE pr.deleted_at IS NULL
				AND pr.exit_ip IS NOT NULL AND TRIM(pr.exit_ip) <> ''
			GROUP BY pr.exit_ip
			HAVING COUNT(a.id) < $1
		) room ON room.ip = p.exit_ip
		WHERE p.deleted_at IS NULL AND p.status = 'active'
			AND p.bucket_enabled IS TRUE
			AND p.exit_ip IS NOT NULL AND TRIM(p.exit_ip) <> ''
		ORDER BY p.bucket_risk_score ASC, room.n ASC, p.id
		LIMIT 1
	`, []any{openAIDowngradePerIPAccountCap})
}

func (r *openAIDowngradeProbeRepository) FindOpenAIDowngradeEscapeProxy(
	ctx context.Context,
	accountID int64,
	currentProxyID *int64,
) (*int64, error) {
	// 逃生只在健康闸内选，无健康候选时返回 nil（调用方留在当前桶 half_open）。
	// 不做无闸回退：把熔断账号迁去另一个疑似限流 IP，比原地半开更糟。
	return r.scanProxyID(ctx, `
		SELECT p.id
		FROM proxies p
		LEFT JOIN proxies current_proxy ON current_proxy.id = $2
		JOIN (
			SELECT pr.exit_ip AS ip, COUNT(a.id) AS n
			FROM proxies pr
			LEFT JOIN accounts a ON a.proxy_id = pr.id
				AND a.platform = 'openai' AND a.deleted_at IS NULL
			WHERE pr.deleted_at IS NULL
				AND pr.exit_ip IS NOT NULL AND TRIM(pr.exit_ip) <> ''
			GROUP BY pr.exit_ip
			HAVING COUNT(a.id) < $3
		) room ON room.ip = p.exit_ip
		LEFT JOIN (
			SELECT pr.exit_ip AS ip,
				COUNT(*) AS samples,
				COUNT(*) FILTER (WHERE r.http_status = 429) AS limited,
				COUNT(DISTINCT r.account_id) FILTER (WHERE r.http_status = 429) AS limited_accounts
			FROM openai_downgrade_probe_results r
			JOIN proxies pr ON pr.id = r.proxy_id
			WHERE r.created_at >= NOW() - make_interval(hours => $4)
			GROUP BY pr.exit_ip
		) health ON health.ip = p.exit_ip
		WHERE p.deleted_at IS NULL AND p.status = 'active'
			AND p.bucket_enabled IS TRUE
			AND p.exit_ip IS NOT NULL AND TRIM(p.exit_ip) <> ''
			AND current_proxy.exit_ip IS NOT NULL AND TRIM(current_proxy.exit_ip) <> ''
			AND p.exit_ip <> current_proxy.exit_ip
			AND NOT (
				COALESCE(health.samples, 0) >= 3
				AND COALESCE(health.limited, 0) * 10 >= COALESCE(health.samples, 0) * 5
				AND COALESCE(health.limited_accounts, 0) >= 2
			)
			AND NOT EXISTS (
				SELECT 1 FROM openai_downgrade_probe_events e
				WHERE e.account_id = $1 AND e.proxy_id = p.id
					AND e.event_type = 'bucket_reprobe'
			)
			AND NOT EXISTS (
				SELECT 1 FROM openai_downgrade_probe_events e
				WHERE e.proxy_id = p.id
					AND e.event_type = 'bucket_rescue'
					AND e.account_id <> $1
			)
		ORDER BY p.bucket_risk_score ASC, room.n ASC, p.id
		LIMIT 1
	`, []any{accountID, currentProxyID, openAIDowngradePerIPAccountCap, openAIDowngradeIPHealthWindowHours})
}

func (r *openAIDowngradeProbeRepository) ListOpenAIDowngradeBuckets(
	ctx context.Context,
) ([]service.OpenAIDowngradeBucket, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT p.id, p.name, p.bucket_role, COALESCE(p.exit_ip, ''),
			p.bucket_capacity, p.bucket_risk_score,
			COALESCE(array_agg(a.id ORDER BY a.id) FILTER (WHERE a.id IS NOT NULL), '{}')
		FROM proxies p
		LEFT JOIN accounts a ON a.proxy_id = p.id
			AND a.platform = 'openai' AND a.deleted_at IS NULL
		WHERE p.deleted_at IS NULL AND p.bucket_enabled IS TRUE
		GROUP BY p.id
		ORDER BY p.bucket_role, p.id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []service.OpenAIDowngradeBucket
	for rows.Next() {
		var item service.OpenAIDowngradeBucket
		if err := rows.Scan(&item.ProxyID, &item.Name, &item.Role, &item.ExitIP,
			&item.Capacity, &item.RiskScore, pq.Array(&item.AccountIDs)); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 判满按每出口 IP 聚合（与绑定策略一致），而非单 proxy 行。
	perIP := make(map[string]int, len(out))
	for i := range out {
		if out[i].ExitIP != "" {
			perIP[out[i].ExitIP] += len(out[i].AccountIDs)
		}
	}
	for i := range out {
		out[i].AtCapacity = out[i].ExitIP != "" &&
			perIP[out[i].ExitIP] >= openAIDowngradePerIPAccountCap
	}
	return out, nil
}

func (r *openAIDowngradeProbeRepository) ListOpenAIDowngradeEvents(
	ctx context.Context,
	since time.Time,
	limit int,
) ([]service.OpenAIDowngradeEvent, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, account_id, proxy_id, event_type, details, created_at
		FROM openai_downgrade_probe_events
		WHERE created_at >= $1
		ORDER BY created_at DESC, id DESC
		LIMIT $2
	`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]service.OpenAIDowngradeEvent, 0, limit)
	for rows.Next() {
		var item service.OpenAIDowngradeEvent
		var raw []byte
		if err := rows.Scan(&item.ID, &item.AccountID, &item.ProxyID, &item.EventType, &raw, &item.CreatedAt); err != nil {
			return nil, err
		}
		item.Details = map[string]any{}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &item.Details); err != nil {
				return nil, err
			}
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (r *openAIDowngradeProbeRepository) ListOpenAIDowngradeAccountStats(
	ctx context.Context,
	since time.Time,
	limit int,
) ([]service.OpenAIDowngradeAccountStat, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT
			a.id,
			COALESCE(s.state, 'on_duty'),
			COALESCE(s.probe_mode, 'normal'),
			a.schedulable,
			a.proxy_id,
			COALESCE(p.exit_ip, ''),
			COUNT(pr.id) FILTER (WHERE pr.created_at >= $1),
			COUNT(pr.id) FILTER (WHERE pr.created_at >= $1
				AND pr.transport_ok AND pr.answer_correct
				AND pr.reasoning_tokens >= $2),
			AVG(pr.reasoning_tokens) FILTER (WHERE pr.created_at >= $1),
			COALESCE(s.consecutive_failures, 0),
			COALESCE(s.last_probe_at, MAX(pr.created_at))
		FROM accounts a
		LEFT JOIN openai_downgrade_probe_states s ON s.account_id = a.id
		LEFT JOIN proxies p ON p.id = a.proxy_id
		LEFT JOIN openai_downgrade_probe_results pr ON pr.account_id = a.id
		WHERE a.platform = 'openai' AND a.deleted_at IS NULL
		GROUP BY a.id, s.state, s.probe_mode, a.schedulable, a.proxy_id,
			p.exit_ip, s.consecutive_failures, s.last_probe_at
		ORDER BY
			CASE COALESCE(s.state, 'on_duty')
				WHEN 'pending_replace' THEN 0
				WHEN 'circuit_open' THEN 1
				WHEN 'reprobe' THEN 2
				ELSE 3
			END,
			a.id
		LIMIT $3
	`, since, service.OpenAIDowngradeRecoveryReasoningMinimum, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]service.OpenAIDowngradeAccountStat, 0, limit)
	for rows.Next() {
		var item service.OpenAIDowngradeAccountStat
		var probeCount, successCount int64
		var avg sql.NullFloat64
		if err := rows.Scan(
			&item.AccountID, &item.State, &item.ProbeMode, &item.Schedulable,
			&item.ProxyID, &item.ProxyExitIP, &probeCount, &successCount,
			&avg, &item.ConsecutiveFailures, &item.LastProbeAt,
		); err != nil {
			return nil, err
		}
		item.ProbeCount24h = probeCount
		item.SuccessCount24h = successCount
		if probeCount > 0 {
			item.SuccessRate24h = float64(successCount) / float64(probeCount)
		}
		if avg.Valid {
			item.AvgReasoningTokens = &avg.Float64
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (r *openAIDowngradeProbeRepository) ListOpenAIDowngradeDashboard(
	ctx context.Context,
	since time.Time,
) (*service.OpenAIDowngradeDashboard, error) {
	buckets, err := r.ListOpenAIDowngradeBuckets(ctx)
	if err != nil {
		return nil, err
	}
	var probes, successes int64
	if err := scanSingleRow(ctx, r.db, `
		SELECT COUNT(*), COUNT(*) FILTER (WHERE transport_ok AND answer_correct
			AND reasoning_tokens >= $1)
		FROM openai_downgrade_probe_results
		WHERE created_at >= $2
	`, []any{service.OpenAIDowngradeRecoveryReasoningMinimum, since}, &probes, &successes); err != nil {
		return nil, err
	}
	return &service.OpenAIDowngradeDashboard{
		Buckets: buckets, ProbeCount24h: probes, SuccessCount24h: successes,
	}, nil
}

// openAIDowngradePurgeBatchSize 保留清理的分批上限：与 usage_logs 清理同思路
// （dashboard_aggregation_repo.go），单批 DELETE 的锁持有时长与 WAL 量有界，
// 避免大表首次清理时长时间阻塞探针写入。
const openAIDowngradePurgeBatchSize = 5000

// PurgeOpenAIDowngradeProbeHistory implements the P2-10 retention policy:
// probe results older than resultsBefore and events older than eventsBefore
// are deleted. Events are audit evidence and use a longer retention window.
// 删除按 ctid 批量进行（r17x H 项），两张表独立分批直到删尽。
func (r *openAIDowngradeProbeRepository) PurgeOpenAIDowngradeProbeHistory(
	ctx context.Context,
	resultsBefore, eventsBefore time.Time,
) (int64, int64, error) {
	purgedResults, err := purgeOpenAIDowngradeRowsBefore(ctx, r.db,
		"openai_downgrade_probe_results", resultsBefore)
	if err != nil {
		return purgedResults, 0, err
	}
	purgedEvents, err := purgeOpenAIDowngradeRowsBefore(ctx, r.db,
		"openai_downgrade_probe_events", eventsBefore)
	if err != nil {
		return purgedResults, 0, err
	}
	return purgedResults, purgedEvents, nil
}

// purgeOpenAIDowngradeRowsBefore 循环删除 table 中 created_at < before 的行，
// 每批 openAIDowngradePurgeBatchSize 行。表名是编译期常量集，不做用户输入拼接。
func purgeOpenAIDowngradeRowsBefore(
	ctx context.Context,
	db sqlExecutor,
	table string,
	before time.Time,
) (int64, error) {
	query := fmt.Sprintf(`
		WITH victims AS (
			SELECT ctid FROM %s
			WHERE created_at < $1
			ORDER BY created_at ASC
			LIMIT $2
		)
		DELETE FROM %s
		WHERE ctid IN (SELECT ctid FROM victims)
	`, table, table)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		res, err := db.ExecContext(ctx, query, before.UTC(), openAIDowngradePurgeBatchSize)
		if err != nil {
			return total, err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += affected
		if affected < openAIDowngradePurgeBatchSize {
			return total, nil
		}
	}
}

// CountOpenAIDowngradeEvents 统计某账号在指定时间点之后的某类事件数，
// 用于判死账号指数退避重试的轮次推导（窄接口 OpenAIDowngradeReplaceEventCounter）。
func (r *openAIDowngradeProbeRepository) CountOpenAIDowngradeEvents(
	ctx context.Context,
	accountID int64,
	eventType string,
	since time.Time,
) (int, error) {
	var n int
	rows, err := r.db.QueryContext(ctx, `
		SELECT COUNT(*) FROM openai_downgrade_probe_events
		WHERE account_id = $1 AND event_type = $2 AND created_at >= $3
	`, accountID, eventType, since)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	if rows.Next() {
		if err := rows.Scan(&n); err != nil {
			return 0, err
		}
	}
	return n, rows.Err()
}

// DeleteOpenAIDowngradeStatesForGoneAccounts 清理已删除（软删或硬删）账号残留的
// 探针状态。软删只置 accounts.deleted_at，不触发 states 的 FK 级联；残留 state 会以
// 陈旧的 next_probe_at 永久占据 ListDue 同 IP 分区的 rank-1，把同桶的活账号饿死在
// rank-2。由 probe runner 的每日保留任务调用（窄接口
// service.OpenAIDowngradeGoneAccountStateCleaner）。
func (r *openAIDowngradeProbeRepository) DeleteOpenAIDowngradeStatesForGoneAccounts(
	ctx context.Context,
) (int64, error) {
	res, err := r.db.ExecContext(ctx, `
		DELETE FROM openai_downgrade_probe_states s
		WHERE NOT EXISTS (
			SELECT 1 FROM accounts a
			WHERE a.id = s.account_id AND a.deleted_at IS NULL
		)
	`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteOpenAIDowngradeState 就地删除单个账号的探针状态：上面每日清理器的
// 单账号版。processState 在 GetByID 命中 ErrAccountNotFound 哨兵（账号已软删）
// 时经窄接口 service.OpenAIDowngradeStateDeleter 调用——ListDue 已 JOIN 活账号，
// 但扫描与删除之间仍有竞态窗口，且就地清理让同桶活账号立刻赢回 rank-1，
// 不必等 24h 周期（2026-09-17：15 僵尸曾钉死全部 5 个在用桶，1055 降智
// 5.5h 未检出）。只删 state 行，results 历史由每日保留任务负责。
func (r *openAIDowngradeProbeRepository) DeleteOpenAIDowngradeState(
	ctx context.Context,
	accountID int64,
) error {
	if accountID <= 0 {
		return errors.New("invalid account id for OpenAI downgrade state deletion")
	}
	_, err := r.db.ExecContext(ctx, `
		DELETE FROM openai_downgrade_probe_states
		WHERE account_id = $1
	`, accountID)
	return err
}
