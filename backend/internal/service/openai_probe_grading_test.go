package service

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAIProbeFullTextNumericGrading(t *testing.T) {
	for _, tc := range []struct {
		text    string
		correct bool
	}{
		{"21", true},
		{"推导中用到21，但结论是22。\nFINAL_ANSWER: 22", true},
		{"不是21", true},
		{"FINAL_ANSWER: 21\nFINAL_ANSWER: 22", true},
		{"无法确定答案", false},
		{"121", false},
		{"22", false},
	} {
		t.Run(tc.text, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":%q}]}],"usage":{"output_tokens_details":{"reasoning_tokens":1500}}}`, tc.text))
			result := OpenAIDowngradeProbeResult{HTTPStatus: 200}
			result.applyResponse(body, openAIDowngradeNumericAnswerPattern(21))
			require.True(t, result.TransportOK)
			require.NotNil(t, result.AnswerVerdict())
			require.Equal(t, tc.correct, *result.AnswerVerdict())
			require.Equal(t, !tc.correct, result.IsDegraded())
			parsed, _, _ := parseOpenAIDowngradeProbeResponse(body, openAIDowngradeNumericAnswerPattern(21))
			require.Equal(t, tc.correct, parsed)
		})
	}
}
