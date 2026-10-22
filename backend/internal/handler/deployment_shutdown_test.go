package handler

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestDeploymentWaitIncludesDetachedIdentitySync(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	runDingTalkSyncAsync(t.Context(), func(context.Context) { close(entered); <-release })
	<-entered
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	err := service.WaitDeploymentAsync(ctx)
	close(release)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	ctx, cancel = context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, service.WaitDeploymentAsync(ctx))
}
