package openairesponses

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/provider"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

type Config struct {
	BaseURL    string
	APIKey     string
	Model      string
	HTTPClient *http.Client
	Headers    map[string]string
}

type Executor struct {
	endpoint   string
	apiKey     string
	model      string
	httpClient *http.Client
	headers    map[string]string
}

func New(config Config) (*Executor, error) {
	baseURL, err := url.Parse(config.BaseURL)
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return nil, errors.New("provider base URL must be an absolute HTTP URL")
	}
	if config.Model == "" {
		return nil, errors.New("provider model is required")
	}
	baseURL.Path = strings.TrimRight(baseURL.Path, "/") + "/responses"
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)}
	} else {
		copy := *client
		client = &copy
	}
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &Executor{
		endpoint: baseURL.String(), apiKey: config.APIKey, model: config.Model,
		httpClient: client, headers: cloneHeaders(config.Headers),
	}, nil
}

func (e *Executor) Execute(ctx context.Context, request core.Request) (provider.EventStream, error) {
	payload := map[string]any{
		"model": e.model, "input": request.Input, "stream": true, "store": false,
	}
	tools, err := normalizeTools(request.Tools)
	if err != nil {
		return nil, err
	}
	if len(tools) > 0 {
		payload["tools"] = tools
	}
	copyRaw(payload, "tool_choice", request.ToolChoice)
	copyRaw(payload, "reasoning", request.Reasoning)
	copyRaw(payload, "client_metadata", request.ClientMetadata)
	copyRaw(payload, "text", request.Text)
	if request.ParallelToolCalls != nil {
		payload["parallel_tool_calls"] = *request.ParallelToolCalls
	}
	if len(request.Include) > 0 {
		payload["include"] = request.Include
	}
	if request.PromptCacheKey != "" {
		payload["prompt_cache_key"] = request.PromptCacheKey
	}
	if request.ServiceTier != "" {
		payload["service_tier"] = request.ServiceTier
	}
	if request.Truncation != "" {
		payload["truncation"] = request.Truncation
	}
	if request.MaxToolCalls != nil {
		payload["max_tool_calls"] = *request.MaxToolCalls
	}
	if request.SafetyIdentifier != "" {
		payload["safety_identifier"] = request.SafetyIdentifier
	}
	if request.Metadata != nil {
		payload["metadata"] = request.Metadata
	}
	if request.Temperature != nil {
		payload["temperature"] = *request.Temperature
	}
	if request.TopP != nil {
		payload["top_p"] = *request.TopP
	}
	if request.MaxOutputTokens != nil {
		payload["max_output_tokens"] = *request.MaxOutputTokens
	}
	if request.EndUserID != "" {
		payload["user"] = request.EndUserID
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "text/event-stream")
	if e.apiKey != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+e.apiKey)
	}
	for key, value := range e.headers {
		httpRequest.Header.Set(key, value)
	}
	httpResponse, err := e.httpClient.Do(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("provider request: %w", err)
	}
	if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 {
		defer httpResponse.Body.Close()
		errorBody, _ := io.ReadAll(io.LimitReader(httpResponse.Body, 64<<10))
		return nil, fmt.Errorf("provider status %d: %s", httpResponse.StatusCode, strings.TrimSpace(string(errorBody)))
	}
	return newStream(httpResponse.Body), nil
}

func copyRaw(payload map[string]any, key string, raw any) {
	switch value := raw.(type) {
	case json.RawMessage:
		if len(value) > 0 {
			payload[key] = value
		}
	case []json.RawMessage:
		if len(value) > 0 {
			payload[key] = value
		}
	}
}

