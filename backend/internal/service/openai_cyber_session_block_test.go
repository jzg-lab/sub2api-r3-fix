package service

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newCyberBlockTestCtx(headers map[string]string, body string) (*gin.Context, []byte) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/openai/v1/responses", strings.NewReader(body))
	for key, value := range headers {
		c.Request.Header.Set(key, value)
	}
	return c, []byte(body)
}

type cyberBlockSettings struct {
	SettingRepository
}

func (s cyberBlockSettings) GetValue(_ context.Context, key string) (string, error) {
	switch key {
	case SettingKeyCyberSessionBlockEnabled:
		return "true", nil
	case SettingKeyCyberSessionBlockTTLSeconds:
		return "60", nil
	default:
		return "", ErrSettingNotFound
	}
}

type cyberBlockCache struct {
	GatewayCache
	blocked  map[string]bool
	scope    map[string]bool
	lengths  []int
	readErr  error
	writeErr error
}

func (c *cyberBlockCache) SetCyberSessionBlocked(_ context.Context, block CyberSessionBlock, _ time.Duration) error {
	if c.writeErr != nil {
		return c.writeErr
	}
	if c.blocked == nil {
		c.blocked = make(map[string]bool)
	}
	for _, key := range block.Keys {
		c.blocked[key] = true
	}
	if block.ScopeKey != "" {
		if c.scope == nil {
			c.scope = make(map[string]bool)
		}
		c.scope[block.ScopeKey] = true
		c.lengths = append(c.lengths, block.TranscriptLengths...)
	}
	return nil
}

func (c *cyberBlockCache) CyberSessionTranscriptLengths(_ context.Context, _ int64, scopeKey string) ([]int, error) {
	if c.readErr != nil {
		return nil, c.readErr
	}
	if !c.scope[scopeKey] {
		return nil, nil
	}
	return c.lengths, nil
}

func (c *cyberBlockCache) IsCyberSessionScopeActive(_ context.Context, scopeKey string) (bool, error) {
	return c.scope[scopeKey], nil
}

func (c *cyberBlockCache) FindCyberSessionBlocked(_ context.Context, keys []string) (string, error) {
	if c.readErr != nil {
		return "", c.readErr
	}
	for _, key := range keys {
		if c.blocked[key] {
			return key, nil
		}
	}
	return "", nil
}

func TestCyberSessionBlockUsesAPIKeyNamespace(t *testing.T) {
	c, body := newCyberBlockTestCtx(map[string]string{"session_id": "same-session"}, `{}`)
	require.NotEmpty(t, CyberSessionExplicitBlockKey(101, c, body))
	require.NotEqual(t, CyberSessionExplicitBlockKey(101, c, body), CyberSessionExplicitBlockKey(202, c, body))
	require.Equal(t, CyberSessionExplicitBlockKey(101, c, body), CyberSessionExplicitBlockKey(101, c, body))
}

func TestFindCyberSessionBlockedForRequestMatchesRecordedHistoryLength(t *testing.T) {
	const apiKeyID = int64(9)
	const clientIP = "203.0.113.20"
	const userAgent = "Codex CLI 1.2.3"
	settings := &SettingService{settingRepo: cyberBlockSettings{}}
	cache := &cyberBlockCache{}
	svc := &OpenAIGatewayService{cache: cache, settingService: settings}
	ctx := context.Background()

	hitBody := []byte(`{"messages":[{"role":"user","content":"setup"},{"role":"assistant","content":"ready"},{"role":"user","content":"trigger"}]}`)
	continuation := []byte(`{"messages":[{"role":"user","content":"setup"},{"role":"assistant","content":"ready"},{"role":"user","content":"trigger"},{"role":"assistant","content":"blocked"},{"role":"user","content":"continue"}]}`)
	blocks := CyberSessionTranscriptBlocks(apiKeyID, hitBody)
	require.NotEmpty(t, blocks)
	block := CyberSessionBlock{
		APIKeyID: apiKeyID, ScopeKey: CyberSessionScopeKey(apiKeyID, clientIP, userAgent),
		Keys: []string{blocks[0].Key}, TranscriptLengths: []int{blocks[0].Length},
	}
	require.Equal(t, block.Keys, cyberSessionTranscriptKeysAt(apiKeyID, continuation, block.TranscriptLengths))
	svc.MarkCyberSessionBlocked(ctx, block)
	require.Contains(t, cache.blocked, block.Keys[0])
	require.Contains(t, cache.scope, block.ScopeKey)
	require.Equal(t, []int{3}, cache.lengths)

	c, _ := newCyberBlockTestCtx(nil, string(continuation))
	key := svc.FindCyberSessionBlockedForRequest(ctx, apiKeyID, c, continuation, clientIP, userAgent)
	require.Equal(t, block.Keys[0], key)
}

func TestCyberSessionBlockStoreFailuresFailOpen(t *testing.T) {
	settings := &SettingService{settingRepo: cyberBlockSettings{}}
	c, body := newCyberBlockTestCtx(map[string]string{"session_id": "session"}, `{}`)
	blockKey := CyberSessionExplicitBlockKey(1, c, body)

	cache := &cyberBlockCache{readErr: errors.New("redis unavailable")}
	svc := &OpenAIGatewayService{cache: cache, settingService: settings}
	require.Empty(t, svc.FindCyberSessionBlockedForRequest(context.Background(), 1, c, body, "", ""))

	cache.readErr = nil
	cache.writeErr = errors.New("redis unavailable")
	svc.MarkCyberSessionBlocked(context.Background(), CyberSessionBlock{APIKeyID: 1, Keys: []string{blockKey}})
	require.Empty(t, cache.blocked)
}
