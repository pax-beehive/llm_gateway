package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/toddzheng/llm-gateway/internal/cacheprotection"
	"github.com/toddzheng/llm-gateway/internal/configuration"
	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/httpapi"
	"github.com/toddzheng/llm-gateway/internal/provider"
	"github.com/toddzheng/llm-gateway/internal/provider/anthropic"
	"github.com/toddzheng/llm-gateway/internal/provider/openaicompat"
	"github.com/toddzheng/llm-gateway/internal/runtime"
	"github.com/toddzheng/llm-gateway/internal/store"
	"github.com/toddzheng/llm-gateway/internal/telemetry"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

type routeConfig struct {
	ID                 string                                `json:"id"`
	Provider           string                                `json:"provider"`
	PublicModel        string                                `json:"public_model"`
	ProviderModel      string                                `json:"provider_model"`
	BaseURL            string                                `json:"base_url"`
	APIKeyEnv          string                                `json:"api_key_env"`
	Region             string                                `json:"region"`
	HomeRegion         string                                `json:"home_region"`
	CredentialScope    string                                `json:"credential_scope"`
	Capabilities       map[string]provider.CapabilitySupport `json:"capabilities"`
	CapabilityRevision int64                                 `json:"capability_revision"`
	Headers            map[string]string                     `json:"headers"`
	InputCost          float64                               `json:"input_cost_per_million"`
	OutputCost         float64                               `json:"output_cost_per_million"`
	CachedInputCost    float64                               `json:"cached_input_cost_per_million"`
	CacheWriteCost     float64                               `json:"cache_write_cost_per_million"`
	PriceSnapshotID    string                                `json:"price_snapshot_id"`
	PriceEffectiveAt   string                                `json:"price_effective_at"`
	PriceSource        string                                `json:"price_source"`
	Currency           string                                `json:"currency"`
	CacheUsageReliable bool                                  `json:"cache_usage_reliable"`
	Healthy            *bool                                 `json:"healthy,omitempty"`
	CacheRefresh       *cacheRefreshConfig                   `json:"cache_refresh,omitempty"`
}

