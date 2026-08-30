package controlrelay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/toddzheng/llm-gateway/internal/access"
	"github.com/toddzheng/llm-gateway/internal/controlevent"
	"github.com/toddzheng/llm-gateway/internal/operations"
	"github.com/toddzheng/llm-gateway/internal/providerconnection"
)

type publisherFunc func(context.Context, controlevent.Audience, int64, int) (controlevent.Batch, error)

type bootstrapPublisherFunc func(context.Context, controlevent.Audience) (Bootstrap, error)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func (function publisherFunc) Publish(ctx context.Context, audience controlevent.Audience, after int64, limit int) (controlevent.Batch, error) {
	return function(ctx, audience, after, limit)
}

func (function bootstrapPublisherFunc) PublishBootstrap(ctx context.Context, audience controlevent.Audience) (Bootstrap, error) {
	return function(ctx, audience)
}

func TestHandlerAuthenticatesAndScopesControlEventBatch(t *testing.T) {
	now := time.Date(2026, 8, 29, 18, 0, 0, 0, time.UTC)
	key := []byte("relay-test-hmac-key-with-at-least-32-bytes")
	verifier, err := operations.NewHMACVerifier(map[string]string{"gateway-a": string(key)}, map[string]string{"gateway-a": "us-west1"}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	var received controlevent.Audience
	handler, err := NewHandler(publisherFunc(func(_ context.Context, audience controlevent.Audience, after int64, limit int) (controlevent.Batch, error) {
		received = audience
		if after != 41 || limit != 17 {
			t.Fatalf("cursor = %d limit = %d", after, limit)
		}
		return controlevent.Batch{Events: []controlevent.Event{{EventID: "cevt-1", DeliverySequence: 42}}, NextCursor: 42, SourceHead: 42}, nil
	}), verifier)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, EventPath+"?after=41&limit=17", nil)
	authorization, err := operations.GatewayAuthorization(key, "gateway-a", now, request.Method, request.URL.RequestURI(), nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", authorization)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	if received != (controlevent.Audience{GatewayID: "gateway-a", Region: "us-west1"}) {
		t.Fatalf("audience = %#v", received)
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", response.Header().Get("Cache-Control"))
	}
	var batch controlevent.Batch
	if err := json.Unmarshal(response.Body.Bytes(), &batch); err != nil {
		t.Fatal(err)
	}
	if batch.NextCursor != 42 || batch.SourceHead != 42 || len(batch.Events) != 1 {
		t.Fatalf("batch = %#v", batch)
	}
}

