package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/toddzheng/llm-gateway/internal/access"
	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/httpapi"
	"github.com/toddzheng/llm-gateway/internal/provider"
	"github.com/toddzheng/llm-gateway/internal/runtime"
	"github.com/toddzheng/llm-gateway/internal/store"
)

func TestAPIKeyPolicyCannotExpandTenantCacheProtectionAccess(t *testing.T) {
	allowCache := true
	principal := access.Principal{
		TenantID: "tenant-a", APIKeyID: "key-a", HomeRegion: "local", ExecutionEpoch: 1,
		TenantPolicy: core.TenantPolicy{Revision: 1, AllowCacheProtection: &allowCache},
		APIKeyPolicy: core.APIKeyPolicy{Revision: 1, AllowCacheProtection: false},
	}
	engine := runtime.NewWithOptions(
		store.NewMemoryResponseStore(),
		provider.NewStaticRouter(provider.NewEchoExecutor()),
		runtime.Options{CacheProtectionMode: runtime.CacheProtectionShadowMode},
	)
	handler := httpapi.New(httpapi.Config{
		Runtime: engine, Authenticator: principalAuthenticatorStub{Principal: principal}, LocalRegion: "local",
	})

	response := performJSON(t, handler, "persisted-key", http.MethodPost, "/v1/responses", map[string]any{
		"model": "echo-v1", "input": "hello",
		"cache_protection": map[string]any{
			"enabled": true, "max_spend_micros": 1000, "max_refreshes": 1,
			"max_protection_window_seconds": 60,
		},
	})
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "API key policy") {
		t.Fatalf("status/body = %d / %s, want API key policy rejection", response.Code, response.Body.String())
	}
}

func TestGatewayAPIKeyPolicyRestrictsOperationModelRegionAndTrustedClientCIDR(t *testing.T) {
	models := []string{"gateway-model"}
	operations := []string{"responses"}
	regions := []string{"local"}
	cidrs := []string{"203.0.113.0/24"}
	principal := access.Principal{
		TenantID: "tenant-a", APIKeyID: "key-a", HomeRegion: "local", ExecutionEpoch: 1,
		TenantPolicy: core.TenantPolicy{Revision: 1},
		APIKeyPolicy: core.APIKeyPolicy{
			Revision: 1, AllowedPublicModels: &models, AllowedOperations: &operations,
			AllowedRegions: &regions, AllowedCIDRs: &cidrs,
		},
	}
	route := provider.Route{
		ID: "policy-route", Provider: "test", Model: "gateway-model", Region: "local", HomeRegion: "local",
		CredentialScope: "test", Healthy: true, Executor: provider.NewEchoExecutor(),
		Profile: provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{
			"text": provider.CapabilityNative,
		}},
		PriceSnapshot: core.PriceSnapshot{ID: "price", Provider: "test", Model: "gateway-model", Region: "local", Currency: "USD", EffectiveAt: 1, Source: "test"},
	}
	handler := httpapi.New(httpapi.Config{
		Runtime:       runtime.New(store.NewMemoryResponseStore(), provider.NewRouter(route)),
		Authenticator: principalAuthenticatorStub{Principal: principal}, LocalRegion: "local",
		TrustedProxyCIDRs: []string{"10.0.0.0/8"},
	})
	allowed := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gateway-model","input":"hello"}`))
	allowed.Header.Set("Authorization", "Bearer key")
	allowed.Header.Set("Content-Type", "application/json")
	allowed.RemoteAddr = "203.0.113.7:54321"
	allowedResponse := httptest.NewRecorder()
	handler.ServeHTTP(allowedResponse, allowed)
	if allowedResponse.Code != http.StatusOK {
		t.Fatalf("allowed status/body = %d / %s", allowedResponse.Code, allowedResponse.Body.String())
	}
	deniedModel := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"other-model","input":"hello"}`))
	deniedModel.Header.Set("Authorization", "Bearer key")
	deniedModel.Header.Set("Content-Type", "application/json")
	deniedModel.RemoteAddr = "203.0.113.7:54321"
	deniedModelResponse := httptest.NewRecorder()
	handler.ServeHTTP(deniedModelResponse, deniedModel)
	if deniedModelResponse.Code != http.StatusForbidden {
		t.Fatalf("denied model status/body = %d / %s", deniedModelResponse.Code, deniedModelResponse.Body.String())
	}
	deniedOperation := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	deniedOperation.Header.Set("Authorization", "Bearer key")
	deniedOperation.RemoteAddr = "203.0.113.7:54321"
	deniedOperationResponse := httptest.NewRecorder()
	handler.ServeHTTP(deniedOperationResponse, deniedOperation)
	if deniedOperationResponse.Code != http.StatusForbidden {
		t.Fatalf("denied operation status/body = %d / %s", deniedOperationResponse.Code, deniedOperationResponse.Body.String())
	}
	spoofed := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gateway-model","input":"hello"}`))
	spoofed.Header.Set("Authorization", "Bearer key")
	spoofed.Header.Set("Content-Type", "application/json")
	spoofed.Header.Set("X-Forwarded-For", "203.0.113.7")
	spoofed.RemoteAddr = "198.51.100.8:54321"
	spoofedResponse := httptest.NewRecorder()
	handler.ServeHTTP(spoofedResponse, spoofed)
	if spoofedResponse.Code != http.StatusForbidden {
		t.Fatalf("spoofed CIDR status/body = %d / %s", spoofedResponse.Code, spoofedResponse.Body.String())
	}
}

