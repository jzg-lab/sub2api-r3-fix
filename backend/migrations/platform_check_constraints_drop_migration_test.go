package migrations

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDropPlatformCheckConstraintsMigration(t *testing.T) {
	content, err := FS.ReadFile("257_platform_catalog_checks.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql,
		"ALTER TABLE user_platform_quotas DROP CONSTRAINT IF EXISTS user_platform_quotas_platform_check;")
	require.Contains(t, sql,
		"ALTER TABLE composite_model_routes DROP CONSTRAINT IF EXISTS composite_model_routes_target_platform_check;")
	require.NotContains(t, sql, "ADD CONSTRAINT user_platform_quotas_platform_check")
	require.NotContains(t, sql, "ADD CONSTRAINT composite_model_routes_target_platform_check")
	require.Contains(t, sql, "'minimax', 'opencode_go'")
}

// Platform membership is validated against the application catalog after 242.
// A later migration must not restore a database whitelist that can drift from it.
func TestLaterMigrationsDoNotRestorePlatformCheckConstraints(t *testing.T) {
	const droppedAt = "257_platform_catalog_checks.sql"
	entries, err := fs.ReadDir(FS, ".")
	require.NoError(t, err)
	comments := regexp.MustCompile(`(?ms)/\*.*?\*/|--[^\n]*`)
	addConstraint := regexp.MustCompile(`(?i)\bADD\s+CONSTRAINT\s+"?(user_platform_quotas_platform_check|composite_model_routes_target_platform_check)\b`)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") || entry.Name() <= droppedAt {
			continue
		}
		t.Run(entry.Name(), func(t *testing.T) {
			content, err := FS.ReadFile(entry.Name())
			require.NoError(t, err)
			sql := comments.ReplaceAll(content, nil)
			require.Nil(t, addConstraint.Find(sql), "platform CHECK constraints removed in 242 must remain managed by application validation")
		})
	}
}
