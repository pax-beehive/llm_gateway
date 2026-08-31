package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/toddzheng/llm-gateway/internal/access"
	"github.com/toddzheng/llm-gateway/internal/accessprojection"
	"github.com/toddzheng/llm-gateway/internal/cacheprotection"
	"github.com/toddzheng/llm-gateway/internal/configuration"
	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/operations"
	"github.com/toddzheng/llm-gateway/internal/provider"
	"github.com/toddzheng/llm-gateway/internal/provider/anthropic"
	"github.com/toddzheng/llm-gateway/internal/quota"
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

func TestGatewayLocalRegionRequiresExplicitProductionConfiguration(t *testing.T) {
	t.Setenv("GATEWAY_ENV", "production")
	t.Setenv("GATEWAY_LOCAL_REGION", "")
	if _, err := gatewayLocalRegion(); err == nil {
		t.Fatal("production accepted an implicit Home Region")
	}
	t.Setenv("GATEWAY_LOCAL_REGION", "us-west")
	if got, err := gatewayLocalRegion(); err != nil || got != "us-west" {
		t.Fatalf("region/error = %q/%v", got, err)
	}
}

func TestGatewayLocalRegionDefaultsOnlyOutsideProduction(t *testing.T) {
	t.Setenv("GATEWAY_ENV", "development")
	t.Setenv("GATEWAY_LOCAL_REGION", "")
	if got, err := gatewayLocalRegion(); err != nil || got != "local" {
		t.Fatalf("region/error = %q/%v", got, err)
	}
}

func TestProductionControlRelayRequiresHTTPSAndGatewayIdentityKey(t *testing.T) {
	t.Setenv("GATEWAY_ENV", "production")
	t.Setenv("GATEWAY_ID", "gateway-west")
	t.Setenv("GATEWAY_CONTROL_RELAY_HMAC_KEY", "gateway-control-relay-hmac-key-00000001")
	t.Setenv("GATEWAY_CONTROL_RELAY_URL", "http://control.example.test")
	if _, err := configureGatewayControlRelayClient(); err == nil {
		t.Fatal("production accepted an unencrypted Control Event relay")
	}
	t.Setenv("GATEWAY_CONTROL_RELAY_URL", "https://control.example.test")
	t.Setenv("GATEWAY_CLOUD_RUN_AUDIENCE", "https://control.example.test")
	if _, err := configureGatewayControlRelayClient(); err != nil {
		t.Fatalf("production HTTPS Control Event relay: %v", err)
	}
	t.Setenv("GATEWAY_CONTROL_RELAY_HMAC_KEY", "")
	t.Setenv("GATEWAY_OPERATIONS_HMAC_KEY", "")
	if _, err := configureGatewayControlRelayClient(); err == nil {
		t.Fatal("production accepted a Control Event relay without a Gateway HMAC key")
	}
}

func TestGatewayReadinessDependencyStates(t *testing.T) {
	tests := map[string]struct {
		ready   func() error
		blocked func() error
	}{
		"routing catalog": {
			ready: func() error { return gatewayRoutingReady(1) }, blocked: func() error { return gatewayRoutingReady(0) },
		},
		"schema": {
			ready: func() error { return gatewaySchemaReady(operations.CurrentDatabaseSchema) }, blocked: func() error { return gatewaySchemaReady(operations.MinimumDatabaseSchema - 1) },
		},
		"durable outbox": {
			ready: func() error { return gatewayOutboxReady(10, 10) }, blocked: func() error { return gatewayOutboxReady(11, 10) },
		},
		"access projection empty": {
			ready: func() error { return gatewayAccessProjectionReady(accessprojection.Status{HeadCount: 1}) }, blocked: func() error { return gatewayAccessProjectionReady(accessprojection.Status{}) },
		},
		"access projection gap": {
			ready: func() error { return gatewayAccessProjectionReady(accessprojection.Status{HeadCount: 1}) }, blocked: func() error { return gatewayAccessProjectionReady(accessprojection.Status{HeadCount: 1, GapCount: 1}) },
		},
		"execution epoch": {
			ready: func() error { return gatewayExecutionEpochReady(false) }, blocked: func() error { return gatewayExecutionEpochReady(true) },
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if err := test.ready(); err != nil {
				t.Fatalf("ready state failed: %v", err)
			}
			if err := test.blocked(); err == nil {
				t.Fatal("unavailable state passed readiness")
			}
		})
	}
}

