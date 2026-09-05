package bff_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/toddzheng/llm-gateway/internal/bff"
)

func testConfig(t *testing.T, gatewayURL, controlURL, meteringURL string) bff.Config {
	return bff.Config{
		Addr:               "127.0.0.1:0",
		GatewayURL:         gatewayURL,
		GatewayAPIKey:      "gateway-key-1",
		GatewayConfigured:  true,
		ControlPlaneURL:    controlURL,
		ControlPlaneToken:  "control-token-1",
		ControlConfigured:  true,
		MeteringURL:        meteringURL,
		MeteringToken:      "metering-token-1",
		MeteringConfigured: true,
		DevAuth:            true,
		PublicURL:          "http://localhost:5173",
		WebDist:            t.TempDir(),
	}
}

func startBFF(t *testing.T, cfg bff.Config) *httptest.Server {
	t.Helper()
	handler, err := bff.NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func TestGatewayProxyInjectsBearerAndStripsBrowserCredentials(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer gateway-key-1" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Cookie"); got != "" {
			t.Errorf("Cookie leaked upstream: %q", got)
		}
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("X-Request-Id"); got != "req-123" {
			t.Errorf("X-Request-Id not propagated: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer upstream.Close()
	server := startBFF(t, testConfig(t, upstream.URL, "http://127.0.0.1:1", "http://127.0.0.1:1"))

	req, _ := http.NewRequest(http.MethodGet, server.URL+"/api/llm/models", nil)
	req.Header.Set("Authorization", "Bearer browser-credential")
	req.Header.Set("Cookie", "session=abc")
	req.Header.Set("X-Request-Id", "req-123")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestGatewayProxyExposesOnlyApprovedRoutes(t *testing.T) {
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := startBFF(t, testConfig(t, upstream.URL, "http://127.0.0.1:1", "http://127.0.0.1:1"))

	for _, path := range []string{
		"/api/llm/conversations",
		"/api/llm/files",
		"/api/llm/responses/resp_1",
	} {
		resp, err := server.Client().Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404", path, resp.StatusCode)
		}
	}
	if calls != 0 {
		t.Fatalf("unapproved routes reached Gateway %d time(s)", calls)
	}
}

func TestControlProxyStripsPrefixAndInjectsToken(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/control/v1/tenants" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer control-token-1" {
			t.Errorf("Authorization = %q", got)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer upstream.Close()
	server := startBFF(t, testConfig(t, "http://127.0.0.1:1", upstream.URL, "http://127.0.0.1:1"))

	resp, err := server.Client().Get(server.URL + "/api/control/v1/tenants")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestControlHealthEndpointsMapToServerRoot(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" && r.URL.Path != "/healthz" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer upstream.Close()
	server := startBFF(t, testConfig(t, "http://127.0.0.1:1", upstream.URL, "http://127.0.0.1:1"))

	for _, p := range []string{"/api/control/readyz", "/api/control/healthz"} {
		resp, err := server.Client().Get(server.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d", p, resp.StatusCode)
		}
	}
}

func TestControlMutationGeneratesIdempotencyKey(t *testing.T) {
	uuidPattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/control/v1/tenants":
			if !uuidPattern.MatchString(key) {
				t.Errorf("generated Idempotency-Key = %q", key)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/control/v1/tenants/tenant-a/transitions":
			if key != "caller-supplied" {
				t.Errorf("caller Idempotency-Key overwritten: %q", key)
			}
		case r.Method == http.MethodGet:
			if key != "" {
				t.Errorf("GET must not receive Idempotency-Key, got %q", key)
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := startBFF(t, testConfig(t, "http://127.0.0.1:1", upstream.URL, "http://127.0.0.1:1"))

	for name, send := range map[string]func() (*http.Response, error){
		"mutation": func() (*http.Response, error) {
			return server.Client().Post(server.URL+"/api/control/v1/tenants", "application/json", strings.NewReader(`{}`))
		},
		"caller key preserved": func() (*http.Response, error) {
			req, _ := http.NewRequest(http.MethodPost, server.URL+"/api/control/v1/tenants/tenant-a/transitions", strings.NewReader(`{}`))
			req.Header.Set("Idempotency-Key", "caller-supplied")
			return server.Client().Do(req)
		},
		"read": func() (*http.Response, error) {
			return server.Client().Get(server.URL + "/api/control/v1/tenants")
		},
	} {
		t.Run(name, func(t *testing.T) {
			resp, err := send()
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
		})
	}
}

func TestSSEStreamFlushesChunksIncrementally(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream ResponseWriter is not a Flusher")
			return
		}
		_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"id\":\"resp_1\"}\n\n")
		flusher.Flush()
		<-release // hold the second chunk until the client confirms the first
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"id\":\"resp_1\",\"status\":\"completed\"}\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()
	server := startBFF(t, testConfig(t, upstream.URL, "http://127.0.0.1:1", "http://127.0.0.1:1"))

	resp, err := server.Client().Post(server.URL+"/api/llm/responses", "application/json", strings.NewReader(`{"model":"m","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-cache, no-transform" {
		t.Fatalf("Cache-Control = %q", got)
	}

	reader := bufio.NewReader(resp.Body)
	first, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("first chunk did not arrive before upstream released the second: %v", err)
	}
	if !strings.Contains(first, "response.created") {
		t.Fatalf("first line = %q", first)
	}
	close(release)
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rest), "response.completed") {
		t.Fatalf("missing completion frame: %q", rest)
	}
}

func TestErrorEnvelopePassesThroughVerbatim(t *testing.T) {
	body := `{"error":{"code":"model_not_found","message":"no such model","type":"invalid_request_error","param":"model"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(body))
	}))
	defer upstream.Close()
	server := startBFF(t, testConfig(t, upstream.URL, "http://127.0.0.1:1", "http://127.0.0.1:1"))

	resp, err := server.Client().Get(server.URL + "/api/llm/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound || string(got) != body {
		t.Fatalf("status/body = %d / %s", resp.StatusCode, got)
	}
}

func TestUnconfiguredUpstreamReturns503Envelope(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1", "http://127.0.0.1:1", "http://127.0.0.1:1")
	cfg.MeteringToken, cfg.MeteringConfigured = "", false
	server := startBFF(t, cfg)

	resp, err := server.Client().Get(server.URL + "/api/metering/v1/usage/summary")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable || envelope.Error.Code != "upstream_not_configured" {
		t.Fatalf("status/code = %d / %q", resp.StatusCode, envelope.Error.Code)
	}
}

func TestBusinessRoutesRequireSession(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1", "http://127.0.0.1:1", "http://127.0.0.1:1")
	cfg.DevAuth = false
	server := startBFF(t, cfg)

	for _, path := range []string{
		"/api/llm/models",
		"/api/control/v1/tenants",
		"/api/metering/v1/usage/summary",
	} {
		resp, err := server.Client().Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s status = %d, want 401", path, resp.StatusCode)
		}
	}
}

func TestBusinessRoutesEnforceDevSessionPermissions(t *testing.T) {
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	cfg := testConfig(t, upstream.URL, upstream.URL, upstream.URL)
	cfg.DevAuthPermissions = []string{"platform:tenants:read"}
	server := startBFF(t, cfg)

	allowed, err := server.Client().Get(server.URL + "/api/control/v1/tenants")
	if err != nil {
		t.Fatal(err)
	}
	allowed.Body.Close()
	if allowed.StatusCode != http.StatusOK {
		t.Fatalf("allowed status = %d", allowed.StatusCode)
	}

	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/control/v1/tenants"},
		{http.MethodGet, "/api/llm/models"},
		{http.MethodGet, "/api/metering/v1/usage/summary"},
	} {
		req, _ := http.NewRequest(tc.method, server.URL+tc.path, strings.NewReader(`{}`))
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s status = %d, want 403", tc.method, tc.path, resp.StatusCode)
		}
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want only the authorized request", calls)
	}
}

func TestBusinessRoutePermissionMatrix(t *testing.T) {
	tests := []struct {
		name       string
		permission string
		method     string
		path       string
		want       int
	}{
		{"models lists models", "gateway:models:read", http.MethodGet, "/api/llm/models", http.StatusOK},
		{"playground lists models", "gateway:playground:use", http.MethodGet, "/api/llm/models", http.StatusOK},
		{"models reads provider metadata", "gateway:models:read", http.MethodGet, "/api/control/v1/provider-connections", http.StatusOK},
		{"models reads routing metadata", "gateway:models:read", http.MethodGet, "/api/control/v1/routing-catalog", http.StatusOK},
		{"providers read model inventory", "platform:providers:read", http.MethodGet, "/api/control/v1/provider-operations/op_1/models", http.StatusOK},
		{"model catalog cannot read provider inventory", "gateway:models:read", http.MethodGet, "/api/control/v1/provider-operations/op_1/models", http.StatusForbidden},
		{"providers read operations", "platform:providers:read", http.MethodGet, "/api/control/v1/provider-operations/op_1", http.StatusOK},
		{"routing reads provider operations", "platform:routing:read", http.MethodGet, "/api/control/v1/provider-operations/op_1", http.StatusOK},
		{"operations reads audit", "platform:operations:read", http.MethodGet, "/api/control/v1/audit", http.StatusOK},
		{"metering writes export", "platform:metering:write", http.MethodPost, "/api/metering/v1/usage/exports", http.StatusOK},
		{"read cannot mutate", "platform:routing:read", http.MethodPost, "/api/control/v1/routing-catalog/drafts", http.StatusForbidden},
		{"unknown control route is closed", "platform:tenants:write", http.MethodPost, "/api/control/v1/unknown", http.StatusNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
			defer upstream.Close()
			cfg := testConfig(t, upstream.URL, upstream.URL, upstream.URL)
			cfg.DevAuthPermissions = []string{tc.permission}
			server := startBFF(t, cfg)
			req, _ := http.NewRequest(tc.method, server.URL+tc.path, strings.NewReader(`{}`))
			resp, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestConfigFromEnvHonorsExplicitEmptyToken(t *testing.T) {
	cfg := bff.ConfigFromEnv(func(name string) (string, bool) {
		if name == "BFF_GATEWAY_API_KEY" {
			return "", true
		}
		return "", false
	})
	if cfg.GatewayConfigured {
		t.Fatal("explicitly empty BFF_GATEWAY_API_KEY must mark the gateway unconfigured")
	}
	if cfg.GatewayAPIKey != "" {
		t.Fatalf("GatewayAPIKey = %q", cfg.GatewayAPIKey)
	}
	if !cfg.ControlConfigured || cfg.ControlPlaneToken != "local-control-admin-token" {
		t.Fatal("missing control token must fall back to the dev default")
	}
	if cfg.Addr != ":8090" || cfg.WebDist != "web/dist" {
		t.Fatalf("defaults = %q / %q", cfg.Addr, cfg.WebDist)
	}
}

func TestResponsesBodyLimit(t *testing.T) {
	reached := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err == nil {
			reached <- struct{}{} // full oversized body reached upstream
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := startBFF(t, testConfig(t, upstream.URL, "http://127.0.0.1:1", "http://127.0.0.1:1"))

	oversized := strings.Repeat("x", 2<<20)
	resp, err := server.Client().Post(server.URL+"/api/llm/responses", "application/json", strings.NewReader(oversized))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	select {
	case <-reached:
		t.Fatal("oversized body reached the upstream in full")
	case <-time.After(time.Second):
	}
}

func TestSPAFallbackServesIndexHTML(t *testing.T) {
	dist := t.TempDir()
	if err := os.WriteFile(dist+"/index.html", []byte("<html>console</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, "http://127.0.0.1:1", "http://127.0.0.1:1", "http://127.0.0.1:1")
	cfg.WebDist = dist
	server := startBFF(t, cfg)

	for _, p := range []string{"/", "/some/client/path"} {
		resp, err := server.Client().Get(server.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(body), "console") {
			t.Fatalf("%s did not serve index.html: %q", p, body)
		}
	}
}
