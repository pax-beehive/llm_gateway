package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/toddzheng/llm-gateway/internal/controlrelay"
	"github.com/toddzheng/llm-gateway/internal/dbtransport"
	"github.com/toddzheng/llm-gateway/internal/operations"
)

type config struct {
	databaseURL string
	through     int64
	limit       int
	staleAfter  time.Duration
	devMode     bool
}

func main() {
	if err := run(context.Background()); err != nil {
		slog.Error("Control Event retention failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	configuration, err := configFromEnv()
	if err != nil {
		return err
	}
	if !configuration.devMode {
		if err := dbtransport.RequireAuthenticatedEncryption(configuration.databaseURL); err != nil {
			return fmt.Errorf("Control Event retention database transport: %w", err)
		}
	}
	database, err := sql.Open("pgx", configuration.databaseURL)
	if err != nil {
		return err
	}
	defer database.Close()
	connectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := database.PingContext(connectCtx); err != nil {
		return fmt.Errorf("connect Control Event retention database: %w", err)
	}
	var version int
	if err := database.QueryRowContext(ctx, `SELECT current_version FROM gateway_schema_metadata WHERE component='gateway'`).Scan(&version); err != nil {
		return fmt.Errorf("read Control Event retention schema version: %w", err)
	}
	if version != operations.CurrentDatabaseSchema {
		return fmt.Errorf("Control Event retention requires schema version %d, found %d", operations.CurrentDatabaseSchema, version)
	}
	retention, err := controlrelay.NewRetention(database, time.Now, configuration.staleAfter)
	if err != nil {
		return err
	}
	result, err := retention.PruneThrough(ctx, configuration.through, configuration.limit)
	if err != nil {
		return err
	}
	slog.Info("Control Event retention completed", "requested_through", result.RequestedThrough,
		"safe_through", result.SafeThrough, "minimum_cursor", result.MinimumCursor, "deleted", result.Deleted)
	return nil
}

func configFromEnv() (config, error) {
	databaseURL := strings.TrimSpace(os.Getenv("CONTROL_EVENT_RETENTION_DATABASE_URL"))
	throughValue := strings.TrimSpace(os.Getenv("CONTROL_EVENT_RETENTION_THROUGH"))
	if databaseURL == "" || throughValue == "" {
		return config{}, errors.New("CONTROL_EVENT_RETENTION_DATABASE_URL and CONTROL_EVENT_RETENTION_THROUGH are required")
	}
	if strings.TrimSpace(os.Getenv("CONTROL_EVENT_RETENTION_CONFIRM")) != "prune-control-events" {
		return config{}, errors.New("CONTROL_EVENT_RETENTION_CONFIRM=prune-control-events is required")
	}
	through, err := strconv.ParseInt(throughValue, 10, 64)
	if err != nil || through < 0 {
		return config{}, errors.New("CONTROL_EVENT_RETENTION_THROUGH must be a non-negative cursor")
	}
	limit := 1000
	if value := strings.TrimSpace(os.Getenv("CONTROL_EVENT_RETENTION_LIMIT")); value != "" {
		limit, err = strconv.Atoi(value)
		if err != nil || limit < 1 || limit > 10_000 {
			return config{}, errors.New("CONTROL_EVENT_RETENTION_LIMIT must be between 1 and 10000")
		}
	}
	staleAfter := 5 * time.Minute
	if value := strings.TrimSpace(os.Getenv("CONTROL_EVENT_RETENTION_GATEWAY_STALE_AFTER")); value != "" {
		staleAfter, err = time.ParseDuration(value)
		if err != nil || staleAfter < time.Minute || staleAfter > 24*time.Hour {
			return config{}, errors.New("CONTROL_EVENT_RETENTION_GATEWAY_STALE_AFTER must be between 1m and 24h")
		}
	}
	return config{databaseURL: databaseURL, through: through, limit: limit, staleAfter: staleAfter,
		devMode: os.Getenv("CONTROL_EVENT_RETENTION_DEV_MODE") == "true"}, nil
}
