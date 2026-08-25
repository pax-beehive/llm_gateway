package openaicompat

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/provider"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

type Config struct {
	BaseURL    string
	APIKey     string
	Model      string
	Dialect    Dialect
	HTTPClient *http.Client
	Headers    map[string]string
}

type Dialect string

const (
	DialectOpenAI   Dialect = "openai"
	DialectDeepSeek Dialect = "deepseek"
	DialectGemini   Dialect = "gemini"
)

type Executor struct {
	endpoint   string
	apiKey     string
	model      string
	dialect    Dialect
	httpClient *http.Client
	headers    map[string]string
}

func New(config Config) (*Executor, error) {
	baseURL, err := url.Parse(config.BaseURL)
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return nil, errors.New("provider base URL must be an absolute HTTP URL")
	}
	baseURL.Path = strings.TrimRight(baseURL.Path, "/") + "/chat/completions"
	if config.Model == "" {
		return nil, errors.New("provider model is required")
	}
	if config.Dialect == "" {
		config.Dialect = DialectOpenAI
	}
	switch config.Dialect {
	case DialectOpenAI, DialectDeepSeek, DialectGemini:
	default:
		return nil, fmt.Errorf("unsupported OpenAI-compatible provider dialect %q", config.Dialect)
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)}
	}
	return &Executor{
		endpoint: baseURL.String(), apiKey: config.APIKey, model: config.Model,
		dialect: config.Dialect, httpClient: client, headers: cloneHeaders(config.Headers),
	}, nil
}

func (e *Executor) Execute(ctx context.Context, request core.Request) (provider.EventStream, error) {
	messages, err := messagesFromItems(request.Input)
	if err != nil {
		return nil, err
	}
	payload := map[string]any{"model": e.model, "messages": messages, "stream": true, "stream_options": map[string]bool{"include_usage": true}}
	if e.dialect == DialectDeepSeek {
		// The canonical adapter does not yet expose portable reasoning replay.
		// Disable DeepSeek thinking so private reasoning_content is never silently discarded.
		payload["thinking"] = map[string]string{"type": "disabled"}
	}
	if request.Temperature != nil {
		payload["temperature"] = *request.Temperature
	}
	if request.TopP != nil {
		payload["top_p"] = *request.TopP
	}
	if request.MaxOutputTokens != nil {
		payload[e.maxTokensField()] = *request.MaxOutputTokens
	}
	if len(request.Stop) > 0 {
		payload["stop"] = request.Stop
	}
	if len(request.Tools) > 0 {
		payload["tools"] = request.Tools
	}
	if len(request.ToolChoice) > 0 {
		payload["tool_choice"] = request.ToolChoice
	}
	if request.EndUserID != "" {
		payload[e.userField()] = e.endUserValue(request.TenantID, request.EndUserID)
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
	if e.dialect == DialectGemini {
		httpRequest.Header.Set("x-goog-api-client", "llm-gateway/0.1.0")
	}
	for key, value := range e.headers {
		httpRequest.Header.Set(key, value)
	}
	started := time.Now()
	httpResponse, err := e.httpClient.Do(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("provider request: %w", err)
	}
	if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 {
		defer httpResponse.Body.Close()
		errorBody, _ := io.ReadAll(io.LimitReader(httpResponse.Body, 64<<10))
		return nil, fmt.Errorf("provider status %d after %s: %s", httpResponse.StatusCode, time.Since(started).Round(time.Millisecond), strings.TrimSpace(string(errorBody)))
	}
	return newSSEStream(httpResponse.Body, e.dialect), nil
}

func (e *Executor) maxTokensField() string {
	if e.dialect == DialectDeepSeek {
		return "max_tokens"
	}
	return "max_completion_tokens"
}

func (e *Executor) userField() string {
	if e.dialect == DialectDeepSeek {
		return "user_id"
	}
	return "user"
}

func (e *Executor) endUserValue(tenantID, value string) string {
	if e.dialect != DialectDeepSeek {
		return value
	}
	if tenantID == "" {
		tenantID = "anonymous"
	}
	digest := hmac.New(sha256.New, []byte(tenantID))
	_, _ = digest.Write([]byte(value))
	return fmt.Sprintf("gw_%x", digest.Sum(nil))
}

func messagesFromItems(items []core.Item) ([]map[string]any, error) {
	messages := make([]map[string]any, 0, len(items))
	for _, item := range items {
		switch item.Type {
		case "message":
			content, err := openAIContent(item.Content)
			if err != nil {
				return nil, err
			}
			messages = append(messages, map[string]any{"role": item.Role, "content": content})
		case "function_call_output":
			messages = append(messages, map[string]any{"role": "tool", "tool_call_id": item.CallID, "content": item.Output})
		default:
			return nil, fmt.Errorf("unsupported canonical input item type %q", item.Type)
		}
	}
	return messages, nil
}

func openAIContent(parts []core.Content) (any, error) {
	textOnly := true
	var text strings.Builder
	content := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case "input_text", "output_text", "text":
			text.WriteString(part.Text)
			content = append(content, map[string]any{"type": "text", "text": part.Text})
		case "input_image":
			if part.ImageURL == "" {
				return nil, errors.New("OpenAI-compatible Chat adapter requires image_url for input_image")
			}
			textOnly = false
			image := map[string]any{"url": part.ImageURL}
			if part.Detail != "" {
				image["detail"] = part.Detail
			}
			content = append(content, map[string]any{"type": "image_url", "image_url": image})
		default:
			return nil, fmt.Errorf("unsupported message content type %q", part.Type)
		}
	}
	if textOnly {
		return text.String(), nil
	}
	return content, nil
}

