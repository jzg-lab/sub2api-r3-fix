package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

const MaxAPIKeyRouteGroups = 10

func (k *APIKey) CandidateGroupIDs() []int64 {
	if k == nil {
		return nil
	}
	if len(k.RouteGroupIDs) > 0 {
		return append([]int64(nil), k.RouteGroupIDs...)
	}
	if k.GroupID != nil && *k.GroupID > 0 {
		return []int64{*k.GroupID}
	}
	return nil
}

func (k *APIKey) HasSmartRoutes() bool { return k != nil && len(k.RouteGroupIDs) > 1 }

// Only these entry points defer group admission to the route selector.
func APIKeySmartRouteEndpoint(method, path string) bool {
	path = strings.TrimRight(path, "/")
	if method == http.MethodGet {
		switch path {
		case "/models", "/v1/models", "/backend-api/codex/models", "/responses", "/v1/responses", "/backend-api/codex/responses":
			return true
		}
	}
	if method != http.MethodPost {
		return false
	}
	switch path {
	case "/v1/messages", "/chat/completions", "/v1/chat/completions", "/responses", "/v1/responses", "/backend-api/codex/responses":
		return true
	}
	return false
}

func normalizeAPIKeyGroupIDs(primary *int64, ids []int64) ([]int64, *int64, error) {
	if ids == nil {
		if primary == nil {
			return nil, nil, nil
		}
		ids = []int64{*primary}
	}
	if len(ids) == 0 || len(ids) > MaxAPIKeyRouteGroups {
		return nil, nil, infraerrors.BadRequest("API_KEY_GROUP_IDS_INVALID", "group_ids must contain between 1 and 10 groups")
	}
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id <= 0 || seen[id] {
			return nil, nil, infraerrors.BadRequest("API_KEY_GROUP_IDS_INVALID", "group_ids must contain unique positive IDs")
		}
		seen[id] = true
	}
	if primary != nil && *primary != ids[0] {
		return nil, nil, infraerrors.BadRequest("API_KEY_GROUP_ID_MISMATCH", "group_id must match the first group_ids entry")
	}
	first := ids[0]
	return append([]int64(nil), ids...), &first, nil
}

func (s *APIKeyService) validateAPIKeyGroupRoutes(ctx context.Context, user *User, primary *int64, ids []int64) ([]int64, *int64, error) {
	ids, primary, err := normalizeAPIKeyGroupIDs(primary, ids)
	if err != nil {
		return nil, nil, err
	}
	for _, id := range ids {
		group, err := s.groupRepo.GetByID(ctx, id)
		if err != nil {
			return nil, nil, fmt.Errorf("get group: %w", err)
		}
		if !group.IsActive() || !s.canUserBindGroup(ctx, user, group) {
			return nil, nil, ErrGroupNotAllowed
		}
	}
	// A legacy single-group key needs no separate route list.
	if len(ids) <= 1 {
		ids = nil
	}
	return ids, primary, nil
}

// APIKeyForRoute returns a request-owned snapshot; auth-cache objects are never mutated.
func (s *APIKeyService) APIKeyForRoute(ctx context.Context, key *APIKey, groupID int64) (*APIKey, error) {
	if key == nil || key.User == nil {
		return nil, ErrGroupNotAllowed
	}
	bound := false
	for _, id := range key.CandidateGroupIDs() {
		if id == groupID {
			bound = true
			break
		}
	}
	if !bound {
		return nil, ErrGroupNotAllowed
	}
	group, err := s.groupRepo.GetByIDLite(ctx, groupID)
	if err != nil {
		return nil, err
	}
	if group == nil || !IsGroupContextValid(group) || !group.IsActive() {
		return nil, ErrGroupNotAllowed
	}
	if !group.IsSubscriptionType() && !key.User.CanBindGroup(group.ID, group.IsExclusive) {
		return nil, ErrGroupNotAllowed
	}
	routed := *key
	user := *key.User
	// The auth snapshot's override belongs to the primary group.
	user.UserGroupRPMOverride, user.UserGroupRPMOverrideChecked = nil, false
	routed.User, routed.Group, routed.GroupID = &user, group, &group.ID
	return &routed, nil
}

func ContextWithAPIKeyRoute(ctx context.Context, key *APIKey) context.Context {
	return context.WithValue(ctx, ctxkey.Group, key.Group)
}

type apiKeyRouteFailureCache interface {
	FailedAPIKeyRouteGroups(context.Context, int64, string, []int64) (map[int64]bool, error)
	MarkAPIKeyRouteFailed(context.Context, int64, string, int64) error
}

type apiKeyRouteSessionCache interface {
	APIKeyRouteSession(context.Context, int64, string, string) (int64, error)
	RememberAPIKeyRouteSession(context.Context, int64, string, string, int64) error
}

func (s *APIKeyService) OrderAPIKeyRoutes(ctx context.Context, key *APIKey, scope string, sessions ...string) []int64 {
	ids := key.CandidateGroupIDs()
	ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	cache, ok := s.cache.(apiKeyRouteFailureCache)
	if !ok {
		return ids
	}
	failed, err := cache.FailedAPIKeyRouteGroups(ctx, key.ID, scope, ids)
	if err != nil {
		logger.LegacyPrintf("service.api_key_routes", "read failure preferences: %v", err)
		return ids
	}
	ordered := make([]int64, 0, len(ids))
	for _, id := range ids {
		if !failed[id] {
			ordered = append(ordered, id)
		}
	}
	for _, id := range ids {
		if failed[id] {
			ordered = append(ordered, id)
		}
	}
	if sessionCache, ok := s.cache.(apiKeyRouteSessionCache); ok && len(sessions) > 0 && sessions[0] != "" {
		if groupID, err := sessionCache.APIKeyRouteSession(ctx, key.ID, scope, sessions[0]); err == nil && !failed[groupID] {
			for i, id := range ordered {
				if id == groupID {
					copy(ordered[1:i+1], ordered[:i])
					ordered[0] = id
					break
				}
			}
		}
	}
	return ordered
}

