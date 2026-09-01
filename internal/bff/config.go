// Package bff implements the backend-for-frontend serving the operations
// console SPA and proxying browser API calls to the gateway, control plane,
// and metering services. It never logs tokens, request bodies, prompts, or
// response bodies.
package bff

import "strings"

// Config carries the BFF runtime configuration. A token that is explicitly set
// to the empty string marks the corresponding upstream as not configured; its
// proxy then answers 503 upstream_not_configured instead of forwarding.
type Config struct {
	Addr string

	GatewayURL         string
	GatewayAPIKey      string
	GatewayConfigured  bool
	ControlPlaneURL    string
	ControlPlaneToken  string
	ControlConfigured  bool
	MeteringURL        string
	MeteringToken      string
	MeteringConfigured bool

	WebDist string
}

// LookupEnv matches os.LookupEnv so ConfigFromEnv stays testable.
type LookupEnv func(name string) (string, bool)

// ConfigFromEnv builds Config from the environment. Missing variables fall back
// to development defaults; a variable present but empty is honored as an
// explicit empty value (for tokens this means "upstream not configured").
func ConfigFromEnv(lookup LookupEnv) Config {
	value := func(name, fallback string) string {
		if v, ok := lookup(name); ok {
			return v
		}
		return fallback
	}
	token := func(name, fallback string) (string, bool) {
		if v, ok := lookup(name); ok {
			v = strings.TrimSpace(v)
			return v, v != ""
		}
		return fallback, true
	}
	cfg := Config{
		Addr:            value("BFF_ADDR", ":8090"),
		GatewayURL:      value("BFF_GATEWAY_URL", "http://localhost:8080"),
		ControlPlaneURL: value("BFF_CONTROL_PLANE_URL", "http://localhost:8081"),
		MeteringURL:     value("BFF_METERING_URL", "http://localhost:8082"),
		WebDist:         value("BFF_WEB_DIST", "web/dist"),
	}
	cfg.GatewayAPIKey, cfg.GatewayConfigured = token("BFF_GATEWAY_API_KEY", "dev-token")
	cfg.ControlPlaneToken, cfg.ControlConfigured = token("BFF_CONTROL_PLANE_TOKEN", "local-control-admin-token")
	cfg.MeteringToken, cfg.MeteringConfigured = token("BFF_METERING_TOKEN", "local-metering-admin-token")
	return cfg
}
