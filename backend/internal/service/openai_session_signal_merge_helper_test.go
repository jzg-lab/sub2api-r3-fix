//go:build unit

package service

import (
	"net/http"
	"net/http/httptest"

	"github.com/gin-gonic/gin"
)

// 测试助手：构造带 header 的 gin.Context（session-id 等 explicit 信号用）。
func newTestGinContextWithHeaders(hdr map[string]string) *gin.Context {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	c.Request = req
	return c
}

// 测试助手：带空 Request 的 context（body-only 信号用例；
// GetHeader 需要 c.Request 非 nil，不能真用 nil Request）。
func ginContextNoRequest() *gin.Context {
	return newTestGinContextWithHeaders(nil)
}
