package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/toddzheng/llm-gateway/internal/configuration"
	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/provider"
	"github.com/toddzheng/llm-gateway/internal/provider/anthropic"
)

func TestCacheProtectionModeDefaultsOff(t *testing.T) {
	t.Setenv("GATEWAY_CACHE_PROTECTION_MODE", "")
	mode, err := cacheProtectionModeFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if mode != "off" {
		t.Fatalf("mode = %q, want off", mode)
	}
}

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
		"id":"draining","provider":"openai","public_model":"gateway-model",
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

func TestRoutesFromJSONRejectsProviderOutsideFirstReleaseScope(t *testing.T) {
	payload := []byte(`[{
		"id":"other-provider","provider":"other","public_model":"gateway-model",
		"provider_model":"provider-model","base_url":"https://provider.test/v1",
		"region":"local","home_region":"local","healthy":true,
		"capabilities":{"text":"native"},"price_snapshot_id":"price-1",
		"price_effective_at":"2026-01-01T00:00:00Z","price_source":"contract","currency":"USD"
	}]`)
	if _, err := routesFromJSON(payload); err == nil {
		t.Fatal("expected Provider outside the first-release scope to be rejected")
	}
}

func TestPublishRoutesRejectsUnsupportedProviderBeforeDurablePublication(t *testing.T) {
	repository := &recordingConfigurationRepository{}
	payload := json.RawMessage(`[{
		"id":"other-provider","provider":"other","public_model":"gateway-model",
		"provider_model":"provider-model","base_url":"https://provider.test/v1",
		"region":"local","home_region":"local","healthy":true,
		"capabilities":{"text":"native"},"price_snapshot_id":"price-1",
		"price_effective_at":"2026-01-01T00:00:00Z","price_source":"contract","currency":"USD"
	}]`)
	if _, _, err := publishRoutes(context.Background(), repository, 0, 1, payload, "operator"); err == nil {
		t.Fatal("expected invalid Provider publication to fail")
	}
	if repository.publishCalls != 0 {
		t.Fatalf("publication calls = %d, want 0 before route validation", repository.publishCalls)
	}
}

func TestCacheWorkerResolverRequiresCurrentTenantPermission(t *testing.T) {
	protector := cacheProtectorStub{}
	anchor := provider.CacheAnchor{
		TenantID: "tenant-a", RouteID: "anthropic-route", Provider: "anthropic",
		Region: "local", CredentialScope: "tenant-primary",
	}
	router := provider.NewRouter(provider.Route{
		ID: anchor.RouteID, Provider: anchor.Provider, Region: anchor.Region,
		CredentialScope: anchor.CredentialScope, CacheProtector: protector,
	})
	if got := resolveCacheProtectorForTenant(router, nil, anchor); got != nil {
		t.Fatal("Cache Protector resolved without explicit Tenant permission")
	}
	allowCache := true
	policies := map[string]core.TenantPolicy{"tenant-a": {AllowCacheProtection: &allowCache}}
	if got := resolveCacheProtectorForTenant(router, policies, anchor); got == nil {
		t.Fatal("Cache Protector was not resolved for an explicitly permitted Tenant")
	}
	allowCache = false
	if got := resolveCacheProtectorForTenant(router, policies, anchor); got != nil {
		t.Fatal("Cache Protector resolved after Tenant permission was revoked")
	}
}

