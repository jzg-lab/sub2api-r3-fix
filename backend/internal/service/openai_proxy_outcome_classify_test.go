package service

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClassifyOpenAIDowngradeProxyOutcome(t *testing.T) {
	cases := []struct {
		name   string
		result *OpenAIDowngradeProbeResult
		want   string
	}{
		{"nil result", nil, OpenAIProxyOutcomeInconclusive},
		{"unauthorized", &OpenAIDowngradeProbeResult{HTTPStatus: http.StatusUnauthorized}, OpenAIProxyOutcomeAuthError},
		{"forbidden", &OpenAIDowngradeProbeResult{HTTPStatus: http.StatusForbidden}, OpenAIProxyOutcomeAuthError},
		{
			"transport failure is proxy-attributable",
			&OpenAIDowngradeProbeResult{ErrorMessage: "probe transport failed"},
			OpenAIProxyOutcomeNetworkError,
		},
		{
			"token failure happened before proxy contact",
			&OpenAIDowngradeProbeResult{ErrorMessage: "access token unavailable"},
			OpenAIProxyOutcomeInconclusive,
		},
		{
			"missing local dependencies is not proxy evidence",
			&OpenAIDowngradeProbeResult{ErrorMessage: "probe dependencies unavailable"},
			OpenAIProxyOutcomeInconclusive,
		},
		{"rate limited is inconclusive", &OpenAIDowngradeProbeResult{HTTPStatus: http.StatusTooManyRequests}, OpenAIProxyOutcomeInconclusive},
		{"server error is inconclusive", &OpenAIDowngradeProbeResult{HTTPStatus: http.StatusInternalServerError}, OpenAIProxyOutcomeInconclusive},
		{
			"wrong answer with truncation fingerprint is degraded",
			&OpenAIDowngradeProbeResult{TransportOK: true, AnswerCorrect: false, ReasoningTokens: downgradeProbeIntPtr(516)},
			OpenAIProxyOutcomeDegraded,
		},
		{
			"correct answer with starved reasoning is inconclusive",
			&OpenAIDowngradeProbeResult{TransportOK: true, AnswerCorrect: true, ReasoningTokens: downgradeProbeIntPtr(600)},
			OpenAIProxyOutcomeInconclusive,
		},
		{
			"recovered probe is a success",
			&OpenAIDowngradeProbeResult{TransportOK: true, AnswerCorrect: true, ReasoningTokens: downgradeProbeIntPtr(OpenAIDowngradeRecoveryReasoningMinimum)},
			OpenAIProxyOutcomeSuccess,
		},
		{
			"mid-range tokens prove nothing either way",
			&OpenAIDowngradeProbeResult{TransportOK: true, AnswerCorrect: true, ReasoningTokens: downgradeProbeIntPtr(1000)},
			OpenAIProxyOutcomeInconclusive,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, ClassifyOpenAIDowngradeProxyOutcome(tc.result))
		})
	}
}
