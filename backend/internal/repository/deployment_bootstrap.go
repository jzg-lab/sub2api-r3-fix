package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"strings"
)

// A standby may inspect an installed database, never create or repair it.
func verifyDeploymentMigrations(ctx context.Context, db *sql.DB, source fs.FS) error {
	var baseline int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM atlas_schema_revisions").Scan(&baseline); err != nil {
		return err
	}
	if baseline == 0 {
		return fmt.Errorf("deployment requires an existing migration baseline")
	}
	files, err := fs.Glob(source, "*.sql")
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("deployment migration manifest is empty")
	}
	for _, name := range files {
		data, err := fs.ReadFile(source, name)
		if err != nil {
			return err
		}
		content := strings.TrimSpace(string(data))
		if content == "" {
			continue
		}
		sum := sha256.Sum256([]byte(content))
		want := hex.EncodeToString(sum[:])
		var got string
		err = db.QueryRowContext(ctx, "SELECT checksum FROM schema_migrations WHERE filename = $1", name).Scan(&got)
		if errors.Is(err, sql.ErrNoRows) {
			if alias, ok := migrationFilenameAliases[name]; ok && want == alias.checksum {
				err = db.QueryRowContext(ctx, "SELECT checksum FROM schema_migrations WHERE filename = $1", alias.filename).Scan(&got)
			}
		}
		if err != nil {
			return fmt.Errorf("deployment requires applied migration %s: %w", name, err)
		}
		if got != want && !isMigrationChecksumCompatible(name, got, want) {
			return fmt.Errorf("deployment migration checksum mismatch: %s", name)
		}
	}
	return nil
}