func TestGatewayAPIKeyPolicyEnforcesConcurrentResponseLimit(t *testing.T) {
	limit := 1
	started := make(chan struct{})
	release := make(chan struct{})
	executor := blockingExecutor{started: started, release: release, delegate: provider.NewEchoExecutor()}
	route := provider.Route{
		ID: "concurrency-route", Provider: "test", Model: "gateway-model", Region: "local", HomeRegion: "local",
		CredentialScope: "test", Healthy: true, Executor: executor,
		Profile:       provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{"text": provider.CapabilityNative}},
		PriceSnapshot: core.PriceSnapshot{ID: "price", Provider: "test", Model: "gateway-model", Region: "local", Currency: "USD", EffectiveAt: 1, Source: "test"},
	}
	principal := access.Principal{
		TenantID: "tenant-a", APIKeyID: "key-a", HomeRegion: "local", ExecutionEpoch: 1,
		TenantPolicy: core.TenantPolicy{Revision: 1},
		APIKeyPolicy: core.APIKeyPolicy{Revision: 1, MaxConcurrentResponses: &limit},
	}
	handler := httpapi.New(httpapi.Config{
		Runtime:       runtime.New(store.NewMemoryResponseStore(), provider.NewRouter(route)),
		Authenticator: principalAuthenticatorStub{Principal: principal}, LocalRegion: "local",
	})
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		firstDone <- performJSON(t, handler, "key", http.MethodPost, "/v1/responses", map[string]any{"model": "gateway-model", "input": "first"})
	}()
	<-started
	second := performJSON(t, handler, "key", http.MethodPost, "/v1/responses", map[string]any{"model": "gateway-model", "input": "second"})
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second status/body = %d / %s", second.Code, second.Body.String())
	}
	close(release)
	if first := <-firstDone; first.Code != http.StatusOK {
		t.Fatalf("first status/body = %d / %s", first.Code, first.Body.String())
	}
}

type principalAuthenticatorStub struct {
	Principal access.Principal
	Err       error
}

func (s principalAuthenticatorStub) Authenticate(context.Context, string) (access.Principal, error) {
	return s.Principal, s.Err
}

func TestTenantCanCreateAndRetrieveResponse(t *testing.T) {
	t.Parallel()

	responseStore := store.NewMemoryResponseStore()
	executor := provider.NewEchoExecutor()
	engine := runtime.New(responseStore, provider.NewStaticRouter(executor))
	handler := httpapi.New(httpapi.Config{
		Runtime: engine,
		Authenticator: httpapi.StaticAuthenticator{
			"tenant-a-key": "tenant-a",
			"tenant-b-key": "tenant-b",
		},
	})

	created := performJSON(t, handler, "tenant-a-key", http.MethodPost, "/v1/responses", map[string]any{
		"model": "echo-v1",
		"input": "hello gateway",
	})
	if created.Code != http.StatusOK {
		t.Fatalf("create status = %d, body = %s", created.Code, created.Body.String())
	}

	var got core.Response
	decodeJSON(t, created, &got)
	if got.Status != core.ResponseStatusCompleted {
		t.Fatalf("status = %q, want %q", got.Status, core.ResponseStatusCompleted)
	}
	if got.Model != "echo-v1" {
		t.Fatalf("model = %q, want echo-v1", got.Model)
	}
	if text := got.OutputText(); text != "hello gateway" {
		t.Fatalf("output text = %q, want hello gateway", text)
	}

	retrieved := performJSON(t, handler, "tenant-a-key", http.MethodGet, "/v1/responses/"+got.ID, nil)
	if retrieved.Code != http.StatusOK {
		t.Fatalf("retrieve status = %d, body = %s", retrieved.Code, retrieved.Body.String())
	}
	var persisted core.Response
	decodeJSON(t, retrieved, &persisted)
	if persisted.ID != got.ID || persisted.OutputText() != "hello gateway" {
		t.Fatalf("persisted response = %#v, want id %q and output hello gateway", persisted, got.ID)
	}

	isolated := performJSON(t, handler, "tenant-b-key", http.MethodGet, "/v1/responses/"+got.ID, nil)
	if isolated.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant retrieve status = %d, want %d", isolated.Code, http.StatusNotFound)
	}
}

