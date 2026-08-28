package secretcustody

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Token struct {
	AccessToken string
	ExpiresAt   time.Time
}

type TokenProvider interface {
	Token(context.Context) (Token, error)
}

type GCPConfig struct {
	ProjectID     string
	Endpoint      string
	HTTPClient    *http.Client
	TokenProvider TokenProvider
}

type GCP struct {
	projectID string
	endpoint  *url.URL
	client    *http.Client
	tokens    TokenProvider
}

func NewGCP(config GCPConfig) (*GCP, error) {
	if !resourceNamePart(config.ProjectID) || config.TokenProvider == nil {
		return nil, errors.New("GCP Secret Custody requires project ID and Workload Identity token provider")
	}
	endpointValue := strings.TrimSpace(config.Endpoint)
	if endpointValue == "" {
		endpointValue = "https://secretmanager.googleapis.com/v1"
	}
	endpoint, err := url.Parse(endpointValue)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil {
		return nil, errors.New("GCP Secret Manager endpoint must be absolute HTTPS")
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	} else {
		copy := *client
		if copy.Timeout <= 0 || copy.Timeout > 10*time.Second {
			copy.Timeout = 10 * time.Second
		}
		client = &copy
	}
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &GCP{projectID: config.ProjectID, endpoint: endpoint, client: client, tokens: config.TokenProvider}, nil
}

func resourceNamePart(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '-' || character == '_' || character == '.' {
			continue
		}
		return false
	}
	return true
}

func (store *GCP) Put(ctx context.Context, key string, material []byte) (Reference, error) {
	if key == "" || len(material) == 0 || len(material) > 64<<10 {
		return Reference{}, errors.New("bounded Secret Custody key and material are required")
	}
	digest := sha256.Sum256([]byte(key))
	secretID := "llm-gateway-pc-" + hex.EncodeToString(digest[:20])
	parent := "projects/" + url.PathEscape(store.projectID)
	secretName := parent + "/secrets/" + secretID
	status, err := store.request(ctx, http.MethodPost, parent+"/secrets?secretId="+url.QueryEscape(secretID), map[string]any{
		"replication": map[string]any{"automatic": map[string]any{}},
	}, nil)
	if err != nil && status != http.StatusConflict {
		return Reference{}, err
	}
	if status == http.StatusConflict {
		reference := Reference{Name: secretName + "/versions/latest", Version: "latest"}
		current, accessErr := store.Access(ctx, reference)
		if accessErr == nil {
			defer clear(current)
			if bytes.Equal(current, material) {
				return reference, nil
			}
			return Reference{}, ErrConflict
		}
	}
	var response struct {
		Name string `json:"name"`
	}
	_, err = store.request(ctx, http.MethodPost, secretName+":addVersion", map[string]any{
		"payload": map[string]string{"data": base64.StdEncoding.EncodeToString(material)},
	}, &response)
	if err != nil {
		return Reference{}, err
	}
	if !strings.HasPrefix(response.Name, secretName+"/versions/") {
		return Reference{}, errors.New("GCP Secret Manager returned an invalid version reference")
	}
	parts := strings.Split(response.Name, "/")
	return Reference{Name: response.Name, Version: parts[len(parts)-1]}, nil
}

func (store *GCP) Access(ctx context.Context, reference Reference) ([]byte, error) {
	if !strings.HasPrefix(reference.Name, "projects/"+store.projectID+"/secrets/") || !strings.Contains(reference.Name, "/versions/") {
		return nil, ErrNotFound
	}
	var response struct {
		Payload struct {
			Data string `json:"data"`
		} `json:"payload"`
	}
	status, err := store.request(ctx, http.MethodGet, reference.Name+":access", nil, &response)
	if status == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	material, err := base64.StdEncoding.DecodeString(response.Payload.Data)
	if err != nil || len(material) == 0 || len(material) > 64<<10 {
		return nil, errors.New("GCP Secret Manager returned invalid secret material")
	}
	return material, nil
}

func (store *GCP) request(ctx context.Context, method, resource string, body any, destination any) (int, error) {
	endpoint := *store.endpoint
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/" + strings.TrimLeft(strings.Split(resource, "?")[0], "/")
	if index := strings.IndexByte(resource, '?'); index >= 0 {
		endpoint.RawQuery = resource[index+1:]
	}
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), reader)
	if err != nil {
		return 0, err
	}
	token, err := store.tokens.Token(ctx)
	if err != nil || token.AccessToken == "" {
		return 0, errors.New("obtain Secret Manager Workload Identity token")
	}
	request.Header.Set("Authorization", "Bearer "+token.AccessToken)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := store.client.Do(request)
	if err != nil {
		return 0, errors.New("GCP Secret Manager request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return response.StatusCode, fmt.Errorf("GCP Secret Manager status %d", response.StatusCode)
	}
	if destination == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return response.StatusCode, nil
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(payload) > 1<<20 || json.Unmarshal(payload, destination) != nil {
		return response.StatusCode, errors.New("decode bounded GCP Secret Manager response")
	}
	return response.StatusCode, nil
}

type MetadataTokenProvider struct {
	client *http.Client
	mu     sync.Mutex
	token  Token
}

func NewMetadataTokenProvider() *MetadataTokenProvider {
	return &MetadataTokenProvider{client: &http.Client{
		Timeout:       2 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (provider *MetadataTokenProvider) Token(ctx context.Context) (Token, error) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.token.AccessToken != "" && time.Until(provider.token.ExpiresAt) > time.Minute {
		return provider.token, nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token", nil)
	if err != nil {
		return Token{}, err
	}
	request.Header.Set("Metadata-Flavor", "Google")
	response, err := provider.client.Do(request)
	if err != nil {
		return Token{}, errors.New("Workload Identity metadata token request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Metadata-Flavor") != "Google" {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return Token{}, errors.New("Workload Identity metadata token rejected")
	}
	var payload struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64<<10))
	if err := decoder.Decode(&payload); err != nil || payload.AccessToken == "" || payload.ExpiresIn <= 0 {
		return Token{}, errors.New("invalid Workload Identity metadata token")
	}
	provider.token = Token{AccessToken: payload.AccessToken, ExpiresAt: time.Now().Add(time.Duration(payload.ExpiresIn) * time.Second)}
	return provider.token, nil
}
