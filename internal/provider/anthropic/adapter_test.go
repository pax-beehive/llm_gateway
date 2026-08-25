package anthropic_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/provider/anthropic"
)

func TestAdapterSharesExactSerializationAcrossStreamingAndCacheRefresh(t *testing.T) {
	t.Parallel()

	transport := &anthropicTransport{}
	adapter, err := anthropic.NewAdapter(anthropic.AdapterConfig{
		BaseURL: "https://api.anthropic.test/v1", APIKey: "test-key", APIVersion: "2023-06-01",
		Model: "claude-test", RouteID: "anthropic-us", CredentialScope: "tenant-primary", Region: "us-west",
		TTL: 5 * time.Minute, CacheWritePerMillionMicros: 12_500_000,
		EnablePromptCaching: true,
		HTTPClient:          &http.Client{Transport: transport},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := core.Request{
		TenantID: "tenant-a", EndUserID: "customer-user-42", ContextItemCount: 1,
		Input: []core.Item{
			{Type: "message", Role: "system", Content: []core.Content{{Type: "input_text", Text: "stable system"}}},
			{Type: "message", Role: "user", Content: []core.Content{{Type: "input_text", Text: "question"}}},
		},
	}
	stream, err := adapter.Execute(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	delta, err := stream.Recv()
	if err != nil || delta.Delta != "answer" {
		t.Fatalf("delta = %#v, err = %v", delta, err)
	}
	completed, err := stream.Recv()
	if err != nil || completed.Usage == nil || completed.Usage.CachedInputTokens != 90 || completed.Usage.InputTokens != 100 {
		t.Fatalf("completed event = %#v, err = %v", completed, err)
	}

	response := core.Response{Output: []core.Item{{
		Type: "message", Role: "assistant", Content: []core.Content{{Type: "output_text", Text: "answer"}},
	}}, Usage: *completed.Usage}
	observation, err := adapter.BuildCacheAnchor(context.Background(), request, response)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Anchor.RouteID != "anthropic-us" || observation.Anchor.PrefixHash == "" || observation.RefreshCostMicros != 1_125 {
		t.Fatalf("observation = %#v", observation)
	}
	if capability := adapter.Inspect(context.Background(), observation.Anchor); !capability.Supported {
		t.Fatalf("refresh recipe rejected: %s", capability.Reason)
	}
	result, err := adapter.Refresh(context.Background(), observation.Anchor)
	if err != nil || result.Status != "succeeded" || !result.UsageReliable || !result.ExpiresAt.After(time.Now()) || result.Usage.InputTokens != 100 {
		t.Fatalf("refresh = %#v, err = %v", result, err)
	}

	transport.mu.Lock()
	defer transport.mu.Unlock()
	if len(transport.requests) != 2 {
		t.Fatalf("provider requests = %d, want execute + refresh", len(transport.requests))
	}
	var live, refresh map[string]any
	if json.Unmarshal(transport.requests[0], &live) != nil || json.Unmarshal(transport.requests[1], &refresh) != nil {
		t.Fatal("requests are not JSON")
	}
	if live["stream"] != true || refresh["stream"] != false || refresh["max_tokens"] != float64(0) {
		t.Fatalf("live/refresh transport modes = %#v / %#v", live, refresh)
	}
	if !strings.Contains(string(transport.requests[0]), `"cache_control"`) || !strings.Contains(string(transport.requests[1]), `"cache_control"`) {
		t.Fatal("live and refresh payloads must share explicit cache breakpoints")
	}
	if !strings.Contains(string(transport.requests[0]), `"metadata":{"user_id":"customer-user-42"}`) {
		t.Fatalf("live request dropped end-user identity: %s", transport.requests[0])
	}
}

func TestAdapterDoesNotInventCacheLeaseWithoutProviderUsageEvidence(t *testing.T) {
	t.Parallel()
	adapter, err := anthropic.NewAdapter(anthropic.AdapterConfig{
		BaseURL: "https://api.anthropic.test/v1", APIKey: "test-key", Model: "claude-test",
		RouteID: "anthropic-us", CredentialScope: "tenant-primary", Region: "us-west",
		EnablePromptCaching: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := core.Request{TenantID: "tenant-a", Input: []core.Item{
		{Type: "message", Role: "system", Content: []core.Content{{Type: "input_text", Text: "short system"}}},
		{Type: "message", Role: "user", Content: []core.Content{{Type: "input_text", Text: "question"}}},
	}}
	if _, err := adapter.BuildCacheAnchor(context.Background(), request, core.Response{}); err == nil {
		t.Fatal("expected missing provider cache evidence to reject lease creation")
	}
}

func TestAdapterExecutesOfficialMessagesProtocolWithCachingDisabled(t *testing.T) {
	t.Parallel()

	client := &http.Client{Transport: anthropicRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/v1/messages" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		if got := request.Header.Get("x-api-key"); got != "fake-key" {
			t.Fatalf("x-api-key = %q", got)
		}
		if got := request.Header.Get("anthropic-version"); got != "2023-06-01" {
			t.Fatalf("anthropic-version = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != "claude-test" || body["stream"] != true || body["max_tokens"] != float64(16) {
			t.Fatalf("payload = %#v", body)
		}
		encoded, _ := json.Marshal(body)
		if strings.Contains(string(encoded), "cache_control") {
			t.Fatalf("refresh-disabled route emitted cache_control: %s", encoded)
		}
		stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3}}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":1}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream)), Request: request}, nil
	})}
	adapter, err := anthropic.NewAdapter(anthropic.AdapterConfig{
		BaseURL: "https://api.anthropic.test/v1", APIKey: "fake-key", APIVersion: "2023-06-01",
		Model: "claude-test", RouteID: "anthropic-test", CredentialScope: "test", Region: "test",
		EnablePromptCaching: false, HTTPClient: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	maxTokens := 16
	stream, err := adapter.Execute(context.Background(), core.Request{
		Input:           []core.Item{{Type: "message", Role: "user", Content: []core.Content{{Type: "input_text", Text: "hello"}}}},
		MaxOutputTokens: &maxTokens,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if event, err := stream.Recv(); err != nil || event.Delta != "ok" {
		t.Fatalf("delta = %#v, err = %v", event, err)
	}
	if event, err := stream.Recv(); err != nil || event.Usage == nil || event.Usage.TotalTokens != 4 {
		t.Fatalf("usage = %#v, err = %v", event, err)
	}
}

func TestAdapterSerializesTypedImageInput(t *testing.T) {
	t.Parallel()

	client := &http.Client{Transport: anthropicRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(body["messages"])
		for _, expected := range []string{`"type":"image"`, `"type":"url"`, `"url":"https://example.test/image.png"`} {
			if !strings.Contains(string(encoded), expected) {
				t.Fatalf("messages missing %s: %s", expected, encoded)
			}
		}
		stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3}}}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":1}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream)), Request: request}, nil
	})}
	adapter, err := anthropic.NewAdapter(anthropic.AdapterConfig{
		BaseURL: "https://api.anthropic.test/v1", APIKey: "secret", Model: "claude-test",
		RouteID: "anthropic-test", CredentialScope: "test", Region: "test", HTTPClient: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := adapter.Execute(context.Background(), core.Request{Input: []core.Item{{
		Type: "message", Role: "user", Content: []core.Content{
			{Type: "input_text", Text: "describe"},
			{Type: "input_image", ImageURL: "https://example.test/image.png"},
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if event, err := stream.Recv(); err != nil || event.Usage == nil || event.Usage.TotalTokens != 4 {
		t.Fatalf("completion = %#v, err = %v", event, err)
	}
}

func TestAdapterRejectsRedirectsWithoutFollowingProviderCredentials(t *testing.T) {
	t.Parallel()

	calls := 0
	client := &http.Client{Transport: anthropicRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.Host == "attacker.test" {
			t.Fatalf("Anthropic request followed a cross-origin redirect: %#v", request.Header)
		}
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": []string{"https://attacker.test/messages"}},
			Body:       io.NopCloser(strings.NewReader("redirect")),
			Request:    request,
		}, nil
	})}
	adapter, err := anthropic.NewAdapter(anthropic.AdapterConfig{
		BaseURL: "https://api.anthropic.test/v1", APIKey: "secret", Model: "claude-test",
		RouteID: "anthropic-test", CredentialScope: "test", Region: "test", HTTPClient: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.Execute(context.Background(), core.Request{Input: []core.Item{{
		Type: "message", Role: "user", Content: []core.Content{{Type: "input_text", Text: "hello"}},
	}}})
	if err == nil || !strings.Contains(err.Error(), "status 302") {
		t.Fatalf("Execute() error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("HTTP calls = %d, want 1", calls)
	}
}

func TestAdapterReturnsProviderFailuresWithoutRetrying(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusNotFound} {
		status := status
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			calls := 0
			client := &http.Client{Transport: anthropicRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{
					StatusCode: status, Body: io.NopCloser(strings.NewReader(`{"error":"conformance"}`)), Request: request,
				}, nil
			})}
			adapter, err := anthropic.NewAdapter(anthropic.AdapterConfig{
				BaseURL: "https://api.anthropic.test/v1", APIKey: "secret", Model: "claude-test",
				RouteID: "anthropic-test", CredentialScope: "test", Region: "test", HTTPClient: client,
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = adapter.Execute(context.Background(), core.Request{Input: []core.Item{{
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

type anthropicTransport struct {
	mu       sync.Mutex
	requests [][]byte
}

type anthropicRoundTripFunc func(*http.Request) (*http.Response, error)

func (f anthropicRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func (t *anthropicTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(request.Body)
	t.mu.Lock()
	t.requests = append(t.requests, body)
	t.mu.Unlock()
	if strings.Contains(string(body), `"stream":true`) {
		stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"cache_read_input_tokens\":90,\"cache_creation_input_tokens\":0}}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"answer\"}}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":2}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream)), Request: request}, nil
	}
	refresh := `{"content":[],"stop_reason":"max_tokens","usage":{"input_tokens":10,"output_tokens":0,"cache_read_input_tokens":90,"cache_creation_input_tokens":0}}`
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(refresh)), Request: request}, nil
}
