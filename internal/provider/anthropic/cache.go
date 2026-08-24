package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/provider"
)

type CacheConfig struct {
	BaseURL    string
	APIKey     string
	APIVersion string
	HTTPClient *http.Client
	TTL        time.Duration
}

type CacheProtector struct {
	endpoint   string
	apiKey     string
	apiVersion string
	httpClient *http.Client
	ttl        time.Duration
}

func NewCacheProtector(config CacheConfig) (*CacheProtector, error) {
	baseURL, err := url.Parse(config.BaseURL)
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return nil, errors.New("Anthropic base URL must be absolute")
	}
	baseURL.Path = strings.TrimRight(baseURL.Path, "/") + "/messages"
	if config.APIKey == "" {
		return nil, errors.New("Anthropic API key is required")
	}
	if config.APIVersion == "" {
		config.APIVersion = "2023-06-01"
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{Timeout: 60 * time.Second}
	}
	if config.TTL == 0 {
		config.TTL = 5 * time.Minute
	}
	return &CacheProtector{
		endpoint: baseURL.String(), apiKey: config.APIKey, apiVersion: config.APIVersion,
		httpClient: config.HTTPClient, ttl: config.TTL,
	}, nil
}

func (p *CacheProtector) Inspect(_ context.Context, anchor provider.CacheAnchor) provider.CacheCapability {
	if anchor.Provider != "anthropic" {
		return provider.CacheCapability{Reason: "provider mismatch"}
	}
	var request map[string]json.RawMessage
	if len(anchor.SerializedPrefix) == 0 || json.Unmarshal(anchor.SerializedPrefix, &request) != nil {
		return provider.CacheCapability{Reason: "serialized Anthropic refresh request is required"}
	}
	if len(request["cache_control"]) == 0 && !containsCacheControl(request["system"]) && !containsCacheControl(request["tools"]) && !containsCacheControl(request["messages"]) {
		return provider.CacheCapability{Reason: "explicit cache_control breakpoint is required"}
	}
	for _, forbidden := range []string{"thinking", "output_config"} {
		if value := request[forbidden]; len(value) > 0 && string(value) != "null" {
			return provider.CacheCapability{Reason: forbidden + " is incompatible with zero-output refresh"}
		}
	}
	if choice := request["tool_choice"]; len(choice) > 0 && string(choice) != "null" {
		var parsed struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(choice, &parsed) != nil || (parsed.Type != "auto" && parsed.Type != "none") {
			return provider.CacheCapability{Reason: "forced tool choice is incompatible with zero-output refresh"}
		}
	}
	return provider.CacheCapability{Supported: true}
}

func (p *CacheProtector) Refresh(ctx context.Context, anchor provider.CacheAnchor) (provider.RefreshResult, error) {
	capability := p.Inspect(ctx, anchor)
	if !capability.Supported {
		return provider.RefreshResult{Status: "rejected"}, errors.New(capability.Reason)
	}
	var request map[string]json.RawMessage
	if err := json.Unmarshal(anchor.SerializedPrefix, &request); err != nil {
		return provider.RefreshResult{Status: "rejected"}, err
	}
	request["max_tokens"] = json.RawMessage("0")
	request["stream"] = json.RawMessage("false")
	body, err := json.Marshal(request)
	if err != nil {
		return provider.RefreshResult{Status: "rejected"}, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return provider.RefreshResult{Status: "uncertain"}, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("x-api-key", p.apiKey)
	httpRequest.Header.Set("anthropic-version", p.apiVersion)
	httpResponse, err := p.httpClient.Do(httpRequest)
	if err != nil {
		return provider.RefreshResult{Status: "uncertain"}, fmt.Errorf("Anthropic cache refresh outcome uncertain: %w", err)
	}
	defer httpResponse.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(httpResponse.Body, 1<<20))
	if err != nil {
		return provider.RefreshResult{Status: "uncertain"}, fmt.Errorf("read Anthropic refresh response: %w", err)
	}
	if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 {
		return provider.RefreshResult{Status: "rejected", ProviderUsage: responseBody}, fmt.Errorf("Anthropic refresh status %d: %s", httpResponse.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	var response struct {
		Content    []json.RawMessage `json:"content"`
		StopReason string            `json:"stop_reason"`
		Usage      struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(responseBody, &response); err != nil {
		return provider.RefreshResult{Status: "uncertain", ProviderUsage: responseBody}, fmt.Errorf("decode Anthropic refresh response: %w", err)
	}
	if len(response.Content) != 0 || response.Usage.OutputTokens != 0 || response.StopReason != "max_tokens" {
		return provider.RefreshResult{Status: "uncertain", ProviderUsage: responseBody}, errors.New("Anthropic refresh unexpectedly produced output")
	}
	usage := core.Usage{
		InputTokens:  response.Usage.InputTokens + response.Usage.CacheCreationInputTokens,
		OutputTokens: response.Usage.OutputTokens, CachedInputTokens: response.Usage.CacheReadInputTokens,
	}
	usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	return provider.RefreshResult{
		Status: "succeeded", Usage: usage, ProviderUsage: responseBody, ExpiresAt: time.Now().UTC().Add(p.ttl),
	}, nil
}

func containsCacheControl(value json.RawMessage) bool {
	return bytes.Contains(value, []byte(`"cache_control"`))
}
