package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/provider"
)

type AdapterConfig struct {
	BaseURL                    string
	APIKey                     string
	APIVersion                 string
	HTTPClient                 *http.Client
	TTL                        time.Duration
	Model                      string
	RouteID                    string
	CredentialScope            string
	Region                     string
	CacheWritePerMillionMicros int64
	EnablePromptCaching        bool
}

type Adapter struct {
	*CacheProtector
	model                      string
	routeID                    string
	credentialScope            string
	region                     string
	cacheWritePerMillionMicros int64
	promptCaching              bool
}

func NewAdapter(config AdapterConfig) (*Adapter, error) {
	if config.Model == "" || config.RouteID == "" || config.CredentialScope == "" || config.Region == "" {
		return nil, errors.New("Anthropic adapter requires model, route ID, credential scope, and region")
	}
	protector, err := NewCacheProtector(CacheConfig{
		BaseURL: config.BaseURL, APIKey: config.APIKey, APIVersion: config.APIVersion,
		HTTPClient: config.HTTPClient, TTL: config.TTL,
	})
	if err != nil {
		return nil, err
	}
	return &Adapter{
		CacheProtector: protector, model: config.Model, routeID: config.RouteID,
		credentialScope: config.CredentialScope, region: config.Region,
		cacheWritePerMillionMicros: config.CacheWritePerMillionMicros,
		promptCaching:              config.EnablePromptCaching,
	}, nil
}

