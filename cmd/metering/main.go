package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/toddzheng/llm-gateway/internal/controlapi"
	"github.com/toddzheng/llm-gateway/internal/dbtransport"
	"github.com/toddzheng/llm-gateway/internal/metering"
	"github.com/toddzheng/llm-gateway/internal/telemetry"
)

func main() {
	if err := run(); err != nil {
		slog.Error("Metering stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	shutdownTelemetry, err := telemetry.Configure(ctx, envOr("OTEL_SERVICE_NAME", "llm-gateway-metering"))
	if err != nil {
		return err
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = shutdownTelemetry(shutdownCtx)
	}()
	devMode := os.Getenv("METERING_DEV_MODE") == "true"
	databaseURL := strings.TrimSpace(os.Getenv("METERING_DATABASE_URL"))
	if databaseURL == "" {
		return errors.New("METERING_DATABASE_URL is required")
	}
	if !devMode {
		if os.Getenv("METERING_DATABASE_TRANSPORT_ATTESTATION") != "authenticated-encrypted" {
			return errors.New("production Metering requires authenticated database transport attestation")
		}
		if err := dbtransport.RequireAuthenticatedEncryption(databaseURL); err != nil {
			return err
		}
	}
	database, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return err
	}
	defer database.Close()
	database.SetMaxOpenConns(20)
	database.SetMaxIdleConns(5)
	connectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := database.PingContext(connectCtx); err != nil {
		return err
	}
	if !devMode {
		var role string
		if err := database.QueryRowContext(connectCtx, `SELECT current_user`).Scan(&role); err != nil {
			return err
		}
		if expected := strings.TrimSpace(os.Getenv("METERING_DB_ROLE")); expected == "" || role != expected {
			return errors.New("Metering database role does not match METERING_DB_ROLE")
		}
	}
	if os.Getenv("METERING_MIGRATE") == "true" {
		if !devMode {
			return errors.New("METERING_MIGRATE is development-only")
		}
		if err := metering.Migrate(ctx, database); err != nil {
			return err
		}
	}
	service, err := metering.NewService(database, time.Now)
	if err != nil {
		return err
	}
	if os.Getenv("METERING_BOOTSTRAP_LEDGER") == "true" {
		for {
			count, err := service.BackfillGatewayLedger(ctx, 1000)
			if err != nil {
				return fmt.Errorf("bootstrap Metering from Gateway ledger: %w", err)
			}
			if count == 0 {
				break
			}
		}
		if _, err := service.Rebuild(ctx); err != nil {
			return fmt.Errorf("rebuild Metering after bootstrap: %w", err)
		}
		return nil
	}
	verifier, err := configureVerifier(devMode)
	if err != nil {
		return err
	}
	exportDirectory := strings.TrimSpace(os.Getenv("METERING_EXPORT_DIRECTORY"))
	if exportDirectory == "" {
		if !devMode {
			return errors.New("METERING_EXPORT_DIRECTORY is required")
		}
		exportDirectory = "/tmp/llm-gateway-metering-exports"
	}
	exportStore, err := metering.NewFileExportStore(exportDirectory)
	if err != nil {
		return err
	}
	signingKey := []byte(os.Getenv("METERING_EXPORT_SIGNING_KEY"))
	if devMode && len(signingKey) == 0 {
		signingKey = []byte("local-development-metering-export-signing-key")
	}
	if len(signingKey) < 32 {
		return errors.New("METERING_EXPORT_SIGNING_KEY must contain at least 32 bytes")
	}
	handler, err := metering.NewHandler(service, verifier, exportStore, signingKey, time.Now)
	if err != nil {
		return err
	}
	go runWorkers(ctx, service, exportStore, envOr("METERING_WORKER_ID", "metering-local"))
	server := &http.Server{Addr: envOr("METERING_ADDR", ":8082"), Handler: otelhttp.NewHandler(handler, "metering.http"), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 20}
	errorsChannel := make(chan error, 1)
	go func() { errorsChannel <- server.ListenAndServe() }()
	select {
	case err := <-errorsChannel:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	}
}

func configureVerifier(devMode bool) (metering.IdentityVerifier, error) {
	if devMode {
		token := envOr("METERING_DEV_TOKEN", "local-metering-admin-token")
		return metering.IdentityVerifierFunc(func(_ context.Context, authorization string) (metering.Identity, error) {
			presented := strings.TrimPrefix(authorization, "Bearer ")
			if subtle.ConstantTimeCompare([]byte(presented), []byte(token)) != 1 {
				return metering.Identity{}, errors.New("invalid token")
			}
			return metering.Identity{ActorID: "local-metering-admin", Scopes: []string{metering.ScopePlatformRead, metering.ScopePlatformWrite}}, nil
		}), nil
	}
	verifier, err := controlapi.NewJWKSVerifier(controlapi.JWKSVerifierConfig{URL: os.Getenv("METERING_IAM_JWKS_URL"), Issuer: os.Getenv("METERING_IAM_ISSUER"), Audience: os.Getenv("METERING_IAM_AUDIENCE"), Now: time.Now})
	if err != nil {
		return nil, err
	}
	return metering.IdentityVerifierFunc(func(ctx context.Context, authorization string) (metering.Identity, error) {
		identity, err := verifier.Verify(ctx, authorization)
		return metering.Identity{ActorID: identity.ActorID, TenantID: identity.ActingTenantID, Scopes: identity.Scopes}, err
	}), nil
}

func runWorkers(ctx context.Context, service *metering.Service, exports metering.ExportStore, workerID string) {
	run := func() {
		for count := 0; count < 100; count++ {
			processed, err := service.ConsumeOutboxBatch(ctx, workerID, 100, 30*time.Second)
			if err != nil {
				if !errors.Is(err, context.Canceled) {
					slog.Warn("Metering relay degraded", "error", err)
				}
				return
			}
			worked, err := service.RunNextExport(ctx, exports)
			if err != nil {
				if !errors.Is(err, context.Canceled) {
					slog.Warn("Metering export degraded", "error", err)
				}
				return
			}
			if processed == 0 && !worked {
				return
			}
		}
	}
	run()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