func TestRoutesFromJSONUsesOneAnthropicAdapterForExecutionAndCacheLifecycle(t *testing.T) {
	t.Setenv("ANTHROPIC_TEST_KEY", "test-key")
	payload := []byte(`[{
		"id":"anthropic-us","provider":"anthropic","public_model":"claude",
		"provider_model":"claude-test","base_url":"https://api.anthropic.com/v1",
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
		"provider_model":"provider-model","base_url":"https://api.openai.com/v1",
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
		"provider_model":"provider-model","base_url":"https://api.openai.com/v1",
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
		"provider_model":"provider-model","base_url":"https://api.openai.com/v1",
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
		TenantID: "tenant-a", APIKeyID: "key-a", RouteID: "anthropic-route", Provider: "anthropic",
		Region: "local", CredentialScope: "tenant-primary",
	}
	router := provider.NewRouter(provider.Route{
		ID: anchor.RouteID, Provider: anchor.Provider, Region: anchor.Region,
		CredentialScope: anchor.CredentialScope, CacheProtector: protector,
	})
	if got := resolveCacheProtectorForTenant(context.Background(), router, nil, nil, anchor); got != nil {
		t.Fatal("Cache Protector resolved without explicit Tenant permission")
	}
	allowCache := true
	policies := map[string]core.TenantPolicy{"tenant-a": {AllowCacheProtection: &allowCache}}
	if got := resolveCacheProtectorForTenant(context.Background(), router, nil, policies, anchor); got == nil {
		t.Fatal("Cache Protector was not resolved for an explicitly permitted Tenant")
	}
	allowCache = false
	if got := resolveCacheProtectorForTenant(context.Background(), router, nil, policies, anchor); got != nil {
		t.Fatal("Cache Protector resolved after Tenant permission was revoked")
	}
	allowCache = true
	source := cachePrincipalSourceStub{principal: access.Principal{
		TenantID: "tenant-a", APIKeyID: "key-a",
		TenantPolicy: core.TenantPolicy{AllowCacheProtection: &allowCache},
		APIKeyPolicy: core.APIKeyPolicy{AllowCacheProtection: true},
	}}
	if got := resolveCacheProtectorForTenant(context.Background(), router, source, nil, anchor); got == nil {
		t.Fatal("Cache Protector was not resolved for a currently permitted persisted principal")
	}
	source.principal.APIKeyPolicy.AllowCacheProtection = false
	if got := resolveCacheProtectorForTenant(context.Background(), router, source, nil, anchor); got != nil {
		t.Fatal("Cache Protector resolved after API key permission was revoked")
	}
	source.err = errors.New("database unavailable")
	if got := resolveCacheProtectorForTenant(context.Background(), router, source, nil, anchor); got != nil {
		t.Fatal("Cache Protector resolved when current principal could not be revalidated")
	}
}

func TestRefreshBudgetGateReservesAgainstCurrentSponsorPolicyAndCommitsActualUsage(t *testing.T) {
	allowCache := true
	limit := int64(2_000_000)
	principals := cachePrincipalSourceStub{principal: access.Principal{
		TenantID: "tenant-a", APIKeyID: "key-a",
		TenantPolicy: core.TenantPolicy{Revision: 4, AllowCacheProtection: &allowCache,
			Limits: core.QuotaLimits{RefreshMonthlySpendMicros: &limit, Currency: "USD"}},
		APIKeyPolicy: core.APIKeyPolicy{Revision: 7, AllowCacheProtection: true,
			Limits: core.QuotaLimits{RefreshMonthlySpendMicros: &limit, Currency: "USD"}},
	}}
	controller := &quotaControllerStub{}
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	gate := &refreshBudgetGate{principals: principals, quota: controller, now: func() time.Time { return now }}
	intent := cacheprotection.Intent{
		ID: "refresh-1", TenantID: "tenant-a", Anchor: provider.CacheAnchor{TenantID: "tenant-a", APIKeyID: "key-a"},
		Candidate: cacheprotection.Candidate{
			Economics:            cacheprotection.Economics{RefreshCostMicros: 1_500_000},
			RefreshPriceSnapshot: core.PriceSnapshot{Currency: "USD", CacheWritePerMillionMicros: 10_000_000},
		},
		ProviderResult: provider.RefreshResult{Status: "succeeded", Usage: core.Usage{InputTokens: 100_000, CacheWriteInputTokens: 100_000}},
	}
	reservation, err := gate.Reserve(context.Background(), intent)
	if err != nil || reservation.ID != "quota-refresh-1" {
		t.Fatalf("refresh reservation = %#v / %v", reservation, err)
	}
	if controller.refreshRequest.CacheRefreshIntentID != intent.ID || controller.refreshRequest.APIKeyID != "key-a" ||
		controller.refreshRequest.TenantPolicyRevision != 4 || controller.refreshRequest.APIKeyPolicyRevision != 7 {
		t.Fatalf("refresh reservation request = %#v", controller.refreshRequest)
	}
	if err := gate.Complete(context.Background(), reservation, intent, nil); err != nil {
		t.Fatal(err)
	}
	if controller.committedID != reservation.ID || controller.actual.SpendMicros != 1_000_000 {
		t.Fatalf("refresh settlement = %q / %#v", controller.committedID, controller.actual)
	}
}

type cachePrincipalSourceStub struct {
	principal access.Principal
	err       error
}

type quotaControllerStub struct {
	refreshRequest quota.RefreshReservationRequest
	committedID    string
	actual         quota.ActualUsage
}

func (s *quotaControllerStub) Reserve(context.Context, quota.ReservationRequest) (quota.Reservation, error) {
	return quota.Reservation{}, nil
}

func (s *quotaControllerStub) ReserveRefresh(_ context.Context, request quota.RefreshReservationRequest) (quota.Reservation, error) {
	s.refreshRequest = request
	return quota.Reservation{ID: "quota-refresh-1"}, nil
}

func (s *quotaControllerStub) Commit(_ context.Context, id string, actual quota.ActualUsage) error {
	s.committedID = id
	s.actual = actual
	return nil
}

func (s *quotaControllerStub) Release(context.Context, string) error { return nil }

func (s *quotaControllerStub) Uncertain(context.Context, string) error { return nil }

func (s *quotaControllerStub) Reconcile(context.Context, int) (int, error) { return 0, nil }

func (s cachePrincipalSourceStub) LookupPrincipal(context.Context, string, string) (access.Principal, error) {
	return s.principal, s.err
}

func TestRoutesFromJSONAcceptsFirstReleaseProviders(t *testing.T) {
	t.Setenv("PROVIDER_TEST_KEY", "test-key")
	baseURLs := map[string]string{
		"openai": "https://api.openai.com/v1", "deepseek": "https://api.deepseek.com/v1",
		"anthropic": "https://api.anthropic.com/v1", "gemini": "https://generativelanguage.googleapis.com/v1beta/openai",
	}
	for _, providerName := range []string{"openai", "deepseek", "anthropic", "gemini"} {
		t.Run(providerName, func(t *testing.T) {
			payload := []byte(fmt.Sprintf(`[{
				"id":"%[1]s-route","provider":"%[1]s","public_model":"gateway-model",
				"provider_model":"provider-model","base_url":%[2]q,
				"api_key_env":"PROVIDER_TEST_KEY","region":"local","home_region":"local",
				"credential_scope":"tenant-primary","healthy":true,
				"capabilities":{"text":"native"},"price_snapshot_id":"price-1",
				"price_effective_at":"2026-01-01T00:00:00Z","price_source":"contract","currency":"USD"
			}]`, providerName, baseURLs[providerName]))
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

func TestRoutesFromJSONWiresOnlyDeclaredStageACapabilities(t *testing.T) {
	t.Setenv("STAGE_A_PROVIDER_KEY", "test-key")
	payload := []byte(`[{
		"id":"stage-a-route","provider":"openai","public_model":"gateway-stage-a",
		"provider_model":"provider-stage-a","base_url":"https://api.openai.com/v1",
		"api_key_env":"STAGE_A_PROVIDER_KEY","region":"local","home_region":"local",
		"tenant_ids":["tenant-a"],
		"credential_scope":"tenant-primary","healthy":true,
		"capabilities":{"embeddings":"native","moderation":"native","rerank":"translated"},
		"embedding_path":"/embeddings-v2","moderation_path":"/moderations-v2","rerank_path":"/rank",
		"embedding_dimensions":768,
		"embedding_input_cost_per_million":0.02,
		"moderation_input_cost_per_million":0.01,
		"rerank_document_cost_per_thousand":0.5,
		"price_snapshot_id":"stage-a-price","price_effective_at":"2026-01-01T00:00:00Z",
		"price_source":"contract","currency":"USD"
	}]`)
	routes, err := routesFromJSON(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0].EmbeddingExecutor == nil || routes[0].ModerationExecutor == nil || routes[0].RerankExecutor == nil {
		t.Fatalf("route executors = %#v", routes)
	}
	if len(routes[0].TenantIDs) != 1 || routes[0].TenantIDs[0] != "tenant-a" {
		t.Fatalf("route Tenant visibility = %#v", routes[0].TenantIDs)
	}
	price := routes[0].PriceSnapshot
	if price.EmbeddingInputPerMillionMicros != 20_000 || price.ModerationInputPerMillionMicros != 10_000 || price.RerankDocumentPerThousandMicros != 500_000 {
		t.Fatalf("capability prices = %#v", price)
	}

	payload = bytes.Replace(payload, []byte(`"moderation":"native","rerank":"translated"`), []byte(`"moderation":"unsupported","rerank":"unsupported"`), 1)
	routes, err = routesFromJSON(payload)
	if err != nil {
		t.Fatal(err)
	}
	if routes[0].EmbeddingExecutor == nil || routes[0].ModerationExecutor != nil || routes[0].RerankExecutor != nil {
		t.Fatalf("undeclared capability executors = %#v", routes[0])
	}
}

func TestBuildProviderComponentsWiresProductionDialect(t *testing.T) {
	t.Setenv("PROVIDER_DIALECT_TEST_KEY", "fake-key")
	tests := []struct {
		provider       string
		baseURL        string
		maxTokensField string
		googleHeader   string
	}{
		{provider: "openai", baseURL: "https://api.openai.com/v1", maxTokensField: "max_output_tokens"},
		{provider: "deepseek", baseURL: "https://api.deepseek.com/v1", maxTokensField: "max_tokens"},
		{provider: "gemini", baseURL: "https://generativelanguage.googleapis.com/v1beta/openai", maxTokensField: "max_completion_tokens", googleHeader: "llm-gateway/0.1.0"},
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
				Provider: test.provider, ProviderModel: "test-model", BaseURL: test.baseURL,
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
		"provider_model":"claude-test","base_url":"https://api.anthropic.com/v1",
		"api_key_env":"ANTHROPIC_TEST_KEY","region":"us-west","home_region":"us-west",
		"credential_scope":"tenant-primary","price_snapshot_id":"price-1",
		"price_effective_at":"2026-01-01T00:00:00Z","price_source":"contract","currency":"USD",
		"cache_refresh":{"kind":"anthropic","base_url":"https://other.test/v1","ttl_seconds":300}
	}]`)
	if _, err := routesFromJSON(payload); err == nil {
		t.Fatal("expected split Anthropic cache transport to be rejected")
	}
}

func TestGatewayAPIKeyPepperRingSupportsBoundedVersionMigration(t *testing.T) {
	t.Setenv("GATEWAY_API_KEY_PEPPERS_JSON", `{"1":"old-pepper-at-least-16","2":"current-pepper-at-least-16"}`)
	t.Setenv("GATEWAY_API_KEY_CURRENT_DIGEST_VERSION", "2")
	current, peppers, err := gatewayAPIKeyPepperRingFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if current != 2 || string(peppers[1]) != "old-pepper-at-least-16" || string(peppers[2]) != "current-pepper-at-least-16" {
		t.Fatalf("pepper ring = current %d versions %#v", current, peppers)
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
