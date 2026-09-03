package bff_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/toddzheng/llm-gateway/internal/bff"
	workos "github.com/workos/workos-go/v10"
)

func authConfig(t *testing.T, devAuth bool, permissions []string) bff.Config {
	t.Helper()
	cfg := testConfig(t, "http://127.0.0.1:1", "http://127.0.0.1:1", "http://127.0.0.1:1")
	cfg.DevAuth = devAuth
	cfg.DevAuthPermissions = permissions
	return cfg
}

func TestDevAuthSessionReturnsFixedSession(t *testing.T) {
	server := startBFF(t, authConfig(t, true, nil))
	resp, err := server.Client().Get(server.URL + "/api/auth/session")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var session struct {
		Authenticated bool `json:"authenticated"`
		User          struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"user"`
		Organization struct {
			ID string `json:"id"`
		} `json:"organization"`
		Role        string   `json:"role"`
		Permissions []string `json:"permissions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		t.Fatal(err)
	}
	if !session.Authenticated || session.User.ID == "" || session.Organization.ID == "" || session.Role == "" {
		t.Fatalf("incomplete session: %+v", session)
	}
	if len(session.Permissions) == 0 {
		t.Fatal("expected default permission set")
	}
}

func TestDevAuthSessionHonorsPermissionOverride(t *testing.T) {
	server := startBFF(t, authConfig(t, true, []string{"platform:tenants:read"}))
	resp, err := server.Client().Get(server.URL + "/api/auth/session")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var session struct {
		Permissions []string `json:"permissions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		t.Fatal(err)
	}
	if len(session.Permissions) != 1 || session.Permissions[0] != "platform:tenants:read" {
		t.Fatalf("permissions = %v", session.Permissions)
	}
}

func TestDevAuthLoginRedirectsToSameOriginReturnTo(t *testing.T) {
	server := startBFF(t, authConfig(t, true, nil))
	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	cases := map[string]string{
		"/%23%2Foverview":            "/#/overview",
		"/":                          "/",
		"":                           "/",
		"https%3A%2F%2Fevil.example": "/",
		"%2F%2Fevil.example":         "/",
		"%2F%5C%5Cevil.example":      "/",
		"%2F%5Cevil.example":         "/",
		"javascript%3Aalert(1)":      "/",
	}
	for query, want := range cases {
		url := server.URL + "/api/auth/login"
		if query != "" {
			url += "?return_to=" + query
		}
		resp, err := client.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("return_to=%q: status = %d", query, resp.StatusCode)
		}
		if got := resp.Header.Get("Location"); got != want {
			t.Errorf("return_to=%q: Location = %q, want %q", query, got, want)
		}
	}
}

func TestConfigFromEnvParsesAuthBoundaries(t *testing.T) {
	tests := []struct {
		name      string
		env       map[string]string
		wantDev   bool
		wantError bool
	}{
		{name: "true", env: map[string]string{"BFF_DEV_AUTH": "YES", "BFF_ADDR": "127.0.0.1:8090"}, wantDev: true},
		{name: "false", env: map[string]string{"BFF_DEV_AUTH": "0"}},
		{name: "invalid", env: map[string]string{"BFF_DEV_AUTH": "sometimes"}, wantError: true},
		{name: "dev auth non-loopback", env: map[string]string{"BFF_DEV_AUTH": "true", "BFF_PUBLIC_URL": "https://console.example.com"}, wantDev: true, wantError: true},
		{name: "dev auth wildcard bind", env: map[string]string{"BFF_DEV_AUTH": "true", "BFF_ADDR": ":8090"}, wantDev: true, wantError: true},
		{name: "partial WorkOS", env: map[string]string{"BFF_WORKOS_CLIENT_ID": "client_123"}, wantError: true},
		{name: "short cookie password", env: map[string]string{
			"BFF_WORKOS_API_KEY": "sk_test", "BFF_WORKOS_CLIENT_ID": "client_123",
			"BFF_WORKOS_COOKIE_PASSWORD": "short", "BFF_WORKOS_OPERATOR_ORGANIZATION_ID": "org_123",
		}, wantError: true},
		{name: "complete WorkOS", env: map[string]string{
			"BFF_PUBLIC_URL":     "https://console.example.com",
			"BFF_WORKOS_API_KEY": "sk_test", "BFF_WORKOS_CLIENT_ID": "client_123",
			"BFF_WORKOS_COOKIE_PASSWORD": strings.Repeat("x", 32), "BFF_WORKOS_OPERATOR_ORGANIZATION_ID": "org_123",
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := bff.ConfigFromEnv(func(name string) (string, bool) {
				v, ok := tc.env[name]
				return v, ok
			})
			if cfg.DevAuth != tc.wantDev {
				t.Fatalf("DevAuth = %v, want %v", cfg.DevAuth, tc.wantDev)
			}
			if tc.name == "complete WorkOS" && (!cfg.WorkOSConfigured || !cfg.SessionCookieSecure) {
				t.Fatalf("complete WorkOS config = configured:%v secure:%v", cfg.WorkOSConfigured, cfg.SessionCookieSecure)
			}
			_, err := bff.NewHandler(cfg)
			if (err != nil) != tc.wantError {
				t.Fatalf("NewHandler error = %v, wantError %v", err, tc.wantError)
			}
		})
	}
}

func TestDevAuthLogoutReturnsRedirect(t *testing.T) {
	server := startBFF(t, authConfig(t, true, nil))
	resp, err := server.Client().Post(server.URL+"/api/auth/logout", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		RedirectTo string `json:"redirect_to"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.RedirectTo != "/" {
		t.Fatalf("redirect_to = %q", body.RedirectTo)
	}
}

func TestAuthFailsClosedWithoutDevMode(t *testing.T) {
	server := startBFF(t, authConfig(t, false, nil))

	resp, err := server.Client().Get(server.URL + "/api/auth/session")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("session status = %d", resp.StatusCode)
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != "session_required" {
		t.Fatalf("code = %q", envelope.Error.Code)
	}

	for _, path := range []string{"/api/auth/login", "/api/auth/logout"} {
		req, _ := http.NewRequest(http.MethodPost, server.URL+path, nil)
		if strings.HasSuffix(path, "login") {
			req.Method = http.MethodGet
		}
		r, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusNotImplemented {
			t.Errorf("%s status = %d", path, r.StatusCode)
		}
	}
}

func TestWorkOSPKCECallbackSessionAuthorizationAndLogout(t *testing.T) {
	accessToken := testJWT(t, map[string]any{
		"sid": "session_123", "org_id": "org_operator", "role": "platform-viewer",
		"permissions": []string{"platform:tenants:read", "platform:tenants:write"}, "exp": time.Now().Add(time.Hour).Unix(),
	})
	var organizationLookupCalls int
	workOS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/user_management/authenticate":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["grant_type"] != "authorization_code" || body["code"] != "code_123" || body["code_verifier"] == "" {
				t.Errorf("authenticate body = %#v", body)
			}
			if _, ok := body["client_secret"]; ok || r.Header.Get("Authorization") != "" {
				t.Error("PKCE exchange must not send the WorkOS API key")
			}
			writeTestJSON(w, map[string]any{
				"user": map[string]any{
					"id": "user_123", "email": "ada@example.com", "first_name": "Ada", "last_name": "Lovelace",
					"profile_picture_url": nil,
				},
				"organization_id": "org_operator", "access_token": accessToken, "refresh_token": "refresh_secret",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/organizations/org_operator":
			organizationLookupCalls++
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer workOS.Close()

	var upstreamCalls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cfg := testConfig(t, upstream.URL, upstream.URL, upstream.URL)
	cfg.DevAuth = false
	cfg.WorkOSConfigured = true
	cfg.WorkOSAPIKey = "sk_test"
	cfg.WorkOSClientID = "client_123"
	cfg.WorkOSCookiePassword = strings.Repeat("c", 32)
	cfg.WorkOSOperatorOrganizationID = "org_operator"
	cfg.WorkOSBaseURL = workOS.URL
	cfg.PublicURL = "http://localhost:5173"
	cfg.SessionCookieSecure = false
	server := startBFF(t, cfg)
	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}

	login, err := client.Get(server.URL + "/api/auth/login?return_to=%2F%23%2Ftenants")
	if err != nil {
		t.Fatal(err)
	}
	login.Body.Close()
	if login.StatusCode != http.StatusFound {
		t.Fatalf("login status = %d", login.StatusCode)
	}
	authorizeURL, err := url.Parse(login.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if authorizeURL.Host != strings.TrimPrefix(workOS.URL, "http://") || authorizeURL.Query().Get("code_challenge") == "" || authorizeURL.Query().Get("organization_id") != "org_operator" {
		t.Fatalf("authorize URL = %s", authorizeURL)
	}
	loginCookie := cookieNamed(t, login.Cookies(), "ugw_login")
	if !loginCookie.HttpOnly || loginCookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("login cookie flags = %#v", loginCookie)
	}
	badCallbackReq, _ := http.NewRequest(http.MethodGet, server.URL+"/api/auth/callback?code=code_123&state=wrong", nil)
	badCallbackReq.AddCookie(loginCookie)
	badCallback, err := client.Do(badCallbackReq)
	if err != nil {
		t.Fatal(err)
	}
	badCallback.Body.Close()
	if badCallback.Header.Get("Location") != cfg.PublicURL+"/?auth_error=login_failed&auth_stage=state_mismatch" {
		t.Fatalf("failed callback location = %q", badCallback.Header.Get("Location"))
	}
	if cleared := cookieNamed(t, badCallback.Cookies(), "ugw_login"); cleared.MaxAge != -1 {
		t.Fatalf("login cookie MaxAge = %d", cleared.MaxAge)
	}

	callbackReq, _ := http.NewRequest(http.MethodGet, server.URL+"/api/auth/callback?code=code_123&state="+url.QueryEscape(authorizeURL.Query().Get("state")), nil)
	callbackReq.AddCookie(loginCookie)
	callback, err := client.Do(callbackReq)
	if err != nil {
		t.Fatal(err)
	}
	callback.Body.Close()
	if callback.StatusCode != http.StatusFound || callback.Header.Get("Location") != "/#/tenants" {
		t.Fatalf("callback status/location = %d / %q", callback.StatusCode, callback.Header.Get("Location"))
	}
	if cleared := cookieNamed(t, callback.Cookies(), "ugw_login"); cleared.MaxAge != -1 {
		t.Fatalf("successful callback login cookie MaxAge = %d", cleared.MaxAge)
	}
	sessionCookie := cookieNamed(t, callback.Cookies(), "ugw_session")
	if !sessionCookie.HttpOnly || strings.Contains(sessionCookie.Value, "refresh_secret") {
		t.Fatal("session cookie must be HttpOnly and sealed")
	}

	sessionReq, _ := http.NewRequest(http.MethodGet, server.URL+"/api/auth/session", nil)
	sessionReq.AddCookie(sessionCookie)
	sessionResp, err := client.Do(sessionReq)
	if err != nil {
		t.Fatal(err)
	}
	defer sessionResp.Body.Close()
	var view struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		Organization struct {
			Name string `json:"name"`
		} `json:"organization"`
		Permissions []string `json:"permissions"`
	}
	if err := json.NewDecoder(sessionResp.Body).Decode(&view); err != nil {
		t.Fatal(err)
	}
	if view.User.ID != "user_123" || view.Organization.Name != "org_operator" || len(view.Permissions) != 2 {
		t.Fatalf("session view = %+v", view)
	}
	if organizationLookupCalls != 0 {
		t.Fatalf("organization lookup calls = %d, want 0", organizationLookupCalls)
	}

	readReq, _ := http.NewRequest(http.MethodGet, server.URL+"/api/control/v1/tenants", nil)
	readReq.AddCookie(sessionCookie)
	readResp, err := client.Do(readReq)
	if err != nil {
		t.Fatal(err)
	}
	readResp.Body.Close()
	if readResp.StatusCode != http.StatusOK {
		t.Fatalf("authorized read status = %d", readResp.StatusCode)
	}

	for name, origin := range map[string]string{"missing": "", "cross origin": "https://evil.example", "same origin": cfg.PublicURL} {
		t.Run(name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, server.URL+"/api/control/v1/tenants", strings.NewReader(`{}`))
			req.AddCookie(sessionCookie)
			if origin != "" {
				req.Header.Set("Origin", origin)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			want := http.StatusForbidden
			if origin == cfg.PublicURL {
				want = http.StatusOK
			}
			if resp.StatusCode != want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, want)
			}
		})
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstream calls = %d, want the authorized read and same-origin mutation", upstreamCalls)
	}

	logoutReq, _ := http.NewRequest(http.MethodPost, server.URL+"/api/auth/logout", nil)
	logoutReq.AddCookie(sessionCookie)
	logoutReq.Header.Set("Origin", cfg.PublicURL)
	logoutResp, err := client.Do(logoutReq)
	if err != nil {
		t.Fatal(err)
	}
	defer logoutResp.Body.Close()
	var logoutBody struct {
		RedirectTo string `json:"redirect_to"`
	}
	if err := json.NewDecoder(logoutResp.Body).Decode(&logoutBody); err != nil {
		t.Fatal(err)
	}
	logoutURL, err := url.Parse(logoutBody.RedirectTo)
	if err != nil || logoutURL.Host != strings.TrimPrefix(workOS.URL, "http://") || logoutURL.Query().Get("session_id") != "session_123" {
		t.Fatalf("logout URL = %q", logoutBody.RedirectTo)
	}
	cleared := cookieNamed(t, logoutResp.Cookies(), "ugw_session")
	if cleared.MaxAge != -1 {
		t.Fatalf("session cookie MaxAge = %d", cleared.MaxAge)
	}
}

