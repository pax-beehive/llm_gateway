package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/toddzheng/llm-gateway/internal/access"
	"github.com/toddzheng/llm-gateway/internal/accessprojection"
	"github.com/toddzheng/llm-gateway/internal/cacheprotection"
	"github.com/toddzheng/llm-gateway/internal/capability"
	"github.com/toddzheng/llm-gateway/internal/configuration"
	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/dbtransport"
	"github.com/toddzheng/llm-gateway/internal/httpapi"
	"github.com/toddzheng/llm-gateway/internal/migrations"
	"github.com/toddzheng/llm-gateway/internal/provider"
	"github.com/toddzheng/llm-gateway/internal/quota"
	"github.com/toddzheng/llm-gateway/internal/routingcatalog"
	"github.com/toddzheng/llm-gateway/internal/runtime"
	"github.com/toddzheng/llm-gateway/internal/store"
	"github.com/toddzheng/llm-gateway/internal/telemetry"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

type routeConfig = routingcatalog.RouteConfig

type cacheRefreshConfig = routingcatalog.CacheRefreshConfig

func main() {
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		if err := healthcheck(); err != nil {
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		slog.Error("gateway stopped", "error", err)
		os.Exit(1)
	}
}

func healthcheck() error {
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	response, err := client.Get("http://127.0.0.1:8080/healthz")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("health status %d", response.StatusCode)
	}
	return nil
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	shutdownTelemetry, err := telemetry.Configure(ctx, envOr("OTEL_SERVICE_NAME", "llm-gateway"))
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
	apiKeys, err := parseStringMapEnv("GATEWAY_API_KEYS_JSON")
	if err != nil {
		return fmt.Errorf("GATEWAY_API_KEYS_JSON: %w", err)
	}
	homeRegions, err := parseStringMapEnv("GATEWAY_TENANT_HOME_REGIONS_JSON")
	if err != nil {
		return fmt.Errorf("GATEWAY_TENANT_HOME_REGIONS_JSON: %w", err)
	}
	executionEpochs, err := parseInt64MapEnv("GATEWAY_TENANT_EXECUTION_EPOCHS_JSON")
	if err != nil {
		return fmt.Errorf("GATEWAY_TENANT_EXECUTION_EPOCHS_JSON: %w", err)
	}
	tenantPolicies, err := parseTenantPoliciesEnv("GATEWAY_TENANT_POLICIES_JSON")
	if err != nil {
		return fmt.Errorf("GATEWAY_TENANT_POLICIES_JSON: %w", err)
	}
	homeRegionURLs, err := parseStringMapEnv("GATEWAY_HOME_REGION_URLS_JSON")
	if err != nil {
		return fmt.Errorf("GATEWAY_HOME_REGION_URLS_JSON: %w", err)
	}
	localRegion := envOr("GATEWAY_LOCAL_REGION", "local")
	trustedProxyCIDRs, err := parseTrustedProxyCIDRsEnv("GATEWAY_TRUSTED_PROXY_CIDRS")
	if err != nil {
		return err
	}

	responseStore, database, cleanup, err := configureStore(apiKeys, homeRegions, executionEpochs, tenantPolicies)
	if err != nil {
		return err
	}
	defer cleanup()
	authenticator, err := configureAuthenticator(ctx, database, apiKeys, homeRegions, executionEpochs, tenantPolicies)
	if err != nil {
		return err
	}
	router, err := configureRouter(ctx, database)
	if err != nil {
		return err
	}
	var intentRepository cacheprotection.IntentRepository
	var quotaController quota.Controller
	if database != nil {
		intentRepository = cacheprotection.NewPostgresIntentRepository(database)
		quotaController = quota.NewPostgresController(database, time.Now)
	} else {
		intentRepository = cacheprotection.NewMemoryIntentRepository()
	}
	principalSource, _ := authenticator.(cachePrincipalSource)
	var refreshBudget cacheprotection.RefreshBudget
	if quotaController != nil && principalSource != nil {
		refreshBudget = &refreshBudgetGate{principals: principalSource, quota: quotaController, now: time.Now}
	}
	cacheCoordinator := cacheprotection.NewCoordinatorWithBudget(intentRepository, time.Now, refreshBudget)
	cacheProtectionMode, err := cacheProtectionModeFromEnv()
	if err != nil {
		return err
	}
	holdoutPercent, err := strconv.Atoi(envOr("GATEWAY_CACHE_PROTECTION_HOLDOUT_PERCENT", "10"))
	if err != nil || holdoutPercent < 0 || holdoutPercent > 100 {
		return errors.New("GATEWAY_CACHE_PROTECTION_HOLDOUT_PERCENT must be an integer from 0 to 100")
	}
	engine := runtime.NewWithOptions(responseStore, router, runtime.Options{
		CacheCoordinator:    cacheCoordinator,
		OnCacheError:        func(err error) { slog.Warn("cache protection degraded", "error", err) },
		OnCoordinationError: func(err error) { slog.Warn("gateway coordination degraded", "error", err) },
		CacheProtectionMode: cacheProtectionMode, CacheHoldoutPercent: holdoutPercent,
		TenantPolicies:  tenantPolicies,
		QuotaController: quotaController,
	})
	capabilityUsageStore, ok := responseStore.(capability.Store)
	if !ok {
		return errors.New("configured store does not support capability usage")
	}
	capabilityEngine := capability.New(capabilityUsageStore, router, capability.Options{QuotaController: quotaController})
	if cacheProtectionMode != runtime.CacheProtectionOffMode {
		go runCacheWorker(ctx, cacheCoordinator, router, principalSource, tenantPolicies)
	}
	if quotaController != nil {
		go runQuotaReconciliationWorker(ctx, quotaController)
	}
	if projection, ok := authenticator.(*accessprojection.Store); ok {
		go runAccessProjectionWorker(ctx, projection)
	}
	go runRetentionWorker(ctx, responseStore, localRegion)
	handler := httpapi.New(httpapi.Config{
		Runtime: engine, CapabilityRuntime: capabilityEngine, CapabilityCatalog: router,
		ModelCatalog: router, Authenticator: authenticator, TenantHomeRegions: homeRegions,
		TenantExecutionEpochs: executionEpochs, LocalRegion: localRegion, HomeRegionURLs: homeRegionURLs,
		TrustedProxyCIDRs: trustedProxyCIDRs,
	})

	address := envOr("GATEWAY_ADDR", ":8080")
	server := &http.Server{
		Addr: address, Handler: otelhttp.NewHandler(handler, "gateway.http"), ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 20,
	}
	serveErrors := make(chan error, 1)
	go func() {
		slog.Info("gateway listening", "address", address)
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

func cacheProtectionModeFromEnv() (string, error) {
	mode := envOr("GATEWAY_CACHE_PROTECTION_MODE", runtime.CacheProtectionOffMode)
	switch mode {
	case runtime.CacheProtectionOffMode, runtime.CacheProtectionShadowMode, runtime.CacheProtectionAnthropicCanaryMode:
		return mode, nil
	default:
		return "", errors.New("GATEWAY_CACHE_PROTECTION_MODE must be off, shadow, or anthropic-one-refresh-canary")
	}
}

func configureStore(apiKeys, homeRegions map[string]string, executionEpochs map[string]int64, tenantPolicies map[string]core.TenantPolicy) (runtime.ResponseStore, *sql.DB, func(), error) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		if os.Getenv("GATEWAY_DEV_MEMORY_STORE") != "true" {
			return nil, nil, func() {}, errors.New("DATABASE_URL is required unless GATEWAY_DEV_MEMORY_STORE=true")
		}
		return store.NewMemoryResponseStore(), nil, func() {}, nil
	}
	if os.Getenv("GATEWAY_ENV") == "production" && os.Getenv("GATEWAY_DURABILITY_ATTESTATION") != "sync-multi-az" {
		return nil, nil, func() {}, errors.New("production requires GATEWAY_DURABILITY_ATTESTATION=sync-multi-az")
	}
	if os.Getenv("GATEWAY_ENV") == "production" && os.Getenv("GATEWAY_DATABASE_TRANSPORT_ATTESTATION") != "authenticated-encrypted" {
		return nil, nil, func() {}, errors.New("production requires GATEWAY_DATABASE_TRANSPORT_ATTESTATION=authenticated-encrypted")
	}
	if os.Getenv("GATEWAY_ENV") == "production" {
		if err := dbtransport.RequireAuthenticatedEncryption(databaseURL); err != nil {
			return nil, nil, func() {}, fmt.Errorf("Gateway database transport: %w", err)
		}
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, nil, func() {}, err
	}
	cleanup := func() { _ = db.Close() }
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		cleanup()
		return nil, nil, func() {}, fmt.Errorf("connect PostgreSQL: %w", err)
	}
	postgresStore := store.NewPostgresResponseStore(db)
	if os.Getenv("GATEWAY_MIGRATE") == "true" {
		if err := migrations.Migrate(ctx, db); err != nil {
			cleanup()
			return nil, nil, func() {}, err
		}
	}
	return postgresStore, db, cleanup, nil
}

