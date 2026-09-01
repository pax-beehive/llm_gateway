package bff

import (
	"context"
	"net/http"
	"strings"
)

type sessionContextKey struct{}

func accessTokenFromContext(ctx context.Context) string {
	session, _ := ctx.Value(sessionContextKey{}).(*sessionView)
	if session == nil {
		return ""
	}
	return session.accessToken
}

type permissionRequirement struct {
	Any []string
}

func authorizeBusinessRequest(auth *authService, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requirement, exposed := permissionForRequest(r)
		if !exposed {
			writeError(w, http.StatusNotFound, "not_found", "BFF route is not exposed")
			return
		}
		session, failure := auth.authenticate(w, r)
		if failure != nil {
			writeError(w, failure.Status, failure.Code, failure.Message)
			return
		}
		if failure := auth.checkMutationOrigin(r); failure != nil {
			writeError(w, failure.Status, failure.Code, failure.Message)
			return
		}
		if !hasAnyPermission(session.Permissions, requirement.Any) {
			writeError(w, http.StatusForbidden, "permission_denied", "You do not have permission to perform this action")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionContextKey{}, session)))
	})
}

func hasAnyPermission(granted, required []string) bool {
	if len(required) == 0 {
		return true
	}
	for _, got := range granted {
		for _, want := range required {
			if got == want {
				return true
			}
		}
	}
	return false
}

func permissionForRequest(r *http.Request) (permissionRequirement, bool) {
	path := r.URL.Path
	switch path {
	case "/api/llm/healthz", "/api/llm/readyz", "/api/control/healthz", "/api/control/readyz", "/api/metering/healthz", "/api/metering/readyz":
		return permissionRequirement{}, r.Method == http.MethodGet || r.Method == http.MethodHead
	case "/api/llm/models":
		return permissionRequirement{Any: []string{"gateway:models:read", "gateway:playground:use"}}, r.Method == http.MethodGet
	case "/api/llm/responses":
		return permissionRequirement{Any: []string{"gateway:playground:use"}}, r.Method == http.MethodPost
	}

	if strings.HasPrefix(path, "/api/control/v1/tenants") {
		return readWritePermission(r.Method, "platform:tenants:read", "platform:tenants:write")
	}
	if strings.HasPrefix(path, "/api/control/v1/provider-connections") {
		if r.Method == http.MethodGet {
			return permissionRequirement{Any: []string{"platform:providers:read", "gateway:models:read"}}, true
		}
		return readWritePermission(r.Method, "platform:providers:read", "platform:providers:write")
	}
	if strings.HasPrefix(path, "/api/control/v1/provider-operations") {
		if r.Method != http.MethodGet {
			return permissionRequirement{}, false
		}
		return permissionRequirement{Any: []string{"platform:providers:read", "platform:routing:read"}}, true
	}
	if strings.HasPrefix(path, "/api/control/v1/routing-catalog") {
		if r.Method == http.MethodGet {
			return permissionRequirement{Any: []string{"platform:routing:read", "gateway:models:read"}}, true
		}
		return readWritePermission(r.Method, "platform:routing:read", "platform:routing:write")
	}
	if strings.HasPrefix(path, "/api/control/v1/routing-publications") {
		return readWritePermission(r.Method, "platform:routing:read", "platform:routing:write")
	}
	if strings.HasPrefix(path, "/api/control/v1/operations") || strings.HasPrefix(path, "/api/control/v1/audit") {
		return permissionRequirement{Any: []string{"platform:operations:read"}}, r.Method == http.MethodGet
	}

	if strings.HasPrefix(path, "/api/metering/v1/") {
		switch {
		case strings.HasPrefix(path, "/api/metering/v1/usage/"),
			strings.HasPrefix(path, "/api/metering/v1/quota-denials"),
			strings.HasPrefix(path, "/api/metering/v1/responses/"),
			strings.HasPrefix(path, "/api/metering/v1/tenants/"),
			strings.HasPrefix(path, "/api/metering/v1/operations/"),
			strings.HasPrefix(path, "/api/metering/v1/admin/"):
			return readWritePermission(r.Method, "platform:metering:read", "platform:metering:write")
		}
	}
	return permissionRequirement{}, false
}

func readWritePermission(method, read, write string) (permissionRequirement, bool) {
	switch method {
	case http.MethodGet, http.MethodHead:
		return permissionRequirement{Any: []string{read}}, true
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return permissionRequirement{Any: []string{write}}, true
	default:
		return permissionRequirement{}, false
	}
}
