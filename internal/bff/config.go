// Package bff implements the backend-for-frontend serving the operations
// console SPA and proxying browser API calls to the gateway, control plane,
// and metering services. It never logs tokens, request bodies, prompts, or
// response bodies.
package bff

import (
	"fmt"
	"net/url"
	"strings"
)

// Config carries the BFF runtime configuration. A token that is explicitly set
// to the empty string marks the corresponding upstream as not configured; its
// proxy then answers 503 upstream_not_configured instead of forwarding.
type Config struct {
	Addr string

	GatewayURL               string
	GatewayAPIKey            string
	GatewayConfigured        bool
	GatewayCloudRunAudience  string
	ControlPlaneURL          string
	ControlPlaneToken        string
	ControlConfigured        bool
	ControlCloudRunAudience  string
	MeteringURL              string
	MeteringToken            string
	MeteringConfigured       bool
	MeteringCloudRunAudience string

	// DevAuth enables the development auth mode: /api/auth/session returns a
	// fixed dev session. Never enable in production. DevAuthPermissions, when
	// non-nil, replaces the default full-permission set (to exercise
	// permission-gated UI locally).
	DevAuth            bool
	DevAuthPermissions []string

	// PublicURL is the browser-visible console origin. It is used to build the
	// WorkOS callback/logout URLs and to enforce same-origin mutation requests.
	PublicURL string

	// WorkOS credentials and the single organization allowed into the phase-one
	// operator console. WorkOSConfigured is true only when the complete set is
	// present; partial configuration is rejected by NewHandler.
	WorkOSAPIKey                 string
	WorkOSClientID               string
	WorkOSCookiePassword         string
	WorkOSOperatorOrganizationID string
	WorkOSBaseURL                string
	WorkOSConfigured             bool
	SessionCookieSecure          bool

	WebDist string

	configErr error
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
		Addr:                     value("BFF_ADDR", ":8090"),
		GatewayURL:               value("BFF_GATEWAY_URL", "http://localhost:8080"),
		GatewayCloudRunAudience:  strings.TrimSpace(value("BFF_GATEWAY_CLOUD_RUN_AUDIENCE", "")),
		ControlPlaneURL:          value("BFF_CONTROL_PLANE_URL", "http://localhost:8081"),
		ControlCloudRunAudience:  strings.TrimSpace(value("BFF_CONTROL_PLANE_CLOUD_RUN_AUDIENCE", "")),
		MeteringURL:              value("BFF_METERING_URL", "http://localhost:8082"),
		MeteringCloudRunAudience: strings.TrimSpace(value("BFF_METERING_CLOUD_RUN_AUDIENCE", "")),
		PublicURL:                strings.TrimRight(value("BFF_PUBLIC_URL", "http://localhost:5173"), "/"),
		WebDist:                  value("BFF_WEB_DIST", "web/dist"),
	}
	cfg.GatewayAPIKey, cfg.GatewayConfigured = token("BFF_GATEWAY_API_KEY", "dev-token")
	cfg.ControlPlaneToken, cfg.ControlConfigured = token("BFF_CONTROL_PLANE_TOKEN", "local-control-admin-token")
	cfg.MeteringToken, cfg.MeteringConfigured = token("BFF_METERING_TOKEN", "local-metering-admin-token")
	if v, ok := lookup("BFF_DEV_AUTH"); ok {
		var err error
		cfg.DevAuth, err = parseEnvBool("BFF_DEV_AUTH", v)
		if err != nil {
			cfg.configErr = err
		}
	}
	if v, ok := lookup("BFF_DEV_AUTH_PERMISSIONS"); ok {
		// Present (even empty) means explicit: empty string → no permissions.
		cfg.DevAuthPermissions = []string{}
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				cfg.DevAuthPermissions = append(cfg.DevAuthPermissions, p)
			}
		}
	}

	cfg.WorkOSAPIKey = strings.TrimSpace(value("BFF_WORKOS_API_KEY", ""))
	cfg.WorkOSClientID = strings.TrimSpace(value("BFF_WORKOS_CLIENT_ID", ""))
	cfg.WorkOSCookiePassword = strings.TrimSpace(value("BFF_WORKOS_COOKIE_PASSWORD", ""))
	cfg.WorkOSOperatorOrganizationID = strings.TrimSpace(value("BFF_WORKOS_OPERATOR_ORGANIZATION_ID", ""))
	cfg.WorkOSBaseURL = strings.TrimRight(strings.TrimSpace(value("BFF_WORKOS_BASE_URL", "")), "/")

	workOSValues := []string{
		cfg.WorkOSAPIKey,
		cfg.WorkOSClientID,
		cfg.WorkOSCookiePassword,
		cfg.WorkOSOperatorOrganizationID,
	}
	present := 0
	for _, v := range workOSValues {
		if v != "" {
			present++
		}
	}
	switch {
	case present == len(workOSValues):
		cfg.WorkOSConfigured = true
	case present != 0 && cfg.configErr == nil:
		cfg.configErr = fmt.Errorf("WorkOS auth requires BFF_WORKOS_API_KEY, BFF_WORKOS_CLIENT_ID, BFF_WORKOS_COOKIE_PASSWORD, and BFF_WORKOS_OPERATOR_ORGANIZATION_ID")
	}
	if cfg.WorkOSConfigured && len(cfg.WorkOSCookiePassword) < 32 && cfg.configErr == nil {
		cfg.configErr = fmt.Errorf("BFF_WORKOS_COOKIE_PASSWORD must be at least 32 characters")
	}

	publicURL, err := url.Parse(cfg.PublicURL)
	if err != nil || publicURL.Scheme == "" || publicURL.Host == "" || publicURL.Path != "" || publicURL.RawQuery != "" || publicURL.Fragment != "" {
		if cfg.configErr == nil {
			cfg.configErr = fmt.Errorf("BFF_PUBLIC_URL must be an origin such as https://console.example.com")
		}
	} else {
		cfg.SessionCookieSecure = publicURL.Scheme == "https"
	}
	if v, ok := lookup("BFF_SESSION_COOKIE_SECURE"); ok {
		secure, parseErr := parseEnvBool("BFF_SESSION_COOKIE_SECURE", v)
		if parseErr != nil && cfg.configErr == nil {
			cfg.configErr = parseErr
		} else {
			cfg.SessionCookieSecure = secure
		}
	}
	return cfg
}

func parseEnvBool(name, raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes":
		return true, nil
	case "0", "false", "no":
		return false, nil
	default:
		return false, fmt.Errorf("%s must be one of true/false, 1/0, or yes/no", name)
	}
}
