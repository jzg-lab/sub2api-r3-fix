package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
)

// CyberSessionBlockStore 是 cyber 会话屏蔽表的存取接口。
// repository 层 gatewayCache 附带实现（类型断言探测接入，不改 GatewayCache
// 共享接口）；启用屏蔽时，缺失存储实现必须拒绝转发。
type CyberSessionBlockStore interface {
	SetCyberSessionBlocked(ctx context.Context, scopeKey string, keys []string, ttl time.Duration) error
	IsCyberSessionScopeActive(ctx context.Context, scopeKey string) (bool, error)
	FindCyberSessionBlocked(ctx context.Context, keys []string) (string, error)
}

const cyberSessionTranscriptLookupOverflowBlockKey = "transcript_lookup_limit_exceeded"
const cyberSessionBlockLookupTimeout = 500 * time.Millisecond

var ErrCyberSessionProtectionUnavailable = errors.New("session protection is unavailable")

// CyberSessionExplicitBlockKey returns an inexpensive exact key when the
// client supplies a stable session signal.
func CyberSessionExplicitBlockKey(apiKeyID int64, c *gin.Context, body []byte) string {
	return hashCyberSessionBlockKey(apiKeyID, explicitOpenAISessionID(c, body))
}

// CyberSessionTranscriptBlockKeys returns the exact full-request key followed
// by an optional rewrite-tolerant context key. The latter is emitted only after
// model-generated history has been observed.
func CyberSessionTranscriptBlockKeys(apiKeyID int64, body []byte) []string {
	return cyberSessionTranscriptWriteKeys(deriveOpenAICyberTranscriptBlockKeys(apiKeyID, body))
}

func cyberSessionTranscriptWriteKeys(derived openAICyberTranscriptBlockKeys) []string {
	if len(derived.lookupKeys) == 0 {
		return nil
	}
	keys := []string{derived.lookupKeys[len(derived.lookupKeys)-1]}
	if derived.preLatestUserKey != "" && derived.preLatestUserKey != keys[0] {
		keys = append(keys, derived.preLatestUserKey)
	}
	return keys
}

func CyberSessionTranscriptLookupKeys(apiKeyID int64, body []byte) []string {
	return deriveOpenAICyberTranscriptBlockKeys(apiKeyID, body).lookupKeys
}

// CyberSessionScopeKey is a coarse, non-blocking fingerprint used only to
// avoid transcript parsing and MGET for sources that never produced a hit.
func CyberSessionScopeKey(apiKeyID int64, clientIP, userAgent string) string {
	if apiKeyID <= 0 {
		return ""
	}
	raw := "cyber-scope:v1|api_key=" + strconv.FormatInt(apiKeyID, 10) +
		"|ip=" + strings.TrimSpace(clientIP) +
		"|ua=" + NormalizeSessionUserAgent(userAgent)
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func hashCyberSessionBlockKey(apiKeyID int64, raw string) string {
	if raw == "" {
		return ""
	}
	isolated := isolateOpenAISessionID(apiKeyID, raw)
	sum := sha256.Sum256([]byte(isolated))
	return hex.EncodeToString(sum[:])
}

// User keys use authenticated database ownership, never a client-supplied user
// field. Keep the namespace separate from legacy API-key keys during rollout.
func CyberSessionUserExplicitBlockKey(userID int64, c *gin.Context, body []byte) string {
	raw := strings.TrimSpace(explicitOpenAISessionID(c, body))
	if userID <= 0 || raw == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("cyber-user-session:v1|user=" + strconv.FormatInt(userID, 10) + "|session=" + raw))
	return hex.EncodeToString(sum[:])
}

func CyberSessionUserScopeKey(userID int64) string {
	if userID <= 0 {
		return ""
	}
	sum := sha256.Sum256([]byte("cyber-user-scope:v1|user=" + strconv.FormatInt(userID, 10)))
	return hex.EncodeToString(sum[:])
}

func CyberSessionUserTranscriptBlockKeys(userID int64, body []byte) []string {
	if userID <= 0 {
		return nil
	}
	return cyberSessionTranscriptWriteKeys(deriveOpenAICyberTranscriptBlockKeysForIdentity("user="+strconv.FormatInt(userID, 10), body))
}

type cyberSessionLookupIdentity struct {
	explicitKey string
	scopeKey    string
	transcript  string
}

// cyberSessionBlockStore 探测 cache 是否具备屏蔽存储能力。
// 注意：若未来以装饰器包装 GatewayCache（如日志/指标装饰器），该装饰器必须同时实现
// CyberSessionBlockStore，否则启用屏蔽时请求会被拒绝
// （编译断言 var _ service.CyberSessionBlockStore = (*gatewayCache)(nil) 只覆盖
// *gatewayCache 本体，无法覆盖其外层包装）。
func (s *OpenAIGatewayService) cyberSessionBlockStore() CyberSessionBlockStore {
	if s == nil || s.cache == nil {
		return nil
	}
	store, ok := s.cache.(CyberSessionBlockStore)
	if !ok {
		return nil
	}
	return store
}

// CyberSessionBlockRuntime returns policy and availability separately.
func (s *OpenAIGatewayService) CyberSessionBlockRuntime(ctx context.Context) (bool, time.Duration, error) {
	if s == nil || s.settingService == nil {
		return false, time.Hour, nil
	}
	return s.settingService.GetCyberSessionBlockRuntime(ctx)
}