func TestTenantListsRoutablePublicModels(t *testing.T) {
	t.Parallel()
	textProfile := provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{"text": provider.CapabilityNative}}
	routes := []provider.Route{
		{ID: "openai-fast", Provider: "openai", Model: "fast-chat", HomeRegion: "us-west", Healthy: true, Profile: textProfile},
		{ID: "gemini-fast", Provider: "gemini", Model: "fast-chat", HomeRegion: "us-west", Healthy: true, Profile: textProfile},
		{ID: "anthropic-smart", Provider: "anthropic", Model: "smart-chat", HomeRegion: "us-west", Healthy: true, Profile: textProfile},
		{ID: "deepseek-draining", Provider: "deepseek", Model: "draining-chat", HomeRegion: "us-west", Healthy: false, Profile: textProfile},
		{ID: "gemini-eu", Provider: "gemini", Model: "eu-chat", HomeRegion: "eu-west", Healthy: true, Profile: textProfile},
		{ID: "opaque-route", Provider: "openai", Model: "opaque-chat", HomeRegion: "us-west", Healthy: true},
	}
	createdAt := time.Unix(1_724_566_400, 0)
	router := provider.NewVersionedRouterAt(1, createdAt, routes)
	handler := httpapi.New(httpapi.Config{
		Runtime:           runtime.New(store.NewMemoryResponseStore(), router),
		ModelCatalog:      router,
		Authenticator:     httpapi.StaticAuthenticator{"tenant-a-key": "tenant-a"},
		TenantHomeRegions: map[string]string{"tenant-a": "us-west"},
	})

	response := performJSON(t, handler, "tenant-a-key", http.MethodGet, "/v1/models", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var catalog struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
			Created *int64 `json:"created"`
		} `json:"data"`
	}
	decodeJSON(t, response, &catalog)
	if catalog.Object != "list" || len(catalog.Data) != 2 {
		t.Fatalf("catalog = %#v", catalog)
	}
	if catalog.Data[0].ID != "fast-chat" || catalog.Data[1].ID != "smart-chat" {
		t.Fatalf("models = %#v", catalog.Data)
	}
	for _, model := range catalog.Data {
		if model.Object != "model" || model.OwnedBy != "gateway" || model.Created == nil || *model.Created != createdAt.Unix() {
			t.Fatalf("model = %#v", model)
		}
	}
}

