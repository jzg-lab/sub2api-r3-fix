package antibypass

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPolicyDefaultsAndBounds(t *testing.T) {
	p, err := ParsePolicy("")
	require.NoError(t, err)
	require.Equal(t, DefaultConfig(), p.Config())
	for _, raw := range []string{"null", "{}", "{", `{"rpm_limit":0}`} {
		_, err := ParsePolicy(raw)
		require.Error(t, err)
	}
	var fields map[string]int
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &fields))
	for key, original := range fields {
		for _, invalid := range []int{-1, 0, 1_000_000} {
			fields[key] = invalid
			raw, err := json.Marshal(fields)
			require.NoError(t, err)
			_, err = ParsePolicy(string(raw))
			require.Error(t, err, key)
		}
		fields[key] = original
	}
	p.RPMLimit, p.MaxConcurrent, p.MaxDistinctKeys = 240, 64, 16
	require.NoError(t, p.Validate())
	require.Equal(t, 240, p.Config().RPMLimit)
	require.Equal(t, 64, p.Config().MaxConcurrent)
	require.Equal(t, 16, p.Config().MaxDistinctKeys)
}