func TestHandlerRejectsUnsignedRequests(t *testing.T) {
	verifier, err := operations.NewHMACVerifier(
		map[string]string{"gateway-a": "relay-test-hmac-key-with-at-least-32-bytes"},
		map[string]string{"gateway-a": "us-west1"}, time.Now,
	)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(publisherFunc(func(context.Context, controlevent.Audience, int64, int) (controlevent.Batch, error) {
		t.Fatal("publisher must not be called")
		return controlevent.Batch{}, nil
	}), verifier)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, EventPath, nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestHandlerReportsRetainedHistoryFloor(t *testing.T) {
	now := time.Date(2026, 8, 29, 18, 30, 0, 0, time.UTC)
	key := []byte("relay-history-hmac-key-with-at-least-32-bytes")
	verifier, err := operations.NewHMACVerifier(map[string]string{"gateway-a": string(key)}, map[string]string{"gateway-a": "us-west1"}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(publisherFunc(func(context.Context, controlevent.Audience, int64, int) (controlevent.Batch, error) {
		return controlevent.Batch{}, &HistoryUnavailableError{MinimumCursor: 41}
	}), verifier)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, EventPath+"?after=9&limit=23", nil)
	authorization, _ := operations.GatewayAuthorization(key, "gateway-a", now, request.Method, request.URL.RequestURI(), nil)
	request.Header.Set("Authorization", authorization)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status/cache = %d/%q", response.Code, response.Header().Get("Cache-Control"))
	}
	var payload struct {
		Error         string `json:"error"`
		MinimumCursor int64  `json:"minimum_cursor"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || payload.Error != "control_event_history_unavailable" || payload.MinimumCursor != 41 {
		t.Fatalf("payload = %#v/%v", payload, err)
	}
}

func TestBootstrapHandlerAndClientUseSignedNoStoreBoundary(t *testing.T) {
	now := time.Date(2026, 8, 29, 18, 45, 0, 0, time.UTC)
	key := []byte("relay-bootstrap-hmac-key-at-least-32-bytes")
	verifier, err := operations.NewHMACVerifier(map[string]string{"gateway-a": string(key)}, map[string]string{"gateway-a": "us-west1"}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewBootstrapHandler(bootstrapPublisherFunc(func(_ context.Context, audience controlevent.Audience) (Bootstrap, error) {
		if audience != (controlevent.Audience{GatewayID: "gateway-a", Region: "us-west1"}) {
			t.Fatalf("audience = %#v", audience)
		}
		return Bootstrap{SchemaVersion: bootstrapSchemaVersion, SourceCursor: 73, CreatedAt: now, Access: []access.Snapshot{}, ProviderConnections: []providerconnection.ExecutionSnapshot{}}, nil
	}), verifier)
	if err != nil {
		t.Fatal(err)
	}
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		result := response.Result()
		if result.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("Cache-Control = %q", result.Header.Get("Cache-Control"))
		}
		return result, nil
	})}
	client, err := NewClient("https://control.example.test", "gateway-a", key, httpClient, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := client.FetchBootstrap(context.Background())
	if err != nil || bootstrap.SourceCursor != 73 || bootstrap.Access == nil || bootstrap.ProviderConnections == nil {
		t.Fatalf("bootstrap = %#v/%v", bootstrap, err)
	}
}

func TestClientFetchesSignedBoundedBatch(t *testing.T) {
	now := time.Date(2026, 8, 29, 19, 0, 0, 0, time.UTC)
	key := []byte("relay-client-hmac-key-with-at-least-32-bytes")
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		verifier, err := operations.NewHMACVerifier(map[string]string{"gateway-a": string(key)}, map[string]string{"gateway-a": "us-west1"}, func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		if _, err := verifier.Verify(request.Context(), request.Header.Get("Authorization"), request.Method, request.URL.RequestURI(), nil); err != nil {
			t.Fatalf("authorization: %v", err)
		}
		if request.URL.RequestURI() != EventPath+"?after=9&limit=23" {
			t.Fatalf("request URI = %q", request.URL.RequestURI())
		}
		payload, _ := json.Marshal(controlevent.Batch{
			Events: []controlevent.Event{{EventID: "cevt-10", DeliverySequence: 10}}, NextCursor: 10, SourceHead: 10,
		})
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(payload))}, nil
	})}
	client, err := NewClient("https://control.example.test", "gateway-a", key, httpClient, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	batch, err := client.Fetch(context.Background(), 9, 23)
	if err != nil {
		t.Fatal(err)
	}
	if batch.NextCursor != 10 || batch.SourceHead != 10 || len(batch.Events) != 1 || batch.Events[0].EventID != "cevt-10" {
		t.Fatalf("batch = %#v", batch)
	}
}

func TestClientReturnsTypedHistoryUnavailableError(t *testing.T) {
	payload := []byte(`{"error":"control_event_history_unavailable","minimum_cursor":41}`)
	httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusConflict, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(payload))}, nil
	})}
	client, err := NewClient("https://control.example.test", "gateway-a", []byte("relay-client-hmac-key-with-at-least-32-bytes"), httpClient, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Fetch(context.Background(), 9, 23)
	var historyErr *HistoryUnavailableError
	if !errors.As(err, &historyErr) || historyErr.MinimumCursor != 41 {
		t.Fatalf("error = %v", err)
	}
}
