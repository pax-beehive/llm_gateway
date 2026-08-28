// Package migrations owns the ordered Gateway PostgreSQL schema. Individual
// persistence adapters own data access; deployment composition owns when this
// sequence is executed.
package migrations

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
)

//go:embed sql/*.sql
var files embed.FS

func Migrate(ctx context.Context, database *sql.DB) error {
	if database == nil {
		return fmt.Errorf("Gateway migrations require PostgreSQL")
	}
	entries, err := fs.ReadDir(files, "sql")
	if err != nil {
		return fmt.Errorf("read embedded Gateway migrations: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		payload, err := files.ReadFile("sql/" + entry.Name())
		if err != nil {
			return fmt.Errorf("read Gateway migration %s: %w", entry.Name(), err)
		}
		if _, err := database.ExecContext(ctx, string(payload)); err != nil {
			return fmt.Errorf("apply Gateway migration %s: %w", entry.Name(), err)
		}
	}
	return nil
}
