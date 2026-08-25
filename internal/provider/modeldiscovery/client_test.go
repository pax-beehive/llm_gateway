package modeldiscovery_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/toddzheng/llm-gateway/internal/provider/modeldiscovery"
)

func TestOpenAICompatibleModelDiscovery(t *testing.T) {
	t.Parallel()

	for _, provider := range []modeldiscovery.Provider{modeldiscovery.OpenAI, modeldiscovery.DeepSeek} {
		provider := provider
		t.Run(string(provider), func(t *testing.T) {
			t.Parallel()
			client, err := modeldiscovery.New(modeldiscovery.Config{
				Provider: provider,
				BaseURL:  "https://provider.test/v1",
				APIKey:   "secret",
				HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
					if request.Method != http.MethodGet || request.URL.Path != "/v1/models" {
						t.Fatalf("request = %s %s", request.Method, request.URL.Path)
					}
					if got := request.Header.Get("Authorization"); got != "Bearer secret" {
						t.Fatalf("Authorization = %q", got)
					}
					return jsonResponse(request, `{"data":[{"id":"model-b","owned_by":"provider"},{"id":"model-a"},{"id":"model-b"}]}`), nil
				})},
			})
			if err != nil {
				t.Fatal(err)
			}
			models, err := client.List(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got := modelIDs(models); strings.Join(got, ",") != "model-b,model-a" {
				t.Fatalf("models = %v", got)
			}
		})
	}
}

