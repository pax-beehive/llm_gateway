package openaicapabilities

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

type Config struct {
	BaseURL           string
	APIKey            string
	Model             string
	HTTPClient        *http.Client
	Headers           map[string]string
	EmbeddingPath     string
	ModerationPath    string
	RerankPath        string
	DefaultDimensions int
	RequestTimeout    time.Duration
}

type Adapter struct {
	baseURL           *url.URL
	apiKey            string
	model             string
	client            *http.Client
	headers           map[string]string
	embeddingPath     string
	moderationPath    string
	rerankPath        string
	defaultDimensions int
	requestTimeout    time.Duration
}

func New(config Config) (*Adapter, error) {
	baseURL, err := url.Parse(strings.TrimRight(config.BaseURL, "/"))
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return nil, errors.New("capability Provider requires an absolute base URL")
	}
	if config.APIKey == "" || config.Model == "" {
		return nil, errors.New("capability Provider requires API key and model")
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{}
	}
	if config.RequestTimeout <= 0 {
		config.RequestTimeout = 60 * time.Second
	}
	if config.EmbeddingPath == "" {
		config.EmbeddingPath = "/embeddings"
	}
	if config.ModerationPath == "" {
		config.ModerationPath = "/moderations"
	}
	if config.RerankPath == "" {
		config.RerankPath = "/rerank"
	}
	for _, path := range []string{config.EmbeddingPath, config.ModerationPath, config.RerankPath} {
		if !strings.HasPrefix(path, "/") || strings.Contains(path, "?") || strings.Contains(path, "#") {
			return nil, errors.New("capability Provider paths must be absolute URL paths")
		}
	}
	return &Adapter{
		baseURL: baseURL, apiKey: config.APIKey, model: config.Model, client: config.HTTPClient,
		headers: cloneHeaders(config.Headers), embeddingPath: config.EmbeddingPath, moderationPath: config.ModerationPath,
		rerankPath: config.RerankPath, defaultDimensions: config.DefaultDimensions, requestTimeout: config.RequestTimeout,
	}, nil
}

