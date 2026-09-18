//go:build unit

package service

import (
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

// 版本一致性锁定：UA 与 version 头是同一个版本声明的多个出口，任何一处各自硬编码
// 都会漂移成互相矛盾的身份（上游 v0.2.6 加了单处 contains 锁定；我们 fork 的 UA
// 形态嵌版本两处——首段 {originator}/{version} 与尾部 ({originator}; {version})——
// 两处都要锁，另锁运行时拼装出口与编译期常量等值）。
func TestCodexVersionConstants_Consistency(t *testing.T) {
	require.True(t, strings.HasPrefix(codexCLIUserAgent, openai.CodexDefaultOriginator+"/"+codexCLIVersion),
		"codexCLIUserAgent 首段必须嵌 codexCLIVersion")
	require.True(t, strings.Contains(codexCLIUserAgent, "("+openai.CodexDefaultOriginator+"; "+codexCLIVersion+")"),
		"codexCLIUserAgent 尾部 (originator; version) 组必须嵌同一 codexCLIVersion")

	require.True(t, strings.Contains(DefaultOpenAICodexUserAgent, codexCLIVersion),
		"DefaultOpenAICodexUserAgent must embed codexCLIVersion")

	// 运行时规范 UA 拼装（版本热更新路径）与编译期兜底常量必须逐字节一致：
	// 默认版本下两个出口是同一个身份。
	require.Equal(t, codexCLIUserAgent, buildCodexCLIUserAgent(codexCLIVersion),
		"buildCodexCLIUserAgent(codexCLIVersion) 必须与编译期 codexCLIUserAgent 完全一致")
}
