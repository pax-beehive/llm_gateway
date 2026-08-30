package metering

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
)

//go:embed migrations/000001_metering.sql
var migration001 string

//go:embed migrations/000002_correction_actor.sql
var migration002 string

func Migrate(ctx context.Context, database *sql.DB) error {
	if database == nil {
		return errors.New("Metering migration requires PostgreSQL")
	}
	for index, migration := range []string{migration001, migration002} {
		if _, err := database.ExecContext(ctx, migration); err != nil {
			return fmt.Errorf("migrate Metering step %d: %w", index+1, err)
		}
	}
	return nil
}
