package tenantadmin

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
)

//go:embed migrations/000001_tenant_admin.sql
var migration string

func Migrate(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return fmt.Errorf("tenant administration migration requires PostgreSQL")
	}
	if _, err := db.ExecContext(ctx, migration); err != nil {
		return fmt.Errorf("migrate tenant administration: %w", err)
	}
	return nil
}