func TestCodexListsRoutableModels(t *testing.T) {
	textProfile := codexCapabilityProfile()
	router := provider.NewVersionedRouterAt(1, time.Unix(1_724_566_400, 0), []provider.Route{
		{ID: "openai-fast", Provider: "openai", Model: "codex-model", HomeRegion: "us-west", Healthy: true, Profile: textProfile},
	})
	handler := httpapi.New(httpapi.Config{
		Runtime:           runtime.New(store.NewMemoryResponseStore(), router),
		ModelCatalog:      router,
		Authenticator:     httpapi.StaticAuthenticator{"tenant-a-key": "tenant-a"},
		TenantHomeRegions: map[string]string{"tenant-a": "us-west"},
		LocalRegion:       "us-west",
	})

	response := performJSON(t, handler, "tenant-a-key", http.MethodGet, "/v1/models?client_version=1.2.3", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var catalog struct {
		Models []struct {
			Slug           string `json:"slug"`
			ContextWindow  int    `json:"context_window"`
			SupportedInAPI bool   `json:"supported_in_api"`
		} `json:"models"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Models) != 1 || catalog.Models[0].Slug != "codex-model" || catalog.Models[0].ContextWindow == 0 || !catalog.Models[0].SupportedInAPI {
		t.Fatalf("models = %#v", catalog.Models)
	}
}

func TestCodexListsRoutableModelsByClientHeader(t *testing.T) {
	textProfile := codexCapabilityProfile()
	router := provider.NewVersionedRouterAt(1, time.Unix(1_724_566_400, 0), []provider.Route{
		{ID: "openai-fast", Provider: "openai", Model: "codex-model", HomeRegion: "us-west", Healthy: true, Profile: textProfile},
	})
	handler := httpapi.New(httpapi.Config{
		Runtime: runtime.New(store.NewMemoryResponseStore(), router), ModelCatalog: router,
		Authenticator: httpapi.StaticAuthenticator{"tenant-a-key": "tenant-a"}, TenantHomeRegions: map[string]string{"tenant-a": "us-west"},
	})

	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer tenant-a-key")
	request.Header.Set("Originator", "codex_cli_rs")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"models"`) {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func codexCapabilityProfile() provider.CapabilityProfile {
	return provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{
		"text": provider.CapabilityNative, "responses_native": provider.CapabilityNative,
		"tools": provider.CapabilityNative, "reasoning": provider.CapabilityNative,
	}}
}

func TestStatefulWriteIsForwardedToTenantHomeRegion(t *testing.T) {
	t.Parallel()
	forwarded := make(chan *http.Request, 1)
	forwardClient := &http.Client{Transport: httpRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		forwarded <- request.Clone(context.Background())
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{"forwarded":true}`)), Request: request,
		}, nil
	})}
	handler := httpapi.New(httpapi.Config{
		Runtime:           runtime.New(store.NewMemoryResponseStore(), provider.NewStaticRouter(provider.NewEchoExecutor())),
		Authenticator:     httpapi.StaticAuthenticator{"tenant-a-key": "tenant-a"},
		TenantHomeRegions: map[string]string{"tenant-a": "us-west"}, LocalRegion: "cn-north",
		HomeRegionURLs: map[string]string{"us-west": "https://us-west.gateway.test"}, ForwardClient: forwardClient,
	})
	response := performJSON(t, handler, "tenant-a-key", http.MethodPost, "/v1/responses", map[string]any{
		"model": "echo-v1", "input": "home region",
	})
	if response.Code != http.StatusOK || !response.Flushed || !strings.Contains(response.Body.String(), `"forwarded":true`) {
		t.Fatalf("status/body = %d / %s", response.Code, response.Body.String())
	}
	request := <-forwarded
	if request.URL.Path != "/v1/responses" || request.Header.Get("Authorization") != "Bearer tenant-a-key" {
		t.Fatalf("forwarded request = %s / %q", request.URL.Path, request.Header.Get("Authorization"))
	}
}

type httpRoundTripFunc func(*http.Request) (*http.Response, error)

func (function httpRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestChatCompletionsStreamsCanonicalEventsAsSSE(t *testing.T) {
	t.Parallel()

	handler := newTestHandler()
	response := performJSON(t, handler, "tenant-a-key", http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":  "echo-v1",
		"stream": true,
		"messages": []map[string]any{
			{"role": "system", "content": "answer plainly"},
			{"role": "user", "content": "hello stream"},
		},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("stream status = %d, body = %s", response.Code, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/event-stream") {
		t.Fatalf("content type = %q, want text/event-stream", contentType)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	stream := string(body)
	if !strings.Contains(stream, `"delta":{"content":"answer plainlyhello stream"}`) {
		t.Fatalf("stream does not contain assistant delta: %s", stream)
	}
	if !strings.HasSuffix(stream, "data: [DONE]\n\n") {
		t.Fatalf("stream does not end with [DONE]: %s", stream)
	}
}

func TestResponsesStreamUsesNamedMonotonicCanonicalEvents(t *testing.T) {
	t.Parallel()

	handler := newTestHandler()
	response := performJSON(t, handler, "tenant-a-key", http.MethodPost, "/v1/responses", map[string]any{
		"model": "echo-v1", "input": "hello responses stream", "stream": true,
	})
	if response.Code != http.StatusOK {
		t.Fatalf("stream status = %d, body = %s", response.Code, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/event-stream") {
		t.Fatalf("content type = %q, want text/event-stream", contentType)
	}
	stream := response.Body.String()
	for _, expected := range []string{
		"event: response.created\n", `"sequence_number":1`,
		"event: response.output_text.delta\n", `"sequence_number":2`,
		"event: response.completed\n", `"sequence_number":3`,
	} {
		if !strings.Contains(stream, expected) {
			t.Fatalf("stream missing %q: %s", expected, stream)
		}
	}
}

func TestStoreFalseReturnsFinalResponseWithoutRetainingIt(t *testing.T) {
	t.Parallel()

	handler := newTestHandler()
	created := performJSON(t, handler, "tenant-a-key", http.MethodPost, "/v1/responses", map[string]any{
		"model": "echo-v1", "input": "ephemeral", "store": false,
	})
	if created.Code != http.StatusOK {
		t.Fatalf("create status = %d, body = %s", created.Code, created.Body.String())
	}
	var response core.Response
	decodeJSON(t, created, &response)
	if response.Status != core.ResponseStatusCompleted {
		t.Fatalf("status = %q, want completed", response.Status)
	}

	retrieved := performJSON(t, handler, "tenant-a-key", http.MethodGet, "/v1/responses/"+response.ID, nil)
	if retrieved.Code != http.StatusNotFound {
		t.Fatalf("retrieve status = %d, want 404 for store:false", retrieved.Code)
	}
}

func TestUnknownCompatibilityModeIsRejected(t *testing.T) {
	t.Parallel()
	response := performJSON(t, newTestHandler(), "tenant-a-key", http.MethodPost, "/v1/responses", map[string]any{
		"model": "echo-v1", "input": "hello", "compatibility_mode": "permissive",
	})
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "strict or best_effort") {
		t.Fatalf("status/body = %d / %s", response.Code, response.Body.String())
	}
}

func TestCacheProtectionOptInIsForbiddenWhileGatewayModeIsOff(t *testing.T) {
	t.Parallel()
	cacheProtection := map[string]any{
		"enabled": true, "max_spend_micros": 1_000_000, "max_refreshes": 1,
		"max_protection_window_seconds": 3600,
	}
	tests := []struct {
		name   string
		target string
		body   map[string]any
	}{
		{name: "Responses", target: "/v1/responses", body: map[string]any{
			"model": "echo-v1", "input": "hello", "cache_protection": cacheProtection,
		}},
		{name: "Responses stream", target: "/v1/responses", body: map[string]any{
			"model": "echo-v1", "input": "hello", "stream": true, "cache_protection": cacheProtection,
		}},
		{name: "Responses background", target: "/v1/responses", body: map[string]any{
			"model": "echo-v1", "input": "hello", "background": true, "cache_protection": cacheProtection,
		}},
		{name: "Chat stream", target: "/v1/chat/completions", body: map[string]any{
			"model": "echo-v1", "messages": []map[string]any{{"role": "user", "content": "hello"}},
			"stream": true, "cache_protection": cacheProtection,
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := performJSON(t, newTestHandler(), "tenant-a-key", http.MethodPost, test.target, test.body)
			if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "cache_protection_not_allowed") {
				t.Fatalf("status/body = %d / %s, want explicit Cache Protection forbidden response", response.Code, response.Body.String())
			}
		})
	}
}

func TestChatMultimodalContentBecomesTypedCanonicalInput(t *testing.T) {
	t.Parallel()
	captured := make(chan core.Request, 1)
	handler := handlerForExecutor(captureExecutor{capture: captured, delegate: provider.NewEchoExecutor()})
	response := performJSON(t, handler, "tenant-a-key", http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "gateway-model",
		"messages": []any{map[string]any{
			"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "describe"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.test/image.png", "detail": "low"}},
			},
		}},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status/body = %d / %s", response.Code, response.Body.String())
	}
	request := <-captured
	if len(request.Input) != 1 || len(request.Input[0].Content) != 2 ||
		request.Input[0].Content[1].Type != "input_image" ||
		request.Input[0].Content[1].ImageURL != "https://example.test/image.png" ||
		!slices.Contains(request.RequestedFeatures, "multimodal") {
		t.Fatalf("canonical multimodal request = %#v", request)
	}
}

func TestBackgroundResponseReturnsBeforeDurableCompletion(t *testing.T) {
	t.Parallel()

	handler := newTestHandler()
	created := performJSON(t, handler, "tenant-a-key", http.MethodPost, "/v1/responses", map[string]any{
		"model": "echo-v1", "input": "background", "background": true,
	})
	if created.Code != http.StatusOK {
		t.Fatalf("create status = %d, body = %s", created.Code, created.Body.String())
	}
	var initial core.Response
	decodeJSON(t, created, &initial)
	if initial.Status != core.ResponseStatusInProgress {
		t.Fatalf("initial status = %q, want in_progress", initial.Status)
	}

	deadline := time.Now().Add(time.Second)
	for {
		retrieved := performJSON(t, handler, "tenant-a-key", http.MethodGet, "/v1/responses/"+initial.ID, nil)
		if retrieved.Code != http.StatusOK {
			t.Fatalf("retrieve status = %d, body = %s", retrieved.Code, retrieved.Body.String())
		}
		var final core.Response
		decodeJSON(t, retrieved, &final)
		if final.Status == core.ResponseStatusCompleted {
			if final.OutputText() != "background" {
				t.Fatalf("final output = %q", final.OutputText())
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background response did not complete: %#v", final)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestResponsesCreateIsIdempotentPerTenantAndRequestHash(t *testing.T) {
	t.Parallel()

	handler := newTestHandler()
	body := map[string]any{"model": "echo-v1", "input": "idempotent"}
	first := performJSONWithIdempotency(t, handler, "tenant-a-key", "request-42", body)
	second := performJSONWithIdempotency(t, handler, "tenant-a-key", "request-42", body)
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("statuses = %d/%d, bodies = %s / %s", first.Code, second.Code, first.Body.String(), second.Body.String())
	}
	var firstResponse, secondResponse core.Response
	decodeJSON(t, first, &firstResponse)
	decodeJSON(t, second, &secondResponse)
	if firstResponse.ID != secondResponse.ID {
		t.Fatalf("response IDs = %q/%q, want same idempotent response", firstResponse.ID, secondResponse.ID)
	}

	conflict := performJSONWithIdempotency(t, handler, "tenant-a-key", "request-42", map[string]any{
		"model": "echo-v1", "input": "different request",
	})
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflict status = %d, want 409; body = %s", conflict.Code, conflict.Body.String())
	}
}

func TestResponsesExposesCanonicalToolsAndSamplingFields(t *testing.T) {
	t.Parallel()
	captured := make(chan core.Request, 1)
	executor := captureExecutor{capture: captured, delegate: provider.NewEchoExecutor()}
	handler := handlerForExecutor(executor)
	response := performJSON(t, handler, "tenant-a-key", http.MethodPost, "/v1/responses", map[string]any{
		"model": "gateway-model", "instructions": "system:", "input": "user",
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name": "lookup", "parameters": map[string]any{"type": "object"},
		}}},
		"tool_choice": "required", "temperature": 0.2, "top_p": 0.9,
		"max_output_tokens": 42, "stop": []string{"END"}, "user": "end-user",
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	request := <-captured
	if len(request.Tools) != 1 || string(request.ToolChoice) != `"required"` || request.MaxOutputTokens == nil || *request.MaxOutputTokens != 42 || request.EndUserID != "end-user" {
		t.Fatalf("canonical request fields = %#v", request)
	}
	if len(request.Input) != 2 || request.Input[0].Role != "system" || request.Input[1].Role != "user" {
		t.Fatalf("canonical instructions/input = %#v", request.Input)
	}
	if strings.Join(request.RequestedFeatures, ",") != "text,tools,sampling,end_user_id" {
		t.Fatalf("requested features = %#v", request.RequestedFeatures)
	}
}

func TestResponsesAcceptsCodexNativeFieldsAndToolShapes(t *testing.T) {
	t.Parallel()
	captured := make(chan core.Request, 1)
	executor := captureExecutor{capture: captured, delegate: provider.NewEchoExecutor()}
	handler := handlerForExecutor(executor)
	response := performJSON(t, handler, "tenant-a-key", http.MethodPost, "/v1/responses", map[string]any{
		"model": "gateway-model", "store": false,
		"input": []any{
			map[string]any{"type": "message", "role": "developer", "content": []any{map[string]any{"type": "input_text", "text": "work locally"}}},
			map[string]any{"type": "function_call", "call_id": "old-call", "name": "exec_command", "arguments": `{"cmd":"pwd"}`},
			map[string]any{"type": "function_call_output", "call_id": "old-call", "output": "/workspace"},
		},
		"tools": []any{
			map[string]any{"type": "function", "name": "exec_command", "parameters": map[string]any{"type": "object"}},
			map[string]any{"type": "namespace", "name": "mcp", "tools": []any{}},
			map[string]any{"type": "web_search", "external_web_access": false},
		},
		"tool_choice": "auto", "parallel_tool_calls": true,
		"reasoning": map[string]any{"summary": "auto"}, "include": []string{"reasoning.encrypted_content"},
		"prompt_cache_key": "thread-1", "client_metadata": map[string]any{"thread_id": "thread-1"},
		"text": map[string]any{"verbosity": "low"}, "service_tier": "default", "truncation": "auto",
		"max_tool_calls": 20, "safety_identifier": "tenant-user",
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	request := <-captured
	if request.ParallelToolCalls == nil || !*request.ParallelToolCalls || request.PromptCacheKey != "thread-1" || request.MaxToolCalls == nil || *request.MaxToolCalls != 20 {
		t.Fatalf("Codex request fields = %#v", request)
	}
	if len(request.Input) != 3 || request.Input[0].Role != "developer" || string(request.Input[1].Arguments) != `{"cmd":"pwd"}` {
		t.Fatalf("Codex input = %#v", request.Input)
	}
	for _, feature := range []string{"text", "tools", "reasoning", "responses_native"} {
		if !slices.Contains(request.RequestedFeatures, feature) {
			t.Fatalf("requested features = %#v, missing %q", request.RequestedFeatures, feature)
		}
	}
}

func TestChatCompletionPreservesToolCallRoundTrip(t *testing.T) {
	t.Parallel()
	captured := make(chan core.Request, 1)
	usage := core.Usage{InputTokens: 20, OutputTokens: 3, TotalTokens: 23}
	executor := captureExecutor{capture: captured, delegate: fixedExecutor{events: []core.Event{
		{Type: "response.output_item.done", Item: &core.Item{
			Type: "function_call", CallID: "call-next", Name: "lookup", Arguments: json.RawMessage(`{"q":"next"}`),
		}},
		{Type: "response.completed", Usage: &usage, ProviderUsage: json.RawMessage(`{"input_tokens":20}`)},
	}}}
	handler := handlerForExecutor(executor)
	response := performJSON(t, handler, "tenant-a-key", http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "gateway-model",
		"messages": []any{
			map[string]any{"role": "user", "content": "first"},
			map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{
				"id": "call-old", "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{"q":"old"}`},
			}}},
			map[string]any{"role": "tool", "tool_call_id": "call-old", "content": "old result"},
		},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object"}}}},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	request := <-captured
	if len(request.Input) != 3 || request.Input[1].Type != "function_call" || request.Input[2].Type != "function_call_output" {
		t.Fatalf("canonical tool history = %#v", request.Input)
	}
	var body struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content   any                `json:"content"`
				ToolCalls []wireChatToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	decodeJSON(t, response, &body)
	if len(body.Choices) != 1 || body.Choices[0].FinishReason != "tool_calls" || body.Choices[0].Message.Content != nil ||
		len(body.Choices[0].Message.ToolCalls) != 1 || body.Choices[0].Message.ToolCalls[0].ID != "call-next" {
		t.Fatalf("chat tool response = %#v", body)
	}
}

