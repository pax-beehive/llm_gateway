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
	Runtime           *runtime.Runtime
	Authenticator     Authenticator
	TenantHomeRegions map[string]string
}

type Server struct {
	runtime       *runtime.Runtime
	authenticator Authenticator
	homeRegions   map[string]string
	mux           *http.ServeMux
}

func New(config Config) http.Handler {
	server := &Server{runtime: config.Runtime, authenticator: config.Authenticator, homeRegions: config.TenantHomeRegions, mux: http.NewServeMux()}
	server.mux.HandleFunc("POST /v1/responses", server.createResponse)
	server.mux.HandleFunc("GET /v1/responses/{response_id}", server.getResponse)
	server.mux.HandleFunc("DELETE /v1/responses/{response_id}", server.deleteResponse)
	server.mux.HandleFunc("POST /v1/responses/{response_id}/cancel", server.cancelResponse)
	server.mux.HandleFunc("GET /v1/responses/{response_id}/input_items", server.inputItems)
	server.mux.HandleFunc("POST /v1/chat/completions", server.chatCompletions)
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
	Model              string                 `json:"model"`
	Input              json.RawMessage        `json:"input"`
	Stream             bool                   `json:"stream"`
	Background         bool                   `json:"background"`
	Store              *bool                  `json:"store"`
	PreviousResponseID string                 `json:"previous_response_id"`
	Conversation       string                 `json:"conversation"`
	Metadata           map[string]string      `json:"metadata"`
	CompatibilityMode  core.CompatibilityMode `json:"compatibility_mode"`
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
	input, err := decodeInput(payload.Input)
	if err != nil {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", err.Error(), "input")
		return
	}
	storeResponse := true
	if payload.Store != nil {
		storeResponse = *payload.Store
	}
	canonical := core.Request{
		TenantID: request.Header.Get("X-Authenticated-Tenant-ID"), Model: payload.Model, Input: input,
		Stream: payload.Stream, Background: payload.Background, Store: storeResponse,
		PreviousResponseID: payload.PreviousResponseID, ConversationID: payload.Conversation,
		CompatibilityMode: payload.CompatibilityMode, RequestedFeatures: []string{"text"}, Metadata: payload.Metadata,
		HomeRegion:     s.homeRegion(request),
		IdempotencyKey: strings.TrimSpace(request.Header.Get("Idempotency-Key")),
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

type chatCompletionRequest struct {
	Model               string                 `json:"model"`
	Messages            []chatMessage          `json:"messages"`
	Stream              bool                   `json:"stream"`
	StreamOptions       *chatStreamOptions     `json:"stream_options,omitempty"`
	Tools               []json.RawMessage      `json:"tools,omitempty"`
	ToolChoice          json.RawMessage        `json:"tool_choice,omitempty"`
	Temperature         *float64               `json:"temperature,omitempty"`
	TopP                *float64               `json:"top_p,omitempty"`
	MaxTokens           *int                   `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int                   `json:"max_completion_tokens,omitempty"`
	Stop                json.RawMessage        `json:"stop,omitempty"`
	User                string                 `json:"user,omitempty"`
	Metadata            map[string]string      `json:"metadata,omitempty"`
	CompatibilityMode   core.CompatibilityMode `json:"compatibility_mode,omitempty"`
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Name       string          `json:"name,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
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
	items, err := canonicalChatMessages(payload.Messages)
	if err != nil {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", err.Error(), "messages")
		return
	}
	features := []string{"text"}
	if payload.Stream {
		features = append(features, "streaming")
	}
	if len(payload.Tools) > 0 || len(payload.ToolChoice) > 0 {
		features = append(features, "tools")
	}
	if payload.Temperature != nil || payload.TopP != nil || payload.MaxTokens != nil || payload.MaxCompletionTokens != nil || len(payload.Stop) > 0 {
		features = append(features, "sampling")
	}
	maxOutputTokens := payload.MaxCompletionTokens
	if maxOutputTokens == nil {
		maxOutputTokens = payload.MaxTokens
	}
	canonical := core.Request{
		TenantID: tenantID(request), Model: payload.Model, Input: items, Stream: payload.Stream, Store: true,
		CompatibilityMode: payload.CompatibilityMode, RequestedFeatures: features, Metadata: payload.Metadata,
		HomeRegion: s.homeRegion(request),
		Tools:      payload.Tools, ToolChoice: payload.ToolChoice, Temperature: payload.Temperature,
		TopP: payload.TopP, MaxOutputTokens: maxOutputTokens, Stop: payload.Stop, EndUserID: payload.User,
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
			return writeSSE(responseWriter, map[string]any{
				"id": responseID, "object": "chat.completion.chunk", "created": createdAt, "model": canonical.Model,
				"choices": []any{map[string]any{
					"index": 0,
					"delta": map[string]any{"tool_calls": []any{map[string]any{
						"index": 0, "id": event.Item.CallID, "type": "function",
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
		if err := json.Unmarshal(message.Content, &text); err != nil {
			return nil, errors.New("only string chat message content is supported in this compatibility tier")
		}
		itemType := "message"
		contentType := "input_text"
		item := core.Item{Type: itemType, Role: message.Role, Name: message.Name, CallID: message.ToolCallID}
		if message.Role == "tool" {
			item.Type = "function_call_output"
			item.Output = text
		} else {
			item.Content = []core.Content{{Type: contentType, Text: text}}
		}
		items = append(items, item)
	}
	return items, nil
}

func chatCompletion(response core.Response) map[string]any {
	return map[string]any{
		"id": response.ID, "object": "chat.completion", "created": response.CreatedAt, "model": response.Model,
		"choices": []any{map[string]any{
			"index": 0, "message": map[string]any{"role": "assistant", "content": response.OutputText()}, "finish_reason": "stop",
		}},
		"usage": chatUsage(response.Usage),
	}
}

func chatUsage(usage core.Usage) map[string]int64 {
	return map[string]int64{
		"prompt_tokens": usage.InputTokens, "completion_tokens": usage.OutputTokens, "total_tokens": usage.TotalTokens,
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
	for index := range items {
		if items[index].Type == "" {
			return nil, errors.New("every input item must have a type")
		}
	}
	return items, nil
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

func writeStoreError(responseWriter http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(responseWriter, http.StatusNotFound, "not_found", "response not found", "")
		return
	}
	if errors.Is(err, store.ErrConflict) {
		writeError(responseWriter, http.StatusConflict, "conflict", err.Error(), "")
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
