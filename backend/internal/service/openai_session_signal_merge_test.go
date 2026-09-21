//go:build unit

package service

import (
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newSessionSignalService(t *testing.T) *OpenAIGatewayService {
	t.Helper()
	// 会话信号三方法只读 cfg（MaxLineSize 等），零值 service 即可；
	// 不走多参构造器。
	return &OpenAIGatewayService{}
}

// r17w 改动⑤回归：合并形态与旧"GenerateSessionHash + ExtractSessionID
// 双调用"在三种信号形态下返回值完全一致——explicit header、body
// prompt_cache_key、无信号（content-seed 只进 hash 不进 sessionID）。
func TestGenerateSessionHashAndSessionID_MatchesLegacyPair(t *testing.T) {
	svc := newSessionSignalService(t)
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name string
		body []byte
		hdr  map[string]string
	}{
		{"explicit header", []byte(`{"model":"gpt-5"}`), map[string]string{"session-id": "sess-1"}},
		{"body prompt_cache_key", []byte(`{"prompt_cache_key":"pk-9","model":"gpt-5"}`), nil},
		{"no signal", []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`), nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// header 用例经 http.Request 注入；body-only 用例用无 Request 的 context。
			mergedCtx := ginContextNoRequest()
			pairCtx := ginContextNoRequest()
			if len(tc.hdr) > 0 {
				mergedCtx = newTestGinContextWithHeaders(tc.hdr)
				pairCtx = newTestGinContextWithHeaders(tc.hdr)
			}
			hashM, idM := svc.GenerateSessionHashAndSessionID(mergedCtx, tc.body)
			hashL := svc.GenerateSessionHash(pairCtx, tc.body)
			idL := svc.ExtractSessionID(pairCtx, tc.body)
			require.Equal(t, hashL, hashM, "sessionHash 必须与旧形态一致")
			require.Equal(t, idL, idM, "sessionID 必须与旧形态一致")
		})
	}
}

// ExtractSessionID 语义钉住：只返回 explicit 信号，不做 content-seed 兜底。
func TestExtractSessionID_NoContentSeedFallback(t *testing.T) {
	svc := newSessionSignalService(t)
	c := ginContextNoRequest()
	body := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hello"}]}`)
	require.Empty(t, svc.ExtractSessionID(c, body), "无 explicit 信号时 sessionID 必须为空")
	hash := svc.GenerateSessionHash(c, body)
	require.NotEmpty(t, hash, "content-seed 兜底只属于 hash 侧")
}
