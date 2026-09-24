package repository

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// r17ae 票+Cookie 对：cookie_pair 列随票行同进同出；空对落 NULL。
// TicketValue / CookiePair 敏感——断言只对 SQL 形状与参数位，不做日志。

func TestUpsertOpenAICodexTicketPersistsCookiePair(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := &openAIDowngradeProbeRepository{db: db}

	issued := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	ticket := &service.OpenAICodexTicket{
		AccountID: 1115, Model: "gpt-6-astra",
		TicketValue: "t", TicketLen: 332,
		IssuedAt: issued, ExpiresAt: issued.Add(time.Hour),
		ExitFingerprint: "proxy:7", HarvestedMode: "probe",
		CookiePair: "__cflb=cA; __oailb=oA",
	}
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO openai_codex_tickets")).
		WithArgs(int64(1115), "gpt-6-astra", "t", 332, issued,
			issued.Add(time.Hour), "proxy:7", "probe", "__cflb=cA; __oailb=oA").
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := repo.UpsertOpenAICodexTicket(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}

	// 空对落 NULL。
	nullTicket := *ticket
	nullTicket.CookiePair = ""
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO openai_codex_tickets")).
		WithArgs(int64(1115), "gpt-6-astra", "t", 332, issued,
			issued.Add(time.Hour), "proxy:7", "probe", nil).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := repo.UpsertOpenAICodexTicket(context.Background(), &nullTicket); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetOpenAICodexTicketScansCookiePair(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := &openAIDowngradeProbeRepository{db: db}

	issued := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	pair := "__cflb=cB; __oailb=oB"
	rows := sqlmock.NewRows([]string{
		"account_id", "model", "ticket_value", "ticket_len", "issued_at",
		"expires_at", "exit_fingerprint", "harvested_mode", "cookie_pair", "updated_at",
	}).AddRow(int64(1115), "gpt-6-astra", "t", 332, issued,
		issued.Add(time.Hour), "proxy:7", "probe", pair, issued)
	mock.ExpectQuery(regexp.QuoteMeta("FROM openai_codex_tickets")).
		WithArgs(int64(1115), "gpt-6-astra").
		WillReturnRows(rows)
	ticket, err := repo.GetOpenAICodexTicket(context.Background(), 1115, "gpt-6-astra")
	if err != nil {
		t.Fatal(err)
	}
	if ticket.CookiePair != pair {
		t.Fatalf("cookie pair must scan back, got %q", ticket.CookiePair)
	}
	// NULL 列扫描为空串（旧票降级路径）。
	nullRows := sqlmock.NewRows([]string{
		"account_id", "model", "ticket_value", "ticket_len", "issued_at",
		"expires_at", "exit_fingerprint", "harvested_mode", "cookie_pair", "updated_at",
	}).AddRow(int64(1116), "gpt-6-astra", "t", 332, issued,
		issued.Add(time.Hour), "proxy:7", "probe", nil, issued)
	mock.ExpectQuery(regexp.QuoteMeta("FROM openai_codex_tickets")).
		WithArgs(int64(1116), "gpt-6-astra").
		WillReturnRows(nullRows)
	ticket, err = repo.GetOpenAICodexTicket(context.Background(), 1116, "gpt-6-astra")
	if err != nil {
		t.Fatal(err)
	}
	if ticket.CookiePair != "" {
		t.Fatalf("NULL pair must scan as empty, got %q", ticket.CookiePair)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetOpenAICodexTicketPropagatesErrors(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := &openAIDowngradeProbeRepository{db: db}

	mock.ExpectQuery(regexp.QuoteMeta("FROM openai_codex_tickets")).
		WithArgs(int64(1117), "gpt-6-astra").
		WillReturnError(errors.New("db down"))
	if _, err := repo.GetOpenAICodexTicket(context.Background(), 1117, "gpt-6-astra"); err == nil {
		t.Fatal("query error must propagate")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
