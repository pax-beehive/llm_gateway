package routingcatalog_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/provider"
	"github.com/toddzheng/llm-gateway/internal/providerconnection"
	"github.com/toddzheng/llm-gateway/internal/routingcatalog"
)

func TestValidateAcceptsRouteBackedByEnabledProviderConnection(t *testing.T) {
	document := validDocument()
	lookup := routingcatalog.ConnectionLookupFunc(func(_ context.Context, id string) (routingcatalog.ConnectionDescriptor, error) {
		if id != "pc-openai-us" {
			t.Fatalf("connection ID = %q", id)
		}
		return routingcatalog.ConnectionDescriptor{
			ID: "pc-openai-us", Provider: "openai", Region: "us-west", CredentialScope: "organization-a",
			Enabled: true, CapabilityProfile: provider.CapabilityProfile{Revision: 4, Features: map[string]provider.CapabilitySupport{
				"text": provider.CapabilityNative, "streaming": provider.CapabilityNative,
			}},
		}, nil
	})

	report := routingcatalog.Validate(context.Background(), document, lookup)
	if !report.Valid || len(report.Errors) != 0 || report.Hash == "" {
		t.Fatalf("validation report = %#v", report)
	}
}

func TestValidateRejectsCapabilityAbsentFromProviderConnection(t *testing.T) {
	document := validDocument()
	document.Routes[0].Capabilities["rerank"] = provider.CapabilityTranslated
	lookup := routingcatalog.ConnectionLookupFunc(func(context.Context, string) (routingcatalog.ConnectionDescriptor, error) {
		return routingcatalog.ConnectionDescriptor{
			ID: "pc-openai-us", Provider: "openai", Region: "us-west", Enabled: true,
			CapabilityProfile: provider.CapabilityProfile{Revision: 4, Features: map[string]provider.CapabilitySupport{
				"text": provider.CapabilityNative, "streaming": provider.CapabilityNative,
			}},
		}, nil
	})

	report := routingcatalog.Validate(context.Background(), document, lookup)
	if report.Valid || !hasIssue(report.Errors, "capability_not_supported") {
		t.Fatalf("validation report = %#v", report)
	}
}

func TestValidateRequiresExplicitTenantVisibility(t *testing.T) {
	document := validDocument()
	document.Routes[0].TenantVisibility = routingcatalog.TenantVisibilityPolicy{}

	report := routingcatalog.Validate(context.Background(), document, validConnectionLookup())
	if report.Valid || !hasIssue(report.Errors, "tenant_visibility_required") {
		t.Fatalf("validation report = %#v", report)
	}

	document.Routes[0].TenantVisibility.AllTenants = true
	report = routingcatalog.Validate(context.Background(), document, validConnectionLookup())
	if !report.Valid || !hasIssue(report.Warnings, "global_tenant_visibility") {
		t.Fatalf("explicit global visibility report = %#v", report)
	}
}

func TestValidateRejectsCapabilityProfileRevisionMismatch(t *testing.T) {
	document := validDocument()
	document.Routes[0].CapabilityProfileRevision = 3
	report := routingcatalog.Validate(context.Background(), document, validConnectionLookup())
	if report.Valid || !hasIssue(report.Errors, "capability_profile_revision_mismatch") {
		t.Fatalf("validation report = %#v", report)
	}
}

func TestValidateRejectsCoreCatalogRuleViolations(t *testing.T) {
	tests := []struct {
		name   string
		code   string
		mutate func(*routingcatalog.Document)
		lookup routingcatalog.ConnectionLookup
	}{
		{name: "duplicate route ID", code: "duplicate_route_id", mutate: func(document *routingcatalog.Document) {
			duplicate := document.Routes[0]
			document.Routes = append(document.Routes, duplicate)
		}},
		{name: "region mismatch", code: "region_incompatible", mutate: func(document *routingcatalog.Document) {
			document.Routes[0].HomeRegion = "eu-west"
		}},
		{name: "administrative status", code: "invalid_administrative_status", mutate: func(document *routingcatalog.Document) {
			document.Routes[0].AdministrativeStatus = "paused"
		}},
		{name: "selection policy", code: "invalid_selection_policy", mutate: func(document *routingcatalog.Document) {
			document.Routes[0].SelectionPolicy.Weight = 0
		}},
		{name: "provider cost", code: "invalid_provider_cost", mutate: func(document *routingcatalog.Document) {
			document.Routes[0].ProviderCostSnapshot.OutputPerMillionMicros = -1
		}},
		{name: "unsupported cache adapter", code: "cache_protection_unsupported", mutate: func(document *routingcatalog.Document) {
			document.Routes[0].CacheProtectionPolicy = routingcatalog.CacheProtectionRoutePolicy{Enabled: true, TTLSeconds: 300}
			document.Routes[0].CacheUsageReliable = true
			document.Routes[0].ProviderCostSnapshot.CacheWritePerMillionMicros = 1
		}},
		{name: "cache TTL", code: "cache_protection_ttl_required", mutate: func(document *routingcatalog.Document) {
			document.Routes[0].CacheProtectionPolicy.Enabled = true
			document.Routes[0].CacheUsageReliable = true
			document.Routes[0].ProviderCostSnapshot.Provider = "anthropic"
			document.Routes[0].ProviderCostSnapshot.CacheWritePerMillionMicros = 1
		}, lookup: routingcatalog.ConnectionLookupFunc(func(context.Context, string) (routingcatalog.ConnectionDescriptor, error) {
			connection, _ := validConnectionLookup().LookupConnection(context.Background(), "pc-openai-us")
			connection.Provider = "anthropic"
			return connection, nil
		})},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			document := validDocument()
			test.mutate(&document)
			lookup := test.lookup
			if lookup == nil {
				lookup = validConnectionLookup()
			}
			report := routingcatalog.Validate(context.Background(), document, lookup)
			if report.Valid || !hasIssue(report.Errors, test.code) {
				t.Fatalf("validation report = %#v", report)
			}
		})
	}
}