func (a *Adapter) Execute(ctx context.Context, request core.Request) (provider.EventStream, error) {
	maxTokens := 4096
	if request.MaxOutputTokens != nil && *request.MaxOutputTokens > 0 {
		maxTokens = *request.MaxOutputTokens
	}
	stableCount := 0
	if a.promptCaching {
		stableCount = stablePrefixItemCount(request.Input)
	}
	payload, err := a.payload(request.Input, stableCount, request.Tools, request.ToolChoice, maxTokens, true, false)
	if err != nil {
		return nil, err
	}
	if request.EndUserID != "" {
		payload["metadata"] = map[string]any{"user_id": request.EndUserID}
	}
	if request.Temperature != nil {
		payload["temperature"] = *request.Temperature
	}
	if request.TopP != nil {
		payload["top_p"] = *request.TopP
	}
	if len(request.Stop) > 0 {
		var stopSequences []string
		if err := json.Unmarshal(request.Stop, &stopSequences); err != nil {
			var stop string
			if stringErr := json.Unmarshal(request.Stop, &stop); stringErr != nil {
				return nil, errors.New("stop must be a string or array of strings")
			}
			stopSequences = []string{stop}
		}
		if len(stopSequences) == 0 {
			return nil, errors.New("stop must not be empty")
		}
		payload["stop_sequences"] = stopSequences
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	httpRequest, err := a.newRequest(ctx, body)
	if err != nil {
		return nil, err
	}
	httpRequest.Header.Set("Accept", "text/event-stream")
	httpResponse, err := a.httpClient.Do(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("Anthropic execute: %w", err)
	}
	if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 {
		defer httpResponse.Body.Close()
		errorBody, _ := io.ReadAll(io.LimitReader(httpResponse.Body, 64<<10))
		return nil, fmt.Errorf("Anthropic status %d: %s", httpResponse.StatusCode, strings.TrimSpace(string(errorBody)))
	}
	return newAnthropicStream(httpResponse.Body), nil
}

func (a *Adapter) CurrentCacheAnchor(_ context.Context, request core.Request) (provider.CacheAnchor, bool, error) {
	if !a.promptCaching {
		return provider.CacheAnchor{}, false, nil
	}
	stableCount := stablePrefixItemCount(request.Input)
	if stableCount == 0 && len(request.Tools) == 0 {
		return provider.CacheAnchor{}, false, nil
	}
	payload, err := a.payload(
		request.Input[:stableCount], stableCount, request.Tools, nil, 0, false, true,
	)
	if err != nil {
		return provider.CacheAnchor{}, false, err
	}
	anchor, err := a.anchor(request.TenantID, request.APIKeyID, payload)
	return anchor, err == nil, err
}

func (a *Adapter) BuildCacheAnchor(_ context.Context, request core.Request, response core.Response) (provider.CacheObservation, error) {
	if !a.promptCaching {
		return provider.CacheObservation{}, errors.New("Anthropic prompt caching is disabled for this route")
	}
	stableCount := stablePrefixItemCount(request.Input)
	if stableCount == 0 && len(request.Tools) == 0 {
		return provider.CacheObservation{}, errors.New("Anthropic cache protection requires a stable system or tools prefix")
	}
	prefixTokens := max(response.Usage.CacheWriteInputTokens, response.Usage.CachedInputTokens)
	if prefixTokens <= 0 {
		return provider.CacheObservation{}, errors.New("Anthropic did not confirm creation or reuse of the stable prompt cache prefix")
	}
	payload, err := a.payload(request.Input[:stableCount], stableCount, request.Tools, nil, 0, false, true)
	if err != nil {
		return provider.CacheObservation{}, err
	}
	anchor, err := a.anchor(request.TenantID, request.APIKeyID, payload)
	if err != nil {
		return provider.CacheObservation{}, err
	}
	return provider.CacheObservation{
		Anchor: anchor, EstimatedExpiresAt: time.Now().UTC().Add(a.ttl), PrefixTokens: prefixTokens,
		RefreshCostMicros: perMillion(prefixTokens, a.cacheWritePerMillionMicros),
	}, nil
}

func (a *Adapter) anchor(tenantID, apiKeyID string, payload map[string]any) (provider.CacheAnchor, error) {
	serialized, err := json.Marshal(payload)
	if err != nil {
		return provider.CacheAnchor{}, err
	}
	digest := sha256.Sum256(serialized)
	hash := hex.EncodeToString(digest[:])
	return provider.CacheAnchor{
		TenantID: tenantID, APIKeyID: apiKeyID, RouteID: a.routeID, Provider: "anthropic", Model: a.model,
		CredentialScope: a.credentialScope, Region: a.region, CacheKey: hash[:32],
		PrefixHash: "sha256:" + hash, SerializedPrefix: serialized,
	}, nil
}

func (a *Adapter) payload(items []core.Item, stableCount int, tools []json.RawMessage, toolChoice json.RawMessage, maxTokens int, stream, placeholder bool) (map[string]any, error) {
	var system []map[string]any
	var messages []map[string]any
	var lastStable map[string]any
	for index, item := range items {
		stable := index < stableCount
		blocks, role, err := anthropicBlocks(item)
		if err != nil {
			return nil, err
		}
		if item.Type == "message" && (item.Role == "system" || item.Role == "developer") {
			system = append(system, blocks...)
			if stable && len(blocks) > 0 {
				lastStable = blocks[len(blocks)-1]
			}
			continue
		}
		if len(blocks) == 0 {
			continue
		}
		if len(messages) > 0 && messages[len(messages)-1]["role"] == role {
			content := messages[len(messages)-1]["content"].([]map[string]any)
			messages[len(messages)-1]["content"] = append(content, blocks...)
		} else {
			messages = append(messages, map[string]any{"role": role, "content": blocks})
		}
		if stable {
			lastStable = blocks[len(blocks)-1]
		}
	}
	if lastStable != nil {
		lastStable["cache_control"] = map[string]any{"type": "ephemeral", "ttl": ttlName(a.ttl)}
	}
	if placeholder {
		messages = append(messages, map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": "warmup"}}})
	}
	if len(messages) == 0 {
		return nil, errors.New("Anthropic request requires at least one non-system message")
	}
	payload := map[string]any{
		"model": a.model, "max_tokens": maxTokens, "stream": stream, "messages": messages,
	}
	if len(system) > 0 {
		payload["system"] = system
	}
	translatedTools, err := translateTools(tools)
	if err != nil {
		return nil, err
	}
	if len(translatedTools) > 0 {
		if a.promptCaching && lastStable == nil {
			translatedTools[len(translatedTools)-1]["cache_control"] = map[string]any{"type": "ephemeral", "ttl": ttlName(a.ttl)}
		}
		payload["tools"] = translatedTools
	}
	if len(toolChoice) > 0 {
		choice, err := translateToolChoice(toolChoice)
		if err != nil {
			return nil, err
		}
		payload["tool_choice"] = choice
	}
	return payload, nil
}

func translateToolChoice(raw json.RawMessage) (map[string]any, error) {
	var name string
	if err := json.Unmarshal(raw, &name); err == nil {
		switch name {
		case "auto", "none":
			return map[string]any{"type": name}, nil
		case "required":
			return map[string]any{"type": "any"}, nil
		default:
			return nil, fmt.Errorf("unsupported tool_choice %q", name)
		}
	}
	var choice struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &choice); err != nil || choice.Type != "function" || choice.Function.Name == "" {
		return nil, errors.New("tool_choice must be auto, none, required, or a named function")
	}
	return map[string]any{"type": "tool", "name": choice.Function.Name}, nil
}

func anthropicBlocks(item core.Item) ([]map[string]any, string, error) {
	switch item.Type {
	case "message":
		blocks := make([]map[string]any, 0, len(item.Content))
		for _, content := range item.Content {
			switch content.Type {
			case "input_text", "output_text", "text":
				blocks = append(blocks, map[string]any{"type": "text", "text": content.Text})
			case "input_image":
				if content.ImageURL == "" {
					return nil, "", errors.New("Anthropic adapter requires image_url for input_image")
				}
				blocks = append(blocks, map[string]any{
					"type": "image", "source": map[string]any{"type": "url", "url": content.ImageURL},
				})
			default:
				return nil, "", fmt.Errorf("Anthropic adapter does not support content type %q", content.Type)
			}
		}
		role := item.Role
		if role == "system" || role == "developer" {
			return blocks, role, nil
		}
		if role != "assistant" {
			role = "user"
		}
		return blocks, role, nil
	case "function_call":
		var input any = map[string]any{}
		if len(item.Arguments) > 0 && json.Unmarshal(item.Arguments, &input) != nil {
			return nil, "", errors.New("invalid function-call arguments")
		}
		return []map[string]any{{"type": "tool_use", "id": item.CallID, "name": item.Name, "input": input}}, "assistant", nil
	case "function_call_output":
		return []map[string]any{{"type": "tool_result", "tool_use_id": item.CallID, "content": item.Output}}, "user", nil
	default:
		return nil, "", fmt.Errorf("Anthropic adapter does not support item type %q", item.Type)
	}
}

func translateTools(tools []json.RawMessage) ([]map[string]any, error) {
	translated := make([]map[string]any, 0, len(tools))
	for _, raw := range tools {
		var tool struct {
			Type     string `json:"type"`
			Function struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
		}
		if err := json.Unmarshal(raw, &tool); err != nil || tool.Type != "function" || tool.Function.Name == "" {
			return nil, errors.New("only client function tools can be translated to Anthropic")
		}
		var schema any = map[string]any{"type": "object"}
		if len(tool.Function.Parameters) > 0 && json.Unmarshal(tool.Function.Parameters, &schema) != nil {
			return nil, errors.New("invalid function tool parameters")
		}
		translated = append(translated, map[string]any{
			"name": tool.Function.Name, "description": tool.Function.Description, "input_schema": schema,
		})
	}
	return translated, nil
}

func (a *Adapter) newRequest(ctx context.Context, body []byte) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", a.apiKey)
	request.Header.Set("anthropic-version", a.apiVersion)
	return request, nil
}

