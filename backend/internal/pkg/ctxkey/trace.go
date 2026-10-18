package ctxkey

import (
	"context"

	"github.com/google/uuid"
)

// TraceID accepts only UUID correlation IDs, never arbitrary context strings.
func TraceID(ctx context.Context, key Key) string {
	if ctx == nil || (key != RequestID && key != ClientRequestID) {
		return ""
	}
	value, _ := ctx.Value(key).(string)
	if len(value) != 36 {
		return ""
	}
	if _, err := uuid.Parse(value); err != nil {
		return ""
	}
	return value
}
