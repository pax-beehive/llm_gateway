package openairesponses_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/provider/openairesponses"
)

func TestExecutorPreservesCodexResponsesFieldsAndNormalizesEvents(t *testing.T) {
	t.Parallel()
	zero := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://api.openai.test/v1/responses" {
			t.Fatalf("URL = %s", request.URL)
		}
		if request.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatalf("Authorization = %q", request.Header.Get("Authorization"))
		}
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload["model"] != "gpt-test" || payload["store"] != false || payload["stream"] != true || payload["prompt_cache_key"] != "thread-1" {
			t.Fatalf("payload = %#v", payload)
		}
		if _, ok := payload["reasoning"].(map[string]any); !ok || payload["parallel_tool_calls"] != true {
			t.Fatalf("Codex fields = %#v", payload)
		}
		input := payload["input"].([]any)
		if _, exists := input[0].(map[string]any)["id"]; exists {
			t.Fatalf("Gateway persistence ID leaked to provider: %#v", input[0])
		}
		if input[1].(map[string]any)["arguments"] != `{"path":"state.txt"}` {
			t.Fatalf("function arguments = %#v", input[1])
		}
		if input[1].(map[string]any)["id"] != "fc-upstream" {
			t.Fatalf("provider-originated ID was not preserved: %#v", input[1])
		}
		body := strings.Join([]string{
			`event: response.created`, `data: {"type":"response.created","response":{"id":"upstream"}}`, ``,
			`event: response.output_text.delta`, `data: {"type":"response.output_text.delta","item_id":"msg-1","output_index":0,"content_index":0,"delta":"working"}`, ``,
			`event: response.output_item.done`, `data: {"type":"response.output_item.done","output_index":1,"item":{"id":"rs-1","type":"reasoning","status":"completed","summary":[],"encrypted_content":"sealed"}}`, ``,
			`event: response.output_item.done`, `data: {"type":"response.output_item.done","output_index":2,"item":{"id":"fc-1","type":"function_call","status":"completed","call_id":"call-1","name":"exec_command","arguments":"{\"cmd\":\"pwd\"}"}}`, ``,
			`event: response.completed`, `data: {"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":3,"total_tokens":13,"input_tokens_details":{"cached_tokens":4}}}}`, ``,
		}, "\n")
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})}
	executor, err := openairesponses.New(openairesponses.Config{
		BaseURL: "https://api.openai.test/v1", APIKey: "test-key", Model: "gpt-test", HTTPClient: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	parallel := true
	stream, err := executor.Execute(context.Background(), core.Request{
		Input: []core.Item{
			{ID: "item_gateway-internal", Type: "message", Role: "user", Content: []core.Content{{Type: "input_text", Text: "inspect"}}},
			{ID: "fc-upstream", Type: "function_call", CallID: "old-call", Name: "read", Arguments: json.RawMessage(`{"path":"state.txt"}`)},
		},
		Tools:      []json.RawMessage{json.RawMessage(`{"type":"function","name":"exec_command","parameters":{"type":"object"}}`)},
		ToolChoice: json.RawMessage(`"auto"`), ParallelToolCalls: &parallel,
		Reasoning: json.RawMessage(`{"summary":"auto"}`), Include: []string{"reasoning.encrypted_content"},
		PromptCacheKey: "thread-1", ClientMetadata: json.RawMessage(`{"thread_id":"thread-1"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	events := make([]core.Event, 0, 4)
	for {
		event, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			t.Fatal(recvErr)
		}
		events = append(events, event)
	}
	if len(events) != 4 || events[0].Delta != "working" || events[0].OutputIndex == nil || *events[0].OutputIndex != zero {
		t.Fatalf("events = %#v", events)
	}
	if events[1].Item == nil || events[1].Item.Type != "reasoning" || events[1].Item.EncryptedContent != "sealed" {
		t.Fatalf("reasoning event = %#v", events[1])
	}
	if events[2].Item == nil || events[2].Item.Name != "exec_command" || string(events[2].Item.Arguments) != `{"cmd":"pwd"}` {
		t.Fatalf("function event = %#v", events[2])
	}
	if events[3].Usage == nil || events[3].Usage.CachedInputTokens != 4 || events[3].Usage.TotalTokens != 13 {
		t.Fatalf("usage event = %#v", events[3])
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }
