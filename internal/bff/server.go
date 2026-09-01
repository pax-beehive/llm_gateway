package bff

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/toddzheng/llm-gateway/internal/cloudrunidentity"
)

const maxLLMRequestBody = 1 << 20 // 1 MiB

// NewHandler builds the BFF HTTP handler: /api proxies plus the SPA fallback.
func NewHandler(cfg Config) (http.Handler, error) {
	auth, err := newAuthService(cfg)
	if err != nil {
		return nil, fmt.Errorf("BFF auth configuration: %w", err)
	}
	gatewayURL, err := url.Parse(cfg.GatewayURL)
	if err != nil {
		return nil, fmt.Errorf("BFF_GATEWAY_URL: %w", err)
	}
	controlURL, err := url.Parse(cfg.ControlPlaneURL)
	if err != nil {
		return nil, fmt.Errorf("BFF_CONTROL_PLANE_URL: %w", err)
	}
	meteringURL, err := url.Parse(cfg.MeteringURL)
	if err != nil {
		return nil, fmt.Errorf("BFF_METERING_URL: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if _, err := os.Stat(filepath.Join(cfg.WebDist, "index.html")); err != nil {
			writeError(w, http.StatusServiceUnavailable, "web_dist_missing", "web console build is unavailable")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})

	registerAuthRoutes(mux, auth)

	// Gateway LLM endpoints: /api/llm/X → /v1/X, except healthz/readyz which
	// pass through unchanged. No Idempotency-Key is added here; the gateway
	// owns request identity for Responses.
	gateway, err := newUpstreamProxy(gatewayURL, cfg.GatewayAPIKey, false, cfg.GatewayCloudRunAudience, "/api/llm", func(req *http.Request) {
		if req.URL.Path != "/healthz" && req.URL.Path != "/readyz" {
			req.URL.Path = "/v1" + req.URL.Path
		}
	})
	if err != nil {
		return nil, fmt.Errorf("BFF_GATEWAY_CLOUD_RUN_AUDIENCE: %w", err)
	}
	if !cfg.GatewayConfigured {
		gateway = nil
	}
	gatewayHandler := upstreamOrUnconfigured("/api/llm", gateway, cfg.GatewayConfigured)
	mux.Handle("GET /api/llm/models", authorizeBusinessRequest(auth, gatewayHandler))
	mux.Handle("POST /api/llm/responses", authorizeBusinessRequest(auth, limitBody(gatewayHandler, maxLLMRequestBody)))
	mux.Handle("GET /api/llm/healthz", authorizeBusinessRequest(auth, gatewayHandler))
	mux.Handle("GET /api/llm/readyz", authorizeBusinessRequest(auth, gatewayHandler))
	mux.Handle("/api/llm/", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "LLM BFF route is not exposed")
	}))

	// Control plane: strip /api, inject admin token, add Idempotency-Key on
	// mutations that do not already carry one. Health endpoints live at the
	// server root, not under /control, so remap them after the /api strip.
	control, err := newUpstreamProxy(controlURL, cfg.ControlPlaneToken, true, cfg.ControlCloudRunAudience, "/api", func(req *http.Request) {
		if req.URL.Path == "/control/healthz" || req.URL.Path == "/control/readyz" {
			req.URL.Path = strings.TrimPrefix(req.URL.Path, "/control")
		}
		ensureIdempotencyKey(req)
	})
	if err != nil {
		return nil, fmt.Errorf("BFF_CONTROL_PLANE_CLOUD_RUN_AUDIENCE: %w", err)
	}
	mux.Handle("/api/control/", authorizeBusinessRequest(auth, upstreamOrUnconfigured("/api/control/", control, cfg.ControlConfigured)))

	// Metering: strip /api, inject admin token. Health endpoints live at the
	// server root, not under /metering.
	metering, err := newUpstreamProxy(meteringURL, cfg.MeteringToken, true, cfg.MeteringCloudRunAudience, "/api", func(req *http.Request) {
		if req.URL.Path == "/metering/healthz" || req.URL.Path == "/metering/readyz" {
			req.URL.Path = strings.TrimPrefix(req.URL.Path, "/metering")
		}
	})
	if err != nil {
		return nil, fmt.Errorf("BFF_METERING_CLOUD_RUN_AUDIENCE: %w", err)
	}
	mux.Handle("/api/metering/", authorizeBusinessRequest(auth, upstreamOrUnconfigured("/api/metering/", metering, cfg.MeteringConfigured)))

	mux.Handle("/", spaHandler(cfg.WebDist))

	return logRequests(mux), nil
}

// newTransport returns a Transport with sane dial/TLS timeouts but no overall
// response timeout, so SSE streams stay open as long as the upstream streams.
func newTransport() *http.Transport {
	return &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		MaxIdleConns:          64,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// newUpstreamProxy reverse-proxies to target, stripping browser credentials and
// injecting the upstream bearer token. stripPrefix is removed from the request
// path before joining it with the target path. mutate may adjust the outbound
// request (e.g. add Idempotency-Key).
func newUpstreamProxy(target *url.URL, token string, useSessionToken bool, cloudRunAudience, stripPrefix string, mutate func(*http.Request)) (*httputil.ReverseProxy, error) {
	var transport http.RoundTripper = newTransport()
	if cloudRunAudience != "" {
		var err error
		transport, err = cloudrunidentity.NewTransport(cloudRunAudience, transport)
		if err != nil {
			return nil, err
		}
	}
	proxy := &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1, // flush each upstream write immediately (SSE correctness)
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.URL.Path = joinPath(target.Path, strings.TrimPrefix(req.URL.Path, stripPrefix))
			req.Host = target.Host
			req.Header.Del("Authorization")
			req.Header.Del("Cookie")
			upstreamToken := token
			if useSessionToken && accessTokenFromContext(req.Context()) != "" {
				upstreamToken = accessTokenFromContext(req.Context())
			}
			if upstreamToken != "" {
				req.Header.Set("Authorization", "Bearer "+upstreamToken)
			}
			if mutate != nil {
				mutate(req)
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
			if mediaType == "text/event-stream" {
				resp.Header.Del("Content-Length")
				resp.Header.Del("Content-Encoding")
				resp.Header.Set("Cache-Control", "no-cache, no-transform")
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			if errors.Is(err, http.ErrAbortHandler) {
				return
			}
			writeError(w, http.StatusBadGateway, "upstream_unavailable", "upstream request failed")
		},
	}
	return proxy, nil
}

func joinPath(base, p string) string {
	if base == "" || base == "/" {
		return p
	}
	return path.Join(base, p)
}

func upstreamOrUnconfigured(name string, proxy *httputil.ReverseProxy, configured bool) http.Handler {
	if !configured {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeError(w, http.StatusServiceUnavailable, "upstream_not_configured",
				fmt.Sprintf("upstream for %s is not configured on the BFF", name))
		})
	}
	return proxy
}

// limitBody caps request bodies with http.MaxBytesReader.
func limitBody(next http.Handler, maxBytes int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		next.ServeHTTP(w, r)
	})
}

// ensureIdempotencyKey adds a crypto-random Idempotency-Key to mutation methods
// that do not already carry one.
func ensureIdempotencyKey(req *http.Request) {
	switch req.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return
	}
	if req.Header.Get("Idempotency-Key") != "" {
		return
	}
	req.Header.Set("Idempotency-Key", newUUID())
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Errorf("bff: crypto/rand unavailable: %w", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"code": code, "message": message},
	})
}

// spaHandler serves the built console from dist and falls back to index.html
// for unknown non-/api paths (hash routing means deep links are rare, but the
// fallback keeps refreshes on any client-side path working).
func spaHandler(dist string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only GET is supported")
			return
		}
		clean := path.Clean("/" + strings.TrimPrefix(r.URL.Path, "/"))
		candidate := filepath.Join(dist, filepath.FromSlash(clean))
		if clean != "/" {
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				http.ServeFile(w, r, candidate)
				return
			}
		}
		index := filepath.Join(dist, "index.html")
		if _, err := os.Stat(index); err != nil {
			writeError(w, http.StatusServiceUnavailable, "web_dist_missing",
				"web console build not found; run `npm run build` in web/ or set BFF_WEB_DIST")
			return
		}
		http.ServeFile(w, r, index)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// Unwrap lets ResponseController reach the underlying flusher/hijacker.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// logRequests logs method, path, status, and duration. It must never log
// tokens, request bodies, prompts, or response bodies.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		wrapped := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(wrapped, r)
		slog.Info("bff request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", wrapped.status,
			"duration_ms", time.Since(started).Milliseconds(),
		)
	})
}
