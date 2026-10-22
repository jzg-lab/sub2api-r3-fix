package deployment

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGatePauseResume(t *testing.T) {
	var gate Gate
	_, ok := gate.Enter()
	require.False(t, ok)
	gate.SetOpen(true)
	done, ok := gate.Enter()
	require.True(t, ok)
	gate.SetOpen(false)
	_, ok = gate.Enter()
	require.False(t, ok)
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	require.ErrorIs(t, gate.Wait(ctx), context.DeadlineExceeded)
	done()
	done()
	require.Zero(t, gate.Pending())
	require.NoError(t, gate.Wait(context.Background()))
	gate.SetOpen(true)
	done, ok = gate.Enter()
	require.True(t, ok)
	done()
}