func (s *APIKeyService) RememberAPIKeyRouteSession(ctx context.Context, key *APIKey, scope, session string) {
	if key == nil || key.GroupID == nil || session == "" || ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if cache, ok := s.cache.(apiKeyRouteSessionCache); ok {
		_ = cache.RememberAPIKeyRouteSession(ctx, key.ID, scope, session, *key.GroupID)
	}
}

func (s *APIKeyService) MarkAPIKeyRouteFailed(ctx context.Context, key *APIKey, scope string) {
	if key == nil || key.GroupID == nil || ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if cache, ok := s.cache.(apiKeyRouteFailureCache); ok {
		if err := cache.MarkAPIKeyRouteFailed(ctx, key.ID, scope, *key.GroupID); err != nil {
			logger.LegacyPrintf("service.api_key_routes", "write failure preference: %v", err)
		}
	}
}

// All attempts of one client request share the global RPM admission.
type APIKeyRouteAdmission struct {
	mu             sync.Mutex
	once           sync.Once
	err            error
	groupRPM       map[int64]error
	failedAccounts map[int64]struct{}
	canFailover    bool
}
type apiKeyRouteAdmissionKey struct{}

func WithAPIKeyRouteAdmission(ctx context.Context, canFailover ...bool) context.Context {
	enabled := len(canFailover) == 0 || canFailover[0]
	return context.WithValue(ctx, apiKeyRouteAdmissionKey{}, &APIKeyRouteAdmission{canFailover: enabled, groupRPM: make(map[int64]error), failedAccounts: make(map[int64]struct{})})
}

func HasAPIKeyRouteAdmission(ctx context.Context) bool {
	state, ok := ctx.Value(apiKeyRouteAdmissionKey{}).(*APIKeyRouteAdmission)
	return ok && state.canFailover
}

func FailAPIKeyRouteAccount(ctx context.Context, accountID int64) {
	if state, ok := ctx.Value(apiKeyRouteAdmissionKey{}).(*APIKeyRouteAdmission); ok && accountID > 0 {
		state.mu.Lock()
		state.failedAccounts[accountID] = struct{}{}
		state.mu.Unlock()
	}
}

func APIKeyRouteExcludedAccounts(ctx context.Context, excluded map[int64]struct{}) map[int64]struct{} {
	state, ok := ctx.Value(apiKeyRouteAdmissionKey{}).(*APIKeyRouteAdmission)
	if !ok {
		return excluded
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.failedAccounts) == 0 {
		return excluded
	}
	merged := make(map[int64]struct{}, len(excluded)+len(state.failedAccounts))
	for id := range excluded {
		merged[id] = struct{}{}
	}
	for id := range state.failedAccounts {
		merged[id] = struct{}{}
	}
	return merged
}

// Preserve model mappings and passthroughs; vendor cards do not define protocol compatibility.
func (s *GatewayService) SmartRouteModelCompatible(ctx context.Context, key *APIKey, endpoint, model string) bool {
	group := key.Group
	if group == nil {
		return false
	}
	if group.Platform == PlatformComposite {
		return true
	}
	if endpoint == "messages" && (group.Platform == PlatformOpenAI || group.Platform == PlatformGrok || IsCNProvider(group.Platform)) {
		// The Messages bridge enforces this group's existing dispatch mappings.
		return true
	}
	mapping, restricted := s.ResolveChannelMappingAndRestrict(ctx, key.GroupID, model)
	if restricted {
		return false
	}
	requested := model
	if mapping.Mapped && mapping.MappedModel != "" {
		requested = mapping.MappedModel
	}
	detected, known := DetectModelPlatform(requested)
	if !known || detected == group.Platform || (group.Platform == PlatformAntigravity && (detected == PlatformAnthropic || detected == PlatformGemini)) {
		return true
	}
	if s.accountRepo == nil {
		return false
	}
	for _, candidate := range s.GetAvailableModels(ctx, key.GroupID, group.Platform) {
		if strings.EqualFold(candidate, model) || strings.EqualFold(candidate, requested) || matchWildcard(candidate, model) || matchWildcard(candidate, requested) {
			return true
		}
	}
	return false
}

// Locate existing response affinity among this key's routes before selecting a provider.
func (s *OpenAIGatewayService) SmartRouteResponseGroup(ctx context.Context, key *APIKey, responseID string, requireOwner bool) (int64, error) {
	for _, id := range key.CandidateGroupIDs() {
		if requireOwner {
			owned, err := s.ValidateOpenAIHTTPResponseOwner(ctx, id, responseID, key.UserID, key.ID)
			if err != nil {
				return 0, err
			}
			if owned {
				return id, nil
			}
		} else {
			accountID, err := s.getOpenAIWSStateStore().GetResponseAccount(ctx, id, responseID)
			if err != nil {
				return 0, err
			}
			if accountID > 0 {
				return id, nil
			}
		}
	}
	return 0, nil
}

// A failed WebSocket handshake has not sent the model request to the provider.
func SmartRouteWebSocketDialFailure(err error) *UpstreamFailoverError {
	var dial *openAIWSDialError
	if !errors.As(err, &dial) || errors.Is(err, context.Canceled) {
		return nil
	}
	status := dial.StatusCode
	if status == 0 {
		status = http.StatusBadGateway
	}
	if status != http.StatusTooManyRequests && status < http.StatusInternalServerError {
		return nil
	}
	return newOpenAIUpstreamFailoverError(status, dial.ResponseHeaders, dial.ResponseBody, "", false)
}
