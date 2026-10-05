package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// r17al 滑误分层测试（2026-09-26 1187 案：rt4142 满血答错被单针熔断冤枉，
// two_dim 随机题健康带固有 ~18% 滑误率）。分层语义：
//   - 铁证（356 票 / 低 rt 答错 / 截断指纹答对）→ 单针杀不变（2026-09-22 裁定）
//   - 嫌疑（满血 rt≥1400 答错且无 356 票）→ 不熔断，两连错才熔断

func TestSuspectMissVerdict(t *testing.T) {
	// 1187 案原样：rt4142 答错、turn_state 780（健康凭据长）→ 嫌疑。
	suspect := OpenAIDowngradeProbeResult{
		TransportOK:     true,
		HTTPStatus:      200,
		AnswerCorrect:   false,
		ReasoningTokens: downgradeProbeIntPtr(4142),
		TurnStateLen:    780,
	}
	require.True(t, suspect.IsDegraded())
	require.True(t, suspect.IsSuspectMiss())

	// 低 rt 答错：降智形态学核心 → 铁证。
	lowRT := suspect
	lowRT.ReasoningTokens = downgradeProbeIntPtr(516)
	require.False(t, lowRT.IsSuspectMiss())

	// 356 票在场（即使满血答错）：铁证（双信号纪律）。
	ts356 := suspect
	ts356.TurnStateLen = 356
	require.False(t, ts356.IsSuspectMiss())

	// 健康带下缘以下（rt<1400）答错：铁证（保守带，维持原纪律）。
	edge := suspect
	edge.ReasoningTokens = downgradeProbeIntPtr(1399)
	require.False(t, edge.IsSuspectMiss())

	// 传输失败：既非降智也非嫌疑。
	transportFail := suspect
	transportFail.TransportOK = false
	require.False(t, transportFail.IsDegraded())
	require.False(t, transportFail.IsSuspectMiss())
}

func TestApplySuspectMissDoesNotSingleShotCircuit(t *testing.T) {
	state := OpenAIDowngradeProbeState{State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal"}
	suspect := OpenAIDowngradeProbeResult{
		TransportOK:     true,
		HTTPStatus:      200,
		AnswerCorrect:   false,
		ReasoningTokens: downgradeProbeIntPtr(4142),
		TurnStateLen:    780,
	}

	first := ApplyOpenAIDowngradeProbeResult(state, suspect, time.Now())
	require.False(t, first.Circuit, "满血答错单针不得熔断")
	require.Equal(t, OpenAIDowngradeStateOnDuty, first.NextState)
	require.Equal(t, 1, first.State.ConsecutiveFailures)
	require.Equal(t, 0, first.State.ConsecutiveSuccesses, "嫌疑针必须清连胜")

	// 第二针再嫌疑（滑误独立近似 3%）→ 两连熔断。
	second := ApplyOpenAIDowngradeProbeResult(first.State, suspect, time.Now())
	require.True(t, second.Circuit)
	require.Equal(t, OpenAIDowngradeStateCircuitOpen, second.NextState)

	// 复检答对（满血）→ 清败回 on_duty 干净状态。
	recovered := OpenAIDowngradeProbeResult{
		TransportOK:     true,
		HTTPStatus:      200,
		AnswerCorrect:   true,
		ReasoningTokens: downgradeProbeIntPtr(1513),
		TurnStateLen:    780,
	}
	cleared := ApplyOpenAIDowngradeProbeResult(first.State, recovered, time.Now())
	require.False(t, cleared.Circuit)
	require.Equal(t, 0, cleared.State.ConsecutiveFailures)
	require.Equal(t, 1, cleared.State.ConsecutiveSuccesses)
}

func TestApplySuspectThenHardEvidenceCircuits(t *testing.T) {
	// 嫌疑针不熔断，但留下的连败计数让紧随的铁证针立即熔断（failures=2）。
	state := OpenAIDowngradeProbeState{State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal"}
	suspect := OpenAIDowngradeProbeResult{
		TransportOK:     true,
		HTTPStatus:      200,
		AnswerCorrect:   false,
		ReasoningTokens: downgradeProbeIntPtr(4142),
		TurnStateLen:    780,
	}
	first := ApplyOpenAIDowngradeProbeResult(state, suspect, time.Now())
	require.False(t, first.Circuit)

	hard := OpenAIDowngradeProbeResult{
		TransportOK:     true,
		HTTPStatus:      200,
		AnswerCorrect:   false,
		ReasoningTokens: downgradeProbeIntPtr(516),
	}
	second := ApplyOpenAIDowngradeProbeResult(first.State, hard, time.Now())
	require.True(t, second.Circuit)
}

func TestApplyHardEvidenceSingleShotRegression(t *testing.T) {
	// 铁证形态单针杀回归（2026-09-22 裁定不动）。
	onDuty := OpenAIDowngradeProbeState{State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal"}

	// 低 rt 答错。
	lowRTWrong := OpenAIDowngradeProbeResult{
		TransportOK:     true,
		AnswerCorrect:   false,
		ReasoningTokens: downgradeProbeIntPtr(516),
	}
	require.True(t, ApplyOpenAIDowngradeProbeResult(onDuty, lowRTWrong, time.Now()).Circuit)

	// 正确低 rt 仅中性观察，不单独熔断。
	lowRTCorrect := lowRTWrong
	lowRTCorrect.AnswerCorrect = true
	lowRTCorrect.ReasoningTokens = downgradeProbeIntPtr(516)
	require.False(t, ApplyOpenAIDowngradeProbeResult(onDuty, lowRTCorrect, time.Now()).Circuit)

	// 356 票答对 + 满血 rt：Apply 层铁证（processState 侧另有 singleShot 复推）。
	ts356Correct := OpenAIDowngradeProbeResult{
		TransportOK:     true,
		HTTPStatus:      200,
		AnswerCorrect:   true,
		ReasoningTokens: downgradeProbeIntPtr(1513),
		TurnStateLen:    356,
	}
	require.True(t, ApplyOpenAIDowngradeProbeResult(onDuty, ts356Correct, time.Now()).Circuit)
}

func TestApplyCircuitStateSuspectMissNeedsTwoStrikes(t *testing.T) {
	// circuit_open 态复检：嫌疑单针不判死（等下一针），两连才 pending_replace。
	state := OpenAIDowngradeProbeState{
		State:               OpenAIDowngradeStateCircuitOpen,
		ProbeMode:           "normal",
		ConsecutiveFailures: 0,
	}
	suspect := OpenAIDowngradeProbeResult{
		TransportOK:     true,
		HTTPStatus:      200,
		AnswerCorrect:   false,
		ReasoningTokens: downgradeProbeIntPtr(4142),
		TurnStateLen:    780,
	}
	first := ApplyOpenAIDowngradeProbeResult(state, suspect, time.Now())
	require.False(t, first.NeedsReplacement, "熔断态嫌疑单针不得判死")
	require.Equal(t, OpenAIDowngradeStateCircuitOpen, first.NextState)

	second := ApplyOpenAIDowngradeProbeResult(first.State, suspect, time.Now())
	require.True(t, second.NeedsReplacement)
	require.Equal(t, OpenAIDowngradeStatePendingReplace, second.NextState)

	// 铁证在熔断态仍单针判死（原语义回归）。
	hard := OpenAIDowngradeProbeResult{
		TransportOK:     true,
		HTTPStatus:      200,
		AnswerCorrect:   false,
		ReasoningTokens: downgradeProbeIntPtr(516),
	}
	dead := ApplyOpenAIDowngradeProbeResult(state, hard, time.Now())
	require.True(t, dead.NeedsReplacement)
}
