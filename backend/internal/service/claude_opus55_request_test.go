package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateClaudeOpus55Request(t *testing.T) {
	for _, body := range []string{
		`{"thinking":{"type":"enabled"}}`,
		`{"thinking":{"type":"disabled"}}`,
		`{"tool_choice":"required"}`,
		`{"tool_choice":{"type":"any"}}`,
		`{"tool_choice":{"type":"tool","name":"lookup"}}`,
		`{"tool_choice":{"type":"function","name":"lookup"}}`,
	} {
		require.Error(t, validateClaudeOpus55Request([]byte(body), "claude-opus-5-5"))
	}

	for _, body := range []string{
		`{}`,
		`{"thinking":{"type":"adaptive"},"output_config":{"effort":"max"}}`,
		`{"tool_choice":"auto"}`,
		`{"tool_choice":{"type":"auto"}}`,
		`{"thinking":{"type":"enabled"},"tool_choice":{"type":"tool","name":"lookup"}}`,
	} {
		err := validateClaudeOpus55Request([]byte(body), "claude-opus-5")
		require.NoError(t, err)
	}
}
