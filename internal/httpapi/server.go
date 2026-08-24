package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/runtime"
	"github.com/toddzheng/llm-gateway/internal/store"
)

type Authenticator interface {
	Authenticate(*http.Request) (string, error)
}

type StaticAuthenticator map[string]string

func (a StaticAuthenticator) Authenticate(request *http.Request) (string, error) {
	value := request.Header.Get("Authorization")
	token, ok := strings.CutPrefix(value, "Bearer ")
	if !ok || token == "" {
		return "", errors.New("missing bearer token")
	}
	tenantID, ok := a[token]
	if !ok || tenantID == "" {
		return "", errors.New("invalid bearer token")
	}
	return tenantID, nil
}

type Config struct {
	Runtime               *runtime.Runtime
	Authenticator         Authenticator
	TenantHomeRegions     map[string]string
	TenantExecutionEpochs map[string]int64
}

type Server struct {
	runtime         *runtime.Runtime
	authenticator   Authenticator
	homeRegions     map[string]string
	executionEpochs map[string]int64
	mux             *http.ServeMux
}

func New(config Config) http.Handler {
	server := &Server{
		runtime: config.Runtime, authenticator: config.Authenticator, homeRegions: config.TenantHomeRegions,
		executionEpochs: config.TenantExecutionEpochs, mux: http.NewServeMux(),
	}
	server.mux.HandleFunc("POST /v1/responses", server.createResponse)
	server.mux.HandleFunc("GET /v1/responses/{response_id}", server.getResponse)
	server.mux.HandleFunc("DELETE /v1/responses/{response_id}", server.deleteResponse)
	server.mux.HandleFunc("POST /v1/responses/{response_id}/cancel", server.cancelResponse)
	server.mux.HandleFunc("GET /v1/responses/{response_id}/input_items", server.inputItems)
	server.mux.HandleFunc("POST /v1/chat/completions", server.chatCompletions)
	server.mux.HandleFunc("POST /v1/conversations", server.createConversation)
	server.mux.HandleFunc("GET /v1/conversations/{conversation_id}", server.getConversation)
	server.mux.HandleFunc("DELETE /v1/conversations/{conversation_id}", server.deleteConversation)
	server.mux.HandleFunc("GET /v1/conversations/{conversation_id}/items", server.conversationItems)
	server.mux.HandleFunc("POST /v1/conversations/{conversation_id}/items", server.appendConversationItems)
	return server
}

