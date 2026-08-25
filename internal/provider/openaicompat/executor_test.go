package openaicompat_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/provider/openaicompat"
)

func TestExecutorSerializesTypedMultimodalContent(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(body["messages"])
		for _, expected := range []string{`"type":"text"`, `"type":"image_url"`, `"url":"https://example.test/image.png"`, `"detail":"low"`} {
			if !strings.Contains(string(encoded), expected) {
				t.Fatalf("messages missing %s: %s", expected, encoded)
			}
		}
		stream := "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":0,\"total_tokens\":1}}\n\n"
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream)), Request: request}, nil
	})}
	executor, err := openaicompat.New(openaicompat.Config{BaseURL: "https://provider.test/v1", Model: "upstream", HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := executor.Execute(context.Background(), core.Request{Input: []core.Item{{
		Type: "message", Role: "user", Content: []core.Content{
			{Type: "input_text", Text: "describe"},
			{Type: "input_image", ImageURL: "https://example.test/image.png", Detail: "low"},
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
}

func TestExecutorPreservesStreamedFunctionCallAndUsage(t *testing.T) {
	t.Parallel()

	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		body := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"weather\",\"arguments\":\"{\\\"city\\\":\"}}]}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"LA\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":4,\"total_tokens\":14,\"prompt_tokens_details\":{\"cached_tokens\":8}}}\n\n" +
			"data: [DONE]\n\n"
		return &http.Response{
			StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(body)), Request: request,
		}, nil
	})}

	executor, err := openaicompat.New(openaicompat.Config{BaseURL: "https://provider.test/v1", Model: "upstream-model", HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := executor.Execute(context.Background(), core.Request{
		Input: []core.Item{{Type: "message", Role: "user", Content: []core.Content{{Type: "input_text", Text: "weather?"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	toolEvent, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if toolEvent.Item == nil || toolEvent.Item.Type != "function_call" || toolEvent.Item.Name != "weather" || string(toolEvent.Item.Arguments) != `{"city":"LA"}` {
		t.Fatalf("tool event = %#v", toolEvent)
	}
	usageEvent, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if usageEvent.Usage == nil || usageEvent.Usage.CachedInputTokens != 8 || usageEvent.Usage.TotalTokens != 14 {
		t.Fatalf("usage event = %#v", usageEvent)
	}
}

func TestProviderDialectsSerializeOfficialCompatibilityFields(t *testing.T) {
	t.Parallel()

	deepSeekUserID := hmac.New(sha256.New, []byte("tenant-a"))
	_, _ = deepSeekUserID.Write([]byte("customer@example.com:42"))
	tests := []struct {
		name              string
		dialect           openaicompat.Dialect
		maxTokensField    string
		userField         string
		userValue         string
		clientHeaderValue string
	}{
		{name: "OpenAI", dialect: openaicompat.DialectOpenAI, maxTokensField: "max_completion_tokens", userField: "user", userValue: "customer@example.com:42"},
		{name: "DeepSeek", dialect: openaicompat.DialectDeepSeek, maxTokensField: "max_tokens", userField: "user_id", userValue: fmt.Sprintf("gw_%x", deepSeekUserID.Sum(nil))},
		{name: "Gemini", dialect: openaicompat.DialectGemini, maxTokensField: "max_completion_tokens", userField: "user", userValue: "customer@example.com:42", clientHeaderValue: "llm-gateway/0.1.0"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.URL.Path != "/v1/chat/completions" {
					t.Fatalf("path = %q", request.URL.Path)
				}
				if got := request.Header.Get("Authorization"); got != "Bearer fake-key" {
					t.Fatalf("Authorization = %q", got)
				}
				if got := request.Header.Get("x-goog-api-client"); got != test.clientHeaderValue {
					t.Fatalf("x-goog-api-client = %q, want %q", got, test.clientHeaderValue)
				}
				var body map[string]any
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if body[test.maxTokensField] != float64(16) {
					t.Fatalf("%s = %#v", test.maxTokensField, body[test.maxTokensField])
				}
				otherMaxField := "max_tokens"
				if test.maxTokensField == otherMaxField {
					otherMaxField = "max_completion_tokens"
				}
				if _, exists := body[otherMaxField]; exists {
					t.Fatalf("unexpected %s in %#v", otherMaxField, body)
				}
				if body[test.userField] != test.userValue {
					t.Fatalf("%s = %#v", test.userField, body[test.userField])
				}
				if test.userField == "user_id" {
					if _, exists := body["user"]; exists {
						t.Fatalf("DeepSeek payload must use user_id: %#v", body)
					}
					thinking, ok := body["thinking"].(map[string]any)
					if !ok || thinking["type"] != "disabled" {
						t.Fatalf("DeepSeek thinking = %#v", body["thinking"])
					}
				}
				streamOptions, ok := body["stream_options"].(map[string]any)
				if !ok || streamOptions["include_usage"] != true {
					t.Fatalf("stream_options = %#v", body["stream_options"])
				}
				usage := "{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}"
				if test.dialect == openaicompat.DialectDeepSeek {
					usage = "{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2,\"prompt_cache_hit_tokens\":1}"
				}
				stream := "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n" +
					"data: {\"choices\":[],\"usage\":" + usage + "}\n\n" +
					"data: [DONE]\n\n"
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream)), Request: request}, nil
			})}
			maxTokens := 16
			executor, err := openaicompat.New(openaicompat.Config{
				BaseURL: "https://provider.test/v1", APIKey: "fake-key", Model: "test-model",
				Dialect: test.dialect, HTTPClient: client,
			})
			if err != nil {
				t.Fatal(err)
			}
			stream, err := executor.Execute(context.Background(), core.Request{
				TenantID:        "tenant-a",
				Input:           []core.Item{{Type: "message", Role: "user", Content: []core.Content{{Type: "input_text", Text: "hello"}}}},
				MaxOutputTokens: &maxTokens, EndUserID: "customer@example.com:42",
			})
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			if event, err := stream.Recv(); err != nil || event.Delta != "ok" {
				t.Fatalf("delta = %#v, err = %v", event, err)
			}
			usageEvent, err := stream.Recv()
			if err != nil || usageEvent.Usage == nil || usageEvent.Usage.TotalTokens != 2 {
				t.Fatalf("usage = %#v, err = %v", usageEvent, err)
			}
			expectedCached := int64(0)
			if test.dialect == openaicompat.DialectDeepSeek {
				expectedCached = 1
			}
			if usageEvent.Usage.CachedInputTokens != expectedCached {
				t.Fatalf("cached input tokens = %d", usageEvent.Usage.CachedInputTokens)
			}
		})
	}
}

func TestExecutorPreservesContentWhenUsageSharesTheSameChunk(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := "data: {\"choices\":[{\"delta\":{\"content\":\"combined\"}}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\n" +
			"data: [DONE]\n\n"
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})}
	executor, err := openaicompat.New(openaicompat.Config{
		BaseURL: "https://provider.test/v1", Model: "test-model", Dialect: openaicompat.DialectGemini, HTTPClient: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := executor.Execute(context.Background(), core.Request{Input: []core.Item{{
		Type: "message", Role: "user", Content: []core.Content{{Type: "input_text", Text: "hello"}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if event, err := stream.Recv(); err != nil || event.Delta != "combined" {
		t.Fatalf("content = %#v, err = %v", event, err)
	}
	if event, err := stream.Recv(); err != nil || event.Usage == nil || event.Usage.TotalTokens != 3 {
		t.Fatalf("usage = %#v, err = %v", event, err)
	}
}

func TestExecutorDoesNotCompleteWhenInterimUsageStreamDisconnectsBeforeDone(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\n"
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})}
	executor, err := openaicompat.New(openaicompat.Config{
		BaseURL: "https://provider.test/v1", Model: "test-model", Dialect: openaicompat.DialectGemini, HTTPClient: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := executor.Execute(context.Background(), core.Request{Input: []core.Item{{
		Type: "message", Role: "user", Content: []core.Content{{Type: "input_text", Text: "hello"}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if event, err := stream.Recv(); err != nil || event.Delta != "partial" {
		t.Fatalf("content = %#v, err = %v", event, err)
	}
	if event, err := stream.Recv(); err != io.EOF || event.Type == "response.completed" {
		t.Fatalf("terminal event = %#v, err = %v", event, err)
	}
}

func TestDeepSeekRejectsUnexpectedPrivateReasoningContent(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"private chain\"}}]}\n\n"
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})}
	executor, err := openaicompat.New(openaicompat.Config{
		BaseURL: "https://provider.test/v1", Model: "test-model", Dialect: openaicompat.DialectDeepSeek, HTTPClient: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := executor.Execute(context.Background(), core.Request{Input: []core.Item{{
		Type: "message", Role: "user", Content: []core.Content{{Type: "input_text", Text: "hello"}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if _, err := stream.Recv(); err == nil || !strings.Contains(err.Error(), "reasoning_content") {
		t.Fatalf("error = %v", err)
	}
}

func TestExecutorRejectsUnknownDialect(t *testing.T) {
	t.Parallel()
	_, err := openaicompat.New(openaicompat.Config{
		BaseURL: "https://provider.test/v1", Model: "test-model", Dialect: "unknown",
	})
	if err == nil || !strings.Contains(err.Error(), "dialect") {
		t.Fatalf("error = %v", err)
	}
}

func TestExecutorRejectsRedirectsWithoutFollowingProviderCredentials(t *testing.T) {
	t.Parallel()

	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.Host == "attacker.test" {
			t.Fatalf("provider request followed a cross-origin redirect: %#v", request.Header)
		}
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": []string{"https://attacker.test/chat/completions"}},
			Body:       io.NopCloser(strings.NewReader("redirect")),
			Request:    request,
		}, nil
	})}
	executor, err := openaicompat.New(openaicompat.Config{
		BaseURL: "https://provider.test/v1", APIKey: "secret", Model: "test-model", HTTPClient: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = executor.Execute(context.Background(), core.Request{Input: []core.Item{{
		Type: "message", Role: "user", Content: []core.Content{{Type: "input_text", Text: "hello"}},
	}}})
	if err == nil || !strings.Contains(err.Error(), "status 302") {
		t.Fatalf("Execute() error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("HTTP calls = %d, want 1", calls)
	}
}

func TestExecutorReturnsProviderFailuresWithoutRetrying(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusNotFound} {
		status := status
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			calls := 0
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{
					StatusCode: status, Body: io.NopCloser(strings.NewReader(`{"error":"conformance"}`)), Request: request,
				}, nil
			})}
			executor, err := openaicompat.New(openaicompat.Config{
				BaseURL: "https://provider.test/v1", APIKey: "secret", Model: "test-model", HTTPClient: client,
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = executor.Execute(context.Background(), core.Request{Input: []core.Item{{
				Type: "message", Role: "user", Content: []core.Content{{Type: "input_text", Text: "hello"}},
			}}})
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("status %d", status)) {
				t.Fatalf("Execute() error = %v", err)
			}
			if calls != 1 {
				t.Fatalf("HTTP calls = %d, want no adapter retry", calls)
			}
		})
	}
}

func TestExecutorPropagatesRequestCancellationWithoutRetrying(t *testing.T) {
	t.Parallel()

	calls := 0
	entered := make(chan struct{})
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		close(entered)
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	executor, err := openaicompat.New(openaicompat.Config{
		BaseURL: "https://provider.test/v1", APIKey: "secret", Model: "test-model", HTTPClient: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() {
		_, executeErr := executor.Execute(ctx, core.Request{Input: []core.Item{{
			Type: "message", Role: "user", Content: []core.Content{{Type: "input_text", Text: "hello"}},
		}}})
		finished <- executeErr
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("provider transport was not entered")
	}
	cancel()
	select {
	case err = <-finished:
	case <-time.After(time.Second):
		t.Fatal("provider request did not observe cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute() error = %v, want context cancellation", err)
	}
	if calls != 1 {
		t.Fatalf("HTTP calls = %d, want no adapter retry", calls)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
