package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newCyberBlockTestCtx(headers map[string]string, body string) (*gin.Context, []byte) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest("POST", "/openai/v1/responses", strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	c.Request = req
	return c, []byte(body)
}

func TestCyberSessionExplicitBlockKey(t *testing.T) {
	c1, b1 := newCyberBlockTestCtx(map[string]string{"session_id": "sess-abc"}, `{}`)
	k1 := CyberSessionExplicitBlockKey(101, c1, b1)
	require.NotEmpty(t, k1)

	// Same session, different apiKey → different key (isolation).
	c2, b2 := newCyberBlockTestCtx(map[string]string{"session_id": "sess-abc"}, `{}`)
	require.NotEqual(t, k1, CyberSessionExplicitBlockKey(202, c2, b2))

	// Same session + same apiKey → stable key.
	c3, b3 := newCyberBlockTestCtx(map[string]string{"session_id": "sess-abc"}, `{}`)
	require.Equal(t, k1, CyberSessionExplicitBlockKey(101, c3, b3))

	// prompt_cache_key in body counts as explicit.
	c4, b4 := newCyberBlockTestCtx(nil, `{"prompt_cache_key":"pck-1"}`)
	require.NotEmpty(t, CyberSessionExplicitBlockKey(101, c4, b4))

	// No explicit signal → empty key → caller must skip blocking entirely.
	c5, b5 := newCyberBlockTestCtx(nil, `{"input":"hello world"}`)
	require.Empty(t, CyberSessionExplicitBlockKey(101, c5, b5))

	// conversation_id header counts as explicit; key is stable and non-empty.
	c6, b6 := newCyberBlockTestCtx(map[string]string{"conversation_id": "conv-xyz"}, `{}`)
	k6 := CyberSessionExplicitBlockKey(101, c6, b6)
	require.NotEmpty(t, k6)
	c6b, b6b := newCyberBlockTestCtx(map[string]string{"conversation_id": "conv-xyz"}, `{}`)
	require.Equal(t, k6, CyberSessionExplicitBlockKey(101, c6b, b6b), "conversation_id key must be stable")
}

func TestCyberSessionUserKeysHaveIndependentAuthenticatedNamespace(t *testing.T) {
	require.Equal(t, []string{"c994c2a3eea23902b5db0620761b4e7af93e89f0229d6725b61fbd42906ae456"},
		CyberSessionTranscriptBlockKeys(7, []byte(`{"input":"legacy"}`)),
		"legacy transcript keys must retain their pre-user-scope v3 identity")
	c, body := newCyberBlockTestCtx(map[string]string{"session_id": "session"}, `{"input":"hello","user_id":999}`)
	require.NotEqual(t, CyberSessionExplicitBlockKey(7, c, body), CyberSessionUserExplicitBlockKey(7, c, body))
	require.NotEqual(t, CyberSessionScopeKey(7, "", ""), CyberSessionUserScopeKey(7))
	require.NotEqual(t, CyberSessionTranscriptBlockKeys(7, body), CyberSessionUserTranscriptBlockKeys(7, body))
	require.NotEqual(t, CyberSessionUserExplicitBlockKey(7, c, body), CyberSessionUserExplicitBlockKey(8, c, body))
	require.NotEqual(t, CyberSessionUserScopeKey(7), CyberSessionUserScopeKey(8))
	require.NotEqual(t, CyberSessionUserTranscriptBlockKeys(7, body), CyberSessionUserTranscriptBlockKeys(8, body))
	_, spoofed := newCyberBlockTestCtx(nil, `{"input":"hello","user_id":7}`)
	require.Equal(t, CyberSessionUserTranscriptBlockKeys(7, body), CyberSessionUserTranscriptBlockKeys(7, spoofed),
		"request ownership comes from authentication, not payload metadata")
	for _, invalidID := range []int64{0, -1} {
		require.Empty(t, CyberSessionUserExplicitBlockKey(invalidID, c, body))
		require.Empty(t, CyberSessionUserScopeKey(invalidID))
		require.Empty(t, CyberSessionUserTranscriptBlockKeys(invalidID, body))
	}
}

