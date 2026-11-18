package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAIAPIKeyLongContextDefaultMigration(t *testing.T) {
	content, err := FS.ReadFile("272_default_openai_api_key_long_context_billing.sql")
	require.NoError(t, err)

	var statements []string
	for _, line := range strings.Split(string(content), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			statements = append(statements, line)
		}
	}
	sql := strings.Join(strings.Fields(strings.Join(statements, " ")), " ")
	require.Contains(t, sql, "WHERE platform = 'openai' AND type = 'apikey'")
	require.Contains(t, sql, "extra->'openai_long_context_billing_enabled' = 'false'::jsonb")
	require.NotContains(t, sql, "type = 'oauth'")
	require.NotContains(t, sql, "type = 'setup-token'")
}
