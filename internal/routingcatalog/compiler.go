package routingcatalog

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/toddzheng/llm-gateway/internal/provider"
	"github.com/toddzheng/llm-gateway/internal/providerconnection"
)

type RuntimeConnectionResolver interface {
	Resolve(context.Context, string) (providerconnection.ResolvedConnection, error)
}

type RuntimeConnectionResolverFunc func(context.Context, string) (providerconnection.ResolvedConnection, error)

func (function RuntimeConnectionResolverFunc) Resolve(ctx context.Context, id string) (providerconnection.ResolvedConnection, error) {
	return function(ctx, id)
}

func Compile(ctx context.Context, document Document, resolver RuntimeConnectionResolver, httpClient *http.Client) ([]provider.Route, error) {
	routes, _, err := compileManaged(ctx, document, resolver, httpClient, false)
	return routes, err
}

func compileManaged(ctx context.Context, document Document, resolver RuntimeConnectionResolver, httpClient *http.Client, skipAdministrativelyUnavailable bool) ([]provider.Route, ValidationReport, error) {
	if resolver == nil {
		return nil, ValidationReport{}, errors.New("Routing Catalog runtime compilation requires Provider Connection resolution")
	}
	resolved := make(map[string]providerconnection.ResolvedConnection, len(document.Routes))
	for _, managed := range document.Routes {
		if _, exists := resolved[managed.ProviderConnectionID]; exists {
			continue
		}
		connection, err := resolver.Resolve(ctx, managed.ProviderConnectionID)
		if err != nil {
			if skipAdministrativelyUnavailable && errors.Is(err, providerconnection.ErrNotFound) {
				continue
			}
			return nil, ValidationReport{}, fmt.Errorf("resolve Provider Connection %q: %w", managed.ProviderConnectionID, err)
		}
		resolved[managed.ProviderConnectionID] = connection
	}
	effective := document
	if skipAdministrativelyUnavailable {
		effective.Routes = make([]ManagedRoute, 0, len(document.Routes))
		for _, route := range document.Routes {
			if _, available := resolved[route.ProviderConnectionID]; available {
				effective.Routes = append(effective.Routes, route)
			}
		}
		if len(effective.Routes) == 0 {
			return []provider.Route{}, ValidationReport{Valid: true, Hash: validationHash(document, []ValidationIssue{}, []ValidationIssue{}), Errors: []ValidationIssue{}, Warnings: []ValidationIssue{}}, nil
		}
	}
	report := Validate(ctx, effective, ConnectionLookupFunc(func(_ context.Context, id string) (ConnectionDescriptor, error) {
		connection, ok := resolved[id]
		if !ok {
			return ConnectionDescriptor{}, ErrConnectionNotFound
		}
		return descriptorFromResolved(connection), nil
	}))
	if !report.Valid {
		return nil, report, fmt.Errorf("%w: %s", ErrValidationFailed, report.Hash)
	}
	routes := make([]provider.Route, 0, len(effective.Routes))
	for _, managed := range effective.Routes {
		connection := resolved[managed.ProviderConnectionID]
		config := legacyConfigFromManaged(managed, connection.Connection)
		secret := string(connection.Secret)
		executor, cacheProtector, cacheAnchorBuilder, err := buildProviderComponentsWithCredential(config, secret, httpClient)
		if err != nil {
			return nil, report, fmt.Errorf("compile Model Route %q: %w", managed.ID, err)
		}
		capabilityAdapter, err := buildCapabilityAdapterWithCredential(config, secret)
		if err != nil {
			return nil, report, fmt.Errorf("compile Model Route %q capability adapter: %w", managed.ID, err)
		}
		capabilityProfile := provider.CapabilityProfile{Revision: managed.CapabilityProfileRevision, Features: managed.Capabilities}
		route := provider.Route{
			ID: managed.ID, Provider: connection.Connection.Provider, Model: managed.PublicModel,
			Region: managed.ExecutionRegion, HomeRegion: managed.HomeRegion, CredentialScope: connection.Connection.CredentialScope,
			TenantIDs: append([]string(nil), managed.TenantVisibility.TenantIDs...), Healthy: connection.ObservedHealthy == nil || *connection.ObservedHealthy,
			InputCost:  float64(managed.ProviderCostSnapshot.InputPerMillionMicros) / 1_000_000,
			OutputCost: float64(managed.ProviderCostSnapshot.OutputPerMillionMicros) / 1_000_000,
			Profile:    capabilityProfile, Executor: executor, CacheProtector: cacheProtector, CacheAnchorBuilder: cacheAnchorBuilder,
			PriceSnapshot: managed.ProviderCostSnapshot, CacheUsageReliable: managed.CacheUsageReliable,
			AdministrativeStatus: provider.RouteAdministrativeStatus(managed.AdministrativeStatus),
			Priority:             managed.SelectionPolicy.Priority, Weight: managed.SelectionPolicy.Weight,
			MaxConcurrency: managed.SelectionPolicy.MaxConcurrency, StickyRouting: managed.SelectionPolicy.StickyRouting,
		}
		if capabilityAdapter != nil {
			if declaredCapability(managed.Capabilities["embeddings"]) {
				route.EmbeddingExecutor = capabilityAdapter
			}
			if declaredCapability(managed.Capabilities["moderation"]) {
				route.ModerationExecutor = capabilityAdapter
			}
			if declaredCapability(managed.Capabilities["rerank"]) {
				route.RerankExecutor = capabilityAdapter
			}
		}
		routes = append(routes, provider.WithConcurrencyLimit(route))
	}
	return routes, report, nil
}

