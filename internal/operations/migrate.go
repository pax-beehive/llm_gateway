package operations

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
)

//go:embed migrations/000001_operations.sql
var migration string

func Migrate(ctx context.Context, database *sql.DB) error {
	if database == nil {
		return errors.New("Operations migration requires PostgreSQL")
	}
	if _, err := database.ExecContext(ctx, migration); err != nil {
		return fmt.Errorf("migrate Operations: %w", err)
	}
	return nil
}
