package service

import (
	"context"
	"errors"
	"strings"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// ValidateOpenAIWSContinuation applies the HTTP continuation ownership policy
// before any upstream routing or per-turn payload normalization.
func (s *OpenAIGatewayService) ValidateOpenAIWSContinuation(ctx context.Context, groupID int64, payload []byte, userID, apiKeyID int64) error {
	responseID := strings.TrimSpace(gjson.GetBytes(payload, "previous_response_id").String())
	if responseID == "" {
		return nil
	}
	allowed, err := s.ValidateOpenAIHTTPResponseOwner(ctx, groupID, responseID, userID, apiKeyID)
	if err != nil && !errors.Is(err, ErrStickySessionNotFound) {
		return NewOpenAIWSClientCloseError(coderws.StatusInternalError, "continuation ownership unavailable", err)
	}
	if !allowed {
		return NewOpenAIWSClientCloseError(coderws.StatusPolicyViolation, "previous_response_id is unavailable for this user and group", nil)
	}
	return nil
}

// Each relay has one upstream-to-client writer. Persist ownership before the
// first visible ID, including its original upstream account, then refresh at
// completion without writing on every token.
func (s *OpenAIGatewayService) newOpenAIWSResponseOwnerBinder(ctx context.Context, c *gin.Context, accountID int64) func([]byte) error {
	var owner openAIHTTPResponseOwner
	if c != nil {
		raw, _ := c.Get(openAIHTTPResponseOwnerContextKey)
		owner, _ = raw.(openAIHTTPResponseOwner)
	}
	groupID := getOpenAIGroupIDFromContext(c)
	lastBoundID := ""
	return func(payload []byte) error {
		if owner.userID <= 0 || owner.apiKeyID <= 0 {
			return nil
		}
		eventType := gjson.GetBytes(payload, "type").String()
		if !strings.HasPrefix(eventType, "response.") {
			return nil
		}
		responseID := strings.TrimSpace(gjson.GetBytes(payload, "response.id").String())
		if responseID == "" {
			responseID = strings.TrimSpace(gjson.GetBytes(payload, "response_id").String())
		}
		if responseID == "" || (responseID == lastBoundID && !isOpenAIWSTerminalEvent(eventType)) {
			return nil
		}
		if accountID <= 0 {
			return NewOpenAIWSClientCloseError(coderws.StatusInternalError, "continuation account binding unavailable", nil)
		}
		if err := s.BindOpenAIHTTPResponseOwner(ctx, groupID, responseID, owner.userID, owner.apiKeyID); err != nil {
			return NewOpenAIWSClientCloseError(coderws.StatusInternalError, "continuation ownership unavailable", err)
		}
		if err := s.getOpenAIWSStateStore().BindResponseAccount(ctx, groupID, responseID, accountID, s.openAIWSResponseStickyTTL()); err != nil {
			return NewOpenAIWSClientCloseError(coderws.StatusInternalError, "continuation account binding unavailable", err)
		}
		lastBoundID = responseID
		return nil
	}
}