func (s *Server) ServeHTTP(responseWriter http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodGet && request.URL.Path == "/healthz" {
		writeJSON(responseWriter, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	tenantID, err := s.authenticator.Authenticate(request)
	if err != nil {
		writeError(responseWriter, http.StatusUnauthorized, "authentication_error", err.Error(), "")
		return
	}
	request.Header.Set("X-Authenticated-Tenant-ID", tenantID)
	s.mux.ServeHTTP(responseWriter, request)
}

type createResponseRequest struct {
	Model              string                      `json:"model"`
	Input              json.RawMessage             `json:"input"`
	Instructions       string                      `json:"instructions,omitempty"`
	Stream             bool                        `json:"stream"`
	Background         bool                        `json:"background"`
	Store              *bool                       `json:"store"`
	PreviousResponseID string                      `json:"previous_response_id"`
	Conversation       string                      `json:"conversation"`
	Metadata           map[string]string           `json:"metadata"`
	CompatibilityMode  core.CompatibilityMode      `json:"compatibility_mode"`
	Tools              []json.RawMessage           `json:"tools,omitempty"`
	ToolChoice         json.RawMessage             `json:"tool_choice,omitempty"`
	Temperature        *float64                    `json:"temperature,omitempty"`
	TopP               *float64                    `json:"top_p,omitempty"`
	MaxOutputTokens    *int                        `json:"max_output_tokens,omitempty"`
	Stop               json.RawMessage             `json:"stop,omitempty"`
	User               string                      `json:"user,omitempty"`
	CacheProtection    *core.CacheProtectionPolicy `json:"cache_protection,omitempty"`
}

func (s *Server) createResponse(responseWriter http.ResponseWriter, request *http.Request) {
	var payload createResponseRequest
	if err := decodeBody(request, &payload); err != nil {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", err.Error(), "")
		return
	}
	if payload.Model == "" {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", "model is required", "model")
		return
	}
	if err := validateCompatibilityMode(payload.CompatibilityMode); err != nil {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", err.Error(), "compatibility_mode")
		return
	}
	input, err := decodeInput(payload.Input)
	if err != nil {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", err.Error(), "input")
		return
	}
	if payload.Instructions != "" {
		input = append([]core.Item{{
			Type: "message", Role: "system",
			Content: []core.Content{{Type: "input_text", Text: payload.Instructions}},
		}}, input...)
	}
	storeResponse := true
	if payload.Store != nil {
		storeResponse = *payload.Store
	}
	if !storeResponse && payload.Conversation != "" {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", "store:false cannot mutate a Conversation", "store")
		return
	}
	canonical := core.Request{
		TenantID: request.Header.Get("X-Authenticated-Tenant-ID"), Model: payload.Model, Input: input,
		Stream: payload.Stream, Background: payload.Background, Store: storeResponse,
		PreviousResponseID: payload.PreviousResponseID, ConversationID: payload.Conversation,
		CompatibilityMode: payload.CompatibilityMode, RequestedFeatures: requestedFeatures(input), Metadata: payload.Metadata,
		HomeRegion:      s.homeRegion(request),
		ExecutionEpoch:  s.executionEpoch(request),
		IdempotencyKey:  strings.TrimSpace(request.Header.Get("Idempotency-Key")),
		Tools:           payload.Tools,
		ToolChoice:      payload.ToolChoice,
		Temperature:     payload.Temperature,
		TopP:            payload.TopP,
		MaxOutputTokens: payload.MaxOutputTokens,
		Stop:            payload.Stop,
		EndUserID:       payload.User,
		CacheProtection: payload.CacheProtection,
	}
	if len(payload.Tools) > 0 || hasJSONValue(payload.ToolChoice) {
		canonical.RequestedFeatures = append(canonical.RequestedFeatures, "tools")
	}
	if payload.Temperature != nil || payload.TopP != nil || payload.MaxOutputTokens != nil || hasJSONValue(payload.Stop) {
		canonical.RequestedFeatures = append(canonical.RequestedFeatures, "sampling")
	}
	if err := validateCacheProtection(payload.CacheProtection); err != nil {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", err.Error(), "cache_protection")
		return
	}
	if len(canonical.IdempotencyKey) > 256 {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", "Idempotency-Key exceeds 256 bytes", "Idempotency-Key")
		return
	}
	if canonical.IdempotencyKey != "" {
		if payload.Stream || !storeResponse {
			writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", "Idempotency-Key requires a stored non-streaming Response", "Idempotency-Key")
			return
		}
		canonical.RequestHash = responseRequestHash(payload)
	}
	if payload.Stream {
		canonical.RequestedFeatures = append(canonical.RequestedFeatures, "streaming")
	}
	if payload.Background && payload.Stream {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", "background and stream cannot both be true", "background")
		return
	}
	if payload.Background {
		s.startBackgroundResponse(responseWriter, request, canonical)
		return
	}
	if payload.Stream {
		s.streamResponse(responseWriter, request, canonical)
		return
	}
	result, err := s.runtime.Execute(request.Context(), canonical)
	if err != nil {
		writeRuntimeError(responseWriter, result, err)
		return
	}
	writeJSON(responseWriter, http.StatusOK, result)
}

func (s *Server) startBackgroundResponse(responseWriter http.ResponseWriter, request *http.Request, canonical core.Request) {
	created := make(chan core.Response, 1)
	finished := make(chan struct{})
	backgroundContext := context.WithoutCancel(request.Context())
	go func() {
		defer close(finished)
		_, _ = s.runtime.ExecuteStreaming(backgroundContext, canonical, func(event core.Event) error {
			if event.Type == "response.created" && event.Response != nil {
				select {
				case created <- *event.Response:
				default:
				}
			}
			return nil
		})
	}()
	select {
	case response := <-created:
		writeJSON(responseWriter, http.StatusOK, response)
	case <-finished:
		select {
		case response := <-created:
			writeJSON(responseWriter, http.StatusOK, response)
		default:
			writeError(responseWriter, http.StatusBadGateway, "background_start_failed", "background response failed before durable creation", "")
		}
	case <-request.Context().Done():
		writeError(responseWriter, http.StatusRequestTimeout, "request_cancelled", request.Context().Err().Error(), "")
	}
}

func (s *Server) streamResponse(responseWriter http.ResponseWriter, request *http.Request, canonical core.Request) {
	flusher, ok := responseWriter.(http.Flusher)
	if !ok {
		writeError(responseWriter, http.StatusInternalServerError, "streaming_unsupported", "response writer does not support streaming", "")
		return
	}
	responseWriter.Header().Set("Content-Type", "text/event-stream")
	responseWriter.Header().Set("Cache-Control", "no-cache, no-transform")
	responseWriter.Header().Set("Connection", "keep-alive")
	responseWriter.Header().Set("X-Accel-Buffering", "no")
	responseWriter.WriteHeader(http.StatusOK)
	flusher.Flush()

	result, err := s.runtime.ExecuteStreaming(request.Context(), canonical, func(event core.Event) error {
		if event.Type == "gateway.keepalive" {
			_, writeErr := io.WriteString(responseWriter, ": keepalive\n\n")
			flusher.Flush()
			return writeErr
		}
		return writeNamedSSE(responseWriter, event.Type, event, flusher)
	})
	if err != nil && result.Error != nil {
		_ = writeNamedSSE(responseWriter, "response.failed", core.Event{Type: "response.failed", Error: result.Error, Response: &result}, flusher)
	}
}

func (s *Server) getResponse(responseWriter http.ResponseWriter, request *http.Request) {
	result, err := s.runtime.Get(request.Context(), tenantID(request), request.PathValue("response_id"))
	if err != nil {
		writeStoreError(responseWriter, err)
		return
	}
	writeJSON(responseWriter, http.StatusOK, result)
}

func (s *Server) deleteResponse(responseWriter http.ResponseWriter, request *http.Request) {
	responseID := request.PathValue("response_id")
	if err := s.runtime.Delete(request.Context(), tenantID(request), responseID); err != nil {
		writeStoreError(responseWriter, err)
		return
	}
	writeJSON(responseWriter, http.StatusOK, map[string]any{"id": responseID, "object": "response.deleted", "deleted": true})
}

func (s *Server) cancelResponse(responseWriter http.ResponseWriter, request *http.Request) {
	result, err := s.runtime.Cancel(request.Context(), tenantID(request), request.PathValue("response_id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeStoreError(responseWriter, err)
			return
		}
		writeError(responseWriter, http.StatusConflict, "invalid_state", err.Error(), "")
		return
	}
	writeJSON(responseWriter, http.StatusOK, result)
}

func (s *Server) inputItems(responseWriter http.ResponseWriter, request *http.Request) {
	items, err := s.runtime.InputItems(request.Context(), tenantID(request), request.PathValue("response_id"))
	if err != nil {
		writeStoreError(responseWriter, err)
		return
	}
	writeJSON(responseWriter, http.StatusOK, map[string]any{"object": "list", "data": items, "has_more": false})
}

type createConversationRequest struct {
	Items    []core.Item       `json:"items,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

func (s *Server) createConversation(responseWriter http.ResponseWriter, request *http.Request) {
	var payload createConversationRequest
	if err := decodeBody(request, &payload); err != nil {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", err.Error(), "")
		return
	}
	if err := validateItems(payload.Items); err != nil {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", err.Error(), "items")
		return
	}
	conversation, err := s.runtime.CreateConversation(
		request.Context(), tenantID(request), s.homeRegion(request), s.executionEpoch(request), payload.Items, payload.Metadata,
	)
	if err != nil {
		writeStoreError(responseWriter, err)
		return
	}
	writeJSON(responseWriter, http.StatusOK, conversation)
}

func (s *Server) getConversation(responseWriter http.ResponseWriter, request *http.Request) {
	conversation, err := s.runtime.GetConversation(request.Context(), tenantID(request), request.PathValue("conversation_id"))
	if err != nil {
		writeStoreError(responseWriter, err)
		return
	}
	writeJSON(responseWriter, http.StatusOK, conversation)
}

type appendConversationItemsRequest struct {
	Items            []core.Item `json:"items"`
	ExpectedRevision int64       `json:"expected_revision"`
}

func (s *Server) appendConversationItems(responseWriter http.ResponseWriter, request *http.Request) {
	var payload appendConversationItemsRequest
	if err := decodeBody(request, &payload); err != nil {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", err.Error(), "")
		return
	}
	if payload.ExpectedRevision <= 0 {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", "expected_revision must be positive", "expected_revision")
		return
	}
	if err := validateItems(payload.Items); err != nil || len(payload.Items) == 0 {
		if err == nil {
			err = errors.New("items cannot be empty")
		}
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", err.Error(), "items")
		return
	}
	conversation, err := s.runtime.AppendConversationItems(
		request.Context(), tenantID(request), request.PathValue("conversation_id"), payload.Items, payload.ExpectedRevision,
	)
	if err != nil {
		writeStoreError(responseWriter, err)
		return
	}
	writeJSON(responseWriter, http.StatusOK, conversation)
}

func (s *Server) conversationItems(responseWriter http.ResponseWriter, request *http.Request) {
	conversation, err := s.runtime.GetConversation(request.Context(), tenantID(request), request.PathValue("conversation_id"))
	if err != nil {
		writeStoreError(responseWriter, err)
		return
	}
	writeJSON(responseWriter, http.StatusOK, map[string]any{
		"object": "list", "data": conversation.Items, "has_more": false, "revision": conversation.Revision,
	})
}

func (s *Server) deleteConversation(responseWriter http.ResponseWriter, request *http.Request) {
	conversation, err := s.runtime.GetConversation(request.Context(), tenantID(request), request.PathValue("conversation_id"))
	if err != nil {
		writeStoreError(responseWriter, err)
		return
	}
	if err := s.runtime.DeleteConversation(request.Context(), tenantID(request), conversation.ID, conversation.Revision); err != nil {
		writeStoreError(responseWriter, err)
		return
	}
	writeJSON(responseWriter, http.StatusOK, map[string]any{"id": conversation.ID, "object": "conversation.deleted", "deleted": true})
}

type chatCompletionRequest struct {
	Model               string                      `json:"model"`
	Messages            []chatMessage               `json:"messages"`
	Stream              bool                        `json:"stream"`
	StreamOptions       *chatStreamOptions          `json:"stream_options,omitempty"`
	Tools               []json.RawMessage           `json:"tools,omitempty"`
	ToolChoice          json.RawMessage             `json:"tool_choice,omitempty"`
	Temperature         *float64                    `json:"temperature,omitempty"`
	TopP                *float64                    `json:"top_p,omitempty"`
	MaxTokens           *int                        `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int                        `json:"max_completion_tokens,omitempty"`
	Stop                json.RawMessage             `json:"stop,omitempty"`
	User                string                      `json:"user,omitempty"`
	Metadata            map[string]string           `json:"metadata,omitempty"`
	CompatibilityMode   core.CompatibilityMode      `json:"compatibility_mode,omitempty"`
	CacheProtection     *core.CacheProtectionPolicy `json:"cache_protection,omitempty"`
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Name       string          `json:"name,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolCalls  []chatToolCall  `json:"tool_calls,omitempty"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

func (s *Server) chatCompletions(responseWriter http.ResponseWriter, request *http.Request) {
	var payload chatCompletionRequest
	if err := decodeBody(request, &payload); err != nil {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", err.Error(), "")
		return
	}
	if payload.Model == "" || len(payload.Messages) == 0 {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", "model and messages are required", "")
		return
	}
	if err := validateCompatibilityMode(payload.CompatibilityMode); err != nil {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", err.Error(), "compatibility_mode")
		return
	}
	items, err := canonicalChatMessages(payload.Messages)
	if err != nil {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", err.Error(), "messages")
		return
	}
	features := requestedFeatures(items)
	if payload.Stream {
		features = append(features, "streaming")
	}
	if len(payload.Tools) > 0 || hasJSONValue(payload.ToolChoice) {
		features = append(features, "tools")
	}
	if payload.Temperature != nil || payload.TopP != nil || payload.MaxTokens != nil || payload.MaxCompletionTokens != nil || hasJSONValue(payload.Stop) {
		features = append(features, "sampling")
	}
	maxOutputTokens := payload.MaxCompletionTokens
	if maxOutputTokens == nil {
		maxOutputTokens = payload.MaxTokens
	}
	canonical := core.Request{
		TenantID: tenantID(request), Model: payload.Model, Input: items, Stream: payload.Stream, Store: true,
		CompatibilityMode: payload.CompatibilityMode, RequestedFeatures: features, Metadata: payload.Metadata,
		HomeRegion: s.homeRegion(request), ExecutionEpoch: s.executionEpoch(request),
		Tools: payload.Tools, ToolChoice: payload.ToolChoice, Temperature: payload.Temperature,
		TopP: payload.TopP, MaxOutputTokens: maxOutputTokens, Stop: payload.Stop, EndUserID: payload.User,
		CacheProtection: payload.CacheProtection,
	}
	if err := validateCacheProtection(payload.CacheProtection); err != nil {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", err.Error(), "cache_protection")
		return
	}
	if payload.Stream {
		s.streamChatCompletion(responseWriter, request, canonical, payload.StreamOptions)
		return
	}
	result, err := s.runtime.Execute(request.Context(), canonical)
	if err != nil {
		writeRuntimeError(responseWriter, result, err)
		return
	}
	writeJSON(responseWriter, http.StatusOK, chatCompletion(result))
}

func (s *Server) streamChatCompletion(responseWriter http.ResponseWriter, request *http.Request, canonical core.Request, options *chatStreamOptions) {
	flusher, ok := responseWriter.(http.Flusher)
	if !ok {
		writeError(responseWriter, http.StatusInternalServerError, "streaming_unsupported", "response writer does not support streaming", "")
		return
	}
	responseWriter.Header().Set("Content-Type", "text/event-stream")
	responseWriter.Header().Set("Cache-Control", "no-cache, no-transform")
	responseWriter.Header().Set("Connection", "keep-alive")
	responseWriter.Header().Set("X-Accel-Buffering", "no")
	responseWriter.WriteHeader(http.StatusOK)
	flusher.Flush()

	var responseID string
	var createdAt int64
	toolCallOutput := false
	toolCallIndex := 0
	emit := func(event core.Event) error {
		if event.Type == "gateway.keepalive" {
			_, err := io.WriteString(responseWriter, ": keepalive\n\n")
			flusher.Flush()
			return err
		}
		if event.Response != nil && responseID == "" {
			responseID = event.Response.ID
			createdAt = event.Response.CreatedAt
		}
		switch event.Type {
		case "response.created":
			return writeSSE(responseWriter, map[string]any{
				"id": responseID, "object": "chat.completion.chunk", "created": createdAt, "model": canonical.Model,
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}},
			}, flusher)
		case "response.output_text.delta":
			return writeSSE(responseWriter, map[string]any{
				"id": responseID, "object": "chat.completion.chunk", "created": createdAt, "model": canonical.Model,
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": event.Delta}, "finish_reason": nil}},
			}, flusher)
		case "response.output_item.done":
			if event.Item == nil || event.Item.Type != "function_call" {
				return nil
			}
			toolCallOutput = true
			index := toolCallIndex
			toolCallIndex++
			return writeSSE(responseWriter, map[string]any{
				"id": responseID, "object": "chat.completion.chunk", "created": createdAt, "model": canonical.Model,
				"choices": []any{map[string]any{
					"index": 0,
					"delta": map[string]any{"tool_calls": []any{map[string]any{
						"index": index, "id": event.Item.CallID, "type": "function",
						"function": map[string]any{"name": event.Item.Name, "arguments": string(event.Item.Arguments)},
					}}},
					"finish_reason": nil,
				}},
			}, flusher)
		case "response.completed":
			finishReason := "stop"
			if toolCallOutput {
				finishReason = "tool_calls"
			}
			chunk := map[string]any{
				"id": responseID, "object": "chat.completion.chunk", "created": createdAt, "model": canonical.Model,
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finishReason}},
			}
			if options != nil && options.IncludeUsage && event.Usage != nil {
				chunk["usage"] = chatUsage(*event.Usage)
			}
			return writeSSE(responseWriter, chunk, flusher)
		}
		return nil
	}
	result, err := s.runtime.ExecuteStreaming(request.Context(), canonical, emit)
	if err != nil && result.Error != nil {
		_ = writeSSE(responseWriter, map[string]any{"error": result.Error}, flusher)
	}
	_, _ = io.WriteString(responseWriter, "data: [DONE]\n\n")
	flusher.Flush()
}