func configureAuthenticator(
	ctx context.Context,
	db *sql.DB,
	apiKeys, homeRegions map[string]string,
	executionEpochs map[string]int64,
	tenantPolicies map[string]core.TenantPolicy,
) (httpapi.Authenticator, error) {
	if db == nil {
		if len(apiKeys) == 0 {
			return nil, errors.New("GATEWAY_API_KEYS_JSON must configure at least one development token")
		}
		return httpapi.StaticAuthenticator(apiKeys), nil
	}
	currentDigestVersion, digestPeppers, err := gatewayAPIKeyPepperRingFromEnv()
	if err != nil {
		return nil, err
	}
	service, err := access.NewPostgresServiceWithPeppers(db, currentDigestVersion, digestPeppers)
	if err != nil {
		return nil, fmt.Errorf("configure persistent API key authentication: %w", err)
	}
	if os.Getenv("GATEWAY_BOOTSTRAP_ACCESS") == "true" {
		return nil, errors.New("Gateway startup no longer performs access bootstrap; run go run ./cmd/access-bootstrap")
	}
	if len(apiKeys) > 0 {
		return nil, errors.New("GATEWAY_API_KEYS_JSON is one-time input for cmd/access-bootstrap and must not be supplied to the Gateway data plane")
	}
	return configureAccessProjection(ctx, db, service, currentDigestVersion, digestPeppers)
}

