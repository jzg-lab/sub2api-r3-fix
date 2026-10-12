package service

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pluginDirRepoStub embeds AccountRepository so it satisfies the full interface;
// only ListByPlatform is implemented (the sole method the directory listing uses).
// Any other call panics, which keeps the test honest about the surface it touches.
type pluginDirRepoStub struct {
	AccountRepository
	byPlatform map[string][]Account
}

func (r *pluginDirRepoStub) ListByPlatform(_ context.Context, platform string) ([]Account, error) {
	return r.byPlatform[platform], nil
}

func TestListPluginAccounts_ScopeAndSchedulable(t *testing.T) {
	future := time.Now().Add(time.Hour)
	parentID := int64(1)
	openai := []Account{
		// schedulable active oauth. Credentials hold secrets (stripped); Extra is
		// released.
		{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
			Name:        "primary",
			Credentials: map[string]any{"access_token": "SECRET-TOKEN", "refresh_token": "SECRET-REFRESH"},
			Extra:       map[string]any{"existing_key": "ek-value", "openai_compact_mode": "auto"}},
		// active but temp-unschedulable (paused) — status stays active, must still be
		// returned, but not schedulable
		{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
			TempUnschedulableUntil: &future, TempUnschedulableReason: "429 from upstream"},
		// active but rate-limited (429) — status stays active, returned, not schedulable
		{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
			RateLimitResetAt: &future},
		// shadow oauth — excluded entirely
		{ID: 4, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
			ParentAccountID: &parentID},
		// apikey type — out of (openai, oauth) scope, excluded
		{ID: 5, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true},
	}
	svc := &OpenAIGatewayService{accountRepo: &pluginDirRepoStub{byPlatform: map[string][]Account{PlatformOpenAI: openai}}}
	scope := newPluginAccountScope(pluginAccountScopeEntry{Platform: PlatformOpenAI, AccountType: AccountTypeOAuth})

	infos, err := svc.ListPluginAccounts(context.Background(), scope, "", "")
	require.NoError(t, err)

	got := map[int64]PluginAccountInfo{}
	for _, info := range infos {
		got[info.ID] = info
	}
	// 1,2,3 in scope; 4 shadow and 5 apikey excluded.
	require.Len(t, infos, 3)
	assert.Contains(t, got, int64(1))
	assert.Contains(t, got, int64(2))
	assert.Contains(t, got, int64(3))
	assert.NotContains(t, got, int64(4), "shadow account must be excluded")
	assert.NotContains(t, got, int64(5), "out-of-scope account type must be excluded")

	// Host-authoritative schedulable decision.
	assert.True(t, got[1].Schedulable, "active oauth is schedulable")
	assert.False(t, got[2].Schedulable, "temp-unschedulable account is not schedulable")
	assert.False(t, got[3].Schedulable, "rate-limited account is not schedulable")

	// Metadata carries readable info (incl. Extra) but NEVER the raw Credentials blob.
	meta := string(got[1].MetadataJSON)
	assert.NotContains(t, meta, "SECRET-TOKEN", "credentials must never appear in metadata")
	assert.NotContains(t, meta, "SECRET-REFRESH", "credentials must never appear in metadata")
	assert.Contains(t, meta, "ek-value", "Extra is intentionally released")
	assert.Contains(t, meta, "openai_compact_mode", "Extra is intentionally released")
	assert.Contains(t, meta, "primary", "readable name must be present in metadata")
	assert.Contains(t, string(got[2].MetadataJSON), "429 from upstream", "pause reason must be readable in metadata")
}

