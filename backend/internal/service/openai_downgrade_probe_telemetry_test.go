package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 探针遥测默认必须关闭：2026-09-18 生产实证探针遥测与探针共用上游 h2 连接时，
// 服务端按连接降级对待探针（200 全长流缺 usage）。全局遥测开关打开也不得让
// 探针遥测随之生效，必须叠加 OPENAI_PROBE_TELEMETRY=1 显式开启。
func TestOpenAIProbeTelemetryDisabledByDefault(t *testing.T) {
	t.Setenv("OPENAI_CODEX_TELEMETRY_TEST_ENABLE", "1") // 测试进程里放行全局闸
	t.Setenv("OPENAI_PROBE_TELEMETRY", "")
	require.False(t, openAIProbeTelemetryEnabled(), "探针遥测必须默认关闭")

	t.Setenv("OPENAI_PROBE_TELEMETRY", "0")
	require.False(t, openAIProbeTelemetryEnabled())

	t.Setenv("OPENAI_PROBE_TELEMETRY", "1")
	require.True(t, openAIProbeTelemetryEnabled(), "双开关显式开启时应放行")
}

// 全局遥测关闭时，探针专用开关无论取值都不得生效。
func TestOpenAIProbeTelemetryFollowsGlobalKillSwitch(t *testing.T) {
	t.Setenv("OPENAI_CODEX_TELEMETRY_ENABLED", "0")
	t.Setenv("OPENAI_PROBE_TELEMETRY", "1")
	require.False(t, openAIProbeTelemetryEnabled(), "全局闸关闭必须一票否决")
}
