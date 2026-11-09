package schema_test

import (
	"context"
	"database/sql"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/enttest"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestGroupLongContextPricingSQLiteDefault(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	_, err = db.Exec("PRAGMA foreign_keys = ON")
	require.NoError(t, err)
	client := enttest.NewClient(t, enttest.WithOptions(ent.Driver(entsql.OpenDB(dialect.SQLite, db))))
	t.Cleanup(func() { _ = client.Close() })

	var defaultValue string
	err = db.QueryRow(`SELECT dflt_value FROM pragma_table_info('groups') WHERE name = 'long_context_pricing'`).Scan(&defaultValue)
	require.NoError(t, err)
	require.Equal(t, "'{}'", defaultValue)

	group, err := client.Group.Create().SetName(t.Name()).Save(context.Background())
	require.NoError(t, err)
	var stored string
	err = db.QueryRow(`SELECT long_context_pricing FROM groups WHERE id = ?`, group.ID).Scan(&stored)
	require.NoError(t, err)
	require.JSONEq(t, "{}", stored)
}
