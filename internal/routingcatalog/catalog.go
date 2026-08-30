// Package routingcatalog owns the ADR 0006 draft, validation, immutable
// publication, regional projection, and runtime assembly workflow for Model
// Routes. Legacy environment-backed route parsing remains as a bounded
// bootstrap compatibility path.
package routingcatalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/toddzheng/llm-gateway/internal/configuration"
	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/provider"
	"github.com/toddzheng/llm-gateway/internal/provider/anthropic"
	"github.com/toddzheng/llm-gateway/internal/provider/openaicapabilities"
	"github.com/toddzheng/llm-gateway/internal/provider/openaicompat"
	"github.com/toddzheng/llm-gateway/internal/provider/openairesponses"
)

type RouteConfig struct {
	ID                  string                                `json:"id"`
	Provider            string                                `json:"provider"`
	PublicModel         string                                `json:"public_model"`
	ProviderModel       string                                `json:"provider_model"`
	BaseURL             string                                `json:"base_url"`
	APIKeyEnv           string                                `json:"api_key_env"`
	Region              string                                `json:"region"`
	HomeRegion          string                                `json:"home_region"`
	TenantIDs           []string                              `json:"tenant_ids,omitempty"`
	CredentialScope     string                                `json:"credential_scope"`
	Capabilities        map[string]provider.CapabilitySupport `json:"capabilities"`
	CapabilityRevision  int64                                 `json:"capability_revision"`
	Headers             map[string]string                     `json:"headers"`
	InputCost           float64                               `json:"input_cost_per_million"`
	OutputCost          float64                               `json:"output_cost_per_million"`
	EmbeddingInputCost  float64                               `json:"embedding_input_cost_per_million"`
	ModerationInputCost float64                               `json:"moderation_input_cost_per_million"`
	RerankDocumentCost  float64                               `json:"rerank_document_cost_per_thousand"`
	EmbeddingPath       string                                `json:"embedding_path,omitempty"`
	ModerationPath      string                                `json:"moderation_path,omitempty"`
	RerankPath          string                                `json:"rerank_path,omitempty"`
	EmbeddingDimensions int                                   `json:"embedding_dimensions,omitempty"`
	CachedInputCost     float64                               `json:"cached_input_cost_per_million"`
	CacheWriteCost      float64                               `json:"cache_write_cost_per_million"`
	PriceSnapshotID     string                                `json:"price_snapshot_id"`
	PriceEffectiveAt    string                                `json:"price_effective_at"`
	PriceSource         string                                `json:"price_source"`
	Currency            string                                `json:"currency"`
	CacheUsageReliable  bool                                  `json:"cache_usage_reliable"`
	Healthy             *bool                                 `json:"healthy,omitempty"`
	CacheRefresh        *CacheRefreshConfig                   `json:"cache_refresh,omitempty"`
}

type CacheRefreshConfig struct {
	Kind                string  `json:"kind"`
	BaseURL             string  `json:"base_url"`
	APIKeyEnv           string  `json:"api_key_env"`
	TTLSeconds          int64   `json:"ttl_seconds"`
	APIVersion          string  `json:"api_version,omitempty"`
	WriteCostPerMillion float64 `json:"write_cost_per_million"`
}

func Publish(ctx context.Context, repository configuration.Repository, expectedRevision, revision int64, payload json.RawMessage, actor string) (configuration.Snapshot, []provider.Route, error) {
	routes, err := Parse(payload)
	if err != nil {
		return configuration.Snapshot{}, nil, fmt.Errorf("validate model_routes revision %d: %w", revision, err)
	}
	snapshot, err := repository.Publish(ctx, "model_routes", expectedRevision, revision, payload, actor)
	if err != nil {
		return configuration.Snapshot{}, nil, err
	}
	return snapshot, routes, nil
}

