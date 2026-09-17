//go:build unit

package service

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeAccountConcurrencyUsesLocalPolicy(t *testing.T) {
	for _, platform := range []string{PlatformOpenAI, PlatformGrok, PlatformAnthropic, PlatformGemini, "future-platform"} {
		for _, accountType := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
			for _, concurrency := range []int{-5, 0, 1, 10, 50, 100, 1000} {
				t.Run(fmt.Sprintf("%s/%s/%d", platform, accountType, concurrency), func(t *testing.T) {
					require.Equal(t, 50, normalizeAccountConcurrency(platform, accountType, concurrency))
				})
			}
		}
	}
}
