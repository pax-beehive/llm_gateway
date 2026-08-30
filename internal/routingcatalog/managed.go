package routingcatalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/provider"
)

type RouteAdministrativeStatus string

const (
	RouteActive   RouteAdministrativeStatus = "active"
	RouteDraining RouteAdministrativeStatus = "draining"
	RouteDisabled RouteAdministrativeStatus = "disabled"
)

type SelectionPolicy struct {
	Priority       int  `json:"priority"`
	Weight         int  `json:"weight"`
	MaxConcurrency int  `json:"max_concurrency,omitempty"`
	StickyRouting  bool `json:"sticky_routing_eligible,omitempty"`
}

type TenantVisibilityPolicy struct {
	AllTenants           bool             `json:"all_tenants,omitempty"`
	TenantIDs            []string         `json:"tenant_ids,omitempty"`
	LimitPolicyRevisions map[string]int64 `json:"limit_policy_revisions,omitempty"`
}

type CacheProtectionRoutePolicy struct {
	Enabled    bool  `json:"enabled"`
	TTLSeconds int64 `json:"ttl_seconds,omitempty"`
}

type ManagedRoute struct {
	ID                        string                                `json:"route_id"`
	PublicModel               string                                `json:"public_model"`
	ProviderConnectionID      string                                `json:"provider_connection_id"`
	ProviderModel             string                                `json:"provider_model"`
	ExecutionRegion           string                                `json:"execution_region"`
	HomeRegion                string                                `json:"home_region"`
	CapabilityProfileRevision int64                                 `json:"capability_profile_revision"`
	Capabilities              map[string]provider.CapabilitySupport `json:"capabilities"`
	ProviderCostSnapshot      core.PriceSnapshot                    `json:"provider_cost_snapshot"`
	AdministrativeStatus      RouteAdministrativeStatus             `json:"administrative_status"`
	SelectionPolicy           SelectionPolicy                       `json:"selection_policy"`
	TenantVisibility          TenantVisibilityPolicy                `json:"tenant_visibility_policy"`
	CacheUsageReliable        bool                                  `json:"cache_usage_reliable"`
	CacheProtectionPolicy     CacheProtectionRoutePolicy            `json:"cache_protection_policy"`
	EmbeddingPath             string                                `json:"embedding_path,omitempty"`
	ModerationPath            string                                `json:"moderation_path,omitempty"`
	RerankPath                string                                `json:"rerank_path,omitempty"`
	EmbeddingDimensions       int                                   `json:"embedding_dimensions,omitempty"`
}

type Document struct {
	Routes []ManagedRoute `json:"routes"`
}

type ConnectionDescriptor struct {
	ID                string
	Provider          string
	Region            string
	CredentialScope   string
	Enabled           bool
	ObservedHealthy   *bool
	Revision          int64
	CredentialVersion int64
	CapabilityProfile provider.CapabilityProfile
}

type ConnectionLookup interface {
	LookupConnection(context.Context, string) (ConnectionDescriptor, error)
}

type ConnectionLookupFunc func(context.Context, string) (ConnectionDescriptor, error)

func (function ConnectionLookupFunc) LookupConnection(ctx context.Context, id string) (ConnectionDescriptor, error) {
	return function(ctx, id)
}

type TenantPolicyReferenceLookup interface {
	TenantPolicyRevisionExists(context.Context, string, int64) (bool, error)
}

type ValidationIssue struct {
	Code    string `json:"code"`
	Path    string `json:"path"`
	Message string `json:"message"`
}

type ValidationReport struct {
	Valid    bool              `json:"valid"`
	Hash     string            `json:"hash"`
	Errors   []ValidationIssue `json:"errors"`
	Warnings []ValidationIssue `json:"warnings"`
}

var managedResourceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~-]{0,127}$`)

func Validate(ctx context.Context, document Document, connections ConnectionLookup) ValidationReport {
	report := ValidationReport{Errors: []ValidationIssue{}, Warnings: []ValidationIssue{}}
	addError := func(code, path, message string) {
		report.Errors = append(report.Errors, ValidationIssue{Code: code, Path: path, Message: message})
	}
	addWarning := func(code, path, message string) {
		report.Warnings = append(report.Warnings, ValidationIssue{Code: code, Path: path, Message: message})
	}
	if connections == nil {
		addError("connection_lookup_unavailable", "provider_connections", "Provider Connection lookup is required")
	}
	if len(document.Routes) == 0 {
		addError("catalog_empty", "routes", "at least one Model Route is required")
	}
	seenRoutes := make(map[string]struct{}, len(document.Routes))
	publicModels := make(map[string]struct{}, len(document.Routes))
	priceSnapshots := make(map[string]core.PriceSnapshot, len(document.Routes))
	for _, route := range document.Routes {
		if model := strings.TrimSpace(route.PublicModel); model != "" {
			publicModels[model] = struct{}{}
		}
	}
	for index, route := range document.Routes {
		path := fmt.Sprintf("routes[%d]", index)
		if !managedResourceIDPattern.MatchString(route.ID) {
			addError("invalid_route_id", path+".route_id", "Route ID must be a URL-safe resource identifier")
		} else if _, duplicate := seenRoutes[route.ID]; duplicate {
			addError("duplicate_route_id", path+".route_id", "Route ID must be unique within the Routing Catalog")
		}
		seenRoutes[route.ID] = struct{}{}
		if _, collides := publicModels[route.ID]; collides && route.ID != route.PublicModel {
			addError("ambiguous_model_mapping", path+".route_id", "Route ID cannot collide with another public model alias")
		}
		if strings.TrimSpace(route.PublicModel) == "" || strings.TrimSpace(route.ProviderModel) == "" {
			addError("model_required", path, "public_model and provider_model are required")
		}
		if !managedResourceIDPattern.MatchString(route.ProviderConnectionID) {
			addError("invalid_provider_connection_id", path+".provider_connection_id", "Provider Connection ID is required")
			continue
		}
		if route.ExecutionRegion == "" || route.HomeRegion == "" || route.ExecutionRegion != route.HomeRegion {
			addError("region_incompatible", path, "execution_region and home_region must be equal for the first release")
		}
		if route.AdministrativeStatus != RouteActive && route.AdministrativeStatus != RouteDraining && route.AdministrativeStatus != RouteDisabled {
			addError("invalid_administrative_status", path+".administrative_status", "administrative_status must be active, draining, or disabled")
		}
		if route.SelectionPolicy.Priority < 0 || route.SelectionPolicy.Weight <= 0 || route.SelectionPolicy.MaxConcurrency < 0 {
			addError("invalid_selection_policy", path+".selection_policy", "priority and max_concurrency must be non-negative and weight must be positive")
		}
		validateVisibility(route.TenantVisibility, path, addError, addWarning)
		validateTenantPolicyReferences(ctx, route.TenantVisibility, path, connections, addError)
		validateCapabilityProfile(route, path, addError)
		validatePrice(route, path, addError)
		if price := route.ProviderCostSnapshot; price.ID != "" {
			if existing, seen := priceSnapshots[price.ID]; seen && existing != price {
				addError("price_snapshot_identity_conflict", path+".provider_cost_snapshot.id", "one Provider Cost Snapshot ID cannot identify different immutable prices")
			} else {
				priceSnapshots[price.ID] = price
			}
		}
		if connections == nil {
			continue
		}
		connection, err := connections.LookupConnection(ctx, route.ProviderConnectionID)
		if err != nil {
			addError("provider_connection_unavailable", path+".provider_connection_id", "Provider Connection is unavailable")
			continue
		}
		if !connection.Enabled {
			addError("provider_connection_disabled", path+".provider_connection_id", "Provider Connection must be enabled")
		}
		if connection.Region != route.ExecutionRegion {
			addError("provider_connection_region_mismatch", path+".execution_region", "Model Route and Provider Connection regions must match")
		}
		identity, identityErr := provider.ParseIdentity(connection.Provider)
		if identityErr != nil {
			addError("provider_identity_unsupported", path+".provider_connection_id", "Provider Connection identity has no conformance-tested adapter")
		} else if profile, ok := identity.Profile(); !ok || profile.ResponseExecutionSeam == "" {
			addError("provider_adapter_unavailable", path+".provider_connection_id", "Provider Connection identity has no Response execution adapter")
		} else if routeDeclaresStageACapability(route) && profile.CapabilityExecutionSeam != provider.OpenAICompatibleSeam {
			addError("capability_adapter_unavailable", path+".capabilities", "Provider identity has no conformance-tested capability adapter")
		}
		if strings.TrimSpace(connection.CredentialScope) == "" {
			addError("credential_scope_required", path+".provider_connection_id", "Provider Connection requires an explicit credential scope")
		}
		if connection.Provider != route.ProviderCostSnapshot.Provider || connection.Region != route.ProviderCostSnapshot.Region || route.ProviderModel != route.ProviderCostSnapshot.Model {
			addError("price_identity_mismatch", path+".provider_cost_snapshot", "Provider Cost Snapshot must match the Provider Connection, model, and region")
		}
		if route.CapabilityProfileRevision != connection.CapabilityProfile.Revision {
			addError("capability_profile_revision_mismatch", path+".capability_profile_revision", "Model Route must bind the current Provider Connection Capability Profile revision")
		}
		for capability, support := range route.Capabilities {
			available := connection.CapabilityProfile.Features[capability]
			if support == provider.CapabilityNative && available != provider.CapabilityNative ||
				support == provider.CapabilityTranslated && available != provider.CapabilityNative && available != provider.CapabilityTranslated {
				addError("capability_not_supported", path+".capabilities."+capability, "Model Route exceeds the Provider Connection capability declaration")
			}
		}
		if route.CacheProtectionPolicy.Enabled && connection.Provider != "anthropic" {
			addError("cache_protection_unsupported", path+".cache_protection_policy", "active Cache Protection requires a conformance-tested Provider adapter")
		}
		if route.CacheProtectionPolicy.Enabled && route.CacheProtectionPolicy.TTLSeconds <= 0 {
			addError("cache_protection_ttl_required", path+".cache_protection_policy.ttl_seconds", "active Cache Protection requires a positive TTL")
		}
		if route.CacheProtectionPolicy.Enabled && (!route.CacheUsageReliable || route.ProviderCostSnapshot.CacheWritePerMillionMicros <= 0) {
			addError("cache_anchor_inconsistent", path+".cache_protection_policy", "active Cache Protection requires reliable cache usage and a positive immutable cache-write price")
		}
	}
	validateAdvertisedModels(document, addError)
	sort.Slice(report.Errors, func(i, j int) bool {
		if report.Errors[i].Path != report.Errors[j].Path {
			return report.Errors[i].Path < report.Errors[j].Path
		}
		return report.Errors[i].Code < report.Errors[j].Code
	})
	sort.Slice(report.Warnings, func(i, j int) bool {
		if report.Warnings[i].Path != report.Warnings[j].Path {
			return report.Warnings[i].Path < report.Warnings[j].Path
		}
		return report.Warnings[i].Code < report.Warnings[j].Code
	})
	report.Valid = len(report.Errors) == 0
	report.Hash = validationHash(document, report.Errors, report.Warnings)
	return report
}

func routeDeclaresStageACapability(route ManagedRoute) bool {
	for _, capability := range []string{"embeddings", "moderation", "rerank"} {
		if support := route.Capabilities[capability]; support == provider.CapabilityNative || support == provider.CapabilityTranslated {
			return true
		}
	}
	return false
}

func validateVisibility(policy TenantVisibilityPolicy, path string, addError, addWarning func(string, string, string)) {
	if policy.AllTenants && len(policy.TenantIDs) > 0 {
		addError("ambiguous_tenant_visibility", path+".tenant_visibility_policy", "all_tenants and tenant_ids are mutually exclusive")
	}
	if !policy.AllTenants && len(policy.TenantIDs) == 0 {
		addError("tenant_visibility_required", path+".tenant_visibility_policy", "declare all_tenants or at least one Tenant ID")
	}
	if policy.AllTenants && len(policy.LimitPolicyRevisions) > 0 {
		addError("ambiguous_limit_policy_reference", path+".tenant_visibility_policy.limit_policy_revisions", "global visibility cannot bind Tenant Limit Policy revisions")
	}
	if policy.AllTenants {
		addWarning("global_tenant_visibility", path+".tenant_visibility_policy.all_tenants", "route is visible to every Tenant")
	}
	seen := make(map[string]struct{}, len(policy.TenantIDs))
	for _, tenantID := range policy.TenantIDs {
		if !managedResourceIDPattern.MatchString(tenantID) {
			addError("invalid_tenant_visibility", path+".tenant_visibility_policy.tenant_ids", "Tenant visibility contains an invalid Tenant ID")
		} else if _, duplicate := seen[tenantID]; duplicate {
			addError("duplicate_tenant_visibility", path+".tenant_visibility_policy.tenant_ids", "Tenant visibility cannot contain duplicates")
		}
		seen[tenantID] = struct{}{}
	}
}

func validateTenantPolicyReferences(ctx context.Context, policy TenantVisibilityPolicy, path string, lookup ConnectionLookup, addError func(string, string, string)) {
	if policy.AllTenants {
		return
	}
	tenantSet := make(map[string]struct{}, len(policy.TenantIDs))
	for _, tenantID := range policy.TenantIDs {
		tenantSet[tenantID] = struct{}{}
		revision, exists := policy.LimitPolicyRevisions[tenantID]
		if !exists || revision <= 0 {
			addError("limit_policy_reference_required", path+".tenant_visibility_policy.limit_policy_revisions", "each visible Tenant requires a positive immutable Limit Policy revision")
			continue
		}
		if references, ok := lookup.(TenantPolicyReferenceLookup); ok {
			exists, err := references.TenantPolicyRevisionExists(ctx, tenantID, revision)
			if err != nil || !exists {
				addError("limit_policy_reference_unavailable", path+".tenant_visibility_policy.limit_policy_revisions."+tenantID, "referenced Tenant Limit Policy revision is unavailable")
			}
		}
	}
	for tenantID, revision := range policy.LimitPolicyRevisions {
		if _, visible := tenantSet[tenantID]; !visible || revision <= 0 {
			addError("invalid_limit_policy_reference", path+".tenant_visibility_policy.limit_policy_revisions."+tenantID, "Limit Policy references must match visible Tenant IDs and use positive revisions")
		}
	}
}

func validateCapabilityProfile(route ManagedRoute, path string, addError func(string, string, string)) {
	if route.CapabilityProfileRevision <= 0 || len(route.Capabilities) == 0 {
		addError("capability_profile_required", path+".capability_profile_revision", "a Capability Profile revision and capability declaration are required")
		return
	}
	for capability, support := range route.Capabilities {
		if strings.TrimSpace(capability) == "" || support != provider.CapabilityNative && support != provider.CapabilityTranslated && support != provider.CapabilityUnsupported {
			addError("invalid_capability_support", path+".capabilities."+capability, "capability support must be native, translated, or unsupported")
		}
	}
}

func validatePrice(route ManagedRoute, path string, addError func(string, string, string)) {
	price := route.ProviderCostSnapshot
	if price.ID == "" || price.Provider == "" || price.Model == "" || price.Region == "" || price.Currency == "" || price.Source == "" || price.EffectiveAt <= 0 {
		addError("price_snapshot_required", path+".provider_cost_snapshot", "an immutable Provider Cost Snapshot identity is required")
	}
	values := []int64{price.InputPerMillionMicros, price.CachedInputPerMillionMicros, price.CacheWritePerMillionMicros,
		price.OutputPerMillionMicros, price.EmbeddingInputPerMillionMicros, price.ModerationInputPerMillionMicros, price.RerankDocumentPerThousandMicros}
	for _, value := range values {
		if value < 0 || value == math.MinInt64 {
			addError("invalid_provider_cost", path+".provider_cost_snapshot", "Provider costs must be finite and non-negative")
			break
		}
	}
}

func validateAdvertisedModels(document Document, addError func(string, string, string)) {
	advertised := make(map[string]bool)
	for _, route := range document.Routes {
		model := strings.TrimSpace(route.PublicModel)
		if model == "" {
			continue
		}
		if _, exists := advertised[model]; !exists {
			advertised[model] = false
		}
		if route.AdministrativeStatus == RouteActive && route.Capabilities["text"] == provider.CapabilityNative {
			advertised[model] = true
		}
	}
	models := make([]string, 0, len(advertised))
	for model := range advertised {
		models = append(models, model)
	}
	sort.Strings(models)
	for _, model := range models {
		if !advertised[model] {
			addError("public_model_unavailable", "routes", fmt.Sprintf("public model %q requires at least one active native-text Model Route", model))
		}
	}
}

func validationHash(document Document, validationErrors, warnings []ValidationIssue) string {
	payload, err := json.Marshal(struct {
		Document Document          `json:"document"`
		Errors   []ValidationIssue `json:"errors"`
		Warnings []ValidationIssue `json:"warnings"`
	}{Document: document, Errors: validationErrors, Warnings: warnings})
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

var ErrConnectionNotFound = errors.New("Provider Connection not found")
