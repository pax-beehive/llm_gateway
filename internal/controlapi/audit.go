package controlapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/toddzheng/llm-gateway/internal/controlaudit"
	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

func (server *Server) routeAudit(writer http.ResponseWriter, request *http.Request, actor tenantadmin.ActorEnvelope) bool {
	if request.URL.Path != "/control/v1/audit" && request.URL.Path != "/control/v1/audit/" {
		return false
	}
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return true
	}
	if server.audit == nil {
		writeAPIError(writer, http.StatusServiceUnavailable, "service_unavailable", "Control Audit query is not configured")
		return true
	}
	limit, err := optionalInt(request.URL.Query().Get("limit"))
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", "limit must be an integer")
		return true
	}
	from, err := optionalTimestamp(request.URL.Query().Get("from"))
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", "from must be an RFC3339 timestamp")
		return true
	}
	through, err := optionalTimestamp(request.URL.Query().Get("through"))
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", "through must be an RFC3339 timestamp")
		return true
	}
	query := request.URL.Query()
	page, err := server.audit.List(request.Context(), actor, controlaudit.Filter{
		TenantID: query.Get("tenant_id"), AggregateType: query.Get("aggregate_type"), AggregateID: firstNonEmpty(query.Get("aggregate_id"), query.Get("resource")),
		ActorType: query.Get("actor_type"), ActorID: firstNonEmpty(query.Get("actor_id"), query.Get("actor")), Action: query.Get("action"),
		From: from, Through: through, Cursor: query.Get("cursor"), Limit: limit,
	})
	if err != nil {
		switch {
		case errors.Is(err, controlaudit.ErrInvalidArgument):
			writeAPIError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		case errors.Is(err, controlaudit.ErrPolicyDenied):
			writeAPIError(writer, http.StatusForbidden, "policy_denied", err.Error())
		default:
			writeAPIError(writer, http.StatusInternalServerError, "internal_error", "Control Audit query failed")
		}
		return true
	}
	writeJSON(writer, http.StatusOK, page)
	return true
}

func optionalTimestamp(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, value)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
