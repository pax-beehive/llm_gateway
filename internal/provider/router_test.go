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

func route(id string) provider.Route {
	return provider.Route{
		ID: id, Provider: "provider", Model: "model", Region: "local", HomeRegion: "local", Healthy: true,
		Profile: provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{"text": provider.CapabilityNative}},
	}
}