func configureAccessProjection(
	ctx context.Context,
	database *sql.DB,
	authoritative *access.PostgresService,
	currentDigestVersion int16,
	digestPeppers map[int16][]byte,
) (httpapi.Authenticator, error) {
	if os.Getenv("GATEWAY_ACCESS_PROJECTION") != "true" {
		if os.Getenv("GATEWAY_ENV") == "production" {
			return nil, errors.New("production requires GATEWAY_ACCESS_PROJECTION=true")
		}
		return authoritative, nil
	}
	if os.Getenv("GATEWAY_MIGRATE") == "true" {
		if err := accessprojection.Migrate(ctx, database); err != nil {
			return nil, err
		}
	}
	projection, err := accessprojection.New(database, accessprojection.PepperRing{
		CurrentVersion: currentDigestVersion, Peppers: digestPeppers,
	}, time.Now)
	if err != nil {
		return nil, fmt.Errorf("configure Gateway Access Projection: %w", err)
	}
	for {
		result, err := projection.ConsumeControlOutboxBatch(ctx, 1000)
		if err != nil {
			return nil, fmt.Errorf("catch up Gateway Access Projection: %w", err)
		}
		if result.Scanned == 0 {
			break
		}
		if result.Gaps > 0 && result.Applied == 0 && result.Stale == 0 {
			break
		}
	}
	if os.Getenv("GATEWAY_ENV") == "production" {
		if err := projection.ValidatePepperCoverage(ctx); err != nil {
			return nil, fmt.Errorf("gate Gateway Access Projection digest pepper retirement: %w", err)
		}
	}
	return projection, nil
}

