package repository

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

// ListOpenAIProbeHealthSnapshots 批量健康快照聚合（相位A 健康标签数据源）。
// 单条 SQL 拉齐：探针状态机 + manual_paused + 限流 + 资格标志 + 最近一针
// （含 turn_state_len，241 号迁移新列；历史行无此列值=0=展示"—"）。
// LATERAL 取每号最近一针：probe_results 有 (account_id, created_at DESC)
// 索引，LIMIT 1 的 lateral join 是索引扫描，不放大。
func (r *openAIDowngradeProbeRepository) ListOpenAIProbeHealthSnapshots(
	ctx context.Context,
	accountIDs []int64,
) ([]service.OpenAIProbeHealthSnapshot, error) {
	if len(accountIDs) == 0 {
		return nil, nil
	}
	query := `
		SELECT
			a.id,
			COALESCE(s.state, 'on_duty'),
			COALESCE(s.probe_mode, 'normal'),
			a.schedulable,
			COALESCE(c.manual_paused, FALSE),
			a.rate_limited_at,
			COALESCE(a.extra->>'openai_downgrade_qualification', '') = 'true',
			a.extra->'openai_rescue_lane',
			lp.at,
			lp.mode,
			lp.reasoning_tokens,
			lp.transport_ok,
			lp.answer_correct,
			lp.turn_state_len,
			lp.http_status
		FROM accounts a
		LEFT JOIN openai_downgrade_probe_states s ON s.account_id = a.id
		LEFT JOIN openai_downgrade_probe_controls c ON c.account_id = a.id
		LEFT JOIN LATERAL (
			SELECT pr.created_at AS at, pr.mode, pr.reasoning_tokens,
			       pr.transport_ok, pr.answer_correct, pr.turn_state_len, pr.http_status
			FROM openai_downgrade_probe_results pr
			WHERE pr.account_id = a.id
			ORDER BY pr.created_at DESC
			LIMIT 1
		) lp ON TRUE
		WHERE a.id = ANY($1)
		ORDER BY a.id
	`
	rows, err := r.db.QueryContext(ctx, query, pq.Array(accountIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]service.OpenAIProbeHealthSnapshot, 0, len(accountIDs))
	for rows.Next() {
		var snap service.OpenAIProbeHealthSnapshot
		var rateLimitedAt *time.Time
		var rescueMarkerJSON *string
		var lpAt *time.Time
		var lpMode *string
		var lpRT *int
		var lpTransportOK *bool
		var lpCorrect *bool
		var lpLen *int
		var lpStatus *int
		if err := rows.Scan(
			&snap.AccountID, &snap.State, &snap.ProbeMode, &snap.Schedulable,
			&snap.ManualPaused, &rateLimitedAt, &snap.Qualification, &rescueMarkerJSON,
			&lpAt, &lpMode, &lpRT, &lpTransportOK, &lpCorrect, &lpLen, &lpStatus,
		); err != nil {
			return nil, err
		}
		snap.RateLimitedAt = rateLimitedAt
		if rescueMarkerJSON != nil {
			// 标记解析容错（malformed → nil=不在区），仓库不因坏 Extra 报错。
			snap.RescueMarker = service.ParseOpenAIRescueLaneMarkerJSON(*rescueMarkerJSON)
		}
		if lpAt != nil {
			ev := &service.OpenAIProbeLastEvidence{
				At:              *lpAt,
				ReasoningTokens: lpRT,
				AnswerCorrect:   lpCorrect,
				TurnStateLen:    derefInt(lpLen),
				HTTPStatus:      derefInt(lpStatus),
			}
			if lpMode != nil {
				ev.Mode = *lpMode
			}
			if lpTransportOK != nil {
				ev.TransportOK = *lpTransportOK
			}
			ev.Degraded = service.OpenAIProbeEvidenceDegraded(ev)
			snap.LastProbe = ev
		}
		out = append(out, snap)
	}
	return out, rows.Err()
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
