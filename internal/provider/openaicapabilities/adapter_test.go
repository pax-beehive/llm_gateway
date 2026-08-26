package openaicapabilities_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/provider"
	"github.com/toddzheng/llm-gateway/internal/provider/openaicapabilities"
)

func TestEmbeddingUsesProviderModelAndRetainsOnlyUsageEvidence(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.String() != "https://provider.test/v1/embeddings" {
			t.Fatalf("request = %s %s", request.Method, request.URL)
		}
		if request.Header.Get("Authorization") != "Bearer provider-key" || request.Header.Get("X-Route") != "primary" {
			t.Fatalf("headers = %#v", request.Header)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		wire := string(body)
		for _, expected := range []string{`"model":"provider-embedding-model"`, `"input":["alpha","beta"]`, `"dimensions":2`} {
			if !strings.Contains(wire, expected) {
				t.Fatalf("request body missing %s: %s", expected, wire)
			}
		}
		return jsonResponse(request, `{
			"object":"list","model":"provider-embedding-model",
			"data":[
				{"object":"embedding","index":0,"embedding":[0.1,0.2]},
				{"object":"embedding","index":1,"embedding":[0.3,0.4]}
			],
			"usage":{"prompt_tokens":7,"total_tokens":7}
		}`), nil
	})}
	adapter, err := openaicapabilities.New(openaicapabilities.Config{
		BaseURL: "https://provider.test/v1", APIKey: "provider-key", Model: "provider-embedding-model",
		HTTPClient: client, Headers: map[string]string{"X-Route": "primary"},
	})
	if err != nil {
		t.Fatal(err)
	}
	dimensions := 2
	alpha, beta := "alpha", "beta"
	result, err := adapter.Embed(context.Background(), core.EmbeddingRequest{
		Model: "public-model", Input: []core.EmbeddingInput{{Text: &alpha}, {Text: &beta}}, Dimensions: &dimensions,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Model != "public-model" || result.InputUnits != 7 || result.Dimensions != 2 || len(result.Data) != 2 {
		t.Fatalf("result = %#v", result)
	}
	if strings.Contains(string(result.ProviderUsage), "embedding") || string(result.ProviderUsage) != `{"prompt_tokens":7,"total_tokens":7}` {
		t.Fatalf("provider usage = %s", result.ProviderUsage)
	}
}

func TestModerationPreservesTypedCategoriesWithoutInventingUsage(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		wire := string(body)
		if request.URL.String() != "https://provider.test/v1/moderations" ||
			!strings.Contains(wire, `"model":"provider-moderation-model"`) || !strings.Contains(wire, `"input":["first","second"]`) {
			t.Fatalf("request = %s / %s", request.URL, wire)
		}
		return jsonResponse(request, `{
			"id":"modr-provider","model":"provider-moderation-model",
			"results":[{
				"flagged":true,
				"categories":{"violence":true},
				"category_scores":{"violence":0.98},
				"category_applied_input_types":{"violence":["text"]}
			},{"flagged":false,"categories":{"violence":false},"category_scores":{"violence":0.01}}]
		}`), nil
	})}
	adapter, err := openaicapabilities.New(openaicapabilities.Config{
		BaseURL: "https://provider.test/v1", APIKey: "provider-key", Model: "provider-moderation-model", HTTPClient: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := adapter.Moderate(context.Background(), core.ModerationRequest{Model: "public-model", Input: []string{"first", "second"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != "modr-provider" || result.Model != "public-model" || len(result.Results) != 2 || !result.Results[0].Flagged ||
		!result.Results[0].Categories["violence"] || result.Results[0].CategoryScores["violence"] != 0.98 ||
		result.Results[0].CategoryAppliedInputTypes["violence"][0] != "text" {
		t.Fatalf("result = %#v", result)
	}
	if len(result.ProviderUsage) != 0 || result.InputUnits != 0 {
		t.Fatalf("adapter invented moderation usage: %s / %d", result.ProviderUsage, result.InputUnits)
	}
}

func TestRerankUsesConfiguredPathAndNormalizesTokenEvidence(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		wire := string(body)
		if request.URL.String() != "https://provider.test/v1/rank" {
			t.Fatalf("URL = %s", request.URL)
		}
		for _, expected := range []string{
			`"model":"provider-rerank-model"`, `"query":"apple"`, `"documents":["apple tree","ocean"]`,
			`"top_n":1`, `"return_documents":true`,
		} {
			if !strings.Contains(wire, expected) {
				t.Fatalf("request body missing %s: %s", expected, wire)
			}
		}
		return jsonResponse(request, `{
			"id":"rerank-provider","model":"provider-rerank-model",
			"results":[{"index":0,"relevance_score":0.91,"document":{"text":"apple tree"}}],
			"usage":{"total_tokens":13}
		}`), nil
	})}
	adapter, err := openaicapabilities.New(openaicapabilities.Config{
		BaseURL: "https://provider.test/v1", APIKey: "provider-key", Model: "provider-rerank-model",
		RerankPath: "/rank", HTTPClient: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	topN := 1
	result, err := adapter.Rerank(context.Background(), core.RerankRequest{
		Model: "public-model", Query: "apple",
		Documents: []core.RerankDocument{{Text: "apple tree"}, {Text: "ocean"}}, TopN: &topN, ReturnDocuments: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != "rerank-provider" || result.Model != "public-model" || result.Documents != 2 || result.ProviderTokens != 13 ||
		len(result.Results) != 1 || result.Results[0].Index != 0 || result.Results[0].Document == nil {
		t.Fatalf("result = %#v", result)
	}
	if string(result.ProviderUsage) != `{"total_tokens":13}` {
		t.Fatalf("provider usage = %s", result.ProviderUsage)
	}
}

func TestProviderStatusClassifiesOnlyServerFailuresAsAmbiguous(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name               string
		status             int
		sideEffectPossible bool
	}{{"client rejection", http.StatusBadRequest, false}, {"server failure", http.StatusBadGateway, true}} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			adapter, err := openaicapabilities.New(openaicapabilities.Config{
				BaseURL: "https://provider.test/v1", APIKey: "key", Model: "model",
				HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: test.status, Body: io.NopCloser(strings.NewReader(`{}`)), Request: request}, nil
				})},
			})
			if err != nil {
				t.Fatal(err)
			}
			input := "a"
			_, err = adapter.Embed(context.Background(), core.EmbeddingRequest{Model: "model", Input: []core.EmbeddingInput{{Text: &input}}})
			if err == nil || provider.SideEffectPossible(err) != test.sideEffectPossible {
				t.Fatalf("error/side-effect = %v/%v", err, provider.SideEffectPossible(err))
			}
		})
	}
}

func TestProviderRequestTimeoutIsBoundedAndAmbiguous(t *testing.T) {
	t.Parallel()
	adapter, err := openaicapabilities.New(openaicapabilities.Config{
		BaseURL: "https://provider.test/v1", APIKey: "key", Model: "model", RequestTimeout: 10 * time.Millisecond,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			<-request.Context().Done()
			return nil, request.Context().Err()
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := "a"
	_, err = adapter.Embed(context.Background(), core.EmbeddingRequest{Model: "model", Input: []core.EmbeddingInput{{Text: &input}}})
	if !errors.Is(err, context.DeadlineExceeded) || !provider.SideEffectPossible(err) {
		t.Fatalf("error/side-effect = %v/%v", err, provider.SideEffectPossible(err))
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func jsonResponse(request *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(body)), Request: request,
	}
}