func runAccessProjectionWorker(ctx context.Context, projection *accessprojection.Store) {
	run := func() {
		for {
			result, err := projection.ConsumeControlOutboxBatch(ctx, 256)
			if err != nil {
				slog.Warn("Gateway Access Projection consumer degraded", "error", err)
				return
			}
			if result.Gaps > 0 {
				slog.Warn("Gateway Access Projection revision gap detected", "gaps", result.Gaps)
			}
			if result.Scanned < 256 || result.Applied == 0 && result.Stale == 0 {
				break
			}
		}
		if _, err := projection.FlushLastUsed(ctx, 1000); err != nil {
			slog.Warn("Gateway Access Projection last-used flush degraded", "error", err)
		}
		status, err := projection.Status(ctx)
		if err != nil {
			slog.Warn("Gateway Access Projection status unavailable", "error", err)
			return
		}
		attributes := []any{
			"gap_count", status.GapCount,
			"head_count", status.HeadCount,
			"max_aggregate_revision", status.MaxAggregateRevision,
			"pending_event_count", status.PendingEventCount,
			"delivery_lag", status.DeliveryLag,
			"max_apply_lag", status.MaxApplyLag,
			"max_revocation_apply_lag", status.MaxRevocationApplyLag,
		}
		if status.OldestGapAt != nil {
			attributes = append(attributes, "oldest_gap_at", *status.OldestGapAt)
		}
		if status.LastAppliedAt != nil {
			attributes = append(attributes, "last_applied_at", *status.LastAppliedAt)
		}
		if status.OldestPendingAt != nil {
			attributes = append(attributes, "oldest_pending_at", *status.OldestPendingAt)
		}
		if status.LastRevocationAppliedAt != nil {
			attributes = append(attributes, "last_revocation_applied_at", *status.LastRevocationAppliedAt)
		}
		if status.GapCount > 0 {
			slog.Warn("Gateway Access Projection requires snapshot repair", attributes...)
		} else {
			slog.Debug("Gateway Access Projection healthy", attributes...)
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

func gatewayAPIKeyPepperRingFromEnv() (int16, map[int16][]byte, error) {
	encoded := strings.TrimSpace(os.Getenv("GATEWAY_API_KEY_PEPPERS_JSON"))
	if encoded == "" {
		return 1, map[int16][]byte{1: []byte(os.Getenv("GATEWAY_API_KEY_PEPPER"))}, nil
	}
	var configured map[string]string
	if err := json.Unmarshal([]byte(encoded), &configured); err != nil {
		return 0, nil, fmt.Errorf("GATEWAY_API_KEY_PEPPERS_JSON: %w", err)
	}
	currentValue := strings.TrimSpace(os.Getenv("GATEWAY_API_KEY_CURRENT_DIGEST_VERSION"))
	current, err := strconv.ParseInt(currentValue, 10, 16)
	if err != nil || current <= 0 {
		return 0, nil, errors.New("GATEWAY_API_KEY_CURRENT_DIGEST_VERSION must be a positive integer")
	}
	peppers := make(map[int16][]byte, len(configured))
	for versionValue, pepper := range configured {
		version, err := strconv.ParseInt(versionValue, 10, 16)
		if err != nil || version <= 0 {
			return 0, nil, errors.New("GATEWAY_API_KEY_PEPPERS_JSON keys must be positive digest versions")
		}
		peppers[int16(version)] = []byte(pepper)
	}
	return int16(current), peppers, nil
}

func configureRouter(ctx context.Context, database *sql.DB) (*provider.StaticRouter, error) {
	if os.Getenv("GATEWAY_DEV_ECHO") == "true" {
		var tenantIDs []string
		if payload := os.Getenv("GATEWAY_DEV_ROUTE_TENANT_IDS_JSON"); payload != "" {
			if err := json.Unmarshal([]byte(payload), &tenantIDs); err != nil {
				return nil, fmt.Errorf("GATEWAY_DEV_ROUTE_TENANT_IDS_JSON: %w", err)
			}
		}
		return provider.NewStaticRouterForTenants(provider.NewEchoExecutor(), tenantIDs), nil
	}
	if database == nil {
		routes, err := routesFromJSON([]byte(os.Getenv("GATEWAY_ROUTES_JSON")))
		if err != nil {
			return nil, fmt.Errorf("GATEWAY_ROUTES_JSON: %w", err)
		}
		return provider.NewVersionedRouter(1, routes), nil
	}
	repository := configuration.NewPostgresRepository(database)
	snapshot, err := repository.Current(ctx, "model_routes")
	var routes []provider.Route
	if errors.Is(err, configuration.ErrNotFound) {
		if os.Getenv("GATEWAY_BOOTSTRAP_ROUTES") != "true" {
			return nil, errors.New("model_routes configuration is absent; publish it or explicitly set GATEWAY_BOOTSTRAP_ROUTES=true")
		}
		revision, parseErr := strconv.ParseInt(envOr("GATEWAY_CONFIG_REVISION", "1"), 10, 64)
		if parseErr != nil {
			return nil, fmt.Errorf("GATEWAY_CONFIG_REVISION: %w", parseErr)
		}
		snapshot, routes, err = publishRoutes(
			ctx, repository, 0, revision, json.RawMessage(os.Getenv("GATEWAY_ROUTES_JSON")), envOr("GATEWAY_CONFIG_ACTOR", "bootstrap"),
		)
	}
	if err != nil {
		return nil, err
	}
	if os.Getenv("GATEWAY_PUBLISH_ROUTES") == "true" {
		expected, expectedErr := strconv.ParseInt(os.Getenv("GATEWAY_CONFIG_EXPECTED_REVISION"), 10, 64)
		revision, revisionErr := strconv.ParseInt(os.Getenv("GATEWAY_CONFIG_REVISION"), 10, 64)
		if expectedErr != nil || revisionErr != nil {
			return nil, errors.New("publishing routes requires numeric GATEWAY_CONFIG_EXPECTED_REVISION and GATEWAY_CONFIG_REVISION")
		}
		snapshot, routes, err = publishRoutes(
			ctx, repository, expected, revision, json.RawMessage(os.Getenv("GATEWAY_ROUTES_JSON")), envOr("GATEWAY_CONFIG_ACTOR", "operator"),
		)
		if err != nil {
			return nil, err
		}
	}
	if routes == nil {
		routes, err = routesFromJSON(snapshot.Payload)
		if err != nil {
			return nil, fmt.Errorf("model_routes revision %d: %w", snapshot.Revision, err)
		}
	}
	router := provider.NewVersionedRouterAt(snapshot.Revision, snapshot.CreatedAt, routes)
	go func() {
		err := configuration.Watch(ctx, repository, "model_routes", 5*time.Second, func(next configuration.Snapshot) error {
			if next.Revision <= router.Revision() {
				return nil
			}
			nextRoutes, err := routesFromJSON(next.Payload)
			if err != nil {
				return err
			}
			return router.UpdateAt(next.Revision, next.CreatedAt, nextRoutes)
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("route configuration watch stopped", "error", err)
		}
	}()
	return router, nil
}

func publishRoutes(
	ctx context.Context,
	repository configuration.Repository,
	expectedRevision, revision int64,
	payload json.RawMessage,
	actor string,
) (configuration.Snapshot, []provider.Route, error) {
	return routingcatalog.Publish(ctx, repository, expectedRevision, revision, payload, actor)
}

func routesFromJSON(payload []byte) ([]provider.Route, error) {
	return routingcatalog.Parse(payload)
}

func buildProviderComponentsWithHTTPClient(config routeConfig, httpClient *http.Client) (provider.ResponseExecutor, provider.CacheProtector, provider.CacheAnchorBuilder, error) {
	return routingcatalog.BuildProviderComponentsWithHTTPClient(config, httpClient)
}

func runCacheWorker(
	ctx context.Context,
	coordinator *cacheprotection.Coordinator,
	router *provider.StaticRouter,
	principalSource cachePrincipalSource,
	tenantPolicies map[string]core.TenantPolicy,
) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, err := coordinator.RunDue(ctx, 32, func(anchor provider.CacheAnchor) provider.CacheProtector {
				return resolveCacheProtectorForTenant(ctx, router, principalSource, tenantPolicies, anchor)
			})
			if err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("cache protection worker iteration failed", "error", err)
			}
		}
	}
}

