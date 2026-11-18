package service

import (
	"bytes"
	"strings"
)

const geminiSSEFallbackBodyLimit = 1 << 20

type geminiSSEFallbackBody struct {
	buf       bytes.Buffer
	truncated bool
}

func (b *geminiSSEFallbackBody) AddLine(line string) {
	if b == nil || b.truncated {
		return
	}
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, ":") {
		return
	}
	if b.buf.Len()+len(trimmed)+1 > geminiSSEFallbackBodyLimit {
		b.truncated = true
		return
	}
	_, _ = b.buf.WriteString(trimmed)
	_ = b.buf.WriteByte('\n')
}

func (b *geminiSSEFallbackBody) Bytes() []byte {
	if b == nil {
		return nil
	}
	return b.buf.Bytes()
}

func (b *geminiSSEFallbackBody) Truncated() bool {
	return b != nil && b.truncated
}