func ttlName(ttl time.Duration) string {
	if ttl >= time.Hour {
		return "1h"
	}
	return "5m"
}

func perMillion(tokens, rate int64) int64 {
	return (tokens/1_000_000)*rate + (tokens%1_000_000)*rate/1_000_000
}

func stablePrefixItemCount(items []core.Item) int {
	count := 0
	for _, item := range items {
		if item.Type != "message" || (item.Role != "system" && item.Role != "developer") {
			break
		}
		count++
	}
	return count
}

type anthropicStream struct {
	body     io.ReadCloser
	scanner  *bufio.Scanner
	usage    core.Usage
	rawUsage anthropicUsage
	tools    map[int]*anthropicToolCall
	pending  []core.Event
	done     bool
}

type anthropicToolCall struct {
	id        string
	name      string
	arguments strings.Builder
}

func newAnthropicStream(body io.ReadCloser) *anthropicStream {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	return &anthropicStream{body: body, scanner: scanner, tools: make(map[int]*anthropicToolCall)}
}

func (s *anthropicStream) Recv() (core.Event, error) {
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
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		var event struct {
			Type    string `json:"type"`
			Index   int    `json:"index"`
			Message *struct {
				Usage anthropicUsage `json:"usage"`
			} `json:"message"`
			Usage anthropicUsage `json:"usage"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return core.Event{}, err
		}
		if event.Error != nil {
			return core.Event{}, errors.New(event.Error.Message)
		}
		switch event.Type {
		case "message_start":
			if event.Message != nil {
				s.applyUsage(event.Message.Usage)
			}
		case "content_block_start":
			if event.ContentBlock.Type == "tool_use" {
				s.tools[event.Index] = &anthropicToolCall{id: event.ContentBlock.ID, name: event.ContentBlock.Name}
			}
		case "content_block_delta":
			if event.Delta.Type == "text_delta" && event.Delta.Text != "" {
				return core.Event{Type: "response.output_text.delta", Delta: event.Delta.Text}, nil
			}
			if call := s.tools[event.Index]; call != nil {
				call.arguments.WriteString(event.Delta.PartialJSON)
			}
		case "content_block_stop":
			if call := s.tools[event.Index]; call != nil {
				arguments := json.RawMessage(call.arguments.String())
				if !json.Valid(arguments) {
					return core.Event{}, errors.New("Anthropic returned invalid tool arguments")
				}
				delete(s.tools, event.Index)
				item := core.Item{Type: "function_call", CallID: call.id, Name: call.name, Arguments: arguments}
				return core.Event{Type: "response.output_item.done", Item: &item}, nil
			}
		case "message_delta":
			s.applyUsage(event.Usage)
		case "message_stop":
			s.done = true
			s.usage.TotalTokens = s.usage.InputTokens + s.usage.OutputTokens
			providerUsage, _ := json.Marshal(s.rawUsage)
			return core.Event{Type: "response.completed", Usage: &s.usage, ProviderUsage: providerUsage}, nil
		}
	}
	s.done = true
	if err := s.scanner.Err(); err != nil {
		return core.Event{}, err
	}
	return core.Event{}, io.EOF
}

type anthropicUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

func (s *anthropicStream) applyUsage(usage anthropicUsage) {
	if usage.InputTokens != 0 || usage.CacheReadInputTokens != 0 || usage.CacheCreationInputTokens != 0 {
		s.rawUsage.InputTokens = usage.InputTokens
		s.rawUsage.CacheReadInputTokens = usage.CacheReadInputTokens
		s.rawUsage.CacheCreationInputTokens = usage.CacheCreationInputTokens
		s.usage.InputTokens = usage.InputTokens + usage.CacheReadInputTokens + usage.CacheCreationInputTokens
		s.usage.CachedInputTokens = usage.CacheReadInputTokens
		s.usage.CacheWriteInputTokens = usage.CacheCreationInputTokens
	}
	if usage.OutputTokens != 0 {
		s.rawUsage.OutputTokens = usage.OutputTokens
		s.usage.OutputTokens = usage.OutputTokens
	}
}

func (s *anthropicStream) Close() error {
	s.done = true
	return s.body.Close()
}