type cachePrincipalSource interface {
	LookupPrincipal(context.Context, string, string) (access.Principal, error)
}

type refreshBudgetGate struct {
	principals cachePrincipalSource
	quota      quota.Controller
	now        func() time.Time
}

func (g *refreshBudgetGate) Reserve(ctx context.Context, intent cacheprotection.Intent) (cacheprotection.RefreshBudgetReservation, error) {
	principal, err := g.principals.LookupPrincipal(ctx, intent.TenantID, intent.Anchor.APIKeyID)
	if err != nil {
		return cacheprotection.RefreshBudgetReservation{}, err
	}
	if principal.TenantPolicy.AllowCacheProtection == nil || !*principal.TenantPolicy.AllowCacheProtection ||
		!principal.APIKeyPolicy.AllowCacheProtection {
		return cacheprotection.RefreshBudgetReservation{}, errors.New("active refresh is disabled by current principal policy")
	}
	effective, err := quota.EffectiveLimits(principal.TenantPolicy.Limits, principal.APIKeyPolicy.Limits)
	if err != nil {
		return cacheprotection.RefreshBudgetReservation{}, err
	}
	if !quota.HasRefreshLimits(effective) {
		return cacheprotection.RefreshBudgetReservation{}, nil
	}
	reservation, err := g.quota.ReserveRefresh(ctx, quota.RefreshReservationRequest{
		TenantID: intent.TenantID, APIKeyID: intent.Anchor.APIKeyID, CacheRefreshIntentID: intent.ID,
		TenantPolicyRevision: principal.TenantPolicy.Revision, APIKeyPolicyRevision: principal.APIKeyPolicy.Revision,
		TenantLimits: principal.TenantPolicy.Limits, APIKeyLimits: principal.APIKeyPolicy.Limits,
		ReservedSpendMicros: intent.Candidate.Economics.RefreshCostMicros,
		Currency:            intent.Candidate.RefreshPriceSnapshot.Currency, ExpiresAt: g.now().UTC().Add(5 * time.Minute),
	})
	if err != nil {
		return cacheprotection.RefreshBudgetReservation{}, err
	}
	return cacheprotection.RefreshBudgetReservation{ID: reservation.ID}, nil
}