// TestAccountReadableSnapshot_DenylistTripwire fails whenever a new EXPORTED field
// is added to Account without being classified as either safe-to-expose or
// stripped by accountReadableSnapshotJSON. Because the snapshot is a denylist, a
// newly added secret-bearing field would otherwise silently ship to plugins. When
// this test fails: add the field to `stripped` (and zero it in
// accountReadableSnapshotJSON) if it can hold secrets/heavy data, otherwise add it
// to `safeToExpose`.
func TestAccountReadableSnapshot_DenylistTripwire(t *testing.T) {
	// Fields the snapshot intentionally strips. Credentials = long-lived secret
	// (refresh_token) not handed out by ResolveOutboundIdentity. Groups/AccountGroups
	// = relational graphs with back-references that would cycle under encoding/json.
	stripped := map[string]struct{}{
		"Credentials": {}, "Groups": {}, "AccountGroups": {},
	}
	// Readable metadata includes Extra and a proxy copy without its password.
	safeToExpose := map[string]struct{}{
		"ID": {}, "Name": {}, "Notes": {}, "Platform": {}, "Type": {}, "Extra": {},
		"Proxy": {}, "ProxyID": {}, "ProxyFallbackOriginID": {}, "ProxyFallbackOriginName": {},
		"Concurrency": {}, "Priority": {}, "RateMultiplier": {}, "LoadFactor": {},
		"Status": {}, "ErrorMessage": {}, "LastUsedAt": {}, "ExpiresAt": {},
		"AutoPauseOnExpired": {}, "CreatedAt": {}, "UpdatedAt": {}, "Schedulable": {},
		"RateLimitedAt": {}, "RateLimitResetAt": {}, "OverloadUntil": {},
		"TempUnschedulableUntil": {}, "TempUnschedulableReason": {},
		"SessionWindowStart": {}, "SessionWindowEnd": {}, "SessionWindowStatus": {},
		"ParentAccountID": {}, "QuotaDimension": {}, "GroupIDs": {},
	}
	tp := reflect.TypeOf(Account{})
	for i := 0; i < tp.NumField(); i++ {
		f := tp.Field(i)
		if f.PkgPath != "" {
			continue // unexported: never marshaled by encoding/json
		}
		_, isStripped := stripped[f.Name]
		_, isSafe := safeToExpose[f.Name]
		if !isStripped && !isSafe {
			t.Fatalf("Account.%s is a new exported field not classified for the plugin snapshot: "+
				"add it to accountReadableSnapshotJSON's denylist (if it holds secrets or is a "+
				"cyclic/heavy relation) or to safeToExpose (if it is non-secret readable metadata)", f.Name)
		}
	}

	// The raw Credentials blob must never serialize; Extra and the proxy ARE released.
	acct := &Account{
		ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
		Credentials: map[string]any{"access_token": "AT", "refresh_token": "LEAK-REFRESH"},
		Extra:       map[string]any{"opaque": "extra-released"},
		Proxy:       &Proxy{Host: "host", Port: 1, Username: "user", Password: "pw-released"},
	}
	snap := accountReadableSnapshotJSON(acct)
	require.NotNil(t, snap)
	var m map[string]any
	require.NoError(t, json.Unmarshal(snap, &m))
	assert.NotContains(t, string(snap), "LEAK-REFRESH", "raw Credentials must never appear in metadata")
	assert.Contains(t, string(snap), "extra-released", "Extra is intentionally released")
	assert.NotContains(t, string(snap), "pw-released", "metadata must strip proxy passwords")
	assert.Equal(t, "pw-released", acct.Proxy.Password, "source proxy is unchanged")

	// Cycle safety: a populated Groups/AccountGroups back-reference cycle must NOT
	// crash json.Marshal (encoding/json does not detect cycles). Stripping them
	// guarantees the snapshot still returns valid JSON instead of stack-overflowing.
	g := &Group{ID: 7, Name: "g7"}
	ag := AccountGroup{GroupID: 7, Group: g, Account: acct}
	g.AccountGroups = []AccountGroup{ag} // g -> ag -> g  (and ag -> acct -> ...)
	acct.Groups = []*Group{g}
	acct.AccountGroups = []AccountGroup{ag}
	cyc := accountReadableSnapshotJSON(acct)
	require.NotNil(t, cyc, "snapshot must survive a cyclic Groups/AccountGroups graph")
	require.NoError(t, json.Unmarshal(cyc, &m))

	// Shallow-copy safety: the source account must be untouched.
	assert.NotNil(t, acct.Credentials, "snapshot must not mutate the source account")
	assert.NotNil(t, acct.Proxy, "snapshot must not mutate the source account")
	assert.Len(t, acct.Groups, 1, "snapshot must not mutate the source account's Groups")
}

func TestListPluginAccounts_ExcludesNonActiveDefenseInDepth(t *testing.T) {
	// Even if a repo were to return a non-active account, the directory must not
	// expose it (defense-in-depth beyond ListByPlatform's DB filter).
	svc := &OpenAIGatewayService{accountRepo: &pluginDirRepoStub{byPlatform: map[string][]Account{
		PlatformOpenAI: {
			{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true},
			{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusDisabled},
			{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusError},
		},
	}}}
	scope := newPluginAccountScope(pluginAccountScopeEntry{Platform: PlatformOpenAI, AccountType: AccountTypeOAuth})
	infos, err := svc.ListPluginAccounts(context.Background(), scope, "", "")
	require.NoError(t, err)
	require.Len(t, infos, 1)
	assert.Equal(t, int64(1), infos[0].ID)
}

