package schema

import (
	"encoding/json"
	"testing"

	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql/schema"
	"github.com/Wei-Shaw/sub2api/ent/migrate"
	"github.com/stretchr/testify/require"
)

func TestGroupLongContextPricingSchemaMatchesMigration(t *testing.T) {
	foundField := false
	for _, field := range (Group{}).Fields() {
		descriptor := field.Descriptor()
		if descriptor.Name != "long_context_pricing" {
			continue
		}
		foundField = true
		require.False(t, descriptor.Optional, "migration 267 requires NOT NULL")
		encoded, err := json.Marshal(descriptor.Default)
		require.NoError(t, err)
		require.JSONEq(t, "{}", string(encoded), "omitted configuration must inherit model thresholds")
	}
	require.True(t, foundField)

	foundColumn := false
	for _, column := range migrate.GroupsColumns {
		if column.Name != "long_context_pricing" {
			continue
		}
		foundColumn = true
		require.False(t, column.Nullable, "generated schema must preserve migration 267's NOT NULL")
		require.Equal(t, map[string]schema.Expr{
			dialect.Postgres: "'{}'::jsonb",
			dialect.SQLite:   "'{}'",
		}, column.Default, "generated defaults must preserve migration 267 and SQLite compatibility")
	}
	require.True(t, foundColumn)
}
