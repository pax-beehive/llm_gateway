package provider_test

import (
	"context"
	"sync"
	"testing"

	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/provider"
)

func TestVersionedRouterAtomicallyReplacesSnapshotsAndRejectsStaleUpdates(t *testing.T) {
	t.Parallel()

	router := provider.NewVersionedRouter(4, []provider.Route{route("old")})
	var readers sync.WaitGroup
	for range 16 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for range 100 {
				routes, err := router.Candidates(context.Background(), core.Request{Model: "model", HomeRegion: "local", RequestedFeatures: []string{"text"}})
				if err != nil || len(routes) != 1 || (routes[0].ID != "old" && routes[0].ID != "new") {
					t.Errorf("reader observed partial snapshot: %#v, %v", routes, err)
					return
				}
			}
		}()
	}
	if err := router.Update(5, []provider.Route{route("new")}); err != nil {
		t.Fatal(err)
	}
	readers.Wait()
	if router.Revision() != 5 {
		t.Fatalf("revision = %d, want 5", router.Revision())
	}
	routes, err := router.Candidates(context.Background(), core.Request{Model: "model", HomeRegion: "local", RequestedFeatures: []string{"text"}})
	if err != nil || len(routes) != 1 || routes[0].ID != "new" {
		t.Fatalf("new snapshot = %#v, %v", routes, err)
	}
	if err := router.Update(5, []provider.Route{route("stale")}); err == nil {
		t.Fatal("same-revision update succeeded, want rejection")
	}
}

func TestModelCatalogFiltersRequiredNativeFeatures(t *testing.T) {
	t.Parallel()
	native := route("native")
	native.Model = "native-model"
	native.Profile.Features["responses_native"] = provider.CapabilityNative
	translated := route("translated")
	translated.Model = "translated-model"
	translated.Profile.Features["responses_native"] = provider.CapabilityTranslated
	router := provider.NewVersionedRouter(1, []provider.Route{native, translated})

	models, err := router.ListModels(context.Background(), provider.ModelCatalogQuery{RequiredFeatures: []string{"responses_native"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "native-model" {
		t.Fatalf("models = %#v, want native-model", models)
	}
}

func TestTenantScopedRoutesAreNotDisclosedOrSelectedCrossTenant(t *testing.T) {
	t.Parallel()
	tenantRoute := route("tenant-only")
	tenantRoute.TenantIDs = []string{"tenant-a"}
	tenantRoute.Profile.Features["embeddings"] = provider.CapabilityNative
	router := provider.NewVersionedRouter(1, []provider.Route{tenantRoute})

	capabilities, err := router.ListCapabilities(context.Background(), provider.CapabilityCatalogQuery{TenantID: "tenant-b", HomeRegion: "local"})
	if err != nil || len(capabilities) != 0 {
		t.Fatalf("cross-Tenant capability catalog = %#v, %v", capabilities, err)
	}
	models, err := router.ListModels(context.Background(), provider.ModelCatalogQuery{TenantID: "tenant-b", HomeRegion: "local"})
	if err != nil || len(models) != 0 {
		t.Fatalf("cross-Tenant model catalog = %#v, %v", models, err)
	}
	if _, err := router.CapabilityCandidates(context.Background(), provider.CapabilityRouteQuery{
		TenantID: "tenant-b", Model: "model", HomeRegion: "local", Capability: core.CapabilityEmbeddings,
	}); err == nil {
		t.Fatal("cross-Tenant capability route was selected")
	}
	routes, err := router.CapabilityCandidates(context.Background(), provider.CapabilityRouteQuery{
		TenantID: "tenant-a", Model: "model", HomeRegion: "local", Capability: core.CapabilityEmbeddings,
	})
	if err != nil || len(routes) != 1 || routes[0].ID != "tenant-only" {
		t.Fatalf("authorized Tenant routes = %#v, %v", routes, err)
	}
}

func route(id string) provider.Route {
	return provider.Route{
		ID: id, Provider: "provider", Model: "model", Region: "local", HomeRegion: "local", Healthy: true,
		Profile: provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{"text": provider.CapabilityNative}},
	}
}
