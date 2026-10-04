package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// Simulate only the service's consumed-proof context at the repository boundary.
// Proof issuance, one-shot consumption and IP observation are service tests.
type reauthProofContext struct {
	context.Context
	proof service.OpenAIOAuthSession
}

func (c reauthProofContext) Value(key any) any {
	kind := reflect.TypeOf(key)
	if kind != nil && kind.PkgPath() == "github.com/Wei-Shaw/sub2api/internal/service" &&
		kind.Name() == "openAIReauthorizationProofKey" {
		return c.proof
	}
	return c.Context.Value(key)
}

func reauthCommitContext(t *testing.T, stamp time.Time, credentials map[string]any) reauthProofContext {
	t.Helper()
	values := make(map[string]string)
	for _, key := range []string{"access_token", "refresh_token", "id_token", "client_id", "email",
		"chatgpt_account_id", "chatgpt_user_id", "organization_id", "auth_mode", "openai_auth_mode"} {
		values[key], _ = credentials[key].(string)
	}
	raw, err := json.Marshal(values)
	require.NoError(t, err)
	hash := sha256.Sum256(raw)
	return reauthProofContext{Context: t.Context(), proof: service.OpenAIOAuthSession{
		ReauthorizationAccountID: 71, ProxyID: 7,
		ReauthorizationRevision:         stamp.UTC().Format(time.RFC3339Nano),
		ReauthorizationCredentialsHash:  hex.EncodeToString(hash[:]),
		ReauthorizationBrowserSessionID: "0123456789abcdef0123456789abcdef",
		ReauthorizationExitIP:           "198.51.100.25",
		CreatedAt:                       time.Now().UTC(),
	}}
}

func reauthHistoricalExtra(t *testing.T, raw string) string {
	t.Helper()
	var extra map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &extra))
	extra[service.OpenAIOAuthLoginExitIPExtraKey] = "198.51.100.25"
	result, err := json.Marshal(extra)
	require.NoError(t, err)
	return string(result)
}

func TestApplyOAuthCredentialsRequiresBoundProofUnderLock(t *testing.T) {
	for _, mutation := range []string{"missing", "wrong account", "wrong revision", "proxy changed",
		"IP changed", "expired", "future", "tampered credentials"} {
		t.Run(mutation, func(t *testing.T) {
			repo, mock := atomicCreateRepository(t)
			stamp := time.Now().UTC().Truncate(time.Microsecond)
			credentials := map[string]any{"access_token": "fixture"}
			ctx := reauthCommitContext(t, stamp, credentials)
			switch mutation {
			case "wrong account":
				ctx.proof.ReauthorizationAccountID++
			case "wrong revision":
				ctx.proof.ReauthorizationRevision = stamp.Add(time.Microsecond).Format(time.RFC3339Nano)
			case "proxy changed":
				ctx.proof.ProxyID++
			case "IP changed":
				ctx.proof.ReauthorizationExitIP = "198.51.100.26"
			case "expired":
				ctx.proof.CreatedAt = time.Now().Add(-time.Hour)
			case "future":
				ctx.proof.CreatedAt = time.Now().Add(time.Minute)
			case "tampered credentials":
				credentials["access_token"] = "fixture-replaced"
			}
			var requestContext context.Context = ctx
			if mutation == "missing" {
				requestContext = t.Context()
			}
			mock.ExpectBegin()
			mock.ExpectQuery(`(?s)SELECT id, platform, type, credentials, extra, proxy_id, parent_account_id,.*FOR NO KEY UPDATE`).
				WithArgs(int64(71)).
				WillReturnRows(sqlmock.NewRows([]string{
					"id", "platform", "type", "credentials", "extra", "proxy_id",
					"parent_account_id", "status", "schedulable", "updated_at",
				}).AddRow(71, service.PlatformOpenAI, service.AccountTypeOAuth,
					`{"email":"account@example.test"}`, reauthHistoricalExtra(t, `{}`),
					7, nil, service.StatusError, false, stamp))
			mock.ExpectRollback()
			got, err := repo.ApplyOAuthCredentials(requestContext, 71, stamp,
				service.AccountTypeOAuth, credentials, nil)
			require.Nil(t, got)
			if mutation == "IP changed" {
				require.ErrorIs(t, err, service.ErrOpenAIOAuthLoginIPChanged)
			} else {
				require.ErrorIs(t, err, service.ErrOpenAIOAuthReauthorizationProofRequired)
			}
		})
	}
}
