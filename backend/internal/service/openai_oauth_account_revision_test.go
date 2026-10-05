package service

import (
	"maps"
	"strings"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/stretchr/testify/require"
)

func TestOpenAIOAuthAccountRevisionIgnoresRuntimeButBindsIdentity(t *testing.T) {
	_, account, _, _, _ := reauthorizationIPFixture(t)
	account.Credentials = map[string]any{
		"access_token": "fixture", "_token_version": int64(123),
		"model_mapping": map[string]any{"fixture-model": "fixture-model"},
	}
	baseline := OpenAIOAuthAccountRevision(account)
	require.True(t, validOpenAIOAuthAccountRevision(baseline))
	require.NotContains(t, baseline, "fixture")
	for _, change := range []string{
		"runtime", "JSON representation", "token", "token version", "model mapping",
		"id", "platform", "type", "parent", "proxy", "qualification", "login IP", "unserializable",
	} {
		t.Run(change, func(t *testing.T) {
			next := *account
			next.Credentials = maps.Clone(account.Credentials)
			next.Extra = maps.Clone(account.Extra)
			switch change {
			case "runtime":
				now := time.Now()
				next.UpdatedAt = now.Add(time.Second)
				next.LastUsedAt = &now
				next.Name = "renamed"
				next.Status, next.ErrorMessage = StatusError, "expired"
				next.Schedulable = false
				next.Extra["openai_rescue_probe_at"] = now
				next.Extra["model_rate_limits"] = map[string]any{"fixture": now}
			case "JSON representation":
				next.Credentials["_token_version"] = float64(123)
			case "token":
				next.Credentials["access_token"] = "newer-fixture"
			case "token version":
				next.Credentials["_token_version"] = 124
			case "model mapping":
				next.Credentials["model_mapping"] = map[string]any{}
			case "id":
				next.ID++
			case "platform":
				next.Platform = PlatformAnthropic
			case "type":
				next.Type = AccountTypeAPIKey
			case "parent":
				parent := int64(1)
				next.ParentAccountID = &parent
			case "proxy":
				proxy := *account.ProxyID + 1
				next.ProxyID = &proxy
			case "qualification":
				next.Extra[OpenAIOAuthQualifiedProxyExtraKey] = int64(999)
			case "login IP":
				next.Extra[OpenAIOAuthLoginExitIPExtraKey] = "198.51.100.26"
			case "unserializable":
				next.Credentials["invalid"] = make(chan int)
			}
			if change == "runtime" || change == "JSON representation" {
				require.Equal(t, baseline, OpenAIOAuthAccountRevision(&next))
			} else {
				require.NotEqual(t, baseline, OpenAIOAuthAccountRevision(&next))
			}
		})
	}
}

func TestReauthorizationGenerationUsesCapturedIdentityAcrossRuntimeUpdate(t *testing.T) {
	for _, change := range []string{"runtime", "credentials", "invalid revision", "legacy timestamp"} {
		t.Run(change, func(t *testing.T) {
			svc, account, _, _, _ := reauthorizationIPFixture(t)
			stamp := account.UpdatedAt
			revision := OpenAIOAuthAccountRevision(account)
			account.UpdatedAt = stamp.Add(time.Second)
			switch change {
			case "credentials":
				account.Credentials = map[string]any{"access_token": "newer-fixture"}
			case "invalid revision":
				revision = "invalid"
			case "legacy timestamp":
				revision = ""
			}
			result, err := svc.GenerateReauthorizationAuthURL(t.Context(), account.ID,
				stamp.Format(time.RFC3339Nano), account.ProxyID, "", PlatformOpenAI, revision)
			if change != "runtime" {
				require.ErrorIs(t, err, ErrOAuthReauthorizationStale)
				require.Nil(t, result)
				return
			}
			require.NoError(t, err)
			session, err := svc.sessionStore.Get(t.Context(), result.SessionID)
			require.NoError(t, err)
			require.Equal(t, stamp.UTC().Format(time.RFC3339Nano), session.ReauthorizationRevision)
			require.Equal(t, revision, session.ReauthorizationAccountRevision)
		})
	}
}

func TestReauthorizationProofSurvivesRuntimeUpdatesAtEveryBoundary(t *testing.T) {
	svc, account, result, credentials := reauthorizationProofFixture(t)
	stamp := account.UpdatedAt
	account.UpdatedAt = stamp.Add(time.Second)
	ctx, err := svc.ConsumeReauthorizationProof(t.Context(), result.ReauthorizationProof,
		account.ID, stamp, credentials)
	require.NoError(t, err)
	account.UpdatedAt = stamp.Add(2 * time.Second)
	require.NoError(t, ValidateOAuthReauthorizationUpdate(ctx, account, stamp, credentials))
	require.ErrorIs(t, ValidateOAuthReauthorizationUpdate(ctx, account, account.UpdatedAt, credentials),
		ErrOpenAIOAuthReauthorizationProofRequired)
	require.ErrorIs(t, ValidateOAuthReauthorizationUpdate(t.Context(), account, stamp, credentials),
		ErrOAuthReauthorizationStale)
	account.Credentials = map[string]any{"access_token": "newer-fixture"}
	require.ErrorIs(t, ValidateOAuthReauthorizationUpdate(ctx, account, stamp, credentials),
		ErrOAuthReauthorizationStale)
}

func TestReauthorizationLegacySessionsRemainStrict(t *testing.T) {
	svc, account, session, _, _ := reauthorizationIPFixture(t)
	session.ReauthorizationAccountRevision = ""
	require.NoError(t, svc.validateReauthorizationSession(t.Context(), session, false))
	account.UpdatedAt = account.UpdatedAt.Add(time.Second)
	require.ErrorIs(t, svc.validateReauthorizationSession(t.Context(), session, false), ErrOAuthReauthorizationStale)
}

func TestReauthorizationSessionAccountRevisionDecoding(t *testing.T) {
	_, _, session, _, _ := reauthorizationIPFixture(t)
	raw := map[string]any{
		"proxy_id": "7", "created_at": session.CreatedAt.UTC().Format(time.RFC3339Nano),
		"reauthorization_account_id":       "42",
		"reauthorization_revision":         session.ReauthorizationRevision,
		"reauthorization_exit_ip":          session.ReauthorizationExitIP,
		"reauthorization_account_revision": session.ReauthorizationAccountRevision,
	}
	row := &dbent.PendingAuthSession{LocalFlowState: map[string]any{"openai_oauth": raw}}
	decoded, err := decodeOpenAIOAuthSession(row)
	require.NoError(t, err)
	require.Equal(t, session.ReauthorizationAccountRevision, decoded.ReauthorizationAccountRevision)
	for _, invalid := range []any{nil, 123, "invalid", "oauth-v1:" + strings.Repeat("A", 64)} {
		raw["reauthorization_account_revision"] = invalid
		_, err := decodeOpenAIOAuthSession(row)
		require.ErrorIs(t, err, ErrOpenAIOAuthSessionInvalid)
	}
	delete(raw, "reauthorization_account_revision")
	decoded, err = decodeOpenAIOAuthSession(row)
	require.NoError(t, err)
	require.Empty(t, decoded.ReauthorizationAccountRevision)
}
