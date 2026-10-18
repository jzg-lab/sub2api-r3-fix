package ctxkey

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTraceIDRejectsUnboundedOrUnrelatedValues(t *testing.T) {
	const id = "e2d5e88a-84cb-46ee-83e3-586131ca5745"
	require.Empty(t, TraceID(nil, RequestID))
	for _, key := range []Key{RequestID, ClientRequestID} {
		for _, value := range []string{id, strings.ToUpper(id)} {
			require.Equal(t, value, TraceID(context.WithValue(t.Context(), key, value), key))
		}
		for _, value := range []any{"", "fixture-private-marker", "Bearer fixture",
			strings.Repeat("x", 4096), strings.ReplaceAll(id, "-", ""), " " + id,
			"xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx", 42} {
			require.Empty(t, TraceID(context.WithValue(t.Context(), key, value), key))
		}
	}
	require.Empty(t, TraceID(context.WithValue(t.Context(), UserID, id), UserID))
}
