package bff

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type proxyRoundTripFunc func(*http.Request) (*http.Response, error)

func (function proxyRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestControlProxyUsesAuthenticatedWorkOSAccessToken(t *testing.T) {
	target, _ := url.Parse("https://control.example.test")
	proxy, err := newUpstreamProxy(target, "static-development-token", true, "", "/api", nil)
	if err != nil {
		t.Fatal(err)
	}
	proxy.Transport = proxyRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if got := request.Header.Get("Authorization"); got != "Bearer workos-access-token" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := request.Header.Get("Cookie"); got != "" {
			t.Fatalf("Cookie leaked upstream: %q", got)
		}
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{"ok":true}`)), Request: request,
		}, nil
	})
	request := httptest.NewRequest(http.MethodGet, "https://console.example.test/api/control/v1/tenants", nil)
	request.Header.Set("Authorization", "Bearer browser-supplied-token")
	request.Header.Set("Cookie", "ugw_session=sealed")
	request = request.WithContext(context.WithValue(request.Context(), sessionContextKey{}, &sessionView{accessToken: "workos-access-token"}))
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
}

func TestBFFHealthAndReadinessAreUnauthenticated(t *testing.T) {
	dist := t.TempDir()
	if err := os.WriteFile(filepath.Join(dist, "index.html"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Addr: "127.0.0.1:0", GatewayURL: "http://localhost:8080", GatewayAPIKey: "key", GatewayConfigured: true,
		ControlPlaneURL: "http://localhost:8081", ControlPlaneToken: "token", ControlConfigured: true,
		MeteringURL: "http://localhost:8082", MeteringToken: "token", MeteringConfigured: true,
		DevAuth: true, PublicURL: "http://localhost:5173", WebDist: dist,
	}
	handler, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/health", "/ready", "/healthz", "/readyz"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s status = %d", path, recorder.Code)
		}
	}
}
