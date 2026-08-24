package openaicompat_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/provider/openaicompat"
)

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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
