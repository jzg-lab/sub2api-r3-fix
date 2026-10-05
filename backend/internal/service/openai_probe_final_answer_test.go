package service

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenAIProbeFinalAnswerVotes(t *testing.T) {
	for _, tc := range []struct {
		text                         string
		tokens                       int
		inconclusive, degraded, pass bool
	}{
		{"21", 300, false, false, false},
		{"21", 516, false, false, false},
		{"推导21\nFINAL_ANSWER: 22", 300, false, true, false},
		{"不是21", 1500, true, false, false},
		{"FINAL_ANSWER: 21\nFINAL_ANSWER: 22", 1500, true, false, false},
		{"推导中有22\nFINAL_ANSWER: 21", 1500, false, false, true},
	} {
		t.Run(tc.text+fmt.Sprint(tc.tokens), func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":%q}]}],"usage":{"output_tokens_details":{"reasoning_tokens":%d}}}`, tc.text, tc.tokens))
			result := OpenAIDowngradeProbeResult{HTTPStatus: 200}
			result.applyResponse(body, openAIDowngradeNumericAnswerPattern(21))
			require.True(t, result.TransportOK)
			require.Equal(t, tc.inconclusive, result.AnswerInconclusive)
			require.Equal(t, tc.degraded, result.IsDegraded())
			require.Equal(t, tc.pass, result.IsQualificationPass())
			if tc.inconclusive {
				require.Nil(t, result.AnswerVerdict())
			}
			state := OpenAIDowngradeProbeState{State: OpenAIDowngradeStateOnDuty, ConsecutiveSuccesses: 2}
			transition := ApplyOpenAIDowngradeProbeResult(state, result, time.Now())
			if !tc.pass && !tc.degraded {
				require.False(t, transition.Recovered)
				require.False(t, transition.Circuit)
				require.Zero(t, transition.State.ConsecutiveSuccesses)
			}
			result.TurnStateLen = 356
			require.True(t, result.IsDegraded(), "preserve previously approved independent header rule")
		})
	}
}

func TestExtractOpenAIProbeFinalAnswer(t *testing.T) {
	cases := []struct {
		text, want string
		known      bool
	}{
		{"21", "21", true}, {"最少取出 21 个", "21", true}, {"答案：21", "21", true},
		{"推导中用到21，但结论是22。\nFINAL_ANSWER: 22", "22", true},
		{"不能保证21，因此取22。", "", false},
		{"21或22", "", false}, {"21.5", "", false}, {"-21", "", false},
		{"推导里出现21，但没有结论", "", false},
		{"FINAL_ANSWER: 21\nFINAL_ANSWER: 22", "", false},
		{"FINAL_ANSWER: 21\n结论还不确定", "", false},
		{"推导21\nFINAL_ANSWER: 21\n", "21", true},
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			got, known := extractOpenAIProbeFinalAnswer(tc.text)
			if got != tc.want || known != tc.known {
				t.Fatalf("got (%q,%v), want (%q,%v)", got, known, tc.want, tc.known)
			}
		})
	}
}