func normalizeTools(tools []json.RawMessage) ([]json.RawMessage, error) {
	normalized := make([]json.RawMessage, 0, len(tools))
	for _, raw := range tools {
		var tool struct {
			Type     string          `json:"type"`
			Name     string          `json:"name"`
			Function json.RawMessage `json:"function"`
		}
		if err := json.Unmarshal(raw, &tool); err != nil || tool.Type == "" {
			return nil, errors.New("invalid Responses tool")
		}
		if tool.Type != "function" || tool.Name != "" || len(tool.Function) == 0 {
			normalized = append(normalized, append(json.RawMessage(nil), raw...))
			continue
		}
		var function map[string]any
		if err := json.Unmarshal(tool.Function, &function); err != nil || function["name"] == "" {
			return nil, errors.New("invalid function tool")
		}
		function["type"] = "function"
		converted, err := json.Marshal(function)
		if err != nil {
			return nil, err
		}
		normalized = append(normalized, converted)
	}
	return normalized, nil
}

type stream struct {
	body    io.ReadCloser
	scanner *bufio.Scanner
	done    bool
}

func newStream(body io.ReadCloser) *stream {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	return &stream{body: body, scanner: scanner}
}

func (s *stream) Recv() (core.Event, error) {
	if s.done {
		return core.Event{}, io.EOF
	}
	for s.scanner.Scan() {
		line := strings.TrimSpace(s.scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event:") {
			continue
		}
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			s.done = true
			return core.Event{}, io.EOF
		}
		var event wireEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return core.Event{}, fmt.Errorf("decode OpenAI Responses event: %w", err)
		}
		switch event.Type {
		case "response.output_text.delta":
			return core.Event{
				Type: event.Type, Delta: event.Delta, ItemID: event.ItemID,
				OutputIndex: event.OutputIndex, ContentIndex: event.ContentIndex, Logprobs: event.Logprobs,
			}, nil
		case "response.output_item.added", "response.output_item.done":
			if event.Item == nil {
				continue
			}
			return core.Event{Type: event.Type, Item: event.Item, OutputIndex: event.OutputIndex}, nil
		case "response.content_part.added", "response.content_part.done":
			if event.Part == nil {
				continue
			}
			return core.Event{
				Type: event.Type, ItemID: event.ItemID, OutputIndex: event.OutputIndex,
				ContentIndex: event.ContentIndex, Part: event.Part,
			}, nil
		case "response.completed":
			s.done = true
			usage := canonicalUsage(event.Response.Usage)
			providerUsage, _ := json.Marshal(event.Response.Usage)
			return core.Event{Type: event.Type, Usage: &usage, ProviderUsage: providerUsage}, nil
		case "response.failed", "error":
			s.done = true
			if event.Error != nil && event.Error.Message != "" {
				return core.Event{}, errors.New(event.Error.Message)
			}
			if event.Response.Error != nil && event.Response.Error.Message != "" {
				return core.Event{}, errors.New(event.Response.Error.Message)
			}
			return core.Event{}, errors.New("OpenAI Responses request failed")
		}
	}
	s.done = true
	if err := s.scanner.Err(); err != nil {
		return core.Event{}, fmt.Errorf("read OpenAI Responses stream: %w", err)
	}
	return core.Event{}, io.EOF
}

type wireEvent struct {
	Type         string          `json:"type"`
	Delta        string          `json:"delta"`
	ItemID       string          `json:"item_id"`
	OutputIndex  *int            `json:"output_index"`
	ContentIndex *int            `json:"content_index"`
	Logprobs     json.RawMessage `json:"logprobs"`
	Part         *core.Content   `json:"part"`
	Item         *core.Item      `json:"item"`
	Error        *core.Error     `json:"error"`
	Response     struct {
		Usage wireUsage   `json:"usage"`
		Error *core.Error `json:"error"`
	} `json:"response"`
}

type wireUsage struct {
	InputTokens       int64 `json:"input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
	TotalTokens       int64 `json:"total_tokens"`
	InputTokenDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

func canonicalUsage(usage wireUsage) core.Usage {
	return core.Usage{
		InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens,
		TotalTokens: usage.TotalTokens, CachedInputTokens: usage.InputTokenDetails.CachedTokens,
	}
}

func (s *stream) Close() error {
	s.done = true
	return s.body.Close()
}

func cloneHeaders(headers map[string]string) map[string]string {
	cloned := make(map[string]string, len(headers))
	for key, value := range headers {
		cloned[key] = value
	}
	return cloned
}
