package handler

import (
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// r17w 改动①回归：recordCyberPolicyIfMarked 的哈希参数是惰性闭包——
// cyber 未命中（mark == nil）时闭包绝不能被调用，命中时才调用一次。
func TestRecordCyberPolicyIfMarked_LazyHashNotEvaluatedWithoutMark(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &OpenAIGatewayHandler{}
	c := &gin.Context{} // 未 Set cyber mark → GetOpsCyberPolicy 返回 nil

	evaluated := 0
	h.recordCyberPolicyIfMarked(c, nil, nil, nil, "gpt-5", false, nil,
		service.ChannelUsageFields{}, func() string {
			evaluated++
			return "hash"
		})

	require.Equal(t, 0, evaluated, "cyber 未命中时哈希闭包不能被求值")
}

// nil 闭包安全：老调用形态（无哈希）传 nil 不得 panic。
func TestRecordCyberPolicyIfMarked_NilHashClosureSafe(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &OpenAIGatewayHandler{}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	require.NotPanics(t, func() {
		h.recordCyberPolicyIfMarked(c, nil, nil, nil, "gpt-5", false, nil,
			service.ChannelUsageFields{}, nil)
	})
}
