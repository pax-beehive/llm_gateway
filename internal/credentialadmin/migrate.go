package credentialadmin

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
)

//go:embed migrations/000001_gateway_credentials.sql
var migration string

func Migrate(ctx context.Context, database *sql.DB) error {
	if database == nil {
		return errors.New("Gateway Credential Administration migration requires PostgreSQL")
	}
	if _, err := database.ExecContext(ctx, migration); err != nil {
		return fmt.Errorf("migrate Gateway Credential Administration: %w", err)
	}
	return nil
}