func TestConversationOrdersInitialInputAndResponseOutput(t *testing.T) {
	t.Parallel()

	handler := newTestHandler()
	createdConversation := performJSON(t, handler, "tenant-a-key", http.MethodPost, "/v1/conversations", map[string]any{
		"items": []map[string]any{{
			"type": "message", "role": "system",
			"content": []map[string]any{{"type": "input_text", "text": "system:"}},
		}},
	})
	if createdConversation.Code != http.StatusOK {
		t.Fatalf("create conversation status = %d, body = %s", createdConversation.Code, createdConversation.Body.String())
	}
	var conversation core.Conversation
	decodeJSON(t, createdConversation, &conversation)
	if conversation.Revision != 1 {
		t.Fatalf("initial revision = %d, want 1", conversation.Revision)
	}

	createdResponse := performJSON(t, handler, "tenant-a-key", http.MethodPost, "/v1/responses", map[string]any{
		"model": "echo-v1", "conversation": conversation.ID, "input": "user",
	})
	if createdResponse.Code != http.StatusOK {
		t.Fatalf("create response status = %d, body = %s", createdResponse.Code, createdResponse.Body.String())
	}
	var response core.Response
	decodeJSON(t, createdResponse, &response)
	if response.OutputText() != "system:user" {
		t.Fatalf("response output = %q, want full conversation context", response.OutputText())
	}

	retrieved := performJSON(t, handler, "tenant-a-key", http.MethodGet, "/v1/conversations/"+conversation.ID, nil)
	if retrieved.Code != http.StatusOK {
		t.Fatalf("get conversation status = %d, body = %s", retrieved.Code, retrieved.Body.String())
	}
	decodeJSON(t, retrieved, &conversation)
	if conversation.Revision != 3 || conversation.ActiveResponseID != "" {
		t.Fatalf("final conversation revision/active = %d/%q, want 3/empty", conversation.Revision, conversation.ActiveResponseID)
	}
	if len(conversation.Items) != 3 {
		t.Fatalf("conversation items = %#v, want system, user, assistant", conversation.Items)
	}
	if conversation.Items[2].Role != "assistant" {
		t.Fatalf("last item role = %q, want assistant", conversation.Items[2].Role)
	}
}

