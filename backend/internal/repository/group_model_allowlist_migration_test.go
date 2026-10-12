package repository

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestGroupModelAllowlistMigrationPreservesRollbackAndClears(t *testing.T) {
	db := newProbePostgresWithMigrations(t, nil)
	_, err := db.ExecContext(t.Context(), `CREATE TABLE groups (id BIGINT PRIMARY KEY, models_list_config JSONB NOT NULL DEFAULT '{}');
		INSERT INTO groups VALUES (1, '{"enabled":true,"models":["old-model"]}'), (2, '{}')`)
	require.NoError(t, err)
	script, err := migrations.FS.ReadFile("253_group_model_allowlist_and_codex_manifest.sql")
	require.NoError(t, err)
	apply := func() {
		_, err := db.ExecContext(t.Context(), string(script))
		require.NoError(t, err)
	}
	check := func(id int, expected string) {
		var canonical, legacy string
		require.NoError(t, db.QueryRowContext(t.Context(), "SELECT model_allowlist, models_list_config FROM groups WHERE id=$1", id).Scan(&canonical, &legacy))
		require.JSONEq(t, expected, canonical)
		require.JSONEq(t, expected, legacy)
	}
	apply()
	check(1, `{"enabled":true,"models":["old-model"]}`)
	check(2, `{}`)
	_, err = db.ExecContext(t.Context(), `UPDATE groups SET model_allowlist='{}' WHERE id=1`)
	require.NoError(t, err)
	apply()
	check(1, `{}`)
	_, err = db.ExecContext(t.Context(), `UPDATE groups SET models_list_config='{"enabled":true,"models":["rollback-edit"]}' WHERE id=1;
		INSERT INTO groups (id, models_list_config) VALUES (3, '{"enabled":true,"models":["old-binary"]}');
		INSERT INTO groups (id, model_allowlist) VALUES (4, '{"enabled":true,"models":["new-binary"]}')`)
	require.NoError(t, err)
	apply()
	check(1, `{"enabled":true,"models":["rollback-edit"]}`)
	check(3, `{"enabled":true,"models":["old-binary"]}`)
	check(4, `{"enabled":true,"models":["new-binary"]}`)
}
