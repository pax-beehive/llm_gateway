package bff

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	workos "github.com/workos/workos-go/v10"
)

const (
	sessionCookieName = "ugw_session"
	loginCookieName   = "ugw_login"
	loginStateTTL     = 10 * time.Minute
)

var devPermissions = []string{
	"platform:tenants:read",
	"platform:tenants:write",
	"platform:metering:read",
	"platform:metering:write",
	"platform:providers:read",
	"platform:providers:write",
	"platform:routing:read",
	"platform:routing:write",
	"platform:operations:read",
	"gateway:models:read",
	"gateway:playground:use",
}

type sessionView struct {
	Authenticated bool        `json:"authenticated"`
	User          sessionUser `json:"user"`
	Organization  sessionOrg  `json:"organization"`
	Role          string      `json:"role"`
	Permissions   []string    `json:"permissions"`
}

type sessionUser struct {
	ID                string  `json:"id"`
	Email             string  `json:"email"`
	FirstName         string  `json:"first_name"`
	LastName          string  `json:"last_name"`
	ProfilePictureURL *string `json:"profile_picture_url"`
}

type sessionOrg struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type authFailure struct {
	Status  int
	Code    string
	Message string
}

type loginState struct {
	State        string `json:"state"`
	CodeVerifier string `json:"code_verifier"`
	ReturnTo     string `json:"return_to"`
	ExpiresAt    int64  `json:"expires_at"`
}

// appSessionData is compatible with workos.SessionData. The extra organization
// name stays inside the sealed cookie, avoiding an API call on each bootstrap.
type appSessionData struct {
	AccessToken      string                                   `json:"access_token"`
	RefreshToken     string                                   `json:"refresh_token"`
	User             *workos.User                             `json:"user,omitempty"`
	Impersonator     *workos.AuthenticateResponseImpersonator `json:"impersonator,omitempty"`
	OrganizationName string                                   `json:"organization_name,omitempty"`
}

type authService struct {
	cfg       Config
	client    *workos.Client
	dev       *sessionView
	publicURL *url.URL
}

func devSessionView(permissions []string) sessionView {
	if permissions == nil {
		permissions = devPermissions
	}
	return sessionView{
		Authenticated: true,
		User: sessionUser{
			ID:        "user_dev-operator",
			Email:     "dev-operator@localhost",
			FirstName: "Dev",
			LastName:  "Operator",
		},
		Organization: sessionOrg{ID: "org_dev-operators", Name: "Dev Operators"},
		Role:         "platform-admin",
		Permissions:  append([]string(nil), permissions...),
	}
}

func newAuthService(cfg Config) (*authService, error) {
	if cfg.configErr != nil {
		return nil, cfg.configErr
	}
	publicURL, err := url.Parse(cfg.PublicURL)
	if err != nil || publicURL.Scheme == "" || publicURL.Host == "" {
		return nil, fmt.Errorf("invalid BFF_PUBLIC_URL")
	}
	if publicURL.Scheme != "https" && !isLoopbackHost(publicURL.Hostname()) {
		return nil, fmt.Errorf("BFF_PUBLIC_URL must use HTTPS outside local development")
	}
	a := &authService{cfg: cfg, publicURL: publicURL}
	if cfg.DevAuth {
		if !isLoopbackHost(publicURL.Hostname()) {
			return nil, fmt.Errorf("BFF_DEV_AUTH is allowed only on a loopback public URL")
		}
		bindHost, _, splitErr := net.SplitHostPort(cfg.Addr)
		if splitErr != nil || !isLoopbackHost(bindHost) {
			return nil, fmt.Errorf("BFF_DEV_AUTH requires BFF_ADDR to bind a loopback address")
		}
		slog.Warn("bff dev auth mode enabled; never enable in production")
		session := devSessionView(cfg.DevAuthPermissions)
		a.dev = &session
		return a, nil
	}
	if !cfg.WorkOSConfigured {
		if cfg.WorkOSAPIKey != "" || cfg.WorkOSClientID != "" || cfg.WorkOSCookiePassword != "" || cfg.WorkOSOperatorOrganizationID != "" {
			return nil, fmt.Errorf("partial WorkOS configuration is not allowed")
		}
		return a, nil
	}
	if cfg.WorkOSAPIKey == "" || cfg.WorkOSClientID == "" || len(cfg.WorkOSCookiePassword) < 32 || cfg.WorkOSOperatorOrganizationID == "" {
		return nil, fmt.Errorf("incomplete WorkOS configuration")
	}
	if publicURL.Scheme == "https" && !cfg.SessionCookieSecure {
		return nil, fmt.Errorf("secure session cookies are required for an HTTPS public URL")
	}
	opts := []workos.ClientOption{
		workos.WithClientID(cfg.WorkOSClientID),
		workos.WithHTTPClient(&http.Client{Timeout: 10 * time.Second}),
		workos.WithMaxRetries(2),
	}
	if cfg.WorkOSBaseURL != "" {
		baseURL, parseErr := url.Parse(cfg.WorkOSBaseURL)
		if parseErr != nil || baseURL.Host == "" || (baseURL.Scheme != "https" && !isLoopbackHost(baseURL.Hostname())) {
			return nil, fmt.Errorf("BFF_WORKOS_BASE_URL must use HTTPS outside tests")
		}
		opts = append(opts, workos.WithBaseURL(cfg.WorkOSBaseURL))
	}
	a.client = workos.NewClient(cfg.WorkOSAPIKey, opts...)
	return a, nil
}