type cacheRefreshConfig struct {
	Kind                string  `json:"kind"`
	BaseURL             string  `json:"base_url"`
	APIKeyEnv           string  `json:"api_key_env"`
	TTLSeconds          int64   `json:"ttl_seconds"`
	APIVersion          string  `json:"api_version,omitempty"`
	WriteCostPerMillion float64 `json:"write_cost_per_million"`
}

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
	if err != nil || len(apiKeys) == 0 {
		return fmt.Errorf("GATEWAY_API_KEYS_JSON: configure at least one token-to-tenant mapping: %w", err)
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

	responseStore, database, cleanup, err := configureStore(apiKeys, homeRegions, executionEpochs, tenantPolicies)
	if err != nil {
		return err
	}
	defer cleanup()
	router, err := configureRouter(ctx, database)
	if err != nil {
		return err
	}
	var intentRepository cacheprotection.IntentRepository
	if database != nil {
		intentRepository = cacheprotection.NewPostgresIntentRepository(database)
	} else {
		intentRepository = cacheprotection.NewMemoryIntentRepository()
	}
	cacheCoordinator := cacheprotection.NewCoordinator(intentRepository, time.Now)
	cacheProtectionMode := envOr("GATEWAY_CACHE_PROTECTION_MODE", runtime.CacheProtectionShadowMode)
	if cacheProtectionMode != runtime.CacheProtectionShadowMode && cacheProtectionMode != runtime.CacheProtectionAnthropicCanaryMode {
		return errors.New("GATEWAY_CACHE_PROTECTION_MODE must be shadow or anthropic-one-refresh-canary")
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
		TenantPolicies: tenantPolicies,
	})
	go runCacheWorker(ctx, cacheCoordinator, router)
	go runRetentionWorker(ctx, responseStore, localRegion)
	handler := httpapi.New(httpapi.Config{
		Runtime: engine, Authenticator: httpapi.StaticAuthenticator(apiKeys), TenantHomeRegions: homeRegions,
		TenantExecutionEpochs: executionEpochs, LocalRegion: localRegion, HomeRegionURLs: homeRegionURLs,
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

func configureStore(apiKeys, homeRegions map[string]string, executionEpochs map[string]int64, tenantPolicies map[string]core.TenantPolicy) (store.ResponseStore, *sql.DB, func(), error) {
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
		if err := postgresStore.Migrate(ctx); err != nil {
			cleanup()
			return nil, nil, func() {}, err
		}
	}
	if err := ensureTenants(ctx, db, apiKeys, homeRegions, executionEpochs, tenantPolicies); err != nil {
		cleanup()
		return nil, nil, func() {}, err
	}
	return postgresStore, db, cleanup, nil
}

func configureRouter(ctx context.Context, database *sql.DB) (*provider.StaticRouter, error) {
	if os.Getenv("GATEWAY_DEV_ECHO") == "true" {
		return provider.NewStaticRouter(provider.NewEchoExecutor()), nil
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
	if errors.Is(err, configuration.ErrNotFound) {
		if os.Getenv("GATEWAY_BOOTSTRAP_ROUTES") != "true" {
			return nil, errors.New("model_routes configuration is absent; publish it or explicitly set GATEWAY_BOOTSTRAP_ROUTES=true")
		}
		revision, parseErr := strconv.ParseInt(envOr("GATEWAY_CONFIG_REVISION", "1"), 10, 64)
		if parseErr != nil {
			return nil, fmt.Errorf("GATEWAY_CONFIG_REVISION: %w", parseErr)
		}
		snapshot, err = repository.Publish(
			ctx, "model_routes", 0, revision, json.RawMessage(os.Getenv("GATEWAY_ROUTES_JSON")), envOr("GATEWAY_CONFIG_ACTOR", "bootstrap"),
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
		snapshot, err = repository.Publish(
			ctx, "model_routes", expected, revision, json.RawMessage(os.Getenv("GATEWAY_ROUTES_JSON")), envOr("GATEWAY_CONFIG_ACTOR", "operator"),
		)
		if err != nil {
			return nil, err
		}
	}
	routes, err := routesFromJSON(snapshot.Payload)
	if err != nil {
		return nil, fmt.Errorf("model_routes revision %d: %w", snapshot.Revision, err)
	}
	router := provider.NewVersionedRouter(snapshot.Revision, routes)
	go func() {
		err := configuration.Watch(ctx, repository, "model_routes", 5*time.Second, func(next configuration.Snapshot) error {
			if next.Revision <= router.Revision() {
				return nil
			}
			nextRoutes, err := routesFromJSON(next.Payload)
			if err != nil {
				return err
			}
			return router.Update(next.Revision, nextRoutes)
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("route configuration watch stopped", "error", err)
		}
	}()
	return router, nil
}

func routesFromJSON(payload []byte) ([]provider.Route, error) {
	var configs []routeConfig
	if err := json.Unmarshal(payload, &configs); err != nil || len(configs) == 0 {
		return nil, fmt.Errorf("configure at least one model route: %w", err)
	}
	routes := make([]provider.Route, 0, len(configs))
	for index, config := range configs {
		if config.ID == "" || config.PublicModel == "" || config.Provider == "" || config.Region == "" || config.HomeRegion == "" {
			return nil, fmt.Errorf("route %d requires id, provider, public_model, region, and home_region", index)
		}
		if !validNonNegativeCost(config.InputCost) || !validNonNegativeCost(config.OutputCost) || !validNonNegativeCost(config.CachedInputCost) {
			return nil, fmt.Errorf("route %q prices must be finite and non-negative", config.ID)
		}
		executor, cacheProtector, cacheAnchorBuilder, err := buildProviderComponents(config)
		if err != nil {
			return nil, fmt.Errorf("route %q: %w", config.ID, err)
		}
		effectiveAt, err := time.Parse(time.RFC3339, config.PriceEffectiveAt)
		if err != nil || config.PriceSnapshotID == "" || config.PriceSource == "" || config.Currency == "" {
			return nil, fmt.Errorf("route %q requires immutable price_snapshot_id, RFC3339 price_effective_at, price_source, and currency", config.ID)
		}
		cacheWriteCost, err := cacheWriteCostPerMillion(config)
		if err != nil {
			return nil, fmt.Errorf("route %q: %w", config.ID, err)
		}
		routes = append(routes, provider.Route{
			ID: config.ID, Provider: config.Provider, Model: config.PublicModel, Region: config.Region,
			HomeRegion: config.HomeRegion, CredentialScope: config.CredentialScope, Healthy: config.Healthy == nil || *config.Healthy,
			InputCost: config.InputCost, OutputCost: config.OutputCost, Executor: executor,
			Profile: provider.CapabilityProfile{Revision: max(config.CapabilityRevision, 1), Features: config.Capabilities},
			PriceSnapshot: core.PriceSnapshot{
				ID: config.PriceSnapshotID, Provider: config.Provider, Model: config.ProviderModel, Region: config.Region,
				Currency: config.Currency, InputPerMillionMicros: currencyMicros(config.InputCost),
				CachedInputPerMillionMicros: currencyMicros(config.CachedInputCost),
				CacheWritePerMillionMicros:  currencyMicros(cacheWriteCost), OutputPerMillionMicros: currencyMicros(config.OutputCost),
				EffectiveAt: effectiveAt.Unix(), Source: config.PriceSource,
			},
			CacheUsageReliable: config.CacheUsageReliable,
			CacheProtector:     cacheProtector,
			CacheAnchorBuilder: cacheAnchorBuilder,
		})
	}
	return routes, nil
}

func buildProviderComponents(config routeConfig) (provider.ResponseExecutor, provider.CacheProtector, provider.CacheAnchorBuilder, error) {
	if config.Provider == "anthropic" {
		var ttl time.Duration
		var apiVersion string
		var writeCostMicros int64
		if refresh := config.CacheRefresh; refresh != nil {
			if refresh.Kind != "anthropic" {
				return nil, nil, nil, fmt.Errorf("Anthropic route cannot use %q cache refresh", refresh.Kind)
			}
			if refresh.TTLSeconds <= 0 || math.IsNaN(refresh.WriteCostPerMillion) || math.IsInf(refresh.WriteCostPerMillion, 0) || refresh.WriteCostPerMillion < 0 {
				return nil, nil, nil, errors.New("Anthropic cache refresh requires positive ttl_seconds and finite non-negative write_cost_per_million")
			}
			if refresh.BaseURL != "" && strings.TrimRight(refresh.BaseURL, "/") != strings.TrimRight(config.BaseURL, "/") {
				return nil, nil, nil, errors.New("Anthropic execution and cache refresh must use the same base_url")
			}
			if refresh.APIKeyEnv != "" && refresh.APIKeyEnv != config.APIKeyEnv {
				return nil, nil, nil, errors.New("Anthropic execution and cache refresh must use the same credential")
			}
			ttl = time.Duration(refresh.TTLSeconds) * time.Second
			apiVersion = refresh.APIVersion
			cacheWriteCost, err := cacheWriteCostPerMillion(config)
			if err != nil {
				return nil, nil, nil, err
			}
			if cacheWriteCost <= 0 {
				return nil, nil, nil, errors.New("Anthropic prompt caching requires positive cache_write_cost_per_million")
			}
			writeCostMicros = currencyMicros(cacheWriteCost)
		}
		adapter, err := anthropic.NewAdapter(anthropic.AdapterConfig{
			BaseURL: config.BaseURL, APIKey: os.Getenv(config.APIKeyEnv), APIVersion: apiVersion,
			TTL: ttl, Model: config.ProviderModel, RouteID: config.ID,
			CredentialScope: config.CredentialScope, Region: config.Region,
			CacheWritePerMillionMicros: writeCostMicros,
			EnablePromptCaching:        config.CacheRefresh != nil,
		})
		if err != nil {
			return nil, nil, nil, err
		}
		if config.CacheRefresh == nil {
			return adapter, nil, nil, nil
		}
		return adapter, adapter, adapter, nil
	}
	if config.CacheRefresh != nil {
		return nil, nil, nil, errors.New("proactive cache refresh is enabled only for conformance-tested direct Anthropic routes")
	}

	executor, err := openaicompat.New(openaicompat.Config{
		BaseURL: config.BaseURL, APIKey: os.Getenv(config.APIKeyEnv), Model: config.ProviderModel,
		Headers: config.Headers,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	return executor, nil, nil, nil
}

func cacheWriteCostPerMillion(config routeConfig) (float64, error) {
	value := config.CacheWriteCost
	if config.CacheRefresh != nil && config.CacheRefresh.WriteCostPerMillion != 0 {
		if value != 0 && value != config.CacheRefresh.WriteCostPerMillion {
			return 0, errors.New("cache write price is declared inconsistently")
		}
		value = config.CacheRefresh.WriteCostPerMillion
	}
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, errors.New("cache_write_cost_per_million must be finite and non-negative")
	}
	return value, nil
}

func runCacheWorker(ctx context.Context, coordinator *cacheprotection.Coordinator, router *provider.StaticRouter) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, err := coordinator.RunDue(ctx, 32, router.ResolveCacheProtector)
			if err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("cache protection worker iteration failed", "error", err)
			}
		}
	}
}

func runRetentionWorker(ctx context.Context, responseStore store.ResponseStore, localRegion string) {
	retentionStore, ok := responseStore.(store.RetentionStore)
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

func currencyMicros(amount float64) int64 {
	return int64(math.Round(amount * 1_000_000))
}

func validNonNegativeCost(amount float64) bool {
	return !math.IsNaN(amount) && !math.IsInf(amount, 0) && amount >= 0
}

func ensureTenants(ctx context.Context, db *sql.DB, apiKeys, homeRegions map[string]string, executionEpochs map[string]int64, tenantPolicies map[string]core.TenantPolicy) error {
	seen := make(map[string]struct{})
	for _, tenantID := range apiKeys {
		if _, exists := seen[tenantID]; exists {
			continue
		}
		seen[tenantID] = struct{}{}
		homeRegion := homeRegions[tenantID]
		if homeRegion == "" {
			return fmt.Errorf("tenant %q has no configured home region", tenantID)
		}
		executionEpoch := executionEpochs[tenantID]
		if executionEpoch == 0 {
			executionEpoch = 1
		}
		if executionEpoch < 0 {
			return fmt.Errorf("tenant %q has an invalid execution epoch", tenantID)
		}
		policy := tenantPolicies[tenantID]
		if policy.Revision == 0 {
			policy.Revision = 1
		}
		policyDocument := policy
		policyDocument.Revision = 0
		policyPayload, err := json.Marshal(policyDocument)
		if err != nil {
			return fmt.Errorf("encode tenant %q policy: %w", tenantID, err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO tenants (id, home_region, execution_epoch, policy_revision, policy) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (id) DO NOTHING`, tenantID, homeRegion, executionEpoch, policy.Revision, policyPayload); err != nil {
			return fmt.Errorf("ensure tenant %q: %w", tenantID, err)
		}
		var storedRegion string
		var storedEpoch, storedPolicyRevision int64
		var policyMatches bool
		if err := db.QueryRowContext(ctx, `
			SELECT home_region, execution_epoch, policy_revision, policy = $2::jsonb
			FROM tenants WHERE id = $1`, tenantID, policyPayload).Scan(
			&storedRegion, &storedEpoch, &storedPolicyRevision, &policyMatches,
		); err != nil {
			return err
		}
		if storedRegion != homeRegion || storedEpoch != executionEpoch || storedPolicyRevision != policy.Revision || !policyMatches {
			return fmt.Errorf("tenant %q configuration does not match durable home region, execution epoch, or policy revision", tenantID)
		}
	}
	return nil
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
	}
	return result, nil
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
