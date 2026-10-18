package service

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestOpenAIEnvironmentAudit_ReadOnlyEvidence(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	shared := uuid.NewString()
	accounts := []Account{
		{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, CreatedAt: now,
			Extra: map[string]any{codexFingerprintModeExtraKey: "machine", codexFingerprintSeedExtraKey: shared}},
		{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, CreatedAt: now.Add(2 * time.Second),
			Extra: map[string]any{codexFingerprintModeExtraKey: "machine", codexFingerprintSeedExtraKey: shared}},
		{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeOAuth, CreatedAt: now.Add(3 * time.Second),
			Extra: map[string]any{codexFingerprintModeExtraKey: "off", codexFingerprintSeedExtraKey: uuid.NewString()}},
		{ID: 4, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
			Extra: map[string]any{codexFingerprintModeExtraKey: "machine"}},
		{ID: 5, Platform: PlatformAnthropic, Type: AccountTypeOAuth},
		{ID: 6, Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
	}
	before, err := json.Marshal(accounts)
	require.NoError(t, err)
	report := BuildOpenAIEnvironmentAudit(accounts, now)
	require.Len(t, report.Accounts, 4)
	require.Equal(t, int64(1), report.Accounts[0].AccountID)
	require.Equal(t, "machine", report.Accounts[0].EffectiveMode)
	require.Equal(t, "seed_derived", report.Accounts[0].IdentitySource)
	require.Nil(t, report.Accounts[0].IdentityGeneratedAt, "import time is not seed provenance")
	require.Empty(t, report.Accounts[2].InstallationDigest, "off-mode outgoing identity is not known")
	require.Equal(t, "off", report.Accounts[3].EffectiveMode, "missing seed must not be silently repaired")
	require.Equal(t, []OpenAIEnvironmentAuditGroup{
		{Kind: "duplicate_effective_installation", AccountIDs: []int64{1, 2}},
		{Kind: "duplicate_seed", AccountIDs: []int64{1, 2}},
	}, report.Collisions)
	require.Equal(t, []OpenAIEnvironmentAuditGroup{
		{Kind: "same_import_minute_only", AccountIDs: []int64{1, 2, 3}},
	}, report.ImportBatches)
	serialized, err := json.Marshal(report)
	require.NoError(t, err)
	require.NotContains(t, string(serialized), shared)
	require.NotContains(t, string(serialized), resolveConvergedInstallationID(&accounts[0], shared))
	after, err := json.Marshal(accounts)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Equal(t, report, BuildOpenAIEnvironmentAudit(accounts, now), "stable ordering")
}

func TestOpenAIEnvironmentAudit_ConfiguredDeviceCollision(t *testing.T) {
	device := uuid.NewString()
	accounts := []Account{
		{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
			Extra: map[string]any{"openai_device_id": device, codexFingerprintSeedExtraKey: uuid.NewString()}},
		{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
			Extra: map[string]any{"openai_device_id": device, codexFingerprintSeedExtraKey: uuid.NewString()}},
	}
	report := BuildOpenAIEnvironmentAudit(accounts, time.Now())
	require.Equal(t, "configured_device", report.Accounts[0].IdentitySource)
	require.Equal(t, []OpenAIEnvironmentAuditGroup{
		{Kind: "duplicate_effective_installation", AccountIDs: []int64{1, 2}},
	}, report.Collisions)
	require.Empty(t, report.ImportBatches)
}

func TestOpenAIEnvironmentAudit_EmptyCollections(t *testing.T) {
	body, err := json.Marshal(BuildOpenAIEnvironmentAudit(nil, time.Now()))
	require.NoError(t, err)
	require.Contains(t, string(body), `"accounts":[]`)
	require.Contains(t, string(body), `"collisions":[]`)
	require.Contains(t, string(body), `"import_batches":[]`)
}

func TestOpenAIEnvironmentAudit_LoginIPMatchesAuthorizationValidation(t *testing.T) {
	tests := []struct {
		name     string
		extra    map[string]any
		recorded bool
	}{
		{name: "missing"},
		{name: "empty", extra: map[string]any{OpenAIOAuthLoginExitIPExtraKey: ""}},
		{name: "whitespace", extra: map[string]any{OpenAIOAuthLoginExitIPExtraKey: " \t "}},
		{name: "malformed", extra: map[string]any{OpenAIOAuthLoginExitIPExtraKey: "invalid-ip"}},
		{name: "private", extra: map[string]any{OpenAIOAuthLoginExitIPExtraKey: "10.0.0.1"}},
		{name: "loopback", extra: map[string]any{OpenAIOAuthLoginExitIPExtraKey: "127.0.0.1"}},
		{name: "link_local", extra: map[string]any{OpenAIOAuthLoginExitIPExtraKey: "169.254.1.2"}},
		{name: "unspecified", extra: map[string]any{OpenAIOAuthLoginExitIPExtraKey: "::"}},
		{name: "zoned", extra: map[string]any{OpenAIOAuthLoginExitIPExtraKey: "2001:db8::1%en0"}},
		{name: "non_string", extra: map[string]any{OpenAIOAuthLoginExitIPExtraKey: 12345}},
		{name: "mapped_private", extra: map[string]any{OpenAIOAuthLoginExitIPExtraKey: "::ffff:10.0.0.1"}},
		{name: "ipv4", extra: map[string]any{OpenAIOAuthLoginExitIPExtraKey: "198.51.100.25"}, recorded: true},
		{name: "ipv6", extra: map[string]any{OpenAIOAuthLoginExitIPExtraKey: "2001:db8::1"}, recorded: true},
		{name: "mapped_ipv4", extra: map[string]any{OpenAIOAuthLoginExitIPExtraKey: "::ffff:198.51.100.25"}, recorded: true},
		{name: "trimmed", extra: map[string]any{OpenAIOAuthLoginExitIPExtraKey: " 198.51.100.25 "}, recorded: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			accounts := []Account{{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: tt.extra}}
			before, err := json.Marshal(accounts)
			require.NoError(t, err)

			report := BuildOpenAIEnvironmentAudit(accounts, time.Now())
			require.Len(t, report.Accounts, 1)
			require.Equal(t, tt.recorded, report.Accounts[0].LoginIPRecorded)
			_, authorizationErr := OpenAIOAuthLoginExitIP(&accounts[0])
			require.Equal(t, authorizationErr == nil, report.Accounts[0].LoginIPRecorded)

			after, err := json.Marshal(accounts)
			require.NoError(t, err)
			require.Equal(t, before, after, "audit must not repair or replace login IP evidence")
			body, err := json.Marshal(report)
			require.NoError(t, err)
			if raw, ok := tt.extra[OpenAIOAuthLoginExitIPExtraKey].(string); ok && len(raw) > 3 {
				require.NotContains(t, string(body), raw, "audit must not expose login IP")
			}
		})
	}
}
