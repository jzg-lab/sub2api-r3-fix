package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidOpenAIAuthBrowserState(t *testing.T) {
	valid := strings.Repeat("0123456789abcdef", 4)
	require.True(t, validOpenAIAuthBrowserState(valid))

	for _, state := range []string{
		"",
		strings.Repeat("a", openAIAuthBrowserStateLength-1),
		strings.Repeat("a", openAIAuthBrowserStateLength+1),
		strings.Repeat("A", openAIAuthBrowserStateLength),
		strings.Repeat("g", openAIAuthBrowserStateLength),
		strings.Repeat("a", openAIAuthBrowserStateLength-1) + "/",
	} {
		require.False(t, validOpenAIAuthBrowserState(state), state)
	}
}

func TestValidOpenAIAuthBrowserCodeVerifier(t *testing.T) {
	valid := strings.Repeat("0123456789abcdef", 8)
	require.True(t, validOpenAIAuthBrowserCodeVerifier(valid))

	for _, verifier := range []string{
		"",
		strings.Repeat("a", openAIAuthBrowserCodeVerifierLength-1),
		strings.Repeat("a", openAIAuthBrowserCodeVerifierLength+1),
		strings.Repeat("A", openAIAuthBrowserCodeVerifierLength),
		strings.Repeat("g", openAIAuthBrowserCodeVerifierLength),
		strings.Repeat("a", openAIAuthBrowserCodeVerifierLength-1) + "/",
	} {
		require.False(t, validOpenAIAuthBrowserCodeVerifier(verifier), verifier)
	}
}