func newTestHandler() http.Handler {
	responseStore := store.NewMemoryResponseStore()
	executor := provider.NewEchoExecutor()
	engine := runtime.New(responseStore, provider.NewStaticRouter(executor))
	return httpapi.New(httpapi.Config{
		Runtime: engine,
		Authenticator: httpapi.StaticAuthenticator{
			"tenant-a-key": "tenant-a",
			"tenant-b-key": "tenant-b",
		},
	})
}

func handlerForExecutor(executor provider.ResponseExecutor) http.Handler {
	route := provider.Route{
		ID: "test-route", Provider: "test-provider", Model: "gateway-model", Region: "local", HomeRegion: "local",
		CredentialScope: "test", Healthy: true, Executor: executor, CacheUsageReliable: true,
		Profile: provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{
			"text": provider.CapabilityNative, "streaming": provider.CapabilityNative,
			"tools": provider.CapabilityNative, "sampling": provider.CapabilityNative, "multimodal": provider.CapabilityNative,
			"end_user_id": provider.CapabilityNative, "reasoning": provider.CapabilityNative,
			"responses_native": provider.CapabilityNative,
		}},
		PriceSnapshot: core.PriceSnapshot{
			ID: "test-price", Provider: "test-provider", Model: "provider-model", Region: "local",
			Currency: "USD", EffectiveAt: 1, Source: "test",
		},
	}
	return httpapi.New(httpapi.Config{
		Runtime:       runtime.New(store.NewMemoryResponseStore(), provider.NewRouter(route)),
		Authenticator: httpapi.StaticAuthenticator{"tenant-a-key": "tenant-a"},
	})
}