func TestValidateRequiresLimitPolicyRevisionForEachVisibleTenant(t *testing.T) {
	document := validDocument()
	document.Routes[0].TenantVisibility.LimitPolicyRevisions = nil
	report := routingcatalog.Validate(context.Background(), document, validConnectionLookup())
	if report.Valid || !hasIssue(report.Errors, "limit_policy_reference_required") {
		t.Fatalf("validation report = %#v", report)
	}
}

func TestValidateRejectsInconsistentCacheProtectionAnchor(t *testing.T) {
	document := validDocument()
	document.Routes[0].CacheProtectionPolicy = routingcatalog.CacheProtectionRoutePolicy{Enabled: true, TTLSeconds: 300}
	lookup := routingcatalog.ConnectionLookupFunc(func(context.Context, string) (routingcatalog.ConnectionDescriptor, error) {
		connection, _ := validConnectionLookup().LookupConnection(context.Background(), "pc-openai-us")
		connection.Provider = "anthropic"
		return connection, nil
	})
	document.Routes[0].ProviderCostSnapshot.Provider = "anthropic"
	report := routingcatalog.Validate(context.Background(), document, lookup)
	if report.Valid || !hasIssue(report.Errors, "cache_anchor_inconsistent") {
		t.Fatalf("validation report = %#v", report)
	}
}

func TestValidateRequiresEligibleRouteForEveryAdvertisedPublicModel(t *testing.T) {
	document := validDocument()
	draining := document.Routes[0]
	draining.ID = "route-draining-only"
	draining.PublicModel = "draining-only-model"
	draining.AdministrativeStatus = routingcatalog.RouteDraining
	document.Routes = append(document.Routes, draining)

	report := routingcatalog.Validate(context.Background(), document, validConnectionLookup())
	if report.Valid || !hasIssue(report.Errors, "public_model_unavailable") {
		t.Fatalf("validation report = %#v", report)
	}
}

func TestValidateRejectsRouteIDThatCollidesWithAnotherPublicModel(t *testing.T) {
	document := validDocument()
	second := document.Routes[0]
	second.ID = document.Routes[0].PublicModel
	second.PublicModel = "another-model"
	document.Routes = append(document.Routes, second)
	report := routingcatalog.Validate(context.Background(), document, validConnectionLookup())
	if report.Valid || !hasIssue(report.Errors, "ambiguous_model_mapping") {
		t.Fatalf("validation report = %#v", report)
	}
}

func TestValidateRejectsConflictingImmutablePriceSnapshotIdentity(t *testing.T) {
	document := validDocument()
	second := document.Routes[0]
	second.ID = "route-second"
	second.ProviderCostSnapshot.OutputPerMillionMicros++
	document.Routes = append(document.Routes, second)
	report := routingcatalog.Validate(context.Background(), document, validConnectionLookup())
	if report.Valid || !hasIssue(report.Errors, "price_snapshot_identity_conflict") {
		t.Fatalf("validation report = %#v", report)
	}
}

func TestValidateRejectsUnsupportedProviderIdentityAndMissingCredentialScope(t *testing.T) {
	document := validDocument()
	lookup := routingcatalog.ConnectionLookupFunc(func(context.Context, string) (routingcatalog.ConnectionDescriptor, error) {
		connection, _ := validConnectionLookup().LookupConnection(context.Background(), "pc-openai-us")
		connection.Provider = "unregistered-provider"
		connection.CredentialScope = ""
		return connection, nil
	})

	report := routingcatalog.Validate(context.Background(), document, lookup)
	if report.Valid || !hasIssue(report.Errors, "provider_identity_unsupported") || !hasIssue(report.Errors, "credential_scope_required") {
		t.Fatalf("validation report = %#v", report)
	}
}

