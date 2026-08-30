package controlrelay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/toddzheng/llm-gateway/internal/controlevent"
	"github.com/toddzheng/llm-gateway/internal/operations"
)

type secretPublisherFunc func(context.Context, controlevent.Audience, string, int64, int64) ([]byte, error)

func (function secretPublisherFunc) PublishExecutionSecret(ctx context.Context, audience controlevent.Audience, connectionID string, revision, credentialVersion int64) ([]byte, error) {
	return function(ctx, audience, connectionID, revision, credentialVersion)
}

func TestSecretClientClassifiesRelayFailureAsTemporarilyUnavailable(t *testing.T) {
	now := time.Date(2026, 8, 29, 20, 30, 0, 0, time.UTC)
	client, err := NewClient("https://control.example.test", "gateway-a",
		[]byte("secret-client-hmac-key-with-at-least-32-bytes"), &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(http.NoBody)}, nil
		})}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.FetchExecutionSecret(context.Background(), "pc-openai", 7, 3); !errors.Is(err, controlevent.ErrExecutionSecretUnavailable) {
		t.Fatalf("execution secret error = %v, want temporary unavailable", err)
	}
}

func TestSecretHandlerReturnsOnlyExactRegionScopedVersion(t *testing.T) {
	now := time.Date(2026, 8, 29, 20, 0, 0, 0, time.UTC)
	key := []byte("secret-relay-hmac-key-with-at-least-32-bytes")
	verifier, err := operations.NewHMACVerifier(map[string]string{"gateway-a": string(key)}, map[string]string{"gateway-a": "us-west1"}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewSecretHandler(secretPublisherFunc(func(_ context.Context, audience controlevent.Audience, connectionID string, revision, credentialVersion int64) ([]byte, error) {
		if audience.Region != "us-west1" || connectionID != "pc-openai" || revision != 7 || credentialVersion != 3 {
			t.Fatalf("request = %#v %s %d %d", audience, connectionID, revision, credentialVersion)
		}
		return []byte("provider-secret"), nil
	}), verifier)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, SecretPathPrefix+"pc-openai?revision=7&credential_version=3", nil)
	authorization, err := operations.GatewayAuthorization(key, "gateway-a", now, request.Method, request.URL.RequestURI(), nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", authorization)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status = %d headers = %#v", response.Code, response.Header())
	}
	var payload struct {
		Material []byte `json:"material"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if string(payload.Material) != "provider-secret" {
		t.Fatalf("material = %q", payload.Material)
	}
}
