package repository

import (
	"context"
	"database/sql"
	"time"

	"github.com/lib/pq"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// openai_codex_tickets 票表 CRUD（相位B）。TicketValue 敏感凭据：本层不做
// 日志输出；导出/管理端查询一律走脱敏视图（管理端只回长度+时刻+指纹）。

// NewOpenAICodexTicketStore 从探针 repo 取票能力窄视图（同一底层数据库）。
// 票表与探针状态同库同 repo；gateway 侧注入用（探针侧经类型断言取得）。
func NewOpenAICodexTicketStore(db *sql.DB) service.OpenAICodexTicketStore {
	return &openAIDowngradeProbeRepository{db: db}
}
func (r *openAIDowngradeProbeRepository) UpsertOpenAICodexTicket(
	ctx context.Context, ticket *service.OpenAICodexTicket,
) error {
	if ticket == nil {
		return nil
	}
	query := `
		INSERT INTO openai_codex_tickets
			(account_id, model, ticket_value, ticket_len, issued_at, expires_at,
			 exit_fingerprint, harvested_mode, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now(), now())
		ON CONFLICT (account_id, model) DO UPDATE SET
			ticket_value = EXCLUDED.ticket_value,
			ticket_len = EXCLUDED.ticket_len,
			issued_at = EXCLUDED.issued_at,
			expires_at = EXCLUDED.expires_at,
			exit_fingerprint = EXCLUDED.exit_fingerprint,
			harvested_mode = EXCLUDED.harvested_mode,
			updated_at = now()
	`
	_, err := r.db.ExecContext(ctx, query,
		ticket.AccountID, ticket.Model, ticket.TicketValue, ticket.TicketLen,
		ticket.IssuedAt, ticket.ExpiresAt, ticket.ExitFingerprint, ticket.HarvestedMode)
	return err
}

func (r *openAIDowngradeProbeRepository) GetOpenAICodexTicket(
	ctx context.Context, accountID int64, model string,
) (*service.OpenAICodexTicket, error) {
	query := `
		SELECT account_id, model, ticket_value, ticket_len, issued_at,
		       expires_at, exit_fingerprint, harvested_mode, updated_at
		FROM openai_codex_tickets
		WHERE account_id = $1 AND model = $2
	`
	rows, err := r.db.QueryContext(ctx, query, accountID, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	var ticket service.OpenAICodexTicket
	if err := rows.Scan(
		&ticket.AccountID, &ticket.Model, &ticket.TicketValue, &ticket.TicketLen,
		&ticket.IssuedAt, &ticket.ExpiresAt, &ticket.ExitFingerprint,
		&ticket.HarvestedMode, &ticket.UpdatedAt); err != nil {
		return nil, err
	}
	return &ticket, rows.Err()
}

// ListOpenAICodexTicketsByAccounts 批量拉账号的全部票行（管理端脱敏视图用）。
func (r *openAIDowngradeProbeRepository) ListOpenAICodexTicketsByAccounts(
	ctx context.Context, accountIDs []int64,
) ([]service.OpenAICodexTicket, error) {
	if len(accountIDs) == 0 {
		return nil, nil
	}
	query := `
		SELECT account_id, model, ticket_value, ticket_len, issued_at,
		       expires_at, exit_fingerprint, harvested_mode, updated_at
		FROM openai_codex_tickets
		WHERE account_id = ANY($1)
		ORDER BY account_id, model
	`
	rows, err := r.db.QueryContext(ctx, query, pq.Array(accountIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]service.OpenAICodexTicket, 0, len(accountIDs))
	for rows.Next() {
		var t service.OpenAICodexTicket
		if err := rows.Scan(
			&t.AccountID, &t.Model, &t.TicketValue, &t.TicketLen,
			&t.IssuedAt, &t.ExpiresAt, &t.ExitFingerprint,
			&t.HarvestedMode, &t.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r *openAIDowngradeProbeRepository) DeleteExpiredOpenAICodexTickets(
	ctx context.Context, now time.Time,
) (int64, error) {
	result, err := r.db.ExecContext(ctx,
		`DELETE FROM openai_codex_tickets WHERE expires_at < $1`, now)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