// MarkCyberSessionBlocked 把会话写入屏蔽表（写入点：cyber 命中后）。
// A skipped disabled policy is distinct from a failed write.
func (s *OpenAIGatewayService) MarkCyberSessionBlocked(ctx context.Context, scopeKey string, keys []string) error {
	if s == nil || len(keys) == 0 {
		return nil
	}
	enabled, ttl, err := s.CyberSessionBlockRuntime(ctx)
	if err != nil {
		return err
	}
	if !enabled {
		return nil
	}
	if ctx.Err() != nil {
		return ErrCyberSessionProtectionUnavailable
	}
	store := s.cyberSessionBlockStore()
	if store == nil {
		return ErrCyberSessionProtectionUnavailable
	}
	if err := store.SetCyberSessionBlocked(ctx, scopeKey, keys, ttl); err != nil {
		logger.LegacyPrintf("service.openai_gateway", "cyber session block write failed: err=%v", err)
		return ErrCyberSessionProtectionUnavailable
	}
	if ctx.Err() != nil {
		return ErrCyberSessionProtectionUnavailable
	}
	return nil
}

// FindCyberSessionBlockedForRequest applies explicit-first lookup followed by
// scope-gated transcript matching. Enabled protection fails closed when its
// store cannot be checked; an infrastructure failure is not a policy violation.
func (s *OpenAIGatewayService) FindCyberSessionBlockedForRequest(ctx context.Context, apiKeyID int64, c *gin.Context, body []byte, clientIP, userAgent string) (string, error) {
	return s.FindCyberSessionBlockedForUserRequest(ctx, apiKeyID, 0, c, body, clientIP, userAgent)
}

// FindCyberSessionBlockedForUserRequest checks the authenticated user's shared
// blocks and legacy API-key blocks under one deadline. Legacy reads/writes stay
// compatible with records created before this version and with rollback.
func (s *OpenAIGatewayService) FindCyberSessionBlockedForUserRequest(ctx context.Context, apiKeyID, userID int64, c *gin.Context, body []byte, clientIP, userAgent string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, cyberSessionBlockLookupTimeout)
	defer cancel()
	enabled, _, err := s.CyberSessionBlockRuntime(ctx)
	if err != nil {
		return "", err
	}
	if !enabled {
		return "", nil
	}
	if ctx.Err() != nil {
		return "", ErrCyberSessionProtectionUnavailable
	}
	store := s.cyberSessionBlockStore()
	if store == nil {
		return "", ErrCyberSessionProtectionUnavailable
	}
	identities := make([]cyberSessionLookupIdentity, 0, 2)
	if userID > 0 {
		identities = append(identities, cyberSessionLookupIdentity{
			explicitKey: CyberSessionUserExplicitBlockKey(userID, c, body),
			scopeKey:    CyberSessionUserScopeKey(userID),
			transcript:  "user=" + strconv.FormatInt(userID, 10),
		})
	}
	identities = append(identities, cyberSessionLookupIdentity{
		explicitKey: CyberSessionExplicitBlockKey(apiKeyID, c, body),
		scopeKey:    CyberSessionScopeKey(apiKeyID, clientIP, userAgent),
		transcript:  "api_key=" + strconv.FormatInt(apiKeyID, 10),
	})
	explicitKeys := make([]string, 0, len(identities))
	for _, identity := range identities {
		if identity.explicitKey != "" {
			explicitKeys = append(explicitKeys, identity.explicitKey)
		}
	}
	if len(explicitKeys) > 0 {
		key, err := store.FindCyberSessionBlocked(ctx, explicitKeys)
		if err != nil {
			logger.LegacyPrintf("service.openai_gateway", "cyber explicit session read failed: err=%v", err)
			return "", ErrCyberSessionProtectionUnavailable
		}
		if ctx.Err() != nil {
			return "", ErrCyberSessionProtectionUnavailable
		}
		if key != "" {
			return key, nil
		}
	}
	for _, identity := range identities {
		key, err := findCyberSessionBlockedTranscript(ctx, store, identity, body)
		if err != nil || key != "" {
			return key, err
		}
	}
	return "", nil
}

func findCyberSessionBlockedTranscript(ctx context.Context, store CyberSessionBlockStore, identity cyberSessionLookupIdentity, body []byte) (string, error) {
	active, err := store.IsCyberSessionScopeActive(ctx, identity.scopeKey)
	if err != nil {
		logger.LegacyPrintf("service.openai_gateway", "cyber session scope read failed: err=%v", err)
		return "", ErrCyberSessionProtectionUnavailable
	}
	if ctx.Err() != nil {
		return "", ErrCyberSessionProtectionUnavailable
	}
	if !active {
		return "", nil
	}
	transcript := deriveOpenAICyberTranscriptBlockKeysForIdentity(identity.transcript, body)
	if ctx.Err() != nil {
		return "", ErrCyberSessionProtectionUnavailable
	}
	if transcript.lookupKeysTruncated {
		// Once the coarse scope is active, silently dropping old candidates would
		// let a blocked client evade prefix matching by appending dummy items.
		return cyberSessionTranscriptLookupOverflowBlockKey, nil
	}
	keys := transcript.lookupKeys
	if len(keys) == 0 {
		return "", nil
	}
	key, err := store.FindCyberSessionBlocked(ctx, keys)
	if err != nil {
		logger.LegacyPrintf("service.openai_gateway", "cyber session block batch read failed: err=%v", err)
		return "", ErrCyberSessionProtectionUnavailable
	}
	if ctx.Err() != nil {
		return "", ErrCyberSessionProtectionUnavailable
	}
	return key, nil
}