func registerAuthRoutes(mux *http.ServeMux, auth *authService) {
	mux.Handle("GET /api/auth/session", http.HandlerFunc(auth.handleSession))
	mux.Handle("GET /api/auth/login", http.HandlerFunc(auth.handleLogin))
	mux.Handle("GET /api/auth/callback", http.HandlerFunc(auth.handleCallback))
	mux.Handle("POST /api/auth/logout", http.HandlerFunc(auth.handleLogout))
}

func (a *authService) handleSession(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	session, failure := a.authenticate(w, r)
	if failure != nil {
		writeError(w, failure.Status, failure.Code, failure.Message)
		return
	}
	writeJSON(w, http.StatusOK, session)
}

func (a *authService) handleLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	returnTo := sanitizeReturnTo(r.URL.Query().Get("return_to"))
	if a.dev != nil {
		http.Redirect(w, r, returnTo, http.StatusFound)
		return
	}
	if a.client == nil {
		writeError(w, http.StatusNotImplemented, "auth_not_configured", "WorkOS AuthKit login is not configured on this BFF")
		return
	}
	provider := "authkit"
	organizationID := a.cfg.WorkOSOperatorOrganizationID
	result, err := a.client.GetAuthKitPKCEAuthorizationURL(workos.AuthKitAuthorizationURLParams{
		RedirectURI:    a.cfg.PublicURL + "/api/auth/callback",
		Provider:       &provider,
		OrganizationID: &organizationID,
	})
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "auth_unavailable", "Sign in is temporarily unavailable")
		return
	}
	sealed, err := workos.Seal(loginState{
		State:        result.State,
		CodeVerifier: result.CodeVerifier,
		ReturnTo:     returnTo,
		ExpiresAt:    time.Now().Add(loginStateTTL).Unix(),
	}, a.cfg.WorkOSCookiePassword)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "auth_unavailable", "Sign in is temporarily unavailable")
		return
	}
	a.setCookie(w, loginCookieName, sealed, "/api/auth", int(loginStateTTL.Seconds()))
	http.Redirect(w, r, result.URL, http.StatusFound)
}