func TestAnthropicModelDiscoveryPaginates(t *testing.T) {
	t.Parallel()

	calls := 0
	client, err := modeldiscovery.New(modeldiscovery.Config{
		Provider: modeldiscovery.Anthropic,
		BaseURL:  "https://api.anthropic.test/v1",
		APIKey:   "secret",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			calls++
			if request.URL.Path != "/v1/models" || request.URL.Query().Get("limit") != "1000" {
				t.Fatalf("URL = %s", request.URL.String())
			}
			if request.Header.Get("x-api-key") != "secret" || request.Header.Get("anthropic-version") != "2023-06-01" {
				t.Fatalf("headers = %#v", request.Header)
			}
			if calls == 1 {
				if request.URL.Query().Get("after_id") != "" {
					t.Fatalf("first after_id = %q", request.URL.Query().Get("after_id"))
				}
				return jsonResponse(request, `{"data":[{"id":"claude-haiku"}],"has_more":true,"last_id":"cursor-1"}`), nil
			}
			if request.URL.Query().Get("after_id") != "cursor-1" {
				t.Fatalf("second after_id = %q", request.URL.Query().Get("after_id"))
			}
			return jsonResponse(request, `{"data":[{"id":"claude-sonnet"}],"has_more":false,"last_id":"cursor-2"}`), nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	models, err := client.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(modelIDs(models), ","); got != "claude-haiku,claude-sonnet" {
		t.Fatalf("models = %s", got)
	}
}

func TestGeminiModelDiscoveryPaginatesAndFiltersGenerationModels(t *testing.T) {
	t.Parallel()

	calls := 0
	client, err := modeldiscovery.New(modeldiscovery.Config{
		Provider: modeldiscovery.Gemini,
		BaseURL:  "https://generativelanguage.test/v1beta",
		APIKey:   "secret",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			calls++
			if request.Header.Get("x-goog-api-key") != "secret" || request.Header.Get("x-goog-api-client") != "llm-gateway/0.1.0" {
				t.Fatalf("headers = %#v", request.Header)
			}
			if request.URL.Path != "/v1beta/models" || request.URL.Query().Get("pageSize") != "1000" {
				t.Fatalf("URL = %s", request.URL.String())
			}
			if calls == 1 {
				return jsonResponse(request, `{"models":[{"name":"models/gemini-flash","supportedGenerationMethods":["generateContent"]},{"name":"models/text-embedding","supportedGenerationMethods":["embedContent"]}],"nextPageToken":"next"}`), nil
			}
			if request.URL.Query().Get("pageToken") != "next" {
				t.Fatalf("pageToken = %q", request.URL.Query().Get("pageToken"))
			}
			return jsonResponse(request, `{"models":[{"name":"models/gemini-flash-lite","supportedGenerationMethods":["generateContent"]}]}`), nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	models, err := client.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(modelIDs(models), ","); got != "gemini-flash,gemini-flash-lite" {
		t.Fatalf("models = %s", got)
	}
}

func TestSmokeModelSelectionIsCostConsciousAndFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		provider modeldiscovery.Provider
		models   []string
		want     string
		wantErr  bool
	}{
		{name: "OpenAI nano", provider: modeldiscovery.OpenAI, models: []string{"gpt-5", "gpt-5-mini", "gpt-5-nano"}, want: "gpt-5-nano"},
		{name: "OpenAI Luna", provider: modeldiscovery.OpenAI, models: []string{"gpt-5.6-sol", "gpt-5.6-luna"}, want: "gpt-5.6-luna"},
		{name: "OpenAI refuses full model", provider: modeldiscovery.OpenAI, models: []string{"gpt-5"}, wantErr: true},
		{name: "DeepSeek flash", provider: modeldiscovery.DeepSeek, models: []string{"deepseek-v4-pro", "deepseek-v4-flash"}, want: "deepseek-v4-flash"},
		{name: "DeepSeek chat", provider: modeldiscovery.DeepSeek, models: []string{"deepseek-reasoner", "deepseek-chat"}, want: "deepseek-chat"},
		{name: "DeepSeek refuses pro", provider: modeldiscovery.DeepSeek, models: []string{"deepseek-v4-pro"}, wantErr: true},
		{name: "Anthropic haiku", provider: modeldiscovery.Anthropic, models: []string{"claude-opus-4", "claude-haiku-4"}, want: "claude-haiku-4"},
		{name: "Anthropic refuses Opus", provider: modeldiscovery.Anthropic, models: []string{"claude-opus-4"}, wantErr: true},
		{name: "Gemini flash lite", provider: modeldiscovery.Gemini, models: []string{"gemini-pro", "gemini-flash", "gemini-flash-lite", "gemini-flash-image"}, want: "gemini-flash-lite"},
		{name: "Gemini refuses image", provider: modeldiscovery.Gemini, models: []string{"gemini-flash-image"}, wantErr: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			models := make([]modeldiscovery.Model, 0, len(test.models))
			for _, id := range test.models {
				models = append(models, modeldiscovery.Model{ID: id})
			}
			got, err := modeldiscovery.SelectSmokeModel(test.provider, models)
			if test.wantErr {
				if err == nil {
					t.Fatalf("SelectSmokeModel() = %q, want error", got)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("SelectSmokeModel() = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}

func TestModelDiscoveryRejectsProviderErrors(t *testing.T) {
	t.Parallel()

	client, err := modeldiscovery.New(modeldiscovery.Config{
		Provider: modeldiscovery.OpenAI,
		BaseURL:  "https://provider.test/v1",
		APIKey:   "secret",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader("bad key")), Request: request}, nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.List(context.Background()); err == nil || !strings.Contains(err.Error(), "status 401") {
		t.Fatalf("List() error = %v", err)
	}
}

func TestModelDiscoveryRequiresAPIKey(t *testing.T) {
	t.Parallel()

	if _, err := modeldiscovery.New(modeldiscovery.Config{
		Provider: modeldiscovery.OpenAI,
		BaseURL:  "https://provider.test/v1",
	}); err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("New() error = %v", err)
	}
}

func TestModelDiscoveryRejectsRedirectsWithoutForwardingCredentials(t *testing.T) {
	t.Parallel()

	calls := 0
	client, err := modeldiscovery.New(modeldiscovery.Config{
		Provider: modeldiscovery.Gemini,
		BaseURL:  "https://provider.test/v1beta",
		APIKey:   "secret",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			calls++
			if request.URL.Host == "attacker.test" {
				t.Fatalf("credentials followed a cross-origin redirect: %#v", request.Header)
			}
			return &http.Response{
				StatusCode: http.StatusFound,
				Header:     http.Header{"Location": []string{"https://attacker.test/models"}},
				Body:       io.NopCloser(strings.NewReader("redirect")),
				Request:    request,
			}, nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.List(context.Background()); err == nil || !strings.Contains(err.Error(), "status 302") {
		t.Fatalf("List() error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("HTTP calls = %d, want 1", calls)
	}
}

func modelIDs(models []modeldiscovery.Model) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	return ids
}

func jsonResponse(request *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
