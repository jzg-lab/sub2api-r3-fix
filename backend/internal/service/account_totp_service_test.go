package service

import (
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestAccountTOTPInput(t *testing.T) {
	// Public RFC 6238 vector.
	const fixture = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	normalized, err := NormalizeAccountTOTP("gez dgnbv-gy3tqojqgezdgnbvgy3tqojq====")
	require.NoError(t, err)
	require.Equal(t, fixture, normalized)
	for _, invalid := range []string{"", "123456", "otpauth://totp/example", "AAAAAAAAAAAAAAAAB", "AAAA=AAAAAAAAAAA", strings.Repeat("A", 257)} {
		_, err := NormalizeAccountTOTP(invalid)
		require.Error(t, err)
	}
	secret, alias, conflict := fixture, strings.ToLower(fixture), "JBSWY3DPEHPK3PXP"
	revision := "oauth-v1:" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		in    AccountTOTPInput
		valid bool
	}{
		{AccountTOTPInput{ExpectedRevision: revision}, true},
		{AccountTOTPInput{ExpectedRevision: revision, TOTPSecret: &secret, MFASecret: &alias}, true},
		{AccountTOTPInput{ExpectedRevision: revision, TOTPSecret: &secret, MFASecret: &conflict}, false},
		{AccountTOTPInput{ExpectedRevision: revision, TOTPSecret: &secret, Clear: true}, false},
		{AccountTOTPInput{ExpectedRevision: revision, Clear: true}, true},
		{AccountTOTPInput{TOTPSecret: &secret}, false},
	} {
		_, err := tc.in.secret()
		require.Equal(t, tc.valid, err == nil)
	}
}