func (g *refreshBudgetGate) Complete(ctx context.Context, reservation cacheprotection.RefreshBudgetReservation, intent cacheprotection.Intent, outcome error) error {
	if reservation.ID == "" {
		return nil
	}
	if outcome == nil {
		return g.quota.Commit(ctx, reservation.ID, quota.ActualUsage{SpendMicros: cacheprotection.ActualRefreshCost(intent)})
	}
	if intent.ProviderResult.Status == "rejected" {
		return g.quota.Release(ctx, reservation.ID)
	}
	// The provider may have performed the side effect. Preserve the reserved
	// estimate as committed spend until financial evidence can correct it.
	return g.quota.Uncertain(ctx, reservation.ID)
}

func runQuotaReconciliationWorker(ctx context.Context, controller quota.Controller) {
	run := func() {
		for {
			settled, err := controller.Reconcile(ctx, 256)
			if err != nil {
				if !errors.Is(err, context.Canceled) {
					slog.Error("quota reconciliation failed", "error", err)
				}
				return
			}
			if settled < 256 {
				return
			}
		}
	}
	run()
	ticker := time.NewTicker(10 * time.Second)
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

func resolveCacheProtectorForTenant(
	ctx context.Context,
	router *provider.StaticRouter,
	principalSource cachePrincipalSource,
	tenantPolicies map[string]core.TenantPolicy,
	anchor provider.CacheAnchor,
) provider.CacheProtector {
	if principalSource != nil {
		principal, err := principalSource.LookupPrincipal(ctx, anchor.TenantID, anchor.APIKeyID)
		if err != nil || principal.TenantPolicy.AllowCacheProtection == nil ||
			!*principal.TenantPolicy.AllowCacheProtection || !principal.APIKeyPolicy.AllowCacheProtection {
			return nil
		}
		return router.ResolveCacheProtector(anchor)
	}
	policy, configured := tenantPolicies[anchor.TenantID]
	if !configured || policy.AllowCacheProtection == nil || !*policy.AllowCacheProtection {
		return nil
	}
	return router.ResolveCacheProtector(anchor)
}

func runRetentionWorker(ctx context.Context, responseStore runtime.ResponseStore, localRegion string) {
	retentionStore, ok := responseStore.(runtime.RetentionStore)
	if !ok {
		return
	}
	run := func() {
		for {
			scrubbed, err := retentionStore.ScrubExpiredContent(ctx, localRegion, 256)
			if err != nil {
				if !errors.Is(err, context.Canceled) {
					slog.Error("retention scrub failed", "error", err)
				}
				return
			}
			if scrubbed < 256 {
				return
			}
		}
	}
	run()
	ticker := time.NewTicker(time.Minute)
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

func parseInt64MapEnv(name string) (map[string]int64, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return map[string]int64{}, nil
	}
	var result map[string]int64
	if err := json.Unmarshal([]byte(value), &result); err != nil {
		return nil, err
	}
	return result, nil
}

func parseStringMapEnv(name string) (map[string]string, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return map[string]string{}, nil
	}
	var result map[string]string
	if err := json.Unmarshal([]byte(value), &result); err != nil {
		return nil, err
	}
	return result, nil
}

