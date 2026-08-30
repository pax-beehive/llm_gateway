package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/toddzheng/llm-gateway/internal/accessprojection"
	"github.com/toddzheng/llm-gateway/internal/controlrelay"
	"github.com/toddzheng/llm-gateway/internal/credentialadmin"
	"github.com/toddzheng/llm-gateway/internal/dbtransport"
	"github.com/toddzheng/llm-gateway/internal/metering"
	"github.com/toddzheng/llm-gateway/internal/migrations"
	"github.com/toddzheng/llm-gateway/internal/operations"
	"github.com/toddzheng/llm-gateway/internal/providerconnection"
	"github.com/toddzheng/llm-gateway/internal/routingcatalog"
	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

type configuration struct {
	databaseURL      string
	cloudSQLInstance string
}

func main() {
	if err := run(); err != nil {
		slog.Error("schema migration failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	configuration, err := configurationFromEnvironment()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	if err := dbtransport.RequireAuthenticatedTransport(configuration.databaseURL, configuration.cloudSQLInstance); err != nil {
		return fmt.Errorf("schema migration database transport: %w", err)
	}
	database, cleanupTransport, err := dbtransport.Open(ctx, configuration.databaseURL, configuration.cloudSQLInstance)
	if err != nil {
		return err
	}
	defer cleanupTransport()
	defer database.Close()
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)

	if err := database.PingContext(ctx); err != nil {
		return fmt.Errorf("connect PostgreSQL: %w", err)
	}
	if err := acquireMigrationLock(ctx, database); err != nil {
		return err
	}
	defer releaseMigrationLock(database)

	steps := []struct {
		name string
		run  func(context.Context, *sql.DB) error
	}{
		{name: "gateway", run: migrations.Migrate},
		{name: "tenant administration", run: tenantadmin.Migrate},
		{name: "Gateway API key administration", run: credentialadmin.Migrate},
		{name: "Provider Connection control plane", run: providerconnection.Migrate},
		{name: "Routing Catalog", run: routingcatalog.Migrate},
		{name: "Operations", run: operations.Migrate},
		{name: "Gateway Access Projection", run: accessprojection.Migrate},
		{name: "Gateway Provider Connection Projection", run: providerconnection.MigrateGatewayProjection},
		{name: "Control Event relay", run: controlrelay.Migrate},
		{name: "Metering", run: metering.Migrate},
	}
	for _, step := range steps {
		if err := step.run(ctx, database); err != nil {
			return fmt.Errorf("migrate %s: %w", step.name, err)
		}
		slog.Info("schema migration step complete", "component", step.name)
	}
	return verifySchema(ctx, database)
}

func configurationFromEnvironment() (configuration, error) {
	if os.Getenv("SCHEMA_MIGRATION_CONFIRM") != "apply" {
		return configuration{}, errors.New("SCHEMA_MIGRATION_CONFIRM=apply is required")
	}
	databaseURL := strings.TrimSpace(os.Getenv("ADMIN_DATABASE_URL"))
	if databaseURL == "" {
		return configuration{}, errors.New("ADMIN_DATABASE_URL is required")
	}
	return configuration{
		databaseURL:      databaseURL,
		cloudSQLInstance: strings.TrimSpace(os.Getenv("SCHEMA_MIGRATION_CLOUD_SQL_INSTANCE")),
	}, nil
}

func acquireMigrationLock(ctx context.Context, database *sql.DB) error {
	if _, err := database.ExecContext(ctx, `SELECT pg_advisory_lock(hashtext('llm_gateway_schema_migration'))`); err != nil {
		return fmt.Errorf("acquire schema migration lock: %w", err)
	}
	return nil
}

func releaseMigrationLock(database *sql.DB) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := database.ExecContext(ctx, `SELECT pg_advisory_unlock(hashtext('llm_gateway_schema_migration'))`); err != nil {
		slog.Warn("release schema migration lock", "error", err)
	}
}

func verifySchema(ctx context.Context, database *sql.DB) error {
	var gatewayVersion int
	if err := database.QueryRowContext(ctx, `SELECT current_version FROM gateway_schema_metadata WHERE component='gateway'`).Scan(&gatewayVersion); err != nil {
		return fmt.Errorf("verify Gateway schema: %w", err)
	}
	if gatewayVersion != operations.CurrentDatabaseSchema {
		return fmt.Errorf("Gateway schema version %d does not match binary version %d", gatewayVersion, operations.CurrentDatabaseSchema)
	}
	var operationsVersion int
	if err := database.QueryRowContext(ctx, `SELECT current_version FROM operations_schema_metadata WHERE component='control-plane'`).Scan(&operationsVersion); err != nil {
		return fmt.Errorf("verify Operations schema: %w", err)
	}
	var meteringReady bool
	if err := database.QueryRowContext(ctx, `SELECT to_regclass('metering_inbox') IS NOT NULL`).Scan(&meteringReady); err != nil {
		return fmt.Errorf("verify Metering schema: %w", err)
	}
	if !meteringReady {
		return errors.New("Metering schema is missing")
	}
	slog.Info("schema migration verified", "gateway_schema", gatewayVersion, "operations_schema", operationsVersion)
	return nil
}