func TestCyberSessionUserBlockSurvivesAPIKeyRotationWithoutCrossUserBlocking(t *testing.T) {
	for _, mode := range []string{"explicit", "transcript"} {
		t.Run(mode, func(t *testing.T) {
			combo := &comboCacheAndStore{}
			svc := &OpenAIGatewayService{
				cache: combo,
				settingService: &SettingService{settingRepo: &fakeSettingRepo{vals: map[string]string{
					SettingKeyCyberSessionBlockEnabled: "true",
				}}},
			}
			ctx := context.Background()
			const userID = int64(42)
			hit := `{"messages":[{"role":"user","content":"setup"},{"role":"assistant","content":"ready"},{"role":"user","content":"trigger"}]}`
			continuation := `{"messages":[{"role":"user","content":"setup"},{"role":"assistant","content":"ready"},{"role":"user","content":"changed trigger"}]}`
			headers := map[string]string{}
			if mode == "explicit" {
				headers["session_id"] = "same-session"
			}
			hitCtx, hitBody := newCyberBlockTestCtx(headers, hit)
			nextCtx, nextBody := newCyberBlockTestCtx(headers, continuation)
			var scope string
			var keys []string
			if mode == "explicit" {
				keys = []string{CyberSessionUserExplicitBlockKey(userID, hitCtx, hitBody)}
			} else {
				scope = CyberSessionUserScopeKey(userID)
				keys = CyberSessionUserTranscriptBlockKeys(userID, hitBody)
			}
			require.NoError(t, svc.MarkCyberSessionBlocked(ctx, scope, keys))
			for _, apiKeyID := range []int64{101, 202} {
				key, err := svc.FindCyberSessionBlockedForUserRequest(ctx, apiKeyID, userID, nextCtx, nextBody, "203.0.113.2", "different-client/9")
				require.NoError(t, err)
				require.Contains(t, keys, key, "the same user's replacement API key must keep the block")
			}
			key, err := svc.FindCyberSessionBlockedForUserRequest(ctx, 303, userID+1, nextCtx, nextBody, "203.0.113.2", "different-client/9")
			require.NoError(t, err)
			require.Empty(t, key, "identical session/content cannot block another authenticated user")
			unrelatedCtx, unrelated := newCyberBlockTestCtx(map[string]string{"session_id": "unrelated"}, `{"input":"unrelated request"}`)
			key, err = svc.FindCyberSessionBlockedForUserRequest(ctx, 202, userID, unrelatedCtx, unrelated, "", "")
			require.NoError(t, err)
			require.Empty(t, key, "user scope is a lookup hint, not a user-wide ban")
		})
	}
}