func descriptorFromResolved(resolved providerconnection.ResolvedConnection) ConnectionDescriptor {
	connection := resolved.Connection
	return ConnectionDescriptor{
		ID: connection.ID, Provider: connection.Provider, Region: connection.Region, CredentialScope: connection.CredentialScope,
		Enabled: connection.AdministrativeStatus == providerconnection.StatusEnabled, Revision: connection.Revision,
		CredentialVersion: connection.CredentialVersion, CapabilityProfile: connection.CapabilityDeclaration,
		ObservedHealthy: resolved.ObservedHealthy,
	}
}

func legacyConfigFromManaged(managed ManagedRoute, connection providerconnection.ProviderConnection) RouteConfig {
	config := RouteConfig{
		ID: managed.ID, Provider: connection.Provider, PublicModel: managed.PublicModel, ProviderModel: managed.ProviderModel,
		BaseURL: connection.BaseURL, Region: managed.ExecutionRegion, HomeRegion: managed.HomeRegion,
		TenantIDs: append([]string(nil), managed.TenantVisibility.TenantIDs...), CredentialScope: connection.CredentialScope,
		Capabilities: managed.Capabilities, CapabilityRevision: managed.CapabilityProfileRevision,
		EmbeddingPath: managed.EmbeddingPath, ModerationPath: managed.ModerationPath,
		RerankPath: managed.RerankPath, EmbeddingDimensions: managed.EmbeddingDimensions,
		InputCost:           float64(managed.ProviderCostSnapshot.InputPerMillionMicros) / 1_000_000,
		CachedInputCost:     float64(managed.ProviderCostSnapshot.CachedInputPerMillionMicros) / 1_000_000,
		CacheWriteCost:      float64(managed.ProviderCostSnapshot.CacheWritePerMillionMicros) / 1_000_000,
		OutputCost:          float64(managed.ProviderCostSnapshot.OutputPerMillionMicros) / 1_000_000,
		EmbeddingInputCost:  float64(managed.ProviderCostSnapshot.EmbeddingInputPerMillionMicros) / 1_000_000,
		ModerationInputCost: float64(managed.ProviderCostSnapshot.ModerationInputPerMillionMicros) / 1_000_000,
		RerankDocumentCost:  float64(managed.ProviderCostSnapshot.RerankDocumentPerThousandMicros) / 1_000_000,
		PriceSnapshotID:     managed.ProviderCostSnapshot.ID, PriceEffectiveAt: timeFromUnix(managed.ProviderCostSnapshot.EffectiveAt),
		PriceSource: managed.ProviderCostSnapshot.Source, Currency: managed.ProviderCostSnapshot.Currency,
		CacheUsageReliable: managed.CacheUsageReliable,
	}
	if managed.CacheProtectionPolicy.Enabled {
		config.CacheRefresh = &CacheRefreshConfig{
			Kind: connection.Provider, BaseURL: connection.BaseURL, TTLSeconds: managed.CacheProtectionPolicy.TTLSeconds,
			WriteCostPerMillion: config.CacheWriteCost,
		}
	}
	return config
}

func timeFromUnix(value int64) string {
	return time.Unix(value, 0).UTC().Format(time.RFC3339)
}
