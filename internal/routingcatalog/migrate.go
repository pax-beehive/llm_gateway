package routingcatalog

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"sort"
)

//go:embed migrations/*.sql
var managedMigrationFiles embed.FS

func Migrate(ctx context.Context, database *sql.DB) error {
	if database == nil {
		return errors.New("Routing Catalog migrations require PostgreSQL")
	}
	entries, err := managedMigrationFiles.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		payload, err := managedMigrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		if _, err := database.ExecContext(ctx, string(payload)); err != nil {
			return fmt.Errorf("apply Routing Catalog migration %s: %w", name, err)
		}
	}
	return nil
}