func TestCyberSessionUserLookupPreservesLegacyRecordsAndDeadline(t *testing.T) {
	for _, mode := range []string{"explicit", "transcript", "store-failure", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			enabled := "true"
			if mode == "disabled" {
				enabled = "false"
			}
			combo := &comboCacheAndStore{}
			svc := &OpenAIGatewayService{
				cache: combo,
				settingService: &SettingService{settingRepo: &fakeSettingRepo{vals: map[string]string{
					SettingKeyCyberSessionBlockEnabled: enabled,
				}}},
			}
			headers := map[string]string{}
			if mode == "explicit" {
				headers["session_id"] = "old-session"
			}
			c, body := newCyberBlockTestCtx(headers, `{"input":"legacy"}`)
			key := CyberSessionExplicitBlockKey(7, c, body)
			scope := ""
			if mode == "transcript" {
				key = CyberSessionTranscriptBlockKeys(7, body)[0]
				scope = CyberSessionScopeKey(7, "", "")
			}
			if mode == "explicit" || mode == "transcript" {
				require.NoError(t, svc.MarkCyberSessionBlocked(context.Background(), scope, []string{key}))
			} else {
				combo.store.scopeErr = errors.New("unavailable")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			found, err := svc.FindCyberSessionBlockedForUserRequest(ctx, 7, 42, c, body, "", "")
			if mode == "store-failure" {
				require.ErrorIs(t, err, ErrCyberSessionProtectionUnavailable)
				require.Empty(t, found)
				return
			}
			require.NoError(t, err)
			if mode == "disabled" {
				require.Empty(t, found)
				require.Zero(t, combo.store.findCalls)
				require.Zero(t, combo.store.scopeCalls)
				return
			}
			require.Equal(t, key, found)
			deadline, _ := ctx.Deadline()
			require.Equal(t, deadline, combo.store.lookupDeadline, "both scopes keep the same earlier deadline")
			require.Equal(t, 1, combo.store.findCalls, "explicit keys are batched; inactive user transcripts are not read")
		})
	}
}

func TestCyberSessionUserScopeRetainsTranscriptOverflowProtection(t *testing.T) {
	combo := &comboCacheAndStore{store: fakeCyberBlockStore{
		scopes: map[string]bool{CyberSessionUserScopeKey(42): true},
	}}
	svc := &OpenAIGatewayService{
		cache: combo,
		settingService: &SettingService{settingRepo: &fakeSettingRepo{vals: map[string]string{
			SettingKeyCyberSessionBlockEnabled: "true",
		}}},
	}
	messages := make([]map[string]string, maxOpenAICyberTranscriptLookupKeys+1)
	for i := range messages {
		messages[i] = map[string]string{"role": "user", "content": strconv.Itoa(i)}
	}
	body, err := json.Marshal(map[string]any{"messages": messages})
	require.NoError(t, err)
	c, _ := newCyberBlockTestCtx(nil, string(body))
	key, err := svc.FindCyberSessionBlockedForUserRequest(context.Background(), 202, 42, c, body, "", "")
	require.NoError(t, err)
	require.Equal(t, cyberSessionTranscriptLookupOverflowBlockKey, key)
	require.Zero(t, combo.store.findCalls)
}

func TestCyberTranscriptBlockKeysRequireModelGeneratedHistory(t *testing.T) {
	first := []byte(`{"instructions":"shared","input":[{"role":"user","content":"fixed environment"},{"role":"user","content":"question one"}]}`)
	second := []byte(`{"instructions":"shared","input":[{"role":"user","content":"fixed environment"},{"role":"user","content":"question two"}]}`)
	firstKeys := CyberSessionTranscriptBlockKeys(77, first)
	secondKeys := CyberSessionTranscriptBlockKeys(77, second)
	require.Len(t, firstKeys, 1)
	require.Len(t, secondKeys, 1)
	require.NotEqual(t, firstKeys[0], secondKeys[0])

	hit := []byte(`{"messages":[{"role":"user","content":"setup"},{"role":"assistant","content":"ready"},{"role":"user","content":"trigger"}]}`)
	continuation := []byte(`{"messages":[{"role":"user","content":"setup"},{"role":"assistant","content":"ready"},{"role":"user","content":"different trigger"},{"role":"assistant","content":"blocked"},{"role":"user","content":"continue"}]}`)
	hitKeys := CyberSessionTranscriptBlockKeys(77, hit)
	require.Len(t, hitKeys, 2)
	require.Contains(t, CyberSessionTranscriptLookupKeys(77, continuation), hitKeys[1])
}

func TestCyberTranscriptBlockKeysWebSocketResponseCreate(t *testing.T) {
	body := []byte(`{"type":"response.create","response":{"prompt_cache_key":"ws-session","input":[{"role":"user","content":"setup"},{"role":"assistant","content":"ready"},{"role":"user","content":"trigger"}]}}`)
	c, _ := newCyberBlockTestCtx(nil, string(body))
	require.NotEmpty(t, CyberSessionExplicitBlockKey(88, c, body))
	require.Len(t, CyberSessionTranscriptBlockKeys(88, body), 2)
}

