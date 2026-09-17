package service

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

// HTTPUpstream 上游 HTTP 请求接口
// 用于向上游 API（Claude、OpenAI、Gemini 等）发送请求
type HTTPUpstream interface {
	// Do 执行 HTTP 请求（不启用 TLS 指纹）
	Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error)

	// DoWithTLS 执行带 TLS 指纹伪装的 HTTP 请求
	//
	// profile 参数:
	//   - nil: 不启用 TLS 指纹，行为与 Do 方法相同
	//   - non-nil: 使用指定的 Profile 进行 TLS 指纹伪装
	//
	// Profile 由调用方通过 TLSFingerprintProfileService 解析后传入，
	// 支持按账号绑定的数据库 profile 或内置默认 profile。
	DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, profile *tlsfingerprint.Profile) (*http.Response, error)

	// DoProbeWithTLS 执行探针请求的一次性专用传输：不进入按账号缓存的共享连接
	// 池，响应体关闭后回收传输。真实 codex exec 单轮 turn 本就是每进程一条新
	// 连接；探针与真实 turn 同连接复用会被服务端按连接降级（2026-09-18 生产
	// 实证，详见 repository 实现注释），探针必须与共享池隔离。
	DoProbeWithTLS(req *http.Request, proxyURL string, accountConcurrency int, profile *tlsfingerprint.Profile) (*http.Response, error)
}
