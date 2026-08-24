package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/httpapi"
	"github.com/toddzheng/llm-gateway/internal/provider"
	"github.com/toddzheng/llm-gateway/internal/runtime"
	"github.com/toddzheng/llm-gateway/internal/store"
)

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
	if strings.Join(request.RequestedFeatures, ",") != "text,tools,sampling" {
		t.Fatalf("requested features = %#v", request.RequestedFeatures)
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
			"tools": provider.CapabilityNative, "sampling": provider.CapabilityNative,
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
