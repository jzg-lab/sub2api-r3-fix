package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"testing"
	"testing/fstest"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestDeploymentBootstrapNeverWrites(t *testing.T) {
	for _, mode := range []string{"valid", "missing", "mismatch", "baseline"} {
		t.Run(mode, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			baseline := 1
			if mode == "baseline" {
				baseline = 0
			}
			mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM atlas_schema_revisions")).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(baseline))
			if baseline != 0 {
				sum := sha256.Sum256([]byte("SELECT 1;"))
				value := hex.EncodeToString(sum[:])
				if mode == "mismatch" {
					value = "invalid"
				}
				rows := sqlmock.NewRows([]string{"checksum"})
				if mode != "missing" {
					rows.AddRow(value)
				}
				mock.ExpectQuery(regexp.QuoteMeta("SELECT checksum FROM schema_migrations WHERE filename = $1")).WithArgs("001.sql").WillReturnRows(rows)
			}
			err = verifyDeploymentMigrations(context.Background(), db, fstest.MapFS{"001.sql": {Data: []byte("SELECT 1;\n")}})
			if mode == "valid" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestDeploymentBootstrapRequiresExistingSecret(t *testing.T) {
	t.Setenv("DEPLOYMENT_SOCKET", "/private/control.sock")
	client := newSecuritySecretTestClient(t)
	cfg := &config.Config{}
	err := ensureBootstrapSecrets(context.Background(), client, cfg)
	require.ErrorContains(t, err, "persisted JWT")
	count, err := client.SecuritySecret.Query().Count(context.Background())
	require.NoError(t, err)
	require.Zero(t, count)
	secret := "existing-jwt-secret-32bytes-long!!!!"
	_, err = client.SecuritySecret.Create().SetKey(securitySecretKeyJWT).SetValue(secret).Save(context.Background())
	require.NoError(t, err)
	require.NoError(t, ensureBootstrapSecrets(context.Background(), client, cfg))
	require.Equal(t, secret, cfg.JWT.Secret)
	cfg.JWT.Secret = "different-jwt-secret-32bytes-long!!!"
	require.ErrorContains(t, ensureBootstrapSecrets(context.Background(), client, cfg), "differs")
}
