package gemini

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

	"github.com/toddzheng/llm-gateway/internal/provider"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

type CacheProtector struct {
	baseURL    string
	apiKey     string
	ttl        time.Duration
	httpClient *http.Client
}

func NewCacheProtector(baseURL, apiKey string, ttl time.Duration, client *http.Client) (*CacheProtector, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("Gemini base URL must be absolute")
	}
	if apiKey == "" || ttl <= 0 {
		return nil, errors.New("Gemini API key and positive cache TTL are required")
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second, Transport: otelhttp.NewTransport(http.DefaultTransport)}
	}
	return &CacheProtector{baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, ttl: ttl, httpClient: client}, nil
}

func (p *CacheProtector) Inspect(_ context.Context, anchor provider.CacheAnchor) provider.CacheCapability {
	if anchor.Provider != "gemini" {
		return provider.CacheCapability{Reason: "provider mismatch"}
	}
	if !strings.HasPrefix(anchor.CacheKey, "cachedContents/") || strings.Contains(anchor.CacheKey, "..") {
		return provider.CacheCapability{Reason: "Gemini cache key must be a cachedContents resource name"}
	}
	return provider.CacheCapability{Supported: true}
}

func (p *CacheProtector) Refresh(ctx context.Context, anchor provider.CacheAnchor) (provider.RefreshResult, error) {
	capability := p.Inspect(ctx, anchor)
	if !capability.Supported {
		return provider.RefreshResult{Status: "rejected"}, errors.New(capability.Reason)
	}
	body, _ := json.Marshal(map[string]string{"ttl": fmt.Sprintf("%ds", int64(p.ttl.Seconds()))})
	endpoint := p.baseURL + "/" + anchor.CacheKey + "?updateMask=ttl&key=" + url.QueryEscape(p.apiKey)
	request, err := http.NewRequestWithContext(ctx, http.MethodPatch, endpoint, bytes.NewReader(body))
	if err != nil {
		return provider.RefreshResult{Status: "uncertain"}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := p.httpClient.Do(request)
	if err != nil {
		return provider.RefreshResult{Status: "uncertain"}, fmt.Errorf("Gemini cache TTL update outcome uncertain: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return provider.RefreshResult{Status: "uncertain"}, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return provider.RefreshResult{Status: "rejected", ProviderUsage: responseBody}, fmt.Errorf("Gemini cache update status %d: %s", response.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	var metadata struct {
		ExpireTime time.Time       `json:"expireTime"`
		Usage      json.RawMessage `json:"usageMetadata"`
	}
	if err := json.Unmarshal(responseBody, &metadata); err != nil {
		return provider.RefreshResult{Status: "uncertain", ProviderUsage: responseBody}, err
	}
	return provider.RefreshResult{Status: "succeeded", ProviderUsage: metadata.Usage, ExpiresAt: metadata.ExpireTime}, nil
}
