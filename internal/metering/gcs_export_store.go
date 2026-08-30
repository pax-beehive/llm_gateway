package metering

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

const maxExportObjectBytes = 512 << 20

type GCSExportStoreConfig struct {
	Bucket      string
	Prefix      string
	Region      string
	Endpoint    string
	HTTPClient  *http.Client
	AccessToken func(context.Context) (string, error)
}

type GCSExportStore struct {
	bucket      string
	prefix      string
	endpoint    *url.URL
	client      *http.Client
	accessToken func(context.Context) (string, error)
}

func NewGCSExportStore(ctx context.Context, config GCSExportStoreConfig) (*GCSExportStore, error) {
	if !gcsNamePart(config.Bucket) || strings.TrimSpace(config.Region) == "" || config.AccessToken == nil {
		return nil, errors.New("GCS Metering exports require bucket, expected region, and Workload Identity")
	}
	prefix := strings.Trim(strings.TrimSpace(config.Prefix), "/")
	if prefix != "" && path.Clean(prefix) != prefix {
		return nil, errors.New("GCS Metering export prefix is invalid")
	}
	endpointValue := strings.TrimSpace(config.Endpoint)
	if endpointValue == "" {
		endpointValue = "https://storage.googleapis.com"
	}
	endpoint, err := url.Parse(endpointValue)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, errors.New("GCS Metering export endpoint must be absolute HTTPS")
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	} else {
		copy := *client
		if copy.Timeout <= 0 || copy.Timeout > 30*time.Second {
			copy.Timeout = 30 * time.Second
		}
		client = &copy
	}
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	store := &GCSExportStore{bucket: config.Bucket, prefix: prefix, endpoint: endpoint, client: client, accessToken: config.AccessToken}
	if err := store.verifyRegionalBucket(ctx, config.Region); err != nil {
		return nil, err
	}
	return store, nil
}

func (store *GCSExportStore) verifyRegionalBucket(ctx context.Context, expected string) error {
	target := store.endpointURL("storage/v1/b/" + url.PathEscape(store.bucket))
	query := target.Query()
	query.Set("fields", "name,location,locationType")
	target.RawQuery = query.Encode()
	response, err := store.do(ctx, http.MethodGet, target.String(), nil, "")
	if err != nil {
		return fmt.Errorf("verify regional GCS Metering export bucket: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		discardBounded(response.Body)
		return fmt.Errorf("verify regional GCS Metering export bucket: status %d", response.StatusCode)
	}
	var metadata struct {
		Name         string `json:"name"`
		Location     string `json:"location"`
		LocationType string `json:"locationType"`
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || len(payload) > 64<<10 || json.Unmarshal(payload, &metadata) != nil {
		return errors.New("verify regional GCS Metering export bucket: invalid metadata")
	}
	if metadata.Name != store.bucket || !strings.EqualFold(metadata.LocationType, "region") || !strings.EqualFold(metadata.Location, strings.TrimSpace(expected)) {
		return errors.New("GCS Metering export bucket is not in the expected single region")
	}
	return nil
}

func (store *GCSExportStore) Put(ctx context.Context, key string, payload []byte) error {
	object, err := store.objectName(key)
	if err != nil || len(payload) == 0 || len(payload) > maxExportObjectBytes {
		return ErrInvalidArgument
	}
	target := store.endpointURL("upload/storage/v1/b/" + url.PathEscape(store.bucket) + "/o")
	query := target.Query()
	query.Set("uploadType", "media")
	query.Set("name", object)
	query.Set("ifGenerationMatch", "0")
	target.RawQuery = query.Encode()
	response, err := store.do(ctx, http.MethodPost, target.String(), bytes.NewReader(payload), "text/csv; charset=utf-8")
	if err != nil {
		return errors.New("GCS Metering export upload failed")
	}
	defer response.Body.Close()
	discardBounded(response.Body)
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return nil
	}
	if response.StatusCode != http.StatusPreconditionFailed {
		return fmt.Errorf("GCS Metering export upload status %d", response.StatusCode)
	}
	existing, err := store.Get(ctx, key)
	if err != nil {
		return errors.New("verify existing GCS Metering export object")
	}
	if !bytes.Equal(existing, payload) {
		return errors.New("Metering export object already exists with different content")
	}
	return nil
}

func (store *GCSExportStore) Get(ctx context.Context, key string) ([]byte, error) {
	object, err := store.objectName(key)
	if err != nil {
		return nil, err
	}
	// The authenticated XML media endpoint keeps the object name in the URL
	// path and avoids the JSON API's encoded-slash ambiguity.
	target := store.endpointURL(store.bucket + "/" + object)
	response, err := store.do(ctx, http.MethodGet, target.String(), nil, "")
	if err != nil {
		return nil, errors.New("GCS Metering export download failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		discardBounded(response.Body)
		return nil, fmt.Errorf("GCS Metering export download status %d", response.StatusCode)
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxExportObjectBytes+1))
	if err != nil || len(payload) > maxExportObjectBytes {
		return nil, errors.New("GCS Metering export download is invalid or too large")
	}
	return payload, nil
}

func (store *GCSExportStore) objectName(key string) (string, error) {
	if path.Base(key) != key || key == "." || key == "" {
		return "", ErrInvalidArgument
	}
	if store.prefix == "" {
		return key, nil
	}
	return store.prefix + "/" + key, nil
}

func (store *GCSExportStore) endpointURL(resource string) *url.URL {
	target := *store.endpoint
	target.Path = strings.TrimRight(target.Path, "/") + "/" + strings.TrimLeft(resource, "/")
	return &target
}

func (store *GCSExportStore) do(ctx context.Context, method, target string, body io.Reader, contentType string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	token, err := store.accessToken(ctx)
	if err != nil || strings.TrimSpace(token) == "" {
		return nil, errors.New("obtain GCS Workload Identity token")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	return store.client.Do(request)
}

func gcsNamePart(value string) bool {
	if len(value) < 3 || len(value) > 222 || strings.HasPrefix(value, "goog") {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-' || character == '_' || character == '.' {
			continue
		}
		return false
	}
	return value[0] != '.' && value[len(value)-1] != '.'
}

func discardBounded(body io.Reader) { _, _ = io.Copy(io.Discard, io.LimitReader(body, 64<<10)) }
