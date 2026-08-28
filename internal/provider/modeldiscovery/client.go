package modeldiscovery

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

type Provider string

const (
	OpenAI    Provider = "openai"
	DeepSeek  Provider = "deepseek"
	Anthropic Provider = "anthropic"
	Gemini    Provider = "gemini"
)

type Model struct {
	ID      string
	OwnedBy string
}

type Observation struct {
	Models          []Model
	RawResponseHash string
}

type RequestError struct {
	Provider   Provider
	StatusCode int
	Code       string
}

func (err *RequestError) Error() string {
	if err.StatusCode > 0 {
		return fmt.Sprintf("%s model discovery status %d (%s)", err.Provider, err.StatusCode, err.Code)
	}
	return fmt.Sprintf("%s model discovery failed (%s)", err.Provider, err.Code)
}

type Config struct {
	Provider   Provider
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
}

type Client struct {
	provider   Provider
	baseURL    *url.URL
	apiKey     string
	httpClient *http.Client
}

func New(config Config) (*Client, error) {
	switch config.Provider {
	case OpenAI, DeepSeek, Anthropic, Gemini:
	default:
		return nil, fmt.Errorf("unsupported model discovery provider %q", config.Provider)
	}
	if strings.TrimSpace(config.APIKey) == "" {
		return nil, errors.New("model discovery API key is required")
	}
	baseURL, err := url.Parse(config.BaseURL)
	if err != nil || baseURL.Scheme != "https" || baseURL.Host == "" {
		return nil, errors.New("model discovery base URL must be an absolute HTTPS URL")
	}
	baseURL.Path = strings.TrimRight(baseURL.Path, "/")
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)}
	} else {
		clientCopy := *client
		client = &clientCopy
	}
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{provider: config.Provider, baseURL: baseURL, apiKey: config.APIKey, httpClient: client}, nil
}

func (client *Client) List(ctx context.Context) ([]Model, error) {
	observation, err := client.ListObserved(ctx)
	return observation.Models, err
}

func (client *Client) ListObserved(ctx context.Context) (Observation, error) {
	hasher := sha256.New()
	var models []Model
	var err error
	switch client.provider {
	case OpenAI, DeepSeek:
		models, err = client.listOpenAICompatible(ctx, hasher)
	case Anthropic:
		models, err = client.listAnthropic(ctx, hasher)
	case Gemini:
		models, err = client.listGemini(ctx, hasher)
	default:
		err = fmt.Errorf("unsupported model discovery provider %q", client.provider)
	}
	if err != nil {
		return Observation{}, err
	}
	return Observation{Models: models, RawResponseHash: hex.EncodeToString(hasher.Sum(nil))}, nil
}

func (client *Client) listOpenAICompatible(ctx context.Context, observer io.Writer) ([]Model, error) {
	var response struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := client.get(ctx, "/models", nil, &response, observer); err != nil {
		return nil, err
	}
	models := make([]Model, 0, len(response.Data))
	for _, item := range response.Data {
		models = append(models, Model{ID: item.ID, OwnedBy: item.OwnedBy})
	}
	return uniqueModels(models), nil
}

func (client *Client) listAnthropic(ctx context.Context, observer io.Writer) ([]Model, error) {
	models := make([]Model, 0)
	afterID := ""
	for page := 0; page < 100; page++ {
		query := url.Values{"limit": []string{"1000"}}
		if afterID != "" {
			query.Set("after_id", afterID)
		}
		var response struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
			HasMore bool   `json:"has_more"`
			LastID  string `json:"last_id"`
		}
		if err := client.get(ctx, "/models", query, &response, observer); err != nil {
			return nil, err
		}
		for _, item := range response.Data {
			models = append(models, Model{ID: item.ID, OwnedBy: "anthropic"})
		}
		if !response.HasMore {
			return uniqueModels(models), nil
		}
		if response.LastID == "" || response.LastID == afterID {
			return nil, errors.New("Anthropic model discovery returned an invalid pagination cursor")
		}
		afterID = response.LastID
	}
	return nil, errors.New("Anthropic model discovery exceeded 100 pages")
}

