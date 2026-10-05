package repository

import (
	"context"
	"database/sql"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOpenAIProbePostgresInconclusiveAnswer(t *testing.T) {
	db := newProbePostgres(t)
	seedProbePostgres(t, db)
	repo := &openAIDowngradeProbeRepository{db: db}
	proxyID, tokens := int64(3), 1500
	result := &service.OpenAIDowngradeProbeResult{
		AccountID: 7, ProxyID: &proxyID, TransportOK: true, HTTPStatus: 200,
		AnswerInconclusive: true, ReasoningTokens: &tokens,
	}
	require.NoError(t, repo.RecordOpenAIDowngradeProbe(context.Background(), result))
	var transport bool
	var verdict sql.NullBool
	require.NoError(t, db.QueryRow(`SELECT transport_ok, answer_correct
		FROM openai_downgrade_probe_results ORDER BY id DESC LIMIT 1`).Scan(&transport, &verdict))
	require.True(t, transport)
	require.False(t, verdict.Valid)
	_, err := db.Exec(`INSERT INTO openai_downgrade_probe_results
		(account_id, transport_ok, answer_correct) VALUES (7, FALSE, FALSE)`)
	require.ErrorContains(t, err, "openai_downgrade_probe_results_verdict_check")
}