func TestCyberTranscriptLookupKeysAreBoundedAndKeepNewestOrder(t *testing.T) {
	messages := make([]map[string]string, maxOpenAICyberTranscriptLookupKeys+44)
	for i := range messages {
		messages[i] = map[string]string{"role": "user", "content": "message-" + strconv.Itoa(i)}
	}
	body, err := json.Marshal(map[string]any{"messages": messages})
	require.NoError(t, err)

	keys := CyberSessionTranscriptLookupKeys(77, body)
	require.Len(t, keys, maxOpenAICyberTranscriptLookupKeys)

	firstRetainedBody, err := json.Marshal(map[string]any{"messages": messages[:45]})
	require.NoError(t, err)
	firstRetainedPrefix := CyberSessionTranscriptLookupKeys(77, firstRetainedBody)
	require.Equal(t, firstRetainedPrefix[len(firstRetainedPrefix)-1], keys[0])

	fullKey := CyberSessionTranscriptBlockKeys(77, body)[0]
	require.Equal(t, fullKey, keys[len(keys)-1])
}

// --- fakes ---

type fakeCyberBlockStore struct {
	blocked        map[string]bool
	scopes         map[string]bool
	findCalls      int
	scopeCalls     int
	findErr        error
	scopeErr       error
	setErr         error
	waitForContext bool
	lookupDeadline time.Time
}

var _ CyberSessionBlockStore = (*fakeCyberBlockStore)(nil)

func (f *fakeCyberBlockStore) SetCyberSessionBlocked(_ context.Context, scopeKey string, keys []string, _ time.Duration) error {
	if f.setErr != nil {
		return f.setErr
	}
	if f.blocked == nil {
		f.blocked = map[string]bool{}
	}
	for _, key := range keys {
		f.blocked[key] = true
	}
	if scopeKey != "" {
		if f.scopes == nil {
			f.scopes = map[string]bool{}
		}
		f.scopes[scopeKey] = true
	}
	return nil
}

func (f *fakeCyberBlockStore) IsCyberSessionScopeActive(ctx context.Context, scopeKey string) (bool, error) {
	f.scopeCalls++
	f.lookupDeadline, _ = ctx.Deadline()
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	return f.scopes[scopeKey], f.scopeErr
}

