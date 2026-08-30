package provider_test

import (
	"context"
	"fmt"
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

func TestRouterHonorsAdministrativeStatusPriorityAndPinnedDrainingRoute(t *testing.T) {
	highPriority := route("priority-10")
	highPriority.Priority = 10
	lowPriority := route("priority-20")
	lowPriority.Priority = 20
	draining := route("draining")
	draining.Priority = 1
	draining.AdministrativeStatus = provider.RouteDraining
	router := provider.NewVersionedRouter(1, []provider.Route{lowPriority, draining, highPriority})

	routes, err := router.Candidates(context.Background(), core.Request{Model: "model", HomeRegion: "local", RequestedFeatures: []string{"text"}})
	if err != nil || len(routes) != 2 || routes[0].ID != "priority-10" || routes[1].ID != "priority-20" {
		t.Fatalf("new assignment routes = %#v / %v", routes, err)
	}
	routes, err = router.Candidates(context.Background(), core.Request{
		Model: "model", HomeRegion: "local", RequestedFeatures: []string{"text"}, PreferredRouteID: "draining",
	})
	if err != nil || len(routes) != 3 || routes[0].ID != "draining" {
		t.Fatalf("pinned routes = %#v / %v", routes, err)
	}
}

func TestRouterUsesStableWeightedOrderWithinPriority(t *testing.T) {
	light := route("light")
	light.Weight = 1
	heavy := route("heavy")
	heavy.Weight = 9
	router := provider.NewVersionedRouter(1, []provider.Route{light, heavy})
	heavyFirst := 0
	for index := range 1_000 {
		request := core.Request{TenantID: "tenant-a", Model: "model", HomeRegion: "local", RequestedFeatures: []string{"text"}, IdempotencyKey: fmt.Sprintf("request-%d", index)}
		first, err := router.Candidates(context.Background(), request)
		if err != nil || len(first) != 2 {
			t.Fatalf("weighted candidates = %#v / %v", first, err)
		}
		second, err := router.Candidates(context.Background(), request)
		if err != nil || first[0].ID != second[0].ID {
			t.Fatalf("weighted order is unstable: %#v / %#v / %v", first, second, err)
		}
		if first[0].ID == "heavy" {
			heavyFirst++
		}
	}
	if heavyFirst < 800 {
		t.Fatalf("heavy route selected first %d times, want weighted majority", heavyFirst)
	}
}

func TestStickyEligibleRoutesUseExperimentIdentityForWeightedOrder(t *testing.T) {
	first := route("first")
	first.Weight = 1
	first.StickyRouting = true
	second := route("second")
	second.Weight = 1
	second.StickyRouting = true
	router := provider.NewVersionedRouter(1, []provider.Route{first, second})
	var selected string
	for index := range 100 {
		candidates, err := router.Candidates(context.Background(), core.Request{
			TenantID: "tenant-a", Model: "model", HomeRegion: "local", RequestedFeatures: []string{"text"},
			IdempotencyKey: fmt.Sprintf("request-%d", index), ExperimentIdentity: "conversation-a",
		})
		if err != nil {
			t.Fatal(err)
		}
		if selected == "" {
			selected = candidates[0].ID
		} else if candidates[0].ID != selected {
			t.Fatalf("sticky order changed from %q to %q", selected, candidates[0].ID)
		}
	}
}

func route(id string) provider.Route {
	return provider.Route{
		ID: id, Provider: "provider", Model: "model", Region: "local", HomeRegion: "local", Healthy: true,
		Profile: provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{"text": provider.CapabilityNative}},
	}
}