type captureExecutor struct {
	capture  chan<- core.Request
	delegate provider.ResponseExecutor
}

type wireChatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func (e captureExecutor) Execute(ctx context.Context, request core.Request) (provider.EventStream, error) {
	e.capture <- request
	return e.delegate.Execute(ctx, request)
}

type fixedExecutor struct{ events []core.Event }

type blockingExecutor struct {
	started  chan<- struct{}
	release  <-chan struct{}
	delegate provider.ResponseExecutor
}

func (executor blockingExecutor) Execute(ctx context.Context, request core.Request) (provider.EventStream, error) {
	executor.started <- struct{}{}
	select {
	case <-executor.release:
		return executor.delegate.Execute(ctx, request)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (e fixedExecutor) Execute(context.Context, core.Request) (provider.EventStream, error) {
	return &fixedEventStream{events: append([]core.Event(nil), e.events...)}, nil
}

type fixedEventStream struct {
	events []core.Event
	index  int
}

func (s *fixedEventStream) Recv() (core.Event, error) {
	if s.index >= len(s.events) {
		return core.Event{}, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}

func (s *fixedEventStream) Close() error { return nil }

func performJSON(t *testing.T, handler http.Handler, token, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()

	var requestBody bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&requestBody).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, target, &requestBody)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

func performJSONWithIdempotency(t *testing.T, handler http.Handler, token, key string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var requestBody bytes.Buffer
	if err := json.NewEncoder(&requestBody).Encode(body); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", &requestBody)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", key)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func decodeJSON(t *testing.T, recorder *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.NewDecoder(recorder.Body).Decode(target); err != nil {
		t.Fatalf("decode response: %v; body = %s", err, recorder.Body.String())
	}
}