func (f *fakeCyberBlockStore) FindCyberSessionBlocked(ctx context.Context, keys []string) (string, error) {
	f.findCalls++
	f.lookupDeadline, _ = ctx.Deadline()
	if f.waitForContext {
		<-ctx.Done()
		return "", ctx.Err()
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if f.findErr != nil {
		return "", f.findErr
	}
	for _, key := range keys {
		if f.blocked[key] {
			return key, nil
		}
	}
	return "", nil
}

// fakeSettingRepo is a minimal SettingRepository stub for unit tests.
// Only GetValue is exercised by GetCyberSessionBlockRuntime; all other methods
// panic so accidental calls are caught immediately.
type fakeSettingRepo struct {
	vals map[string]string
}

func (r *fakeSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	v, ok := r.vals[key]
	if !ok {
		return "", ErrSettingNotFound
	}
	return v, nil
}
func (r *fakeSettingRepo) Get(_ context.Context, _ string) (*Setting, error) {
	panic("fakeSettingRepo.Get not implemented")
}
func (r *fakeSettingRepo) Set(_ context.Context, _, _ string) error {
	panic("fakeSettingRepo.Set not implemented")
}
func (r *fakeSettingRepo) GetMultiple(_ context.Context, _ []string) (map[string]string, error) {
	panic("fakeSettingRepo.GetMultiple not implemented")
}
func (r *fakeSettingRepo) SetMultiple(_ context.Context, _ map[string]string) error {
	panic("fakeSettingRepo.SetMultiple not implemented")
}
func (r *fakeSettingRepo) GetAll(_ context.Context) (map[string]string, error) {
	panic("fakeSettingRepo.GetAll not implemented")
}
func (r *fakeSettingRepo) Delete(_ context.Context, _ string) error {
	panic("fakeSettingRepo.Delete not implemented")
}

var _ SettingRepository = (*fakeSettingRepo)(nil)

// comboCacheAndStore implements both GatewayCache (no-op stubs) and
// CyberSessionBlockStore (delegates to fakeCyberBlockStore) so it can be
// injected as s.cache and successfully type-asserted to CyberSessionBlockStore.
type comboCacheAndStore struct {
	store fakeCyberBlockStore
}

var _ GatewayCache = (*comboCacheAndStore)(nil)
var _ CyberSessionBlockStore = (*comboCacheAndStore)(nil)

func (c *comboCacheAndStore) GetSessionAccountID(_ context.Context, _ int64, _ string) (int64, error) {
	return 0, errors.New("stub")
}
func (c *comboCacheAndStore) SetSessionAccountID(_ context.Context, _ int64, _ string, _ int64, _ time.Duration) error {
	return nil
}
func (c *comboCacheAndStore) RefreshSessionTTL(_ context.Context, _ int64, _ string, _ time.Duration) error {
	return nil
}
func (c *comboCacheAndStore) DeleteSessionAccountID(_ context.Context, _ int64, _ string) error {
	return nil
}

func (c *comboCacheAndStore) SetGrokVideoPendingBilling(_ context.Context, _ string, _ []byte, _ time.Duration) error {
	return nil
}
func (c *comboCacheAndStore) GetGrokVideoPendingBilling(_ context.Context, _ string) ([]byte, error) {
	return nil, nil
}
func (c *comboCacheAndStore) ClaimGrokVideoBilled(_ context.Context, _ string, _ time.Duration) (bool, error) {
	return true, nil
}

func (c *comboCacheAndStore) ReleaseGrokVideoBilled(_ context.Context, _ string) error {
	return nil
}

func (c *comboCacheAndStore) SetReasoningContent(_ context.Context, _ string, _ string, _ time.Duration) error {
	return nil
}
func (c *comboCacheAndStore) GetReasoningContent(_ context.Context, _ string) (string, error) {
	return "", ErrReasoningContentNotFound
}

func (c *comboCacheAndStore) SetCyberSessionBlocked(ctx context.Context, scopeKey string, keys []string, ttl time.Duration) error {
	return c.store.SetCyberSessionBlocked(ctx, scopeKey, keys, ttl)
}
func (c *comboCacheAndStore) IsCyberSessionScopeActive(ctx context.Context, scopeKey string) (bool, error) {
	return c.store.IsCyberSessionScopeActive(ctx, scopeKey)
}
func (c *comboCacheAndStore) FindCyberSessionBlocked(ctx context.Context, keys []string) (string, error) {
	return c.store.FindCyberSessionBlocked(ctx, keys)
}

// --- tests ---

// Missing service/settings leaves the feature disabled, without store access.
func TestFindCyberSessionBlocked_EmptyAndNilService(t *testing.T) {
	var nilSvc *OpenAIGatewayService
	key, err := nilSvc.FindCyberSessionBlockedForRequest(context.Background(), 1, nil, nil, "", "")
	require.NoError(t, err)
	require.Empty(t, key)
	require.NotPanics(t, func() { nilSvc.MarkCyberSessionBlocked(context.Background(), "", []string{"k"}) })

	svc := &OpenAIGatewayService{}
	key, err = svc.FindCyberSessionBlockedForRequest(context.Background(), 1, nil, nil, "", "")
	require.NoError(t, err)
	require.Empty(t, key)
}

func TestMarkCyberSessionBlockedReportsPersistenceFailure(t *testing.T) {
	for _, tt := range []struct {
		name      string
		cache     GatewayCache
		disabled  bool
		cancelled bool
		wantErr   bool
	}{
		{name: "missing store", wantErr: true},
		{name: "write failed", cache: &comboCacheAndStore{store: fakeCyberBlockStore{setErr: errors.New("store offline")}}, wantErr: true},
		{name: "request cancelled", cache: &comboCacheAndStore{}, cancelled: true, wantErr: true},
		{name: "disabled", disabled: true},
		{name: "persisted", cache: &comboCacheAndStore{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			enabled := "true"
			if tt.disabled {
				enabled = "false"
			}
			svc := &OpenAIGatewayService{
				cache: tt.cache,
				settingService: &SettingService{settingRepo: &fakeSettingRepo{vals: map[string]string{
					SettingKeyCyberSessionBlockEnabled: enabled,
				}}},
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.cancelled {
				cancel()
			}
			err := svc.MarkCyberSessionBlocked(ctx, "scope", []string{"key"})
			if tt.wantErr {
				require.ErrorIs(t, err, ErrCyberSessionProtectionUnavailable)
				if cache, ok := tt.cache.(*comboCacheAndStore); ok {
					require.Empty(t, cache.store.blocked)
				}
			} else {
				require.NoError(t, err)
				if cache, ok := tt.cache.(*comboCacheAndStore); ok {
					require.True(t, cache.store.blocked["key"])
					require.True(t, cache.store.scopes["scope"])
				}
			}
		})
	}
}

// TestCyberSessionBlock_RoundTrip exercises the type-assertion success path:
// mark a session blocked via a combo cache+store, then confirm IsCyberSessionBlocked
// returns true, and an unrelated key returns false.
func TestCyberSessionBlock_RoundTrip(t *testing.T) {
	// SettingService with only settingRepo set — GetCyberSessionBlockRuntime needs
	// nothing else (cfg/proxyRepo/etc. are not touched by this code path).
	settingSvc := &SettingService{
		settingRepo: &fakeSettingRepo{
			vals: map[string]string{
				SettingKeyCyberSessionBlockEnabled:    "true",
				SettingKeyCyberSessionBlockTTLSeconds: "60",
			},
		},
	}

	combo := &comboCacheAndStore{}
	svc := &OpenAIGatewayService{
		cache:          combo,
		settingService: settingSvc,
	}

	ctx := context.Background()
	const testKey = "deadbeef1234"

	c, body := newCyberBlockTestCtx(map[string]string{"session_id": "sess-roundtrip"}, `{}`)
	explicitKey := CyberSessionExplicitBlockKey(1, c, body)
	key, err := svc.FindCyberSessionBlockedForRequest(ctx, 1, c, body, "203.0.113.1", "client/1.0")
	require.NoError(t, err)
	require.Empty(t, key)

	svc.MarkCyberSessionBlocked(ctx, "", []string{explicitKey, testKey})

	key, err = svc.FindCyberSessionBlockedForRequest(ctx, 1, c, body, "203.0.113.1", "client/1.0")
	require.NoError(t, err)
	require.Equal(t, explicitKey, key)
}

func TestFindCyberSessionBlockedForRequestUsesScopeForTranscript(t *testing.T) {
	settingSvc := &SettingService{settingRepo: &fakeSettingRepo{vals: map[string]string{
		SettingKeyCyberSessionBlockEnabled:    "true",
		SettingKeyCyberSessionBlockTTLSeconds: "60",
	}}}
	combo := &comboCacheAndStore{}
	svc := &OpenAIGatewayService{cache: combo, settingService: settingSvc}
	ctx := context.Background()

	hitBody := []byte(`{"messages":[{"role":"user","content":"setup"},{"role":"assistant","content":"ready"},{"role":"user","content":"trigger"}]}`)
	nextBody := []byte(`{"messages":[{"role":"user","content":"setup"},{"role":"assistant","content":"ready"},{"role":"user","content":"different trigger"},{"role":"assistant","content":"blocked"},{"role":"user","content":"continue"}]}`)
	nextCtx, _ := newCyberBlockTestCtx(nil, string(nextBody))
	const clientIP = "203.0.113.20"
	const userAgent = "Codex CLI 1.2.3"
	blockKey := CyberSessionTranscriptBlockKeys(9, hitBody)[1]

	// Without an active source scope, transcript candidates are never blocks.
	key, err := svc.FindCyberSessionBlockedForRequest(ctx, 9, nextCtx, nextBody, clientIP, userAgent)
	require.NoError(t, err)
	require.Empty(t, key)
	scopeKey := CyberSessionScopeKey(9, clientIP, userAgent)
	svc.MarkCyberSessionBlocked(ctx, scopeKey, []string{blockKey})
	key, err = svc.FindCyberSessionBlockedForRequest(ctx, 9, nextCtx, nextBody, clientIP, "Codex CLI 1.2.4")
	require.NoError(t, err)
	require.Equal(t, blockKey, key)
}

func TestFindCyberSessionBlockedForRequestFailsClosedOnScopedTranscriptOverflow(t *testing.T) {
	settingSvc := &SettingService{settingRepo: &fakeSettingRepo{vals: map[string]string{
		SettingKeyCyberSessionBlockEnabled:    "true",
		SettingKeyCyberSessionBlockTTLSeconds: "60",
	}}}
	combo := &comboCacheAndStore{}
	svc := &OpenAIGatewayService{cache: combo, settingService: settingSvc}
	ctx := context.Background()
	const apiKeyID = int64(9)
	const clientIP = "203.0.113.20"
	const userAgent = "Codex CLI 1.2.3"

	messages := make([]map[string]string, maxOpenAICyberTranscriptLookupKeys+1)
	for i := range messages {
		messages[i] = map[string]string{"role": "user", "content": "message-" + strconv.Itoa(i)}
	}
	body, err := json.Marshal(map[string]any{"messages": messages})
	require.NoError(t, err)
	c, _ := newCyberBlockTestCtx(nil, string(body))
	key, err := svc.FindCyberSessionBlockedForRequest(ctx, apiKeyID, c, body, clientIP, userAgent)
	require.NoError(t, err)
	require.Empty(t, key, "overflow alone must not bypass the scope gate")
	combo.store.scopes = map[string]bool{CyberSessionScopeKey(apiKeyID, clientIP, userAgent): true}

	key, err = svc.FindCyberSessionBlockedForRequest(ctx, apiKeyID, c, body, clientIP, userAgent)
	require.NoError(t, err)
	require.Equal(t, cyberSessionTranscriptLookupOverflowBlockKey, key)
	require.Zero(t, combo.store.findCalls, "overflow must not issue an unbounded Redis lookup")
}

func TestFindCyberSessionBlockedForRequestStoreFailureRejects(t *testing.T) {
	for _, stage := range []string{"missing", "explicit", "scope", "transcript"} {
		t.Run(stage, func(t *testing.T) {
			settingSvc := &SettingService{settingRepo: &fakeSettingRepo{vals: map[string]string{
				SettingKeyCyberSessionBlockEnabled: "true",
			}}}
			svc := &OpenAIGatewayService{settingService: settingSvc}
			storeErr := errors.New("store unavailable")
			combo := &comboCacheAndStore{}
			headers := map[string]string{}
			if stage != "missing" {
				svc.cache = combo
			}
			switch stage {
			case "explicit":
				headers["session_id"] = "blocked-session"
				combo.store.findErr = storeErr
			case "scope":
				combo.store.scopeErr = storeErr
			case "transcript":
				combo.store.scopes = map[string]bool{CyberSessionScopeKey(1, "", ""): true}
				combo.store.findErr = storeErr
			}
			c, body := newCyberBlockTestCtx(headers, `{"input":"hello"}`)
			key, err := svc.FindCyberSessionBlockedForRequest(context.Background(), 1, c, body, "", "")
			require.ErrorIs(t, err, ErrCyberSessionProtectionUnavailable)
			require.Empty(t, key, "storage failure is not a policy violation")
			require.NotContains(t, err.Error(), storeErr.Error(), "do not expose storage internals")
		})
	}
}

func TestFindCyberSessionBlockedForRequestDisabledSkipsFailedStore(t *testing.T) {
	combo := &comboCacheAndStore{store: fakeCyberBlockStore{
		findErr: errors.New("store unavailable"), scopeErr: errors.New("store unavailable"),
	}}
	svc := &OpenAIGatewayService{
		cache: combo,
		settingService: &SettingService{settingRepo: &fakeSettingRepo{vals: map[string]string{
			SettingKeyCyberSessionBlockEnabled: "false",
		}}},
	}
	c, body := newCyberBlockTestCtx(map[string]string{"session_id": "session"}, `{}`)
	key, err := svc.FindCyberSessionBlockedForRequest(context.Background(), 1, c, body, "", "")
	require.NoError(t, err)
	require.Empty(t, key)
	require.Zero(t, combo.store.findCalls)
	require.Zero(t, combo.store.scopeCalls)
}

func TestFindCyberSessionBlockedForRequestLookupDeadline(t *testing.T) {
	for _, mode := range []string{"bounded", "canceled", "earlier-deadline"} {
		t.Run(mode, func(t *testing.T) {
			combo := &comboCacheAndStore{}
			svc := &OpenAIGatewayService{
				cache: combo,
				settingService: &SettingService{settingRepo: &fakeSettingRepo{vals: map[string]string{
					SettingKeyCyberSessionBlockEnabled: "true",
				}}},
			}
			ctx := context.Background()
			cancel := func() {}
			switch mode {
			case "canceled":
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "earlier-deadline":
				ctx, cancel = context.WithTimeout(ctx, 20*time.Millisecond)
				combo.store.waitForContext = true
			}
			defer cancel()
			c, body := newCyberBlockTestCtx(map[string]string{"session_id": "session"}, `{}`)
			start := time.Now()
			key, err := svc.FindCyberSessionBlockedForRequest(ctx, 1, c, body, "", "")
			require.Empty(t, key)
			if mode == "bounded" {
				require.NoError(t, err)
				require.False(t, combo.store.lookupDeadline.IsZero())
				require.WithinDuration(t, start.Add(cyberSessionBlockLookupTimeout), combo.store.lookupDeadline, 50*time.Millisecond)
			} else {
				require.ErrorIs(t, err, ErrCyberSessionProtectionUnavailable)
				if mode == "canceled" {
					require.Zero(t, combo.store.findCalls)
					require.Zero(t, combo.store.scopeCalls)
				} else if !combo.store.lookupDeadline.IsZero() {
					deadline, _ := ctx.Deadline()
					require.Equal(t, deadline, combo.store.lookupDeadline)
				}
			}
		})
	}
}

func TestCyberSessionScopeKeyNormalizesUserAgentVersion(t *testing.T) {
	base := CyberSessionScopeKey(7, "203.0.113.10", "Codex CLI 1.2.3")
	require.NotEmpty(t, base)
	require.Equal(t, base, CyberSessionScopeKey(7, "203.0.113.10", "Codex CLI 1.2.4"))
	require.NotEqual(t, base, CyberSessionScopeKey(8, "203.0.113.10", "Codex CLI 1.2.3"))
	require.NotEqual(t, base, CyberSessionScopeKey(7, "203.0.113.11", "Codex CLI 1.2.3"))
}
