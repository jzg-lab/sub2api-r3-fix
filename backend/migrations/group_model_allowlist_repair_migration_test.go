package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGroupModelAllowlistRepairMigration(t *testing.T) {
	content, err := FS.ReadFile("250_repair_group_models_list_config.sql")
	require.NoError(t, err)
	legacySQL := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, legacySQL, "ADD COLUMN IF NOT EXISTS models_list_config JSONB NOT NULL DEFAULT '{}'::jsonb")

	content, err = FS.ReadFile("253_group_model_allowlist_and_codex_manifest.sql")
	require.NoError(t, err)
	compatSQL := strings.Join(strings.Fields(string(content)), " ")
	// Keep the legacy column for rollback and synchronize writes between versions.
	require.Contains(t, compatSQL, "ALTER TABLE groups ADD COLUMN model_allowlist JSONB NOT NULL DEFAULT '{}'::jsonb")
	require.Contains(t, compatSQL, "SET model_allowlist = models_list_config")
	require.Contains(t, compatSQL, "CREATE OR REPLACE FUNCTION sync_group_model_allowlist_legacy()")
	require.Contains(t, compatSQL, "NEW.models_list_config := NEW.model_allowlist")
	require.Contains(t, compatSQL, "NEW.model_allowlist := NEW.models_list_config")
	require.Contains(t, compatSQL, "COMMENT ON COLUMN groups.model_allowlist")
	require.Contains(t, compatSQL, "DROP TRIGGER IF EXISTS groups_sync_model_allowlist_legacy ON groups")
}
