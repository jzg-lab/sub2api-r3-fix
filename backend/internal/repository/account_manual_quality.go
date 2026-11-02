package repository

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

var _ service.ManualQualityStore = (*accountRepository)(nil)

func (r *accountRepository) BeginManualQuality(ctx context.Context, result *service.ManualQualityResult) (bool, error) {
	data, err := json.Marshal(result)
	if err != nil {
		return false, err
	}
	res, err := r.sql.ExecContext(ctx, `INSERT INTO account_manual_quality_tests
 (account_id, model, kind, run_id, started_at, running_until, result)
 SELECT id, $2, $3, $4, NOW(), NOW() + INTERVAL '11 minutes', $5::jsonb
 FROM accounts WHERE id = $1 AND deleted_at IS NULL
 ON CONFLICT (account_id, model, kind) DO UPDATE
 SET run_id = EXCLUDED.run_id, started_at = EXCLUDED.started_at,
 running_until = EXCLUDED.running_until, result = EXCLUDED.result
 WHERE account_manual_quality_tests.running_until IS NULL OR account_manual_quality_tests.running_until < NOW()`,
		result.AccountID, result.Model, result.Kind, result.RunID, string(data))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (r *accountRepository) FinishManualQuality(ctx context.Context, result *service.ManualQualityResult) error {
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	res, err := r.sql.ExecContext(ctx, `UPDATE account_manual_quality_tests SET result = $5::jsonb, running_until = NULL
 WHERE account_id = $1 AND model = $2 AND kind = $3 AND run_id = $4`, result.AccountID, result.Model, result.Kind, result.RunID, string(data))
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return errors.New("quality test result was superseded")
	}
	return err
}

// An expired in-flight run is unknown, never an automatic retry or a success.
const manualQualityResultSQL = `CASE WHEN t.running_until < NOW() AND t.result->>'status' = 'running'
 THEN t.result || '{"status":"unknown","error":"The test was interrupted; its upstream outcome is unknown."}'::jsonb
 ELSE t.result END`

func (r *accountRepository) GetManualQuality(ctx context.Context, accountID int64, model, kind string) (*service.ManualQualityResult, error) {
	rows, err := r.sql.QueryContext(ctx, `SELECT `+manualQualityResultSQL+` FROM account_manual_quality_tests t
 JOIN accounts a ON a.id = t.account_id AND a.deleted_at IS NULL
 WHERE t.account_id = $1 AND t.model = $2 AND t.kind = $3`, accountID, model, kind)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return nil, rows.Err()
	}
	var data []byte
	if err := rows.Scan(&data); err != nil {
		return nil, err
	}
	var result service.ManualQualityResult
	err = json.Unmarshal(data, &result)
	return &result, err
}

func (r *accountRepository) ListManualQuality(ctx context.Context, ids []int64) ([]service.ManualQualityResult, error) {
	rows, err := r.sql.QueryContext(ctx, `SELECT DISTINCT ON (t.account_id)
 (`+manualQualityResultSQL+`) - 'answer' - 'prompt' - 'reference_answer' - 'parameters' - 'usage'
 FROM account_manual_quality_tests t JOIN accounts a ON a.id = t.account_id AND a.deleted_at IS NULL
 WHERE t.account_id = ANY($1) ORDER BY t.account_id, t.started_at DESC`, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []service.ManualQualityResult{}
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var result service.ManualQualityResult
		if err := json.Unmarshal(data, &result); err != nil {
			return nil, err
		}
		out = append(out, result)
	}
	return out, rows.Err()
}
