package service

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIWSResponseOwnerBinder(t *testing.T) {
	ctx := context.Background()
	probe := &ownerPersistenceProbe{t: t}
	svc := &OpenAIGatewayService{cache: probe}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	groupID := int64(81)
	c.Set("api_key", &APIKey{GroupID: &groupID})
	SetOpenAIHTTPResponseOwner(c, 91, 101)
	bind := svc.newOpenAIWSResponseOwnerBinder(ctx, c, 111)
	// Capture the authenticated identity once, independent of later turn state.
	SetOpenAIHTTPResponseOwner(c, 92, 102)
	require.NoError(t, bind([]byte(`{"type":"response.created","response":{"id":"resp_bound"}}`)))
	require.Equal(t, 1, probe.setCalls)
	allowed, err := svc.ValidateOpenAIHTTPResponseOwner(ctx, groupID, "resp_bound", 91, 103)
	require.NoError(t, err)
	require.True(t, allowed)
	accountID, err := svc.getOpenAIWSStateStore().GetResponseAccount(ctx, groupID, "resp_bound")
	require.NoError(t, err)
	require.EqualValues(t, 111, accountID)
	// A new service instance must recover the original account from shared storage.
	reader := &OpenAIGatewayService{cache: probe}
	accountID, err = reader.getOpenAIWSStateStore().GetResponseAccount(ctx, groupID, "resp_bound")
	require.NoError(t, err)
	require.EqualValues(t, 111, accountID)
	for i := 0; i < 100; i++ {
		require.NoError(t, bind([]byte(`{"type":"response.output_text.delta","response_id":"resp_bound","delta":"x"}`)))
	}
	require.Equal(t, 1, probe.setCalls, "token events must not add a Redis write per token")
	require.NoError(t, bind([]byte(`{"type":"response.completed","response":{"id":"resp_bound"}}`)))
	require.Equal(t, 2, probe.setCalls, "terminal event refreshes ownership TTL")
	require.NoError(t, bind([]byte(`{"type":"response.done","response":{"id":"resp_next"}}`)))
	require.Equal(t, 3, probe.setCalls, "direct terminal response must also persist ownership")
}

func TestOpenAIWSResponseOwnerBinderFailureDoesNotCacheSuccess(t *testing.T) {
	ctx := context.Background()
	unavailable := errors.New("owner storage unavailable")
	probe := &ownerPersistenceProbe{t: t, failAt: 1, err: unavailable}
	svc := &OpenAIGatewayService{cache: probe}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	SetOpenAIHTTPResponseOwner(c, 91, 101)
	bind := svc.newOpenAIWSResponseOwnerBinder(ctx, c, 111)
	payload := []byte(`{"type":"response.created","response":{"id":"resp_retry"}}`)
	err := bind(payload)
	require.ErrorIs(t, err, unavailable)
	var closeErr *OpenAIWSClientCloseError
	require.ErrorAs(t, err, &closeErr)
	require.Equal(t, coderws.StatusInternalError, closeErr.StatusCode())
	require.NoError(t, bind(payload))
	require.Equal(t, 2, probe.setCalls)
}

func TestOpenAIWSContinuationFailsClosed(t *testing.T) {
	ctx := context.Background()
	svc := &OpenAIGatewayService{}
	require.NoError(t, svc.BindOpenAIHTTPResponseOwner(ctx, 81, "resp_owned", 91, 101))
	require.NoError(t, svc.ValidateOpenAIWSContinuation(ctx, 81, []byte(`{"previous_response_id":"resp_owned"}`), 91, 102))
	require.NoError(t, svc.ValidateOpenAIWSContinuation(ctx, 81, []byte(`{"input":"new"}`), 91, 101))
	for _, tc := range []struct {
		name, response string
		group, user    int64
	}{
		{"other_user", "resp_owned", 81, 92},
		{"other_group", "resp_owned", 82, 91},
		{"unknown", "resp_unknown", 81, 91},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := svc.ValidateOpenAIWSContinuation(ctx, tc.group, []byte(`{"previous_response_id":"`+tc.response+`"}`), tc.user, 101)
			var closeErr *OpenAIWSClientCloseError
			require.ErrorAs(t, err, &closeErr)
			require.Equal(t, coderws.StatusPolicyViolation, closeErr.StatusCode())
		})
	}
	store := svc.getOpenAIWSStateStore().(*defaultOpenAIWSStateStore)
	store.responseOwnerMu.Lock()
	bindingKey := openAIWSResponseAccountMapKey(81, "resp_owned")
	binding := store.responseOwners[bindingKey]
	binding.expiresAt = time.Now().Add(-time.Second)
	store.responseOwners[bindingKey] = binding
	store.responseOwnerMu.Unlock()
	var closeErr *OpenAIWSClientCloseError
	require.ErrorAs(t, svc.ValidateOpenAIWSContinuation(ctx, 81, []byte(`{"previous_response_id":"resp_owned"}`), 91, 101), &closeErr)
	require.Equal(t, coderws.StatusPolicyViolation, closeErr.StatusCode())

	unavailable := &OpenAIGatewayService{cache: &openAIWSStateStoreTimeoutProbeCache{}}
	require.ErrorAs(t, unavailable.ValidateOpenAIWSContinuation(ctx, 81, []byte(`{"previous_response_id":"resp_owned"}`), 91, 101), &closeErr)
	require.Equal(t, coderws.StatusInternalError, closeErr.StatusCode())
}

type wsResponseAccountFailureCache struct {
	stubGatewayCache
}

func (c *wsResponseAccountFailureCache) SetSessionAccountID(context.Context, int64, string, int64, time.Duration) error {
	return errors.New("account binding storage unavailable")
}

func TestOpenAIWSResponseOwnerBinderRequiresAccountPersistence(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	SetOpenAIHTTPResponseOwner(c, 91, 101)
	for _, accountID := range []int64{0, 111} {
		svc := &OpenAIGatewayService{cache: &wsResponseAccountFailureCache{}}
		bind := svc.newOpenAIWSResponseOwnerBinder(context.Background(), c, accountID)
		var closeErr *OpenAIWSClientCloseError
		err := bind([]byte(`{"type":"response.created","response":{"id":"resp_account_failure"}}`))
		require.ErrorAs(t, err, &closeErr)
		require.Equal(t, coderws.StatusInternalError, closeErr.StatusCode())
	}
}
