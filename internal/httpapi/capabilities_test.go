package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/toddzheng/llm-gateway/internal/access"
	"github.com/toddzheng/llm-gateway/internal/capability"
	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/httpapi"
	"github.com/toddzheng/llm-gateway/internal/provider"
	"github.com/toddzheng/llm-gateway/internal/store"
)

func TestEmbeddingsExecuteThroughCapabilityRoute(t *testing.T) {
	t.Parallel()
	usageStore := store.NewMemoryResponseStore()
	deterministic := provider.NewDeterministicCapabilityExecutor()
	router := provider.NewRouter(provider.Route{
		ID: "embedding-route", Provider: "deterministic", Model: "embed-model", Region: "local", HomeRegion: "local",
		CredentialScope: "test", Healthy: true, EmbeddingExecutor: deterministic,
		Profile: provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{
			"embeddings": provider.CapabilityNative,
		}},
		PriceSnapshot: core.PriceSnapshot{
			ID: "embedding-price", Provider: "deterministic", Model: "embed-model", Region: "local",
			Currency: "USD", EffectiveAt: 1, Source: "test", EmbeddingInputPerMillionMicros: 100_000,
		},
	})
	handler := httpapi.New(httpapi.Config{
		CapabilityRuntime: capability.New(usageStore, router, capability.Options{}),
		Authenticator:     httpapi.StaticAuthenticator{"tenant-a-key": "tenant-a"},
	})

	response := capabilityJSON(t, handler, "/v1/embeddings", map[string]any{
		"model": "embed-model", "input": []string{"alpha", "beta"}, "dimensions": 3,
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload struct {
		Object string `json:"object"`
		Model  string `json:"model"`
		Data   []struct {
			Object    string    `json:"object"`
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
		Usage struct {
			PromptTokens int64 `json:"prompt_tokens"`
			TotalTokens  int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Object != "list" || payload.Model != "embed-model" || len(payload.Data) != 2 {
		t.Fatalf("embedding response = %#v", payload)
	}
	for index, item := range payload.Data {
		if item.Object != "embedding" || item.Index != index || len(item.Embedding) != 3 {
			t.Fatalf("embedding item %d = %#v", index, item)
		}
	}
	if payload.Usage.PromptTokens != 2 || payload.Usage.TotalTokens != 2 {
		t.Fatalf("usage = %#v", payload.Usage)
	}
	if response.Header().Get("X-Gateway-Operation-ID") == "" {
		t.Fatal("missing X-Gateway-Operation-ID")
	}
	records := usageStore.CapabilityUsageRecords("tenant-a")
	if len(records) != 1 || records[0].Capability != core.CapabilityEmbeddings || records[0].InputUnits != 2 || records[0].Dimensions != 3 {
		t.Fatalf("usage records = %#v", records)
	}
	if bytes.Contains(records[0].ProviderUsage, []byte("embedding")) {
		t.Fatalf("usage ledger retained vector content: %s", records[0].ProviderUsage)
	}
}

func TestEmbeddingsSupportBase64Encoding(t *testing.T) {
	t.Parallel()
	usageStore := store.NewMemoryResponseStore()
	deterministic := provider.NewDeterministicCapabilityExecutor()
	router := provider.NewRouter(provider.Route{
		ID: "embedding-route", Provider: "deterministic", Model: "embed-model", Region: "local", HomeRegion: "local", Healthy: true,
		EmbeddingExecutor: deterministic,
		Profile:           provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{"embeddings": provider.CapabilityNative}},
		PriceSnapshot: core.PriceSnapshot{
			ID: "embedding-price", Provider: "deterministic", Model: "embed-model", Region: "local", Currency: "USD", EffectiveAt: 1, Source: "test",
		},
	})
	handler := httpapi.New(httpapi.Config{
		CapabilityRuntime: capability.New(usageStore, router, capability.Options{}),
		Authenticator:     httpapi.StaticAuthenticator{"tenant-a-key": "tenant-a"},
	})
	response := capabilityJSON(t, handler, "/v1/embeddings", map[string]any{
		"model": "embed-model", "input": "alpha", "dimensions": 2, "encoding_format": "base64",
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status/body = %d/%s", response.Code, response.Body.String())
	}
	var payload struct {
		Data []struct {
			Embedding string `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Data) != 1 || payload.Data[0].Embedding == "" {
		t.Fatalf("base64 embedding response = %#v", payload)
	}
}

func TestModerationsReturnTypedResultsAndContentFreeUsage(t *testing.T) {
	t.Parallel()
	usageStore := store.NewMemoryResponseStore()
	deterministic := provider.NewDeterministicCapabilityExecutor()
	router := provider.NewRouter(provider.Route{
		ID: "moderation-route", Provider: "deterministic", Model: "moderation-model", Region: "local", HomeRegion: "local",
		CredentialScope: "test", Healthy: true, ModerationExecutor: deterministic,
		Profile: provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{
			"moderation": provider.CapabilityNative,
		}},
		PriceSnapshot: core.PriceSnapshot{
			ID: "moderation-price", Provider: "deterministic", Model: "moderation-model", Region: "local",
			Currency: "USD", EffectiveAt: 1, Source: "test",
		},
	})
	handler := httpapi.New(httpapi.Config{
		CapabilityRuntime: capability.New(usageStore, router, capability.Options{}),
		Authenticator:     httpapi.StaticAuthenticator{"tenant-a-key": "tenant-a"},
	})

	response := capabilityJSON(t, handler, "/v1/moderations", map[string]any{
		"model": "moderation-model", "input": []string{"ordinary text", "unsafe request"},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Results []struct {
			Flagged        bool               `json:"flagged"`
			Categories     map[string]bool    `json:"categories"`
			CategoryScores map[string]float64 `json:"category_scores"`
		} `json:"results"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ID == "" || payload.Model != "moderation-model" || len(payload.Results) != 2 || payload.Results[0].Flagged || !payload.Results[1].Flagged {
		t.Fatalf("moderation response = %#v", payload)
	}
	if !payload.Results[1].Categories["unsafe"] || payload.Results[1].CategoryScores["unsafe"] != 1 {
		t.Fatalf("moderation categories = %#v", payload.Results[1])
	}
	records := usageStore.CapabilityUsageRecords("tenant-a")
	if len(records) != 1 || records[0].Capability != core.CapabilityModeration || records[0].InputUnits != 2 {
		t.Fatalf("usage records = %#v", records)
	}
	if bytes.Contains(records[0].ProviderUsage, []byte("ordinary")) || bytes.Contains(records[0].ProviderUsage, []byte("unsafe request")) {
		t.Fatalf("usage ledger retained moderation input: %s", records[0].ProviderUsage)
	}
}

func TestRerankReturnsStableIndexesAndDocumentUsage(t *testing.T) {
	t.Parallel()
	usageStore := store.NewMemoryResponseStore()
	deterministic := provider.NewDeterministicCapabilityExecutor()
	router := provider.NewRouter(provider.Route{
		ID: "rerank-route", Provider: "deterministic", Model: "rerank-model", Region: "local", HomeRegion: "local",
		CredentialScope: "test", Healthy: true, RerankExecutor: deterministic,
		Profile: provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{
			"rerank": provider.CapabilityNative,
		}},
		PriceSnapshot: core.PriceSnapshot{
			ID: "rerank-price", Provider: "deterministic", Model: "rerank-model", Region: "local",
			Currency: "USD", EffectiveAt: 1, Source: "test", RerankDocumentPerThousandMicros: 10_000,
		},
	})
	handler := httpapi.New(httpapi.Config{
		CapabilityRuntime: capability.New(usageStore, router, capability.Options{}),
		Authenticator:     httpapi.StaticAuthenticator{"tenant-a-key": "tenant-a"},
	})

	response := capabilityJSON(t, handler, "/v1/rerank", map[string]any{
		"model": "rerank-model", "query": "red apple",
		"documents": []any{"blue ocean", map[string]any{"text": "red apple tree"}, "red bicycle"},
		"top_n":     2, "return_documents": true,
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload struct {
		ID      string `json:"id"`
		Results []struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
			Document       *struct {
				Text string `json:"text"`
			} `json:"document"`
		} `json:"results"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ID == "" || len(payload.Results) != 2 || payload.Results[0].Index != 1 || payload.Results[1].Index != 2 {
		t.Fatalf("rerank response = %#v", payload)
	}
	if payload.Results[0].RelevanceScore <= payload.Results[1].RelevanceScore || payload.Results[0].Document == nil || payload.Results[0].Document.Text != "red apple tree" {
		t.Fatalf("rerank ordering = %#v", payload.Results)
	}
	records := usageStore.CapabilityUsageRecords("tenant-a")
	if len(records) != 1 || records[0].Capability != core.CapabilityRerank || records[0].Documents != 3 {
		t.Fatalf("usage records = %#v", records)
	}
	if bytes.Contains(records[0].ProviderUsage, []byte("red apple")) {
		t.Fatalf("usage ledger retained rerank content: %s", records[0].ProviderUsage)
	}
}

func TestCapabilityQuotaFailureUsesRateLimitContract(t *testing.T) {
	t.Parallel()
	limit := int64(1)
	deterministic := provider.NewDeterministicCapabilityExecutor()
	router := provider.NewRouter(provider.Route{
		ID: "embedding-route", Provider: "deterministic", Model: "embed-model", Region: "local", HomeRegion: "local", Healthy: true,
		EmbeddingExecutor: deterministic,
		Profile:           provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{"embeddings": provider.CapabilityNative}},
		PriceSnapshot: core.PriceSnapshot{
			ID: "embedding-price", Provider: "deterministic", Model: "embed-model", Region: "local", Currency: "USD", EffectiveAt: 1, Source: "test",
		},
	})
	handler := httpapi.New(httpapi.Config{
		CapabilityRuntime: capability.New(store.NewMemoryResponseStore(), router, capability.Options{}),
		Authenticator: principalAuthenticator{principal: access.Principal{
			TenantID: "tenant-a", APIKeyID: "key-a", HomeRegion: "local", ExecutionEpoch: 1,
			TenantPolicy: core.TenantPolicy{Revision: 1, Limits: core.QuotaLimits{EmbeddingInputUnits: &limit}},
			APIKeyPolicy: core.APIKeyPolicy{Revision: 1},
		}},
	})
	response := capabilityJSON(t, handler, "/v1/embeddings", map[string]any{"model": "embed-model", "input": []string{"a", "b"}})
	if response.Code != http.StatusTooManyRequests || !bytes.Contains(response.Body.Bytes(), []byte(`"rate_limit_exceeded"`)) {
		t.Fatalf("status/body = %d/%s", response.Code, response.Body.String())
	}
}

func TestCapabilityCatalogPublishesOnlyHealthyHomeRegionDeclarations(t *testing.T) {
	t.Parallel()
	router := provider.NewVersionedRouterAt(7, time.Unix(1_800_000_000, 0), []provider.Route{
		{ID: "primary", Model: "multi-model", HomeRegion: "us-west", TenantIDs: []string{"tenant-a"}, Healthy: true, Profile: provider.CapabilityProfile{Revision: 3, Features: map[string]provider.CapabilitySupport{
			"text": provider.CapabilityNative, "embeddings": provider.CapabilityNative, "rerank": provider.CapabilityTranslated,
		}}},
		{ID: "unhealthy", Model: "hidden-model", HomeRegion: "us-west", Healthy: false, Profile: provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{
			"moderation": provider.CapabilityNative,
		}}},
		{ID: "other-region", Model: "other-model", HomeRegion: "eu-west", Healthy: true, Profile: provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{
			"embeddings": provider.CapabilityNative,
		}}},
		{ID: "other-tenant", Model: "tenant-b-model", HomeRegion: "us-west", TenantIDs: []string{"tenant-b"}, Healthy: true, Profile: provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{
			"embeddings": provider.CapabilityNative,
		}}},
	})
	handler := httpapi.New(httpapi.Config{
		CapabilityCatalog: router,
		Authenticator: principalAuthenticator{principal: access.Principal{
			TenantID: "tenant-a", HomeRegion: "us-west", ExecutionEpoch: 1,
		}},
	})
	request := httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil)
	request.Header.Set("Authorization", "Bearer tenant-a-key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status/body = %d/%s", response.Code, response.Body.String())
	}
	var payload struct {
		Object string `json:"object"`
		Data   []struct {
			ID           string                                `json:"id"`
			Capabilities map[string]provider.CapabilitySupport `json:"capabilities"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Object != "list" || len(payload.Data) != 1 || payload.Data[0].ID != "multi-model" ||
		payload.Data[0].Capabilities["embeddings"] != provider.CapabilityNative ||
		payload.Data[0].Capabilities["rerank"] != provider.CapabilityTranslated || len(payload.Data[0].Capabilities) != 2 {
		t.Fatalf("catalog = %#v", payload)
	}
}

func TestTranslatedRerankRequiresExplicitBestEffortMode(t *testing.T) {
	t.Parallel()
	deterministic := provider.NewDeterministicCapabilityExecutor()
	router := provider.NewRouter(provider.Route{
		ID: "translated-rerank", Provider: "deterministic", Model: "rerank-model", Region: "local", HomeRegion: "local", Healthy: true,
		RerankExecutor: deterministic,
		Profile:        provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{"rerank": provider.CapabilityTranslated}},
		PriceSnapshot: core.PriceSnapshot{
			ID: "rerank-price", Provider: "deterministic", Model: "rerank-model", Region: "local", Currency: "USD", EffectiveAt: 1, Source: "test",
		},
	})
	handler := httpapi.New(httpapi.Config{
		CapabilityRuntime: capability.New(store.NewMemoryResponseStore(), router, capability.Options{}),
		Authenticator:     httpapi.StaticAuthenticator{"tenant-a-key": "tenant-a"},
	})
	body := map[string]any{"model": "rerank-model", "query": "a", "documents": []string{"a"}}
	strict := capabilityJSON(t, handler, "/v1/rerank", body)
	if strict.Code != http.StatusBadRequest {
		t.Fatalf("strict status/body = %d/%s", strict.Code, strict.Body.String())
	}
	body["compatibility_mode"] = "best_effort"
	bestEffort := capabilityJSON(t, handler, "/v1/rerank", body)
	if bestEffort.Code != http.StatusOK {
		t.Fatalf("best-effort status/body = %d/%s", bestEffort.Code, bestEffort.Body.String())
	}
}

type principalAuthenticator struct{ principal access.Principal }

func (a principalAuthenticator) Authenticate(context.Context, string) (access.Principal, error) {
	return a.principal, nil
}

func capabilityJSON(t *testing.T, handler http.Handler, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(encoded))
	request.Header.Set("Authorization", "Bearer tenant-a-key")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
