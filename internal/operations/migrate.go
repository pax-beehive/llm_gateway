package operations

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
)

//go:embed migrations/000001_operations.sql
var migrationOne string

//go:embed migrations/000002_metering_observations.sql
var migrationTwo string

func Migrate(ctx context.Context, database *sql.DB) error {
	if database == nil {
		return errors.New("Operations migration requires PostgreSQL")
	}
	if _, err := database.ExecContext(ctx, migrationOne); err != nil {
		return fmt.Errorf("migrate Operations: %w", err)
	}
	if _, err := database.ExecContext(ctx, migrationTwo); err != nil {
		return fmt.Errorf("migrate Operations: %w", err)
	}
	return nil
}