func Parse(payload []byte) ([]provider.Route, error) {
	var configs []RouteConfig
	if err := json.Unmarshal(payload, &configs); err != nil || len(configs) == 0 {
		return nil, fmt.Errorf("configure at least one model route: %w", err)
	}
	routes := make([]provider.Route, 0, len(configs))
	for index, config := range configs {
		if config.ID == "" || config.PublicModel == "" || config.Provider == "" || config.Region == "" || config.HomeRegion == "" {
			return nil, fmt.Errorf("route %d requires id, provider, public_model, region, and home_region", index)
		}
		identity, err := provider.ParseIdentity(config.Provider)
		if err != nil {
			return nil, fmt.Errorf("route %d provider %q is outside the first-release scope; supported providers are openai, deepseek, anthropic, and gemini", index, config.Provider)
		}
		if err := identity.ValidateBaseURL(config.BaseURL); err != nil {
			return nil, fmt.Errorf("route %q: %w", config.ID, err)
		}
		if !validCost(config.InputCost) || !validCost(config.OutputCost) || !validCost(config.CachedInputCost) || !validCost(config.EmbeddingInputCost) || !validCost(config.ModerationInputCost) || !validCost(config.RerankDocumentCost) {
			return nil, fmt.Errorf("route %q prices must be finite and non-negative", config.ID)
		}
		if config.EmbeddingDimensions < 0 {
			return nil, fmt.Errorf("route %q embedding_dimensions cannot be negative", config.ID)
		}
		seenTenants := make(map[string]struct{}, len(config.TenantIDs))
		for _, tenantID := range config.TenantIDs {
			if tenantID == "" {
				return nil, fmt.Errorf("route %q tenant_ids cannot contain an empty Tenant ID", config.ID)
			}
			if _, exists := seenTenants[tenantID]; exists {
				return nil, fmt.Errorf("route %q tenant_ids contains duplicate %q", config.ID, tenantID)
			}
			seenTenants[tenantID] = struct{}{}
		}
		executor, cacheProtector, cacheAnchorBuilder, err := BuildProviderComponentsWithHTTPClient(config, nil)
		if err != nil {
			return nil, fmt.Errorf("route %q: %w", config.ID, err)
		}
		capabilityAdapter, err := buildCapabilityAdapter(config)
		if err != nil {
			return nil, fmt.Errorf("route %q capability adapter: %w", config.ID, err)
		}
		effectiveAt, err := time.Parse(time.RFC3339, config.PriceEffectiveAt)
		if err != nil || config.PriceSnapshotID == "" || config.PriceSource == "" || config.Currency == "" {
			return nil, fmt.Errorf("route %q requires immutable price_snapshot_id, RFC3339 price_effective_at, price_source, and currency", config.ID)
		}
		cacheWriteCost, err := cacheWriteCostPerMillion(config)
		if err != nil {
			return nil, fmt.Errorf("route %q: %w", config.ID, err)
		}
		route := provider.Route{
			ID: config.ID, Provider: config.Provider, Model: config.PublicModel, Region: config.Region,
			HomeRegion: config.HomeRegion, CredentialScope: config.CredentialScope, Healthy: config.Healthy == nil || *config.Healthy,
			TenantIDs: append([]string(nil), config.TenantIDs...), InputCost: config.InputCost, OutputCost: config.OutputCost, Executor: executor,
			Profile: provider.CapabilityProfile{Revision: max(config.CapabilityRevision, 1), Features: config.Capabilities},
			PriceSnapshot: core.PriceSnapshot{
				ID: config.PriceSnapshotID, Provider: config.Provider, Model: config.ProviderModel, Region: config.Region,
				Currency: config.Currency, InputPerMillionMicros: currencyMicros(config.InputCost), CachedInputPerMillionMicros: currencyMicros(config.CachedInputCost),
				CacheWritePerMillionMicros: currencyMicros(cacheWriteCost), OutputPerMillionMicros: currencyMicros(config.OutputCost),
				EmbeddingInputPerMillionMicros: currencyMicros(config.EmbeddingInputCost), ModerationInputPerMillionMicros: currencyMicros(config.ModerationInputCost),
				RerankDocumentPerThousandMicros: currencyMicros(config.RerankDocumentCost), EffectiveAt: effectiveAt.Unix(), Source: config.PriceSource,
			},
			CacheUsageReliable: config.CacheUsageReliable, CacheProtector: cacheProtector, CacheAnchorBuilder: cacheAnchorBuilder,
		}
		if capabilityAdapter != nil {
			if declaredCapability(config.Capabilities["embeddings"]) {
				route.EmbeddingExecutor = capabilityAdapter
			}
			if declaredCapability(config.Capabilities["moderation"]) {
				route.ModerationExecutor = capabilityAdapter
			}
			if declaredCapability(config.Capabilities["rerank"]) {
				route.RerankExecutor = capabilityAdapter
			}
		}
		routes = append(routes, route)
	}
	return routes, nil
}

func buildCapabilityAdapter(config RouteConfig) (*openaicapabilities.Adapter, error) {
	return buildCapabilityAdapterWithCredential(config, os.Getenv(config.APIKeyEnv))
}