func TestListPluginAccounts_EmptyScopeReturnsNothing(t *testing.T) {
	svc := &OpenAIGatewayService{accountRepo: &pluginDirRepoStub{byPlatform: map[string][]Account{
		PlatformOpenAI: {{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true}},
	}}}
	infos, err := svc.ListPluginAccounts(context.Background(), PluginAccountScope{}, "", "")
	require.NoError(t, err)
	assert.Empty(t, infos, "an empty scope must never enumerate accounts")
}

func (r *pluginDirRepoStub) GetByID(_ context.Context, id int64) (*Account, error) {
	for _, accounts := range r.byPlatform {
		for i := range accounts {
			if accounts[i].ID == id {
				account := accounts[i]
				return &account, nil
			}
		}
	}
	return nil, nil
}

func TestPluginIdentityHonorsLocalGuards(t *testing.T) {
	for _, tc := range []string{"healthy", "unbound", "paused", "terminated", "cooldown", "invalid_proxy", "revoked_during_lookup"} {
		t.Run(tc, func(t *testing.T) {
			account := Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
				Credentials: map[string]any{"access_token": "token", "chatgpt_account_id": "upstream", "user_agent": "codex_cli_rs/0.151.0 (Mac OS 15.1; arm64)"}, Extra: map[string]any{"codex_fingerprint_mode": "device", "codex_fingerprint_seed": testCodexFingerprintSeed}}
			scope := newPluginAccountScope(pluginAccountScopeEntry{Platform: PlatformOpenAI, AccountType: AccountTypeOAuth})
			allowedCalls := 0
			scope.accountAllowed = func(int64) bool {
				allowedCalls++
				return tc != "unbound" && (tc != "revoked_during_lookup" || allowedCalls == 1)
			}
			scope.pausedAccounts = func(context.Context, []int64) (map[int64]bool, error) { return map[int64]bool{1: tc == "paused"}, nil }
			switch tc {
			case "terminated":
				account.Extra = map[string]any{OpenAIRescueTerminatedAtExtraKey: "now"}
			case "cooldown":
				future := time.Now().Add(time.Hour)
				account.RateLimitResetAt = &future
			case "invalid_proxy":
				id := int64(99)
				account.ProxyID = &id
			}
			svc := &OpenAIGatewayService{accountRepo: &pluginDirRepoStub{byPlatform: map[string][]Account{PlatformOpenAI: {account}}}}
			identity, err := svc.ResolvePluginOutboundIdentity(context.Background(), scope, 1)
			if tc == "invalid_proxy" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if tc == "healthy" {
				require.NotNil(t, identity)
				require.Equal(t, "token", identity.Token)
				require.Equal(t, "upstream", identity.Headers.Get("chatgpt-account-id"))
				require.Contains(t, identity.Headers.Get("User-Agent"), "Mac OS 15.1")
				ids := resolveCodexFingerprintIDsFromRequest(&account, nil)
				require.NotNil(t, ids)
				require.Equal(t, ids.installationID, identity.Headers.Get("x-codex-installation-id"))
			} else {
				require.Nil(t, identity)
			}
		})
	}
}

func TestPluginMetadataPauseMatchesTypedStatus(t *testing.T) {
	svc := &OpenAIGatewayService{accountRepo: &pluginDirRepoStub{byPlatform: map[string][]Account{PlatformOpenAI: {
		{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true},
		{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true},
	}}}}
	scope := newPluginAccountScope(pluginAccountScopeEntry{Platform: PlatformOpenAI, AccountType: AccountTypeOAuth})
	scope.accountAllowed = func(id int64) bool { return id == 1 }
	scope.pausedAccounts = func(context.Context, []int64) (map[int64]bool, error) { return map[int64]bool{1: true}, nil }
	infos, err := svc.ListPluginAccounts(context.Background(), scope, "", "")
	require.NoError(t, err)
	require.Len(t, infos, 1)
	require.False(t, infos[0].Schedulable)
	var snapshot Account
	require.NoError(t, json.Unmarshal(infos[0].MetadataJSON, &snapshot))
	require.False(t, snapshot.Schedulable)
}