func (a *authService) handleCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if a.client == nil {
		writeError(w, http.StatusNotImplemented, "auth_not_configured", "WorkOS AuthKit callback is not configured on this BFF")
		return
	}
	// Delete the one-time state cookie before any redirect commits headers.
	a.clearCookie(w, loginCookieName, "/api/auth")
	if r.URL.Query().Get("error") != "" {
		a.redirectAuthFailure(w, r)
		return
	}
	cookie, err := r.Cookie(loginCookieName)
	if err != nil {
		a.redirectAuthFailure(w, r)
		return
	}
	pending, err := workos.Unseal[loginState](cookie.Value, a.cfg.WorkOSCookiePassword)
	if err != nil || pending.ExpiresAt <= time.Now().Unix() || !constantTimeEqual(pending.State, r.URL.Query().Get("state")) {
		a.redirectAuthFailure(w, r)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		a.redirectAuthFailure(w, r)
		return
	}
	result, err := a.client.AuthKitPKCECodeExchange(r.Context(), workos.AuthKitPKCECodeExchangeParams{
		Code: code, CodeVerifier: pending.CodeVerifier,
	})
	if err != nil || result.User == nil || result.OrganizationID == nil || result.AccessToken == "" || result.RefreshToken == "" || *result.OrganizationID != a.cfg.WorkOSOperatorOrganizationID {
		a.redirectAuthFailure(w, r)
		return
	}
	organization, err := a.client.Organizations().Get(r.Context(), *result.OrganizationID)
	if err != nil || organization == nil || organization.ID != a.cfg.WorkOSOperatorOrganizationID {
		a.redirectAuthFailure(w, r)
		return
	}
	sealed, err := workos.Seal(appSessionData{
		AccessToken:      result.AccessToken,
		RefreshToken:     result.RefreshToken,
		User:             result.User,
		Impersonator:     result.Impersonator,
		OrganizationName: organization.Name,
	}, a.cfg.WorkOSCookiePassword)
	if err != nil {
		a.redirectAuthFailure(w, r)
		return
	}
	a.setCookie(w, sessionCookieName, sealed, "/", 0)
	http.Redirect(w, r, pending.ReturnTo, http.StatusFound)
}

func (a *authService) handleLogout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if a.dev != nil {
		writeJSON(w, http.StatusOK, map[string]string{"redirect_to": "/"})
		return
	}
	if a.client == nil {
		writeError(w, http.StatusNotImplemented, "auth_not_configured", "WorkOS AuthKit logout is not configured on this BFF")
		return
	}
	if failure := a.checkMutationOrigin(r); failure != nil {
		writeError(w, failure.Status, failure.Code, failure.Message)
		return
	}
	cookie, err := r.Cookie(sessionCookieName)
	a.clearCookie(w, sessionCookieName, "/")
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]string{"redirect_to": a.cfg.PublicURL})
		return
	}
	logoutURL, err := workos.NewSession(a.client, cookie.Value, a.cfg.WorkOSCookiePassword).GetLogoutURL(r.Context(), a.cfg.PublicURL)
	if err != nil || !a.isWorkOSURL(logoutURL, "/user_management/sessions/logout") {
		writeJSON(w, http.StatusOK, map[string]string{"redirect_to": a.cfg.PublicURL})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"redirect_to": logoutURL})
}