func TestWorkOSSessionRefreshRotatesSealedCookie(t *testing.T) {
	expiredToken := testJWT(t, map[string]any{
		"sid": "session_123", "org_id": "org_operator", "role": "platform-viewer",
		"permissions": []string{"platform:tenants:read"}, "exp": time.Now().Add(-time.Minute).Unix(),
	})
	freshToken := testJWT(t, map[string]any{
		"sid": "session_123", "org_id": "org_operator", "role": "platform-admin",
		"permissions": []string{"platform:tenants:read", "platform:tenants:write"}, "exp": time.Now().Add(time.Hour).Unix(),
	})
	var refreshCalls, organizationLookupCalls int
	workOS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user_management/authenticate":
			refreshCalls++
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["grant_type"] != "refresh_token" || body["refresh_token"] != "refresh_old" {
				t.Errorf("refresh body = %#v", body)
			}
			if _, ok := body["client_secret"]; ok || r.Header.Get("Authorization") != "" {
				t.Error("PKCE refresh must not send the WorkOS API key")
			}
			writeTestJSON(w, map[string]any{
				"user":            map[string]any{"id": "user_123", "email": "ada@example.com"},
				"organization_id": "org_operator", "access_token": freshToken, "refresh_token": "refresh_new",
			})
		case "/organizations/org_operator":
			organizationLookupCalls++
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer workOS.Close()

	password := strings.Repeat("r", 32)
	sealed, err := workos.Seal(map[string]any{
		"access_token":      expiredToken,
		"refresh_token":     "refresh_old",
		"user":              map[string]any{"id": "user_123", "email": "ada@example.com"},
		"organization_name": "Platform Operators",
	}, password)
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, "http://127.0.0.1:1", "http://127.0.0.1:1", "http://127.0.0.1:1")
	cfg.DevAuth = false
	cfg.WorkOSConfigured = true
	cfg.WorkOSAPIKey = "sk_test"
	cfg.WorkOSClientID = "client_123"
	cfg.WorkOSCookiePassword = password
	cfg.WorkOSOperatorOrganizationID = "org_operator"
	cfg.WorkOSBaseURL = workOS.URL
	server := startBFF(t, cfg)

	req, _ := http.NewRequest(http.MethodGet, server.URL+"/api/auth/session", nil)
	req.AddCookie(&http.Cookie{Name: "ugw_session", Value: sealed})
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || refreshCalls != 1 {
		t.Fatalf("status/refresh calls = %d / %d", resp.StatusCode, refreshCalls)
	}
	rotated := cookieNamed(t, resp.Cookies(), "ugw_session")
	if rotated.Value == sealed || strings.Contains(rotated.Value, "refresh_new") {
		t.Fatal("refresh must rotate to a newly sealed cookie")
	}
	var view struct {
		Organization struct {
			Name string `json:"name"`
		} `json:"organization"`
		Role        string   `json:"role"`
		Permissions []string `json:"permissions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		t.Fatal(err)
	}
	if view.Organization.Name != "Platform Operators" || view.Role != "platform-admin" || len(view.Permissions) != 2 {
		t.Fatalf("refreshed view = %+v", view)
	}
	if organizationLookupCalls != 0 {
		t.Fatalf("organization lookup calls = %d, want 0", organizationLookupCalls)
	}
}

func testJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func cookieNamed(t *testing.T, cookies []*http.Cookie, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range cookies {
		if cookie.Name == name {
			return cookie
		}
	}
	t.Fatalf("cookie %q not found in %v", name, cookies)
	return nil
}

func writeTestJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		panic(fmt.Sprintf("encode test response: %v", err))
	}
}
