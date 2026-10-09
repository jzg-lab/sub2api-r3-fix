package service

import (
	"context"
	"errors"
	"github.com/stretchr/testify/require"
	"net/http"
	"testing"
)

func TestAPIKeyRouteNormalization(t *testing.T) {
	primary := int64(2)
	ids, first, err := normalizeAPIKeyGroupIDs(&primary, []int64{2, 1})
	require.NoError(t, err)
	require.Equal(t, []int64{2, 1}, ids)
	require.Equal(t, primary, *first)
	for _, ids := range [][]int64{{}, {0, 1}, {-1}, {2, 2}, {1, 2}, {2, 1, 3, 4, 5, 6, 7, 8, 9, 10, 11}} {
		_, _, err := normalizeAPIKeyGroupIDs(&primary, ids)
		require.Error(t, err)
	}
	_, first, err = normalizeAPIKeyGroupIDs(&primary, nil)
	require.NoError(t, err)
	require.Equal(t, primary, *first)
	ids, first, err = normalizeAPIKeyGroupIDs(nil, nil)
	require.NoError(t, err)
	require.Nil(t, ids)
	require.Nil(t, first)
}

func TestSmartRouteEndpointBoundaries(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/responses", "/backend-api/codex/responses", "/v1/messages", "/v1/chat/completions"} {
		require.True(t, APIKeySmartRouteEndpoint("POST", path))
	}
	for _, path := range []string{"/v1/responses/compact", "/v1/responses/resp_1/cancel", "/v1/images/generations", "/v1beta/models/gemini:generateContent", "/v1/live"} {
		require.False(t, APIKeySmartRouteEndpoint("POST", path))
	}
}

func TestSmartRouteWebSocketDialSafety(t *testing.T) {
	for _, status := range []int{0, 429, 502, 503, 504} {
		require.NotNil(t, SmartRouteWebSocketDialFailure(&openAIWSDialError{StatusCode: status, Err: errors.New("dial failed")}))
	}
	for _, status := range []int{400, 401, 403, 404, 426} {
		require.Nil(t, SmartRouteWebSocketDialFailure(&openAIWSDialError{StatusCode: status}))
	}
	require.Nil(t, SmartRouteWebSocketDialFailure(&openAIWSDialError{StatusCode: http.StatusBadGateway, Err: context.Canceled}))
	require.Nil(t, SmartRouteWebSocketDialFailure(errors.New("stream interrupted after output")))
}
