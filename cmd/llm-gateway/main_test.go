package main

import (
	"context"
	"testing"

	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/provider"
	"github.com/toddzheng/llm-gateway/internal/provider/anthropic"
)

func TestRoutesFromJSONUsesOneAnthropicAdapterForExecutionAndCacheLifecycle(t *testing.T) {
	t.Setenv("ANTHROPIC_TEST_KEY", "test-key")
	payload := []byte(`[{
		"id":"anthropic-us","provider":"anthropic","public_model":"claude",
		"provider_model":"claude-test","base_url":"https://api.anthropic.test/v1",
		"api_key_env":"ANTHROPIC_TEST_KEY","region":"us-west","home_region":"us-west",
		"credential_scope":"tenant-primary","capabilities":{"text":"native","streaming":"native"},
		"price_snapshot_id":"price-1","price_effective_at":"2026-01-01T00:00:00Z",
		"price_source":"contract","currency":"USD","cache_usage_reliable":true,
		"cache_refresh":{"kind":"anthropic","ttl_seconds":300,"write_cost_per_million":12.5}
	}]`)
	routes, err := routesFromJSON(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 {
		t.Fatalf("routes = %d", len(routes))
	}
	executor, ok := routes[0].Executor.(*anthropic.Adapter)
	if !ok {
		t.Fatalf("executor = %T, want *anthropic.Adapter", routes[0].Executor)
	}
	if routes[0].CacheProtector != executor || routes[0].CacheAnchorBuilder != executor {
		t.Fatal("execution, cache anchor, and refresh must share the same Anthropic adapter")
	}
}

func TestRoutesFromJSONCanDrainAnUnhealthyRoute(t *testing.T) {
	payload := []byte(`[{
		"id":"draining","provider":"compatible","public_model":"gateway-model",
		"provider_model":"provider-model","base_url":"https://provider.test/v1",
		"region":"local","home_region":"local","healthy":false,
		"capabilities":{"text":"native"},"price_snapshot_id":"price-1",
		"price_effective_at":"2026-01-01T00:00:00Z","price_source":"contract","currency":"USD"
	}]`)
	routes, err := routesFromJSON(payload)
	if err != nil {
		t.Fatal(err)
	}
	if routes[0].Healthy {
		t.Fatal("route health = true, want explicitly drained")
	}
	router := provider.NewRouter(routes[0])
	if _, err := router.Candidates(context.Background(), core.Request{
		Model: "gateway-model", HomeRegion: "local", CompatibilityMode: core.CompatibilityStrict,
		RequestedFeatures: []string{"text"},
	}); err == nil {
		t.Fatal("unhealthy route was selected")
	}
}

func TestRoutesFromJSONRejectsSplitAnthropicCacheTransport(t *testing.T) {
	t.Setenv("ANTHROPIC_TEST_KEY", "test-key")
	payload := []byte(`[{
		"id":"anthropic-us","provider":"anthropic","public_model":"claude",
		"provider_model":"claude-test","base_url":"https://api.anthropic.test/v1",
		"api_key_env":"ANTHROPIC_TEST_KEY","region":"us-west","home_region":"us-west",
		"credential_scope":"tenant-primary","price_snapshot_id":"price-1",
		"price_effective_at":"2026-01-01T00:00:00Z","price_source":"contract","currency":"USD",
		"cache_refresh":{"kind":"anthropic","base_url":"https://other.test/v1","ttl_seconds":300}
	}]`)
	if _, err := routesFromJSON(payload); err == nil {
		t.Fatal("expected split Anthropic cache transport to be rejected")
	}
}
