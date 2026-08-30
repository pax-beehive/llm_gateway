package providerconnection

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"sort"
)

//go:embed migrations/*.sql gateway_migrations/*.sql
var migrationFiles embed.FS

func Migrate(ctx context.Context, database *sql.DB) error {
	entries, err := migrationFiles.ReadDir("migrations")
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
		payload, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		if _, err := database.ExecContext(ctx, string(payload)); err != nil {
			return fmt.Errorf("apply Provider Connection migration %s: %w", name, err)
		}
	}
	return nil
}

func MigrateGatewayProjection(ctx context.Context, database *sql.DB) error {
	if database == nil {
		return errors.New("Gateway Provider Connection projection migrations require PostgreSQL")
	}
	payload, err := migrationFiles.ReadFile("gateway_migrations/000001_provider_connection_projection.sql")
	if err != nil {
		return err
	}
	if _, err := database.ExecContext(ctx, string(payload)); err != nil {
		return fmt.Errorf("apply Gateway Provider Connection projection migration: %w", err)
	}
	return nil
}