func (client *Client) listGemini(ctx context.Context, observer io.Writer) ([]Model, error) {
	models := make([]Model, 0)
	pageToken := ""
	for page := 0; page < 100; page++ {
		query := url.Values{"pageSize": []string{"1000"}}
		if pageToken != "" {
			query.Set("pageToken", pageToken)
		}
		var response struct {
			Models []struct {
				Name                       string   `json:"name"`
				SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
			} `json:"models"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := client.get(ctx, "/models", query, &response, observer); err != nil {
			return nil, err
		}
		for _, item := range response.Models {
			if contains(item.SupportedGenerationMethods, "generateContent") {
				models = append(models, Model{ID: strings.TrimPrefix(item.Name, "models/"), OwnedBy: "google"})
			}
		}
		if response.NextPageToken == "" {
			return uniqueModels(models), nil
		}
		if response.NextPageToken == pageToken {
			return nil, errors.New("Gemini model discovery returned a repeated pagination token")
		}
		pageToken = response.NextPageToken
	}
	return nil, errors.New("Gemini model discovery exceeded 100 pages")
}

func (client *Client) get(ctx context.Context, path string, query url.Values, destination any, observer io.Writer) error {
	endpoint := *client.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + path
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	switch client.provider {
	case OpenAI, DeepSeek:
		request.Header.Set("Authorization", "Bearer "+client.apiKey)
	case Anthropic:
		request.Header.Set("x-api-key", client.apiKey)
		request.Header.Set("anthropic-version", "2023-06-01")
	case Gemini:
		request.Header.Set("x-goog-api-key", client.apiKey)
		request.Header.Set("x-goog-api-client", "llm-gateway/0.1.0")
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		return &RequestError{Provider: client.provider, Code: "network_error"}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		code := "unexpected_status"
		switch {
		case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
			code = "authentication_failed"
		case response.StatusCode == http.StatusTooManyRequests:
			code = "rate_limited"
		case response.StatusCode >= 500:
			code = "upstream_unavailable"
		}
		return &RequestError{Provider: client.provider, StatusCode: response.StatusCode, Code: code}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if err != nil || len(body) > 4<<20 {
		return &RequestError{Provider: client.provider, Code: "response_too_large"}
	}
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(body)))
	_, _ = observer.Write(length[:])
	_, _ = observer.Write(body)
	if err := json.Unmarshal(body, destination); err != nil {
		return &RequestError{Provider: client.provider, Code: "invalid_response"}
	}
	return nil
}

func SelectSmokeModel(provider Provider, models []Model) (string, error) {
	type policy struct {
		preferences []string
		excluded    []string
		required    string
	}
	policies := map[Provider]policy{
		OpenAI: {
			preferences: []string{"nano", "mini", "luna", "gpt-3.5"}, required: "gpt",
			excluded: []string{"audio", "realtime", "transcribe", "tts", "search", "image", "embedding", "moderation"},
		},
		DeepSeek:  {preferences: []string{"flash", "deepseek-chat"}, required: "deepseek"},
		Anthropic: {preferences: []string{"haiku", "sonnet"}, required: "claude"},
		Gemini: {
			preferences: []string{"flash-lite", "flash"}, required: "gemini",
			excluded: []string{"image", "live", "tts", "embedding", "veo", "lyria", "robotics", "computer-use", "deep-research"},
		},
	}
	selectedPolicy, ok := policies[provider]
	if !ok {
		return "", fmt.Errorf("unsupported smoke model provider %q", provider)
	}
	candidates := make([]string, 0, len(models))
	for _, model := range models {
		id := strings.TrimSpace(model.ID)
		lowerID := strings.ToLower(id)
		if id == "" || (selectedPolicy.required != "" && !strings.Contains(lowerID, selectedPolicy.required)) || containsSubstring(lowerID, selectedPolicy.excluded) {
			continue
		}
		candidates = append(candidates, id)
	}
	for _, preference := range selectedPolicy.preferences {
		for _, candidate := range candidates {
			if strings.Contains(strings.ToLower(candidate), preference) {
				return candidate, nil
			}
		}
	}
	return "", fmt.Errorf("no conservative text smoke model found for %s", provider)
}

func uniqueModels(models []Model) []Model {
	seen := make(map[string]struct{}, len(models))
	unique := make([]Model, 0, len(models))
	for _, model := range models {
		model.ID = strings.TrimSpace(model.ID)
		if model.ID == "" {
			continue
		}
		if _, exists := seen[model.ID]; exists {
			continue
		}
		seen[model.ID] = struct{}{}
		unique = append(unique, model)
	}
	return unique
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func containsSubstring(value string, candidates []string) bool {
	for _, candidate := range candidates {
		if strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}
