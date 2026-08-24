package main

import (
	"context"
	"database/sql"
	"encoding/json"
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

	"github.com/toddzheng/llm-gateway/internal/httpapi"
	"github.com/toddzheng/llm-gateway/internal/provider"
	"github.com/toddzheng/llm-gateway/internal/provider/openaicompat"
	"github.com/toddzheng/llm-gateway/internal/runtime"
	"github.com/toddzheng/llm-gateway/internal/store"
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
	apiKeys, err := parseStringMapEnv("GATEWAY_API_KEYS_JSON")
	if err != nil || len(apiKeys) == 0 {
		return fmt.Errorf("GATEWAY_API_KEYS_JSON: configure at least one token-to-tenant mapping: %w", err)
	}
	homeRegions, err := parseStringMapEnv("GATEWAY_TENANT_HOME_REGIONS_JSON")
	if err != nil {
		return fmt.Errorf("GATEWAY_TENANT_HOME_REGIONS_JSON: %w", err)
	}

	responseStore, cleanup, err := configureStore(apiKeys, homeRegions)
	if err != nil {
		return err
	}
	defer cleanup()
	router, err := configureRouter()
	if err != nil {
		return err
	}
	engine := runtime.New(responseStore, router)
	handler := httpapi.New(httpapi.Config{
		Runtime: engine, Authenticator: httpapi.StaticAuthenticator(apiKeys), TenantHomeRegions: homeRegions,
	})

	address := envOr("GATEWAY_ADDR", ":8080")
	server := &http.Server{
		Addr: address, Handler: handler, ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 20,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
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

func configureStore(apiKeys, homeRegions map[string]string) (store.ResponseStore, func(), error) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		if os.Getenv("GATEWAY_DEV_MEMORY_STORE") != "true" {
			return nil, func() {}, errors.New("DATABASE_URL is required unless GATEWAY_DEV_MEMORY_STORE=true")
		}
		return store.NewMemoryResponseStore(), func() {}, nil
	}
	if os.Getenv("GATEWAY_ENV") == "production" && os.Getenv("GATEWAY_DURABILITY_ATTESTATION") != "sync-multi-az" {
		return nil, func() {}, errors.New("production requires GATEWAY_DURABILITY_ATTESTATION=sync-multi-az")
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, func() {}, err
	}
	cleanup := func() { _ = db.Close() }
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("connect PostgreSQL: %w", err)
	}
	postgresStore := store.NewPostgresResponseStore(db)
	if os.Getenv("GATEWAY_MIGRATE") == "true" {
		if err := postgresStore.Migrate(ctx); err != nil {
			cleanup()
			return nil, func() {}, err
		}
	}
	if err := ensureTenants(ctx, db, apiKeys, homeRegions); err != nil {
		cleanup()
		return nil, func() {}, err
	}
	return postgresStore, cleanup, nil
}

func configureRouter() (provider.Router, error) {
	if os.Getenv("GATEWAY_DEV_ECHO") == "true" {
		return provider.NewStaticRouter(provider.NewEchoExecutor()), nil
	}
	var configs []routeConfig
	if err := json.Unmarshal([]byte(os.Getenv("GATEWAY_ROUTES_JSON")), &configs); err != nil || len(configs) == 0 {
		return nil, fmt.Errorf("GATEWAY_ROUTES_JSON: configure at least one model route: %w", err)
	}
	routes := make([]provider.Route, 0, len(configs))
	for index, config := range configs {
		if config.ID == "" || config.PublicModel == "" || config.Provider == "" || config.Region == "" || config.HomeRegion == "" {
			return nil, fmt.Errorf("route %d requires id, provider, public_model, region, and home_region", index)
		}
		executor, err := openaicompat.New(openaicompat.Config{
			BaseURL: config.BaseURL, APIKey: os.Getenv(config.APIKeyEnv), Model: config.ProviderModel,
			Headers: config.Headers,
		})
		if err != nil {
			return nil, fmt.Errorf("route %q: %w", config.ID, err)
		}
		routes = append(routes, provider.Route{
			ID: config.ID, Provider: config.Provider, Model: config.PublicModel, Region: config.Region,
			HomeRegion: config.HomeRegion, CredentialScope: config.CredentialScope, Healthy: true,
			InputCost: config.InputCost, OutputCost: config.OutputCost, Executor: executor,
			Profile: provider.CapabilityProfile{Revision: max(config.CapabilityRevision, 1), Features: config.Capabilities},
		})
	}
	return provider.NewRouter(routes...), nil
}

func ensureTenants(ctx context.Context, db *sql.DB, apiKeys, homeRegions map[string]string) error {
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
		if _, err := db.ExecContext(ctx, `
			INSERT INTO tenants (id, home_region) VALUES ($1, $2)
			ON CONFLICT (id) DO UPDATE SET home_region = EXCLUDED.home_region, updated_at = now()`, tenantID, homeRegion); err != nil {
			return fmt.Errorf("ensure tenant %q: %w", tenantID, err)
		}
	}
	return nil
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

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