func parseTrustedProxyCIDRsEnv(name string) ([]string, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return nil, nil
	}
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		prefix, err := netip.ParsePrefix(part)
		if err != nil || prefix.String() != part {
			return nil, fmt.Errorf("%s must contain canonical comma-separated CIDRs", name)
		}
		result = append(result, part)
	}
	return result, nil
}

func parseTenantPoliciesEnv(name string) (map[string]core.TenantPolicy, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return map[string]core.TenantPolicy{}, nil
	}
	var result map[string]core.TenantPolicy
	if err := json.Unmarshal([]byte(value), &result); err != nil {
		return nil, err
	}
	for tenantID, policy := range result {
		if policy.Revision == 0 {
			policy.Revision = 1
			result[tenantID] = policy
		}
		if tenantID == "" || policy.Revision < 1 || policy.MaxConcurrentResponses < 0 || policy.MaxInputItems < 0 || policy.RetentionSeconds < 0 {
			return nil, errors.New("tenant IDs must be non-empty, policy revisions positive, and quotas and retention non-negative")
		}
		if _, err := quota.EffectiveLimits(policy.Limits, core.QuotaLimits{}); err != nil {
			return nil, fmt.Errorf("Tenant %q policy: %w", tenantID, err)
		}
	}
	return result, nil
}

func parseAPIKeyPoliciesEnv(name string) (map[string]core.APIKeyPolicy, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return map[string]core.APIKeyPolicy{}, nil
	}
	var result map[string]core.APIKeyPolicy
	if err := json.Unmarshal([]byte(value), &result); err != nil {
		return nil, err
	}
	for rawKey, policy := range result {
		if rawKey == "" || policy.Revision < 0 {
			return nil, errors.New("API key policy identities must be non-empty and revisions non-negative")
		}
		if policy.Revision == 0 {
			policy.Revision = 1
			result[rawKey] = policy
		}
		if _, err := quota.EffectiveLimits(core.QuotaLimits{}, policy.Limits); err != nil {
			return nil, fmt.Errorf("API key policy: %w", err)
		}
	}
	return result, nil
}

func parseAPIKeyMetadataEnv(name string) (map[string]map[string]any, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return map[string]map[string]any{}, nil
	}
	var result map[string]map[string]any
	if err := json.Unmarshal([]byte(value), &result); err != nil {
		return nil, err
	}
	return result, nil
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