func (a *authService) authenticate(w http.ResponseWriter, r *http.Request) (*sessionView, *authFailure) {
	if a.dev != nil {
		view := *a.dev
		return &view, nil
	}
	if a.client == nil {
		return nil, sessionRequired()
	}
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return nil, sessionRequired()
	}
	sealed := cookie.Value
	session := workos.NewSession(a.client, sealed, a.cfg.WorkOSCookiePassword)
	result, err := session.Authenticate()
	if err != nil {
		a.clearCookie(w, sessionCookieName, "/")
		return nil, sessionRequired()
	}
	if result.NeedsRefresh {
		refreshed, _ := session.Refresh(r.Context())
		if refreshed != nil && refreshed.Authenticated && refreshed.Session != nil {
			organization, orgErr := a.client.Organizations().Get(r.Context(), result.OrganizationID)
			if orgErr != nil || organization == nil {
				return nil, sessionRefreshUnavailable()
			}
			sealed, err = workos.Seal(appSessionData{
				AccessToken:      refreshed.Session.AccessToken,
				RefreshToken:     refreshed.Session.RefreshToken,
				User:             refreshed.Session.User,
				Impersonator:     refreshed.Session.Impersonator,
				OrganizationName: organization.Name,
			}, a.cfg.WorkOSCookiePassword)
			if err != nil {
				return nil, sessionRefreshUnavailable()
			}
			a.setCookie(w, sessionCookieName, sealed, "/", 0)
			result, err = workos.NewSession(a.client, sealed, a.cfg.WorkOSCookiePassword).Authenticate()
			if err != nil {
				return nil, sessionRefreshUnavailable()
			}
		} else if refreshed != nil && refreshed.Reason == "refresh_token_revoked" {
			a.clearCookie(w, sessionCookieName, "/")
			return nil, &authFailure{http.StatusUnauthorized, "session_expired", "Your session has expired"}
		} else {
			return nil, sessionRefreshUnavailable()
		}
	}
	if !result.Authenticated || result.User == nil {
		a.clearCookie(w, sessionCookieName, "/")
		return nil, sessionRequired()
	}
	if result.OrganizationID != a.cfg.WorkOSOperatorOrganizationID {
		a.clearCookie(w, sessionCookieName, "/")
		return nil, &authFailure{http.StatusForbidden, "organization_denied", "This organization cannot access the operations console"}
	}
	data, err := workos.Unseal[appSessionData](sealed, a.cfg.WorkOSCookiePassword)
	if err != nil {
		a.clearCookie(w, sessionCookieName, "/")
		return nil, sessionRequired()
	}
	organizationName := data.OrganizationName
	if organizationName == "" {
		organizationName = result.OrganizationID
	}
	view := sessionView{
		Authenticated: true,
		User: sessionUser{
			ID:                result.User.ID,
			Email:             result.User.Email,
			FirstName:         stringValue(result.User.FirstName),
			LastName:          stringValue(result.User.LastName),
			ProfilePictureURL: result.User.ProfilePictureURL,
		},
		Organization: sessionOrg{ID: result.OrganizationID, Name: organizationName},
		Role:         result.Role,
		Permissions:  append([]string(nil), result.Permissions...),
	}
	return &view, nil
}

func (a *authService) checkMutationOrigin(r *http.Request) *authFailure {
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return nil
	}
	origin := r.Header.Get("Origin")
	if origin == "" && a.dev != nil {
		return nil
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != a.publicURL.Scheme || !strings.EqualFold(u.Host, a.publicURL.Host) || u.User != nil {
		return &authFailure{http.StatusForbidden, "csrf_denied", "Request origin is not allowed"}
	}
	return nil
}

func (a *authService) setCookie(w http.ResponseWriter, name, value, path string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: path, MaxAge: maxAge,
		HttpOnly: true, Secure: a.cfg.SessionCookieSecure, SameSite: http.SameSiteLaxMode,
	})
}

func (a *authService) clearCookie(w http.ResponseWriter, name, path string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: path, MaxAge: -1,
		HttpOnly: true, Secure: a.cfg.SessionCookieSecure, SameSite: http.SameSiteLaxMode,
	})
}

func (a *authService) redirectAuthFailure(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, a.cfg.PublicURL+"/?auth_error=login_failed", http.StatusFound)
}

func (a *authService) isWorkOSURL(raw, wantPath string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Path != wantPath {
		return false
	}
	base := "https://api.workos.com"
	if a.cfg.WorkOSBaseURL != "" {
		base = a.cfg.WorkOSBaseURL
	}
	b, err := url.Parse(base)
	return err == nil && u.Scheme == b.Scheme && strings.EqualFold(u.Host, b.Host)
}

func sanitizeReturnTo(returnTo string) string {
	if returnTo == "" || strings.Contains(returnTo, "\\") || !strings.HasPrefix(returnTo, "/") || strings.HasPrefix(returnTo, "//") {
		return "/"
	}
	u, err := url.ParseRequestURI(returnTo)
	if err != nil || u.IsAbs() || u.Host != "" {
		return "/"
	}
	return returnTo
}

func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func stringValue(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func isLoopbackHost(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}

func sessionRequired() *authFailure {
	return &authFailure{http.StatusUnauthorized, "session_required", "Sign in is required"}
}

func sessionRefreshUnavailable() *authFailure {
	return &authFailure{http.StatusServiceUnavailable, "session_refresh_unavailable", "Session refresh is temporarily unavailable"}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