type sseStream struct {
	body              io.ReadCloser
	scanner           *bufio.Scanner
	dialect           Dialect
	done              bool
	completionEmitted bool
	lastUsage         *core.Usage
	lastProviderUsage json.RawMessage
	pending           []core.Event
	toolCalls         map[int]*toolCall
}

type toolCall struct {
	ID        string
	Name      string
	Arguments strings.Builder
}

func newSSEStream(body io.ReadCloser, dialect Dialect) *sseStream {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	return &sseStream{body: body, scanner: scanner, dialect: dialect, toolCalls: make(map[int]*toolCall)}
}

func (s *sseStream) Recv() (core.Event, error) {
	if len(s.pending) > 0 {
		event := s.pending[0]
		s.pending = s.pending[1:]
		return event, nil
	}
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
			if !s.completionEmitted {
				s.completionEmitted = true
				return core.Event{
					Type: "response.completed", Usage: cloneUsage(s.lastUsage),
					ProviderUsage: append(json.RawMessage(nil), s.lastProviderUsage...),
				}, nil
			}
			return core.Event{}, io.EOF
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens       int64 `json:"prompt_tokens"`
				CompletionTokens   int64 `json:"completion_tokens"`
				TotalTokens        int64 `json:"total_tokens"`
				PromptTokenDetails struct {
					CachedTokens int64 `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
				PromptCacheHitTokens int64 `json:"prompt_cache_hit_tokens"`
			} `json:"usage"`
			Error *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return core.Event{}, fmt.Errorf("decode provider SSE chunk: %w", err)
		}
		if chunk.Error != nil {
			return core.Event{}, fmt.Errorf("provider stream error %s: %s", chunk.Error.Code, chunk.Error.Message)
		}
		var events []core.Event
		if len(chunk.Choices) > 0 {
			choice := chunk.Choices[0]
			if choice.Delta.ReasoningContent != "" && s.dialect == DialectDeepSeek {
				return core.Event{}, errors.New("DeepSeek returned reasoning_content while thinking was disabled")
			}
			for _, delta := range choice.Delta.ToolCalls {
				call := s.toolCalls[delta.Index]
				if call == nil {
					call = &toolCall{}
					s.toolCalls[delta.Index] = call
				}
				if delta.ID != "" {
					call.ID = delta.ID
				}
				if delta.Function.Name != "" {
					call.Name = delta.Function.Name
				}
				call.Arguments.WriteString(delta.Function.Arguments)
			}
			if choice.FinishReason != nil && len(s.toolCalls) > 0 {
				indices := make([]int, 0, len(s.toolCalls))
				for index := range s.toolCalls {
					indices = append(indices, index)
				}
				sort.Ints(indices)
				for _, index := range indices {
					call := s.toolCalls[index]
					if call == nil {
						continue
					}
					arguments := json.RawMessage(call.Arguments.String())
					if !json.Valid(arguments) {
						return core.Event{}, errors.New("provider returned invalid function-call arguments")
					}
					item := core.Item{Type: "function_call", CallID: call.ID, Name: call.Name, Arguments: arguments}
					events = append(events, core.Event{Type: "response.output_item.done", Item: &item})
				}
				s.toolCalls = make(map[int]*toolCall)
			}
			if choice.Delta.Content != "" {
				events = append([]core.Event{{Type: "response.output_text.delta", Delta: choice.Delta.Content}}, events...)
			}
		}
		if chunk.Usage != nil {
			cachedInputTokens := chunk.Usage.PromptTokenDetails.CachedTokens
			if s.dialect == DialectDeepSeek {
				cachedInputTokens = chunk.Usage.PromptCacheHitTokens
			}
			usage := core.Usage{
				InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens,
				TotalTokens: chunk.Usage.TotalTokens, CachedInputTokens: cachedInputTokens,
			}
			s.lastUsage = &usage
			s.lastProviderUsage = append(s.lastProviderUsage[:0], []byte(data)...)
			terminal := len(chunk.Choices) == 0
			for _, choice := range chunk.Choices {
				if choice.FinishReason != nil {
					terminal = true
					break
				}
			}
			if terminal {
				s.completionEmitted = true
				events = append(events, core.Event{
					Type: "response.completed", Usage: cloneUsage(s.lastUsage),
					ProviderUsage: append(json.RawMessage(nil), s.lastProviderUsage...),
				})
			}
		}
		if len(events) > 0 {
			s.pending = append(s.pending, events[1:]...)
			return events[0], nil
		}
	}
	s.done = true
	if err := s.scanner.Err(); err != nil {
		return core.Event{}, fmt.Errorf("read provider SSE: %w", err)
	}
	return core.Event{}, io.EOF
}

func cloneUsage(usage *core.Usage) *core.Usage {
	if usage == nil {
		return nil
	}
	cloned := *usage
	return &cloned
}

func (s *sseStream) Close() error {
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