func canonicalChatMessages(messages []chatMessage) ([]core.Item, error) {
	items := make([]core.Item, 0, len(messages))
	for _, message := range messages {
		if message.Role == "" {
			return nil, errors.New("message role is required")
		}
		var text string
		contentPresent := hasJSONValue(message.Content)
		var content []core.Content
		if contentPresent && json.Unmarshal(message.Content, &text) != nil {
			var parts []struct {
				Type     string `json:"type"`
				Text     string `json:"text,omitempty"`
				ImageURL *struct {
					URL    string `json:"url"`
					Detail string `json:"detail,omitempty"`
				} `json:"image_url,omitempty"`
			}
			if err := json.Unmarshal(message.Content, &parts); err != nil || len(parts) == 0 {
				return nil, errors.New("chat message content must be a string or a non-empty content-part array")
			}
			for _, part := range parts {
				switch part.Type {
				case "text":
					if part.Text == "" {
						return nil, errors.New("text content parts require text")
					}
					content = append(content, core.Content{Type: "input_text", Text: part.Text})
				case "image_url":
					if part.ImageURL == nil || part.ImageURL.URL == "" {
						return nil, errors.New("image_url content parts require a URL")
					}
					content = append(content, core.Content{Type: "input_image", ImageURL: part.ImageURL.URL, Detail: part.ImageURL.Detail})
				default:
					return nil, fmt.Errorf("unsupported chat content part type %q", part.Type)
				}
			}
		}
		item := core.Item{Type: "message", Role: message.Role, Name: message.Name, CallID: message.ToolCallID}
		if message.Role == "tool" {
			if !contentPresent || message.ToolCallID == "" {
				return nil, errors.New("tool messages require string content and tool_call_id")
			}
			item.Type = "function_call_output"
			item.Output = text
			items = append(items, item)
		} else if contentPresent {
			if content == nil {
				content = []core.Content{{Type: "input_text", Text: text}}
			}
			item.Content = content
			items = append(items, item)
		} else if len(message.ToolCalls) == 0 {
			return nil, errors.New("chat messages require string content unless assistant tool_calls are present")
		}
		for _, call := range message.ToolCalls {
			arguments := json.RawMessage(call.Function.Arguments)
			if message.Role != "assistant" || call.Type != "function" || call.ID == "" || call.Function.Name == "" || !json.Valid(arguments) {
				return nil, errors.New("assistant tool_calls require id, function name, and JSON arguments")
			}
			items = append(items, core.Item{
				Type: "function_call", CallID: call.ID, Name: call.Function.Name, Arguments: arguments,
			})
		}
	}
	return items, nil
}