func (a *Adapter) Embed(ctx context.Context, request core.EmbeddingRequest) (core.EmbeddingResult, error) {
	input := make([]any, len(request.Input))
	for index, item := range request.Input {
		if item.Text != nil {
			input[index] = *item.Text
		} else {
			input[index] = item.Tokens
		}
	}
	payload := map[string]any{"model": a.model, "input": input}
	if request.EncodingFormat != "" {
		payload["encoding_format"] = request.EncodingFormat
	}
	if request.Dimensions != nil {
		payload["dimensions"] = *request.Dimensions
	}
	if request.EndUserID != "" {
		payload["user"] = request.EndUserID
	}
	var wire struct {
		Model string `json:"model"`
		Data  []struct {
			Index     int             `json:"index"`
			Embedding json.RawMessage `json:"embedding"`
		} `json:"data"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := a.post(ctx, a.embeddingPath, payload, &wire); err != nil {
		return core.EmbeddingResult{}, err
	}
	data := make([]core.EmbeddingData, len(wire.Data))
	dimensions := int64(0)
	for index, item := range wire.Data {
		data[index].Index = item.Index
		if err := json.Unmarshal(item.Embedding, &data[index].Embedding); err != nil {
			if err := json.Unmarshal(item.Embedding, &data[index].Base64); err != nil {
				return core.EmbeddingResult{}, errors.New("Provider returned an invalid embedding payload")
			}
		}
		if len(data[index].Embedding) > 0 {
			if dimensions == 0 {
				dimensions = int64(len(data[index].Embedding))
			} else if dimensions != int64(len(data[index].Embedding)) {
				return core.EmbeddingResult{}, errors.New("Provider returned inconsistent embedding dimensions")
			}
		}
	}
	if dimensions == 0 {
		if request.Dimensions != nil {
			dimensions = int64(*request.Dimensions)
		} else {
			dimensions = int64(a.defaultDimensions)
		}
	}
	var usage struct {
		PromptTokens int64 `json:"prompt_tokens"`
		InputTokens  int64 `json:"input_tokens"`
	}
	if len(wire.Usage) > 0 && string(wire.Usage) != "null" {
		if err := json.Unmarshal(wire.Usage, &usage); err != nil {
			return core.EmbeddingResult{}, errors.New("Provider returned invalid embedding usage")
		}
	}
	inputUnits := usage.PromptTokens
	if inputUnits == 0 {
		inputUnits = usage.InputTokens
	}
	return core.EmbeddingResult{
		Model: request.Model, Data: data, InputUnits: inputUnits, Dimensions: dimensions,
		ProviderUsage: append(json.RawMessage(nil), wire.Usage...),
	}, nil
}

func (a *Adapter) Moderate(ctx context.Context, request core.ModerationRequest) (core.ModerationResult, error) {
	payload := map[string]any{"model": a.model, "input": request.Input}
	if request.EndUserID != "" {
		payload["user"] = request.EndUserID
	}
	var wire struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Results []struct {
			Flagged                   bool                `json:"flagged"`
			Categories                map[string]bool     `json:"categories"`
			CategoryScores            map[string]float64  `json:"category_scores"`
			CategoryAppliedInputTypes map[string][]string `json:"category_applied_input_types"`
		} `json:"results"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := a.post(ctx, a.moderationPath, payload, &wire); err != nil {
		return core.ModerationResult{}, err
	}
	results := make([]core.ModerationResultItem, len(wire.Results))
	for index, item := range wire.Results {
		results[index] = core.ModerationResultItem{
			Flagged: item.Flagged, Categories: item.Categories, CategoryScores: item.CategoryScores,
			CategoryAppliedInputTypes: item.CategoryAppliedInputTypes,
		}
	}
	var usage struct {
		PromptTokens int64 `json:"prompt_tokens"`
		InputTokens  int64 `json:"input_tokens"`
		TotalTokens  int64 `json:"total_tokens"`
	}
	if len(wire.Usage) > 0 && string(wire.Usage) != "null" {
		if err := json.Unmarshal(wire.Usage, &usage); err != nil {
			return core.ModerationResult{}, errors.New("Provider returned invalid moderation usage")
		}
	}
	inputUnits := usage.InputTokens
	if inputUnits == 0 {
		inputUnits = usage.PromptTokens
	}
	if inputUnits == 0 {
		inputUnits = usage.TotalTokens
	}
	return core.ModerationResult{
		ID: wire.ID, Model: request.Model, Results: results, InputUnits: inputUnits,
		ProviderUsage: append(json.RawMessage(nil), wire.Usage...),
	}, nil
}

func (a *Adapter) Rerank(ctx context.Context, request core.RerankRequest) (core.RerankResult, error) {
	documents := make([]string, len(request.Documents))
	for index, document := range request.Documents {
		documents[index] = document.Text
	}
	payload := map[string]any{"model": a.model, "query": request.Query, "documents": documents}
	if request.TopN != nil {
		payload["top_n"] = *request.TopN
	}
	if request.ReturnDocuments {
		payload["return_documents"] = true
	}
	var wire struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Results []struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
			Document       *struct {
				Text string `json:"text"`
			} `json:"document"`
		} `json:"results"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := a.post(ctx, a.rerankPath, payload, &wire); err != nil {
		return core.RerankResult{}, err
	}
	results := make([]core.RerankResultItem, len(wire.Results))
	for index, item := range wire.Results {
		results[index] = core.RerankResultItem{Index: item.Index, RelevanceScore: item.RelevanceScore}
		if item.Document != nil {
			results[index].Document = &core.RerankDocument{Text: item.Document.Text}
		}
	}
	var usage struct {
		TotalTokens int64 `json:"total_tokens"`
		InputTokens int64 `json:"input_tokens"`
	}
	if len(wire.Usage) > 0 && string(wire.Usage) != "null" {
		if err := json.Unmarshal(wire.Usage, &usage); err != nil {
			return core.RerankResult{}, errors.New("Provider returned invalid rerank usage")
		}
	}
	providerTokens := usage.TotalTokens
	if providerTokens == 0 {
		providerTokens = usage.InputTokens
	}
	return core.RerankResult{
		ID: wire.ID, Model: request.Model, Results: results, Documents: int64(len(request.Documents)), ProviderTokens: providerTokens,
		ProviderUsage: append(json.RawMessage(nil), wire.Usage...),
	}, nil
}

func (a *Adapter) post(ctx context.Context, path string, payload any, target any) error {
	ctx, cancel := context.WithTimeout(ctx, a.requestTimeout)
	defer cancel()
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	endpoint := *a.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + path
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+a.apiKey)
	request.Header.Set("Content-Type", "application/json")
	for key, value := range a.headers {
		request.Header.Set(key, value)
	}
	response, err := a.client.Do(request)
	if err != nil {
		return provider.NewExecutionError(err, true)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return provider.NewExecutionError(err, true)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return provider.NewExecutionError(
			fmt.Errorf("Provider capability request failed with status %d", response.StatusCode), response.StatusCode >= 500,
		)
	}
	if err := json.Unmarshal(body, target); err != nil {
		return provider.NewExecutionError(fmt.Errorf("decode Provider capability response: %w", err), true)
	}
	return nil
}

func cloneHeaders(headers map[string]string) map[string]string {
	cloned := make(map[string]string, len(headers))
	for key, value := range headers {
		cloned[key] = value
	}
	return cloned
}
