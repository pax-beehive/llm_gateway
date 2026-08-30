package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/toddzheng/llm-gateway/internal/controlapi"
	"github.com/toddzheng/llm-gateway/internal/credentialadmin"
	"github.com/toddzheng/llm-gateway/internal/dbtransport"
	"github.com/toddzheng/llm-gateway/internal/migrations"
	"github.com/toddzheng/llm-gateway/internal/providerconnection"
	"github.com/toddzheng/llm-gateway/internal/routingcatalog"
	"github.com/toddzheng/llm-gateway/internal/secretcustody"
	"github.com/toddzheng/llm-gateway/internal/telemetry"
	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

func main() {
	if err := run(); err != nil {
		slog.Error("control plane stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	shutdownTelemetry, err := telemetry.Configure(ctx, envOr("OTEL_SERVICE_NAME", "llm-gateway-control-plane"))
	if err != nil {
		return fmt.Errorf("configure OpenTelemetry: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := shutdownTelemetry(shutdownCtx); err != nil {
			slog.Warn("OpenTelemetry shutdown failed", "error", err)
		}
	}()
	devMode := os.Getenv("CONTROL_PLANE_DEV_MODE") == "true"
	databaseURL := strings.TrimSpace(os.Getenv("CONTROL_PLANE_DATABASE_URL"))
	if databaseURL == "" {
		return errors.New("CONTROL_PLANE_DATABASE_URL is required")
	}
	if !devMode && os.Getenv("CONTROL_PLANE_DATABASE_TRANSPORT_ATTESTATION") != "authenticated-encrypted" {
		return errors.New("production requires CONTROL_PLANE_DATABASE_TRANSPORT_ATTESTATION=authenticated-encrypted")
	}
	if !devMode {
		if err := dbtransport.RequireAuthenticatedEncryption(databaseURL); err != nil {
			return fmt.Errorf("control-plane database transport: %w", err)
		}
	}
	database, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return err
	}
	defer database.Close()
	database.SetMaxOpenConns(20)
	database.SetMaxIdleConns(5)
	database.SetConnMaxLifetime(30 * time.Minute)
	connectCtx, cancelConnect := context.WithTimeout(ctx, 10*time.Second)
	defer cancelConnect()
	if err := database.PingContext(connectCtx); err != nil {
		return fmt.Errorf("connect PostgreSQL: %w", err)
	}
	if err := assertDatabaseRole(connectCtx, database, devMode); err != nil {
		return err
	}
	if os.Getenv("CONTROL_PLANE_MIGRATE") == "true" {
		if !devMode {
			return errors.New("CONTROL_PLANE_MIGRATE is development-only; production migrations require a separate deployment gate")
		}
		if err := migrations.Migrate(ctx, database); err != nil {
			return fmt.Errorf("migrate shared schema: %w", err)
		}
		if err := tenantadmin.Migrate(ctx, database); err != nil {
			return err
		}
		if err := credentialadmin.Migrate(ctx, database); err != nil {
			return err
		}
		if err := providerconnection.Migrate(ctx, database); err != nil {
			return err
		}
		if err := routingcatalog.Migrate(ctx, database); err != nil {
			return err
		}
	}
	administration, err := tenantadmin.NewService(database, time.Now)
	if err != nil {
		return err
	}
	verifier, err := configureIdentityVerifier()
	if err != nil {
		return err
	}
	pepperRing, err := credentialPepperRingFromEnv()
	if err != nil {
		return err
	}
	credentials, err := credentialadmin.NewService(database, pepperRing, time.Now, nil)
	if err != nil {
		return fmt.Errorf("configure Gateway Credential Administration: %w", err)
	}
	if !devMode {
		if err := credentials.ValidatePepperCoverage(ctx, tenantadmin.ActorEnvelope{
			Type: "system", ID: "control-plane-startup", Scopes: []string{tenantadmin.ScopePlatformRead},
		}); err != nil {
			return fmt.Errorf("gate Gateway API Key digest pepper retirement: %w", err)
		}
	}
	secretStore, err := configureSecretCustody(devMode)
	if err != nil {
		return err
	}
	var providerOperator providerconnection.ProviderOperator = providerconnection.NewModelDiscoveryOperator(nil)
	if devMode {
		providerOperator = providerconnection.NewDeterministicOperator()
	}
	providerPolicies, err := configureProviderLiveOperationPolicy(devMode)
	if err != nil {
		return err
	}
	providerConnections, err := providerconnection.NewService(database, secretStore, providerOperator, time.Now, nil, providerPolicies...)
	if err != nil {
		return fmt.Errorf("configure Provider Connection Registry: %w", err)
	}
	connectionLookup, err := routingcatalog.NewPostgresConnectionLookup(database)
	if err != nil {
		return err
	}
	routingCatalog, err := routingcatalog.NewService(database, connectionLookup, time.Now, nil)
	if err != nil {
		return fmt.Errorf("configure Routing Catalog Administration: %w", err)
	}
	go runGatewayAPIKeyGraceReconciler(ctx, credentials)
	go runProviderOperationWorker(ctx, providerConnections)
	go runRoutingCatalogReceiptCollector(ctx, routingCatalog)
	api := controlapi.New(controlapi.Config{
		Administration: administration, Credentials: credentials,
		ProviderConnections: providerConnections, RoutingCatalog: routingCatalog, Verifier: verifier,
	})
	mux := http.NewServeMux()
	mux.Handle("/", api)
	address := envOr("CONTROL_PLANE_ADDR", ":8081")
	server := &http.Server{
		Addr: address, Handler: otelhttp.NewHandler(mux, "control-plane.http"),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 20,
	}
	serveErrors := make(chan error, 1)
	go func() {
		slog.Info("control plane listening", "address", address)
		serveErrors <- server.ListenAndServe()
	}()
	select {
	case err := <-serveErrors:
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

func configureProviderLiveOperationPolicy(devMode bool) ([]providerconnection.LiveOperationPolicy, error) {
	if devMode {
		return []providerconnection.LiveOperationPolicy{providerconnection.StaticLiveOperationPolicy{
			Source: "offline-deterministic-development", ProbeMaxRequests: 1, DiscoveryMaxRequests: 100,
		}}, nil
	}
	mode := strings.TrimSpace(os.Getenv("CONTROL_PROVIDER_LIVE_OPERATIONS"))
	if mode == "" || mode == "disabled" {
		return nil, nil
	}
	if mode != "explicitly-authorized" {
		return nil, errors.New("CONTROL_PROVIDER_LIVE_OPERATIONS must be disabled or explicitly-authorized")
	}
	source := strings.TrimSpace(os.Getenv("CONTROL_PROVIDER_LIVE_AUTHORIZATION_ID"))
	maxRequests, err := strconv.Atoi(os.Getenv("CONTROL_PROVIDER_DISCOVERY_MAX_REQUESTS"))
	if source == "" || err != nil || maxRequests < 1 || maxRequests > 100 {
		return nil, errors.New("authorized live Provider operations require CONTROL_PROVIDER_LIVE_AUTHORIZATION_ID and CONTROL_PROVIDER_DISCOVERY_MAX_REQUESTS=1..100")
	}
	return []providerconnection.LiveOperationPolicy{providerconnection.StaticLiveOperationPolicy{
		Source: source, ProbeMaxRequests: 1, DiscoveryMaxRequests: maxRequests,
	}}, nil
}

func configureSecretCustody(devMode bool) (secretcustody.Store, error) {
	if devMode {
		return secretcustody.NewMemory(), nil
	}
	if strings.TrimSpace(os.Getenv("CONTROL_SECRET_CUSTODY_BACKEND")) != "gcp-secret-manager" {
		return nil, errors.New("production requires CONTROL_SECRET_CUSTODY_BACKEND=gcp-secret-manager")
	}
	projectID := strings.TrimSpace(os.Getenv("CONTROL_GCP_SECRET_PROJECT"))
	if projectID == "" {
		return nil, errors.New("CONTROL_GCP_SECRET_PROJECT is required for GCP Secret Custody")
	}
	store, err := secretcustody.NewGCP(secretcustody.GCPConfig{
		ProjectID: projectID, TokenProvider: secretcustody.NewMetadataTokenProvider(),
	})
	if err != nil {
		return nil, fmt.Errorf("configure GCP Secret Custody: %w", err)
	}
	return store, nil
}

func runProviderOperationWorker(ctx context.Context, service *providerconnection.Service) {
	run := func() {
		for count := 0; count < 100; count++ {
			worked, err := service.RunNext(ctx)
			if err != nil {
				if !errors.Is(err, context.Canceled) {
					slog.Warn("Provider operation worker degraded", "error", err)
				}
				return
			}
			if !worked {
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

func runRoutingCatalogReceiptCollector(ctx context.Context, service *routingcatalog.Service) {
	run := func() {
		for count := 0; count < 256; count++ {
			worked, err := service.CollectNextRolloutReceipt(ctx)
			if err != nil {
				if !errors.Is(err, context.Canceled) {
					slog.Warn("Routing Catalog receipt collection degraded", "error", err)
				}
				return
			}
			if !worked {
				return
			}
		}
	}
	run()
	ticker := time.NewTicker(time.Second)
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

func runGatewayAPIKeyGraceReconciler(ctx context.Context, credentials *credentialadmin.Service) {
	run := func() {
		for {
			count, err := credentials.ReconcileExpiredGrace(ctx, 100)
			if err != nil {
				if !errors.Is(err, context.Canceled) {
					slog.Warn("Gateway API Key grace reconciliation degraded", "error", err)
				}
				return
			}
			if count == 0 {
				return
			}
			slog.Info("Gateway API Key grace predecessors revoked", "count", count)
		}
	}
	run()
	ticker := time.NewTicker(time.Second)
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

func configureIdentityVerifier() (controlapi.IdentityVerifier, error) {
	if os.Getenv("CONTROL_PLANE_DEV_MODE") == "true" {
		token := os.Getenv("CONTROL_PLANE_DEV_TOKEN")
		if token == "" || len(token) < 16 {
			return nil, errors.New("CONTROL_PLANE_DEV_TOKEN must contain at least 16 characters in development mode")
		}
		return controlapi.IdentityVerifierFunc(func(_ context.Context, authorization string) (controlapi.VerifiedIdentity, error) {
			fields := strings.Fields(authorization)
			if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") || subtle.ConstantTimeCompare([]byte(fields[1]), []byte(token)) != 1 {
				return controlapi.VerifiedIdentity{}, errors.New("invalid development identity assertion")
			}
			return controlapi.VerifiedIdentity{
				ActorType: "human", ActorID: "dev-operator",
				Scopes: []string{tenantadmin.ScopePlatformRead, tenantadmin.ScopePlatformWrite},
			}, nil
		}), nil
	}
	jwksURL := strings.TrimSpace(os.Getenv("CONTROL_IAM_JWKS_URL"))
	issuer := strings.TrimSpace(os.Getenv("CONTROL_IAM_ISSUER"))
	audience := strings.TrimSpace(os.Getenv("CONTROL_IAM_AUDIENCE"))
	if jwksURL == "" || issuer == "" || audience == "" {
		return nil, errors.New("CONTROL_IAM_JWKS_URL, CONTROL_IAM_ISSUER, and CONTROL_IAM_AUDIENCE are required")
	}
	return controlapi.NewJWKSVerifier(controlapi.JWKSVerifierConfig{
		URL: jwksURL, Issuer: issuer, Audience: audience, ClockSkew: 30 * time.Second,
	})
}

func assertDatabaseRole(ctx context.Context, database *sql.DB, devMode bool) error {
	if devMode {
		return nil
	}
	expected := strings.TrimSpace(os.Getenv("CONTROL_PLANE_DB_ROLE"))
	if expected == "" {
		return errors.New("CONTROL_PLANE_DB_ROLE is required outside development mode")
	}
	var current string
	if err := database.QueryRowContext(ctx, `SELECT current_user`).Scan(&current); err != nil {
		return fmt.Errorf("inspect control-plane database role: %w", err)
	}
	if current != expected {
		return fmt.Errorf("control-plane database role mismatch: connected as %q", current)
	}
	return nil
}

func credentialPepperRingFromEnv() (credentialadmin.PepperRing, error) {
	encoded := strings.TrimSpace(os.Getenv("CONTROL_API_KEY_PEPPERS_JSON"))
	currentValue := strings.TrimSpace(os.Getenv("CONTROL_API_KEY_CURRENT_DIGEST_VERSION"))
	if encoded == "" || currentValue == "" {
		return credentialadmin.PepperRing{}, errors.New("CONTROL_API_KEY_PEPPERS_JSON and CONTROL_API_KEY_CURRENT_DIGEST_VERSION are required")
	}
	var configured map[string]string
	if err := json.Unmarshal([]byte(encoded), &configured); err != nil {
		return credentialadmin.PepperRing{}, fmt.Errorf("CONTROL_API_KEY_PEPPERS_JSON: %w", err)
	}
	current, err := strconv.ParseInt(currentValue, 10, 16)
	if err != nil || current <= 0 {
		return credentialadmin.PepperRing{}, errors.New("CONTROL_API_KEY_CURRENT_DIGEST_VERSION must be a positive integer")
	}
	peppers := make(map[int16][]byte, len(configured))
	for versionValue, pepper := range configured {
		version, err := strconv.ParseInt(versionValue, 10, 16)
		if err != nil || version <= 0 {
			return credentialadmin.PepperRing{}, errors.New("CONTROL_API_KEY_PEPPERS_JSON keys must be positive digest versions")
		}
		peppers[int16(version)] = []byte(pepper)
	}
	return credentialadmin.PepperRing{CurrentVersion: int16(current), Peppers: peppers}, nil
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
