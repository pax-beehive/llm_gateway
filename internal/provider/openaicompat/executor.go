package openaicompat

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
	baseURL.Path = strings.TrimRight(baseURL.Path, "/") + "/chat/completions"
	if config.Model == "" {
		return nil, errors.New("provider model is required")
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)}
	}
	return &Executor{
		endpoint: baseURL.String(), apiKey: config.APIKey, model: config.Model,
		httpClient: client, headers: cloneHeaders(config.Headers),
	}, nil
}

func (e *Executor) Execute(ctx context.Context, request core.Request) (provider.EventStream, error) {
	messages, err := messagesFromItems(request.Input)
	if err != nil {
		return nil, err
	}
	payload := map[string]any{"model": e.model, "messages": messages, "stream": true, "stream_options": map[string]bool{"include_usage": true}}
	if request.Temperature != nil {
		payload["temperature"] = *request.Temperature
	}
	if request.TopP != nil {
		payload["top_p"] = *request.TopP
	}
	if request.MaxOutputTokens != nil {
		payload["max_completion_tokens"] = *request.MaxOutputTokens
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
	return newSSEStream(httpResponse.Body), nil
}

func messagesFromItems(items []core.Item) ([]map[string]any, error) {
	messages := make([]map[string]any, 0, len(items))
	for _, item := range items {
		switch item.Type {
		case "message":
			var content strings.Builder
			for _, part := range item.Content {
				if part.Type != "input_text" && part.Type != "text" {
					return nil, fmt.Errorf("unsupported message content type %q", part.Type)
				}
				content.WriteString(part.Text)
			}
			messages = append(messages, map[string]any{"role": item.Role, "content": content.String()})
		case "function_call_output":
			messages = append(messages, map[string]any{"role": "tool", "tool_call_id": item.CallID, "content": item.Output})
		default:
			return nil, fmt.Errorf("unsupported canonical input item type %q", item.Type)
		}
	}
	return messages, nil
}

type sseStream struct {
	body      io.ReadCloser
	scanner   *bufio.Scanner
	done      bool
	pending   []core.Event
	toolCalls map[int]*toolCall
}

type toolCall struct {
	ID        string
	Name      string
	Arguments strings.Builder
}

func newSSEStream(body io.ReadCloser) *sseStream {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	return &sseStream{body: body, scanner: scanner, toolCalls: make(map[int]*toolCall)}
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
			return core.Event{}, io.EOF
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
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
		if chunk.Usage != nil {
			usage := core.Usage{
				InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens,
				TotalTokens: chunk.Usage.TotalTokens, CachedInputTokens: chunk.Usage.PromptTokenDetails.CachedTokens,
			}
			return core.Event{Type: "response.completed", Usage: &usage, ProviderUsage: append(json.RawMessage(nil), []byte(data)...)}, nil
		}
		if len(chunk.Choices) > 0 {
			choice := chunk.Choices[0]
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
					s.pending = append(s.pending, core.Event{Type: "response.output_item.done", Item: &item})
				}
				s.toolCalls = make(map[int]*toolCall)
				if len(s.pending) > 0 {
					event := s.pending[0]
					s.pending = s.pending[1:]
					return event, nil
				}
			}
			if choice.Delta.Content != "" {
				return core.Event{Type: "response.output_text.delta", Delta: choice.Delta.Content}, nil
			}
		}
	}
	s.done = true
	if err := s.scanner.Err(); err != nil {
		return core.Event{}, fmt.Errorf("read provider SSE: %w", err)
	}
	return core.Event{}, io.EOF
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