func TestRoutesFromJSONAcceptsFirstReleaseProviders(t *testing.T) {
	t.Setenv("PROVIDER_TEST_KEY", "test-key")
	for _, providerName := range []string{"openai", "deepseek", "anthropic", "gemini"} {
		t.Run(providerName, func(t *testing.T) {
			payload := []byte(fmt.Sprintf(`[{
				"id":"%[1]s-route","provider":"%[1]s","public_model":"gateway-model",
				"provider_model":"provider-model","base_url":"https://%[1]s.test/v1",
				"api_key_env":"PROVIDER_TEST_KEY","region":"local","home_region":"local",
				"credential_scope":"tenant-primary","healthy":true,
				"capabilities":{"text":"native"},"price_snapshot_id":"price-1",
				"price_effective_at":"2026-01-01T00:00:00Z","price_source":"contract","currency":"USD"
			}]`, providerName))
			routes, err := routesFromJSON(payload)
			if err != nil {
				t.Fatal(err)
			}
			if len(routes) != 1 || routes[0].Provider != providerName || routes[0].Executor == nil {
				t.Fatalf("routes = %#v, want configured %s route", routes, providerName)
			}
		})
	}
}

func TestBuildProviderComponentsWiresProductionDialect(t *testing.T) {
	t.Setenv("PROVIDER_DIALECT_TEST_KEY", "fake-key")
	tests := []struct {
		provider       string
		maxTokensField string
		googleHeader   string
	}{
		{provider: "openai", maxTokensField: "max_completion_tokens"},
		{provider: "deepseek", maxTokensField: "max_tokens"},
		{provider: "gemini", maxTokensField: "max_completion_tokens", googleHeader: "llm-gateway/0.1.0"},
	}
	for _, test := range tests {
		t.Run(test.provider, func(t *testing.T) {
			var body map[string]any
			client := &http.Client{Transport: mainRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				if got := request.Header.Get("Authorization"); got != "Bearer fake-key" {
					t.Errorf("Authorization = %q", got)
				}
				if got := request.Header.Get("x-goog-api-client"); got != test.googleHeader {
					t.Errorf("x-goog-api-client = %q", got)
				}
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				return &http.Response{
					StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
					Body: io.NopCloser(strings.NewReader("data: [DONE]\n\n")), Request: request,
				}, nil
			})}
			executor, _, _, err := buildProviderComponentsWithHTTPClient(routeConfig{
				Provider: test.provider, ProviderModel: "test-model", BaseURL: "https://provider.test/v1",
				APIKeyEnv: "PROVIDER_DIALECT_TEST_KEY",
			}, client)
			if err != nil {
				t.Fatal(err)
			}
			maxTokens := 8
			stream, err := executor.Execute(context.Background(), core.Request{
				Input:           []core.Item{{Type: "message", Role: "user", Content: []core.Content{{Type: "input_text", Text: "hello"}}}},
				MaxOutputTokens: &maxTokens,
			})
			if err != nil {
				t.Fatal(err)
			}
			_ = stream.Close()
			if body[test.maxTokensField] != float64(8) {
				t.Fatalf("%s payload = %#v", test.provider, body)
			}
			if test.provider == "deepseek" && !strings.Contains(fmt.Sprint(body["thinking"]), "disabled") {
				t.Fatalf("DeepSeek thinking = %#v", body["thinking"])
			}
		})
	}
}

type mainRoundTripFunc func(*http.Request) (*http.Response, error)

func (f mainRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
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

type recordingConfigurationRepository struct {
	publishCalls int
}

type cacheProtectorStub struct{}

func (cacheProtectorStub) Inspect(context.Context, provider.CacheAnchor) provider.CacheCapability {
	return provider.CacheCapability{Supported: true}
}

func (cacheProtectorStub) Refresh(context.Context, provider.CacheAnchor) (provider.RefreshResult, error) {
	return provider.RefreshResult{Status: "succeeded"}, nil
}

func (r *recordingConfigurationRepository) Current(context.Context, string) (configuration.Snapshot, error) {
	return configuration.Snapshot{}, configuration.ErrNotFound
}

func (r *recordingConfigurationRepository) Publish(_ context.Context, kind string, _ int64, revision int64, payload json.RawMessage, actor string) (configuration.Snapshot, error) {
	r.publishCalls++
	return configuration.Snapshot{Kind: kind, Revision: revision, Payload: payload, CreatedBy: actor}, nil
}