func TestCompileBuildsRuntimeRouteFromProviderConnectionWithoutEnvironmentCredential(t *testing.T) {
	document := validDocument()
	resolver := routingcatalog.RuntimeConnectionResolverFunc(func(_ context.Context, id string) (providerconnection.ResolvedConnection, error) {
		if id != "pc-openai-us" {
			t.Fatalf("connection ID = %q", id)
		}
		return providerconnection.ResolvedConnection{Connection: providerconnection.ProviderConnection{
			ID: id, Provider: "openai", BaseURL: "https://api.openai.com/v1", Region: "us-west", CredentialScope: "organization-a",
			AdministrativeStatus: providerconnection.StatusEnabled, CredentialVersion: 7,
			CapabilityDeclaration: provider.CapabilityProfile{Revision: 4, Features: map[string]provider.CapabilitySupport{
				"text": provider.CapabilityNative, "streaming": provider.CapabilityNative,
			}},
		}, Secret: []byte("managed-provider-secret")}, nil
	})

	routes, err := routingcatalog.Compile(context.Background(), document, resolver, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0].ID != "route-openai-us" || routes[0].Provider != "openai" ||
		routes[0].CredentialScope != "organization-a" || routes[0].Priority != 10 || routes[0].AdministrativeStatus != provider.RouteActive || routes[0].Executor == nil {
		t.Fatalf("compiled routes = %#v", routes)
	}
}

func TestManagedRuntimeSnapshotKeepsCatalogRevisionWhenConnectionIsAdministrativelyUnavailable(t *testing.T) {
	resolver := routingcatalog.RuntimeConnectionResolverFunc(func(context.Context, string) (providerconnection.ResolvedConnection, error) {
		return providerconnection.ResolvedConnection{}, providerconnection.ErrNotFound
	})
	compiler, err := routingcatalog.NewManagedCompiler(resolver, nil)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := compiler.CompileSnapshot(context.Background(), validDocument())
	if err != nil || len(compiled.Routes) != 0 {
		t.Fatalf("available runtime snapshot = %#v / %v", compiled, err)
	}
	if _, err := routingcatalog.Compile(context.Background(), validDocument(), resolver, nil); !errors.Is(err, providerconnection.ErrNotFound) {
		t.Fatalf("strict publication compilation error = %v", err)
	}
}

func hasIssue(issues []routingcatalog.ValidationIssue, code string) bool {
	for _, issue := range issues {
		if issue.Code == code {
			return true
		}
	}
	return false
}

func validConnectionLookup() routingcatalog.ConnectionLookup {
	return routingcatalog.ConnectionLookupFunc(func(context.Context, string) (routingcatalog.ConnectionDescriptor, error) {
		return routingcatalog.ConnectionDescriptor{
			ID: "pc-openai-us", Provider: "openai", Region: "us-west", CredentialScope: "organization-a", Enabled: true,
			CapabilityProfile: provider.CapabilityProfile{Revision: 4, Features: map[string]provider.CapabilitySupport{
				"text": provider.CapabilityNative, "streaming": provider.CapabilityNative,
			}},
		}, nil
	})
}

func validDocument() routingcatalog.Document {
	return routingcatalog.Document{Routes: []routingcatalog.ManagedRoute{{
		ID: "route-openai-us", PublicModel: "gateway-model", ProviderConnectionID: "pc-openai-us",
		ProviderModel: "gpt-model", ExecutionRegion: "us-west", HomeRegion: "us-west",
		CapabilityProfileRevision: 4,
		Capabilities: map[string]provider.CapabilitySupport{
			"text": provider.CapabilityNative, "streaming": provider.CapabilityNative,
		},
		ProviderCostSnapshot: core.PriceSnapshot{
			ID: "price-openai-us-1", Provider: "openai", Model: "gpt-model", Region: "us-west",
			Currency: "USD", EffectiveAt: time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC).Unix(), Source: "provider-contract",
		},
		AdministrativeStatus: routingcatalog.RouteActive,
		SelectionPolicy:      routingcatalog.SelectionPolicy{Priority: 10, Weight: 100},
		TenantVisibility: routingcatalog.TenantVisibilityPolicy{
			TenantIDs: []string{"tenant-a"}, LimitPolicyRevisions: map[string]int64{"tenant-a": 1},
		},
	}}}
}