func chatCompletion(response core.Response) map[string]any {
	message := map[string]any{"role": "assistant", "content": response.OutputText()}
	finishReason := "stop"
	var toolCalls []any
	for _, item := range response.Output {
		if item.Type != "function_call" {
			continue
		}
		toolCalls = append(toolCalls, map[string]any{
			"id": item.CallID, "type": "function",
			"function": map[string]any{"name": item.Name, "arguments": string(item.Arguments)},
		})
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
		finishReason = "tool_calls"
		if response.OutputText() == "" {
			message["content"] = nil
		}
	}
	return map[string]any{
		"id": response.ID, "object": "chat.completion", "created": response.CreatedAt, "model": response.Model,
		"choices": []any{map[string]any{
			"index": 0, "message": message, "finish_reason": finishReason,
		}},
		"usage": chatUsage(response.Usage),
	}
}

func hasJSONValue(value json.RawMessage) bool {
	trimmed := bytes.TrimSpace(value)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

func chatUsage(usage core.Usage) map[string]any {
	return map[string]any{
		"prompt_tokens": usage.InputTokens, "completion_tokens": usage.OutputTokens, "total_tokens": usage.TotalTokens,
		"prompt_tokens_details": map[string]int64{"cached_tokens": usage.CachedInputTokens},
	}
}

func writeSSE(responseWriter http.ResponseWriter, value any, flusher http.Flusher) error {
	var buffer bytes.Buffer
	if err := json.NewEncoder(&buffer).Encode(value); err != nil {
		return err
	}
	data := strings.TrimSuffix(buffer.String(), "\n")
	if _, err := fmt.Fprintf(responseWriter, "data: %s\n\n", data); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func writeNamedSSE(responseWriter http.ResponseWriter, eventName string, value any, flusher http.Flusher) error {
	var buffer bytes.Buffer
	if err := json.NewEncoder(&buffer).Encode(value); err != nil {
		return err
	}
	data := strings.TrimSuffix(buffer.String(), "\n")
	if _, err := fmt.Fprintf(responseWriter, "event: %s\ndata: %s\n\n", eventName, data); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func writeRuntimeError(responseWriter http.ResponseWriter, result core.Response, err error) {
	if errors.Is(err, runtime.ErrQuotaExceeded) {
		writeError(responseWriter, http.StatusTooManyRequests, "rate_limit_exceeded", err.Error(), "")
		return
	}
	if errors.Is(err, store.ErrIdempotencyMismatch) {
		writeError(responseWriter, http.StatusConflict, "idempotency_conflict", store.ErrIdempotencyMismatch.Error(), "Idempotency-Key")
		return
	}
	status := http.StatusBadGateway
	if result.Error != nil && result.Error.Code == "route_not_found" {
		status = http.StatusBadRequest
	}
	if result.Error != nil {
		writeError(responseWriter, status, result.Error.Code, result.Error.Message, "")
		return
	}
	writeError(responseWriter, status, "gateway_error", err.Error(), "")
}

func responseRequestHash(payload createResponseRequest) []byte {
	encoded, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(encoded)
	return digest[:]
}

func decodeInput(raw json.RawMessage) ([]core.Item, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, errors.New("input is required")
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return []core.Item{{Type: "message", Role: "user", Content: []core.Content{{Type: "input_text", Text: text}}}}, nil
	}
	var items []core.Item
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, errors.New("input must be a string or an array of typed items")
	}
	if err := validateItems(items); err != nil {
		return nil, err
	}
	return items, nil
}

func validateItems(items []core.Item) error {
	for index := range items {
		if items[index].Type == "" {
			return errors.New("every item must have a type")
		}
		for _, content := range items[index].Content {
			switch content.Type {
			case "input_text", "output_text", "text":
				if content.Text == "" {
					return errors.New("text content requires text")
				}
			case "input_image":
				if content.ImageURL == "" && content.FileID == "" {
					return errors.New("input_image content requires image_url or file_id")
				}
			case "input_file":
				if content.FileID == "" && content.FileData == "" {
					return errors.New("input_file content requires file_id or file_data")
				}
			default:
				return fmt.Errorf("unsupported content type %q", content.Type)
			}
		}
	}
	return nil
}

func requestedFeatures(items []core.Item) []string {
	features := []string{"text"}
	multimodal, reasoning := false, false
	for _, item := range items {
		if item.Type == "reasoning" || len(item.Summary) > 0 || item.EncryptedContent != "" || len(item.ProviderMetadata) > 0 {
			reasoning = true
		}
		for _, content := range item.Content {
			if content.Type == "input_image" || content.Type == "input_file" {
				multimodal = true
			}
		}
	}
	if multimodal {
		features = append(features, "multimodal")
	}
	if reasoning {
		features = append(features, "reasoning")
	}
	return features
}

func validateCacheProtection(policy *core.CacheProtectionPolicy) error {
	if policy == nil || !policy.Enabled {
		return nil
	}
	if policy.MaxSpendMicros <= 0 || policy.MaxRefreshes <= 0 || policy.MaxProtectionWindowSec <= 0 {
		return errors.New("enabled cache protection requires positive max_spend_micros, max_refreshes, and max_protection_window_seconds")
	}
	if policy.SafetyMarginMicros < 0 {
		return errors.New("cache protection safety_margin_micros cannot be negative")
	}
	if policy.MaxSpendMicros > 1_000_000_000 || policy.MaxRefreshes > 100 || policy.MaxProtectionWindowSec > int64((24*time.Hour)/time.Second) {
		return errors.New("cache protection bounds exceed the supported safety limits")
	}
	return nil
}

func validateCompatibilityMode(mode core.CompatibilityMode) error {
	if mode == "" || mode == core.CompatibilityStrict || mode == core.CompatibilityBestEffort {
		return nil
	}
	return errors.New("compatibility_mode must be strict or best_effort")
}

func decodeBody(request *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(request.Body, 4<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("request body must contain one JSON value")
	}
	return nil
}

func tenantID(request *http.Request) string { return request.Header.Get("X-Authenticated-Tenant-ID") }

func (s *Server) homeRegion(request *http.Request) string {
	if region := s.homeRegions[tenantID(request)]; region != "" {
		return region
	}
	return "local"
}

func (s *Server) executionEpoch(request *http.Request) int64 {
	if epoch := s.executionEpochs[tenantID(request)]; epoch > 0 {
		return epoch
	}
	return 1
}

func writeStoreError(responseWriter http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(responseWriter, http.StatusNotFound, "not_found", "response not found", "")
		return
	}
	if errors.Is(err, store.ErrConflict) {
		writeError(responseWriter, http.StatusConflict, "conflict", err.Error(), "")
		return
	}
	if errors.Is(err, store.ErrConversationBusy) {
		writeError(responseWriter, http.StatusConflict, "conversation_busy", err.Error(), "conversation")
		return
	}
	writeError(responseWriter, http.StatusInternalServerError, "store_error", err.Error(), "")
}

func writeError(responseWriter http.ResponseWriter, status int, code, message, param string) {
	writeJSON(responseWriter, status, map[string]any{"error": core.Error{Code: code, Message: message, Type: "invalid_request_error", Param: param}})
}

func writeJSON(responseWriter http.ResponseWriter, status int, value any) {
	responseWriter.Header().Set("Content-Type", "application/json")
	responseWriter.WriteHeader(status)
	_ = json.NewEncoder(responseWriter).Encode(value)
}