func buildCapabilityAdapterWithCredential(config RouteConfig, apiKey string) (*openaicapabilities.Adapter, error) {
	if !declaredCapability(config.Capabilities["embeddings"]) && !declaredCapability(config.Capabilities["moderation"]) && !declaredCapability(config.Capabilities["rerank"]) {
		return nil, nil
	}
	identity, err := provider.ParseIdentity(config.Provider)
	if err != nil {
		return nil, err
	}
	profile, ok := identity.Profile()
	if !ok || profile.CapabilityExecutionSeam != provider.OpenAICompatibleSeam {
		return nil, errors.New("Provider identity has no conformance-tested Stage A capability seam")
	}
	return openaicapabilities.New(openaicapabilities.Config{BaseURL: config.BaseURL, APIKey: apiKey, Model: config.ProviderModel, Headers: config.Headers, EmbeddingPath: config.EmbeddingPath, ModerationPath: config.ModerationPath, RerankPath: config.RerankPath, DefaultDimensions: config.EmbeddingDimensions})
}

func declaredCapability(support provider.CapabilitySupport) bool {
	return support == provider.CapabilityNative || support == provider.CapabilityTranslated
}

func BuildProviderComponentsWithHTTPClient(config RouteConfig, httpClient *http.Client) (provider.ResponseExecutor, provider.CacheProtector, provider.CacheAnchorBuilder, error) {
	return buildProviderComponentsWithCredential(config, os.Getenv(config.APIKeyEnv), httpClient)
}

func buildProviderComponentsWithCredential(config RouteConfig, apiKey string, httpClient *http.Client) (provider.ResponseExecutor, provider.CacheProtector, provider.CacheAnchorBuilder, error) {
	identity, err := provider.ParseIdentity(config.Provider)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := identity.ValidateBaseURL(config.BaseURL); err != nil {
		return nil, nil, nil, err
	}
	profile, ok := identity.Profile()
	if !ok {
		return nil, nil, nil, errors.New("Provider identity profile is unavailable")
	}
	if profile.ResponseExecutionSeam == provider.AnthropicMessagesSeam {
		var ttl time.Duration
		var apiVersion string
		var writeCostMicros int64
		if refresh := config.CacheRefresh; refresh != nil {
			if refresh.Kind != "anthropic" {
				return nil, nil, nil, fmt.Errorf("Anthropic route cannot use %q cache refresh", refresh.Kind)
			}
			if refresh.TTLSeconds <= 0 || !validCost(refresh.WriteCostPerMillion) {
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
		adapter, err := anthropic.NewAdapter(anthropic.AdapterConfig{BaseURL: config.BaseURL, APIKey: apiKey, APIVersion: apiVersion, TTL: ttl, Model: config.ProviderModel, RouteID: config.ID, HTTPClient: httpClient, CredentialScope: config.CredentialScope, Region: config.Region, CacheWritePerMillionMicros: writeCostMicros, EnablePromptCaching: config.CacheRefresh != nil})
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
	if profile.ResponseExecutionSeam == provider.OpenAIResponsesSeam {
		executor, err := openairesponses.New(openairesponses.Config{BaseURL: config.BaseURL, APIKey: apiKey, Model: config.ProviderModel, HTTPClient: httpClient, Headers: config.Headers})
		return executor, nil, nil, err
	}
	if profile.ResponseExecutionSeam == provider.OpenAICompatibleSeam {
		executor, err := openaicompat.New(openaicompat.Config{BaseURL: config.BaseURL, APIKey: apiKey, Model: config.ProviderModel, Dialect: openaicompat.Dialect(config.Provider), HTTPClient: httpClient, Headers: config.Headers})
		return executor, nil, nil, err
	}
	return nil, nil, nil, errors.New("Provider identity has no conformance-tested Response execution seam")
}

func cacheWriteCostPerMillion(config RouteConfig) (float64, error) {
	value := config.CacheWriteCost
	if config.CacheRefresh != nil && config.CacheRefresh.WriteCostPerMillion != 0 {
		if value != 0 && value != config.CacheRefresh.WriteCostPerMillion {
			return 0, errors.New("cache write price is declared inconsistently")
		}
		value = config.CacheRefresh.WriteCostPerMillion
	}
	if !validCost(value) {
		return 0, errors.New("cache_write_cost_per_million must be finite and non-negative")
	}
	return value, nil
}

func validCost(amount float64) bool {
	return !math.IsNaN(amount) && !math.IsInf(amount, 0) && amount >= 0
}
func currencyMicros(amount float64) int64 { return int64(math.Round(amount * 1_000_000)) }
