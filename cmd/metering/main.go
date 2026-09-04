package main

import (
	"context"
	"crypto/subtle"
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

	"github.com/toddzheng/llm-gateway/internal/cloudrunidentity"
	"github.com/toddzheng/llm-gateway/internal/controlapi"
	"github.com/toddzheng/llm-gateway/internal/dbtransport"
	"github.com/toddzheng/llm-gateway/internal/metering"
	"github.com/toddzheng/llm-gateway/internal/operations"
	"github.com/toddzheng/llm-gateway/internal/secretcustody"
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
	cloudSQLInstance := strings.TrimSpace(os.Getenv("METERING_CLOUD_SQL_INSTANCE"))
	if !devMode {
		if os.Getenv("METERING_DATABASE_TRANSPORT_ATTESTATION") != "authenticated-encrypted" {
			return errors.New("production Metering requires authenticated database transport attestation")
		}
		if err := dbtransport.RequireAuthenticatedTransport(databaseURL, cloudSQLInstance); err != nil {
			return err
		}
	}
	database, cleanupTransport, err := dbtransport.Open(ctx, databaseURL, cloudSQLInstance)
	if err != nil {
		return err
	}
	defer cleanupTransport()
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
	startedAt := time.Now().UTC()
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
	exportStore, err := configureExportStore(ctx, devMode)
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
	reporter, err := configureOperationsReporter(devMode)
	if err != nil {
		return err
	}
	go runWorkers(ctx, service, exportStore, envOr("METERING_WORKER_ID", "metering-local"))
	if reporter != nil {
		go runOperationsReporter(ctx, reporter, service, startedAt)
	}
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

func configureExportStore(ctx context.Context, devMode bool) (metering.ExportStore, error) {
	backend := strings.ToLower(strings.TrimSpace(os.Getenv("METERING_EXPORT_BACKEND")))
	if backend == "" {
		if devMode {
			backend = "filesystem"
		} else {
			backend = "gcs"
		}
	}
	switch backend {
	case "filesystem":
		if !devMode {
			return nil, errors.New("filesystem Metering export backend is development-only")
		}
		directory := strings.TrimSpace(os.Getenv("METERING_EXPORT_DIRECTORY"))
		if directory == "" {
			directory = "/tmp/llm-gateway-metering-exports"
		}
		return metering.NewFileExportStore(directory)
	case "gcs":
		provider := secretcustody.NewMetadataTokenProvider()
		return metering.NewGCSExportStore(ctx, metering.GCSExportStoreConfig{
			Bucket: os.Getenv("METERING_EXPORT_GCS_BUCKET"), Prefix: os.Getenv("METERING_EXPORT_GCS_PREFIX"),
			Region: os.Getenv("METERING_EXPORT_GCS_REGION"),
			AccessToken: func(tokenCtx context.Context) (string, error) {
				token, err := provider.Token(tokenCtx)
				return token.AccessToken, err
			},
		})
	default:
		return nil, errors.New("METERING_EXPORT_BACKEND must be filesystem or gcs")
	}
}

func configureOperationsReporter(devMode bool) (*operations.MeteringReporter, error) {
	endpoint := strings.TrimSpace(os.Getenv("METERING_OPERATIONS_URL"))
	if endpoint == "" {
		if !devMode {
			return nil, errors.New("production requires METERING_OPERATIONS_URL")
		}
		return nil, nil
	}
	if !devMode && !strings.HasPrefix(endpoint, "https://") {
		return nil, errors.New("production METERING_OPERATIONS_URL must use HTTPS")
	}
	key := []byte(os.Getenv("METERING_OPERATIONS_HMAC_KEY"))
	if devMode && len(key) == 0 {
		key = []byte("local-development-metering-hmac-key-001")
	}
	var client *http.Client
	audience := strings.TrimSpace(os.Getenv("METERING_CLOUD_RUN_AUDIENCE"))
	if audience != "" {
		if audience != strings.TrimRight(endpoint, "/") {
			return nil, errors.New("METERING_CLOUD_RUN_AUDIENCE must equal the internal service URL")
		}
		var err error
		client, err = cloudrunidentity.NewClient(audience)
		if err != nil {
			return nil, err
		}
	} else if !devMode {
		return nil, errors.New("production internal calls require METERING_CLOUD_RUN_AUDIENCE")
	}
	return operations.NewMeteringReporter(endpoint, envOr("METERING_ID", "metering-local"),
		envOr("METERING_REGION", "local"), key, client, time.Now)
}

func runOperationsReporter(ctx context.Context, reporter *operations.MeteringReporter, service *metering.Service, startedAt time.Time) {
	report := func() {
		status, err := service.Status(ctx)
		if err == nil {
			err = reporter.Report(ctx, operations.MeteringObservation{
				ProjectionGeneration: status.ProjectionGeneration, ProjectionCutoff: status.ProjectionCutoff,
				PendingEvents: status.PendingEvents, OldestPendingAt: status.OldestPendingAt,
				PoisonEvents: status.PoisonEvents, QueuedExports: status.QueuedExports, StartedAt: startedAt,
			})
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("Metering Operations reporting degraded", "error_code", "operations_report_failed")
		}
	}
	report()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			report()
		}
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
	if os.Getenv("METERING_IAM_DENY_ALL") == "true" {
		return metering.IdentityVerifierFunc(func(context.Context, string) (metering.Identity, error) {
			return metering.Identity{}, errors.New("human IAM is not enabled")
		}), nil
	}
	var verifier controlapi.IdentityVerifier
	var err error
	if strings.EqualFold(strings.TrimSpace(os.Getenv("METERING_IAM_PROVIDER")), "workos") {
		verifier, err = controlapi.NewWorkOSVerifier(controlapi.WorkOSVerifierConfig{
			JWKSURL: os.Getenv("METERING_IAM_JWKS_URL"), Issuer: os.Getenv("METERING_IAM_ISSUER"),
			ClientID: os.Getenv("METERING_IAM_AUDIENCE"), AllowedOrganizationID: os.Getenv("METERING_IAM_ALLOWED_ORGANIZATION_ID"),
			Now: time.Now, ClockSkew: 30 * time.Second,
		})
	} else {
		verifier, err = controlapi.NewJWKSVerifier(controlapi.JWKSVerifierConfig{URL: os.Getenv("METERING_IAM_JWKS_URL"), Issuer: os.Getenv("METERING_IAM_ISSUER"), Audience: os.Getenv("METERING_IAM_AUDIENCE"), Now: time.Now})
	}
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
