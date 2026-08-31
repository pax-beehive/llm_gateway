package main

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/toddzheng/llm-gateway/internal/provider"
	"github.com/toddzheng/llm-gateway/internal/routingcatalog"
	"github.com/toddzheng/llm-gateway/internal/secretcustody"
)

func TestCanaryConfigurationIsBoundedAndConservative(t *testing.T) {
	tenant, key, document := canaryConfiguration()
	if tenant.Limits.DailySpendMicros == nil || *tenant.Limits.DailySpendMicros != 100_000 ||
		tenant.Limits.MaxOutputTokens == nil || *tenant.Limits.MaxOutputTokens != 256 {
		t.Fatalf("tenant limits = %#v", tenant.Limits)
	}
	if key.AllowedPublicModels == nil || len(*key.AllowedPublicModels) != 1 || (*key.AllowedPublicModels)[0] != canaryProviderModel {
		t.Fatalf("key models = %#v", key.AllowedPublicModels)
	}
	if len(document.Routes) != 1 {
		t.Fatalf("route count = %d", len(document.Routes))
	}
	route := document.Routes[0]
	if route.ProviderConnectionID != canaryConnectionID || route.ProviderModel != canaryProviderModel ||
		route.ProviderCostSnapshot.InputPerMillionMicros != 200_000 || route.ProviderCostSnapshot.CachedInputPerMillionMicros != 200_000 ||
		route.ProviderCostSnapshot.OutputPerMillionMicros != 1_200_000 {
		t.Fatalf("route = %#v", route)
	}
	healthy := true
	report := routingcatalog.Validate(context.Background(), document, routingcatalog.ConnectionLookupFunc(func(context.Context, string) (routingcatalog.ConnectionDescriptor, error) {
		return routingcatalog.ConnectionDescriptor{
			ID: canaryConnectionID, Provider: "openai", Region: canaryRegion, CredentialScope: "production-primary",
			Enabled: true, ObservedHealthy: &healthy, Revision: 2, CredentialVersion: 1,
			CapabilityProfile: provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{
				"text": provider.CapabilityNative, "streaming": provider.CapabilityNative,
			}},
		}, nil
	}))
	if !report.Valid {
		t.Fatalf("validation = %#v", report)
	}
}

func TestGatewayKeyStoreChecksAndAddsVersionWithoutLeakingBodies(t *testing.T) {
	material := []byte("gw_test_material_that_is_long_enough")
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Header.Get("Authorization") != "Bearer workload-token" {
			t.Fatalf("authorization = %q", request.Header.Get("Authorization"))
		}
		if calls == 1 {
			return response(request, http.StatusOK, `{"name":"projects/project-a/secrets/canary-key"}`), nil
		}
		if calls == 2 {
			return response(request, http.StatusNotFound, `{"error":{"message":"no versions"}}`), nil
		}
		body, _ := io.ReadAll(request.Body)
		if request.Method != http.MethodPost || !strings.HasSuffix(request.URL.Path, ":addVersion") ||
			!strings.Contains(string(body), base64.StdEncoding.EncodeToString(material)) {
			t.Fatalf("add version request = %s %s %s", request.Method, request.URL.String(), body)
		}
		return response(request, http.StatusOK, `{"name":"projects/123/secrets/canary-key/versions/1"}`), nil
	})}
	store, err := newGatewayKeyStore("project-a", "canary-key", "https://secretmanager.test/v1", client, staticTokenProvider{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RequireSecret(context.Background()); err != nil {
		t.Fatal(err)
	}
	exists, err := store.HasVersion(context.Background())
	if err != nil || exists {
		t.Fatalf("exists = %v, err = %v", exists, err)
	}
	if err := store.AddVersion(context.Background(), material); err != nil {
		t.Fatal(err)
	}

	secretBody := "must-not-escape"
	store.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return response(request, http.StatusForbidden, secretBody), nil
	})
	if err := store.AddVersion(context.Background(), material); err == nil || strings.Contains(err.Error(), secretBody) {
		t.Fatalf("unsafe error = %v", err)
	}
}

type staticTokenProvider struct{}

func (staticTokenProvider) Token(context.Context) (secretcustody.Token, error) {
	return secretcustody.Token{AccessToken: "workload-token"}, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func response(request *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: request}
}
