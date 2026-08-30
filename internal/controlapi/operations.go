package controlapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/toddzheng/llm-gateway/internal/operations"
	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

func (server *Server) receiveGatewayObservation(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(writer, http.MethodPost)
		return
	}
	if server.gatewayVerifier == nil || server.gatewayObservations == nil {
		writeAPIError(writer, http.StatusServiceUnavailable, "service_unavailable", "Gateway observations are not configured")
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, 256<<10+1))
	if err != nil || len(body) == 0 || len(body) > 256<<10 {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", "Gateway observation is invalid")
		return
	}
	identity, err := server.gatewayVerifier.Verify(request.Context(), request.Header.Get("Authorization"), request.Method, request.URL.RequestURI(), body)
	if err != nil {
		writer.Header().Set("WWW-Authenticate", "Gateway-HMAC")
		writeAPIError(writer, http.StatusUnauthorized, "invalid_gateway_identity", "Gateway identity is missing or invalid")
		return
	}
	var observation operations.GatewayObservation
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&observation); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", "Gateway observation is invalid")
		return
	}
	if err := server.gatewayObservations.RecordGatewayObservation(request.Context(), identity, observation); err != nil {
		if errors.Is(err, operations.ErrInvalidArgument) {
			writeAPIError(writer, http.StatusBadRequest, "invalid_request", "Gateway observation is invalid")
			return
		}
		writeAPIError(writer, http.StatusServiceUnavailable, "service_unavailable", "Gateway observation could not be recorded")
		return
	}
	writeJSON(writer, http.StatusAccepted, map[string]string{"status": "accepted"})
}

func (server *Server) routeOperations(writer http.ResponseWriter, request *http.Request, actor tenantadmin.ActorEnvelope) bool {
	const root = "/control/v1/operations"
	if request.URL.Path != root && !strings.HasPrefix(request.URL.Path, root+"/") {
		return false
	}
	if server.operations == nil {
		writeAPIError(writer, http.StatusServiceUnavailable, "service_unavailable", "Operations is not configured")
		return true
	}
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return true
	}
	remainder := strings.TrimPrefix(request.URL.Path, root)
	remainder = strings.Trim(remainder, "/")
	parts := []string{}
	if remainder != "" {
		parts = strings.Split(remainder, "/")
	}
	var value any
	var err error
	switch {
	case len(parts) == 1 && parts[0] == "gateways":
		value, err = server.operations.ListGateways(request.Context(), actor)
	case len(parts) == 2 && parts[0] == "gateways":
		value, err = server.operations.GetGateway(request.Context(), actor, operationPathID(parts[1]))
	case len(parts) == 1 && parts[0] == "publications":
		value, err = server.operations.ListPublications(request.Context(), actor)
	case len(parts) == 2 && parts[0] == "publications":
		value, err = server.operations.GetPublication(request.Context(), actor, operationPathID(parts[1]))
	case len(parts) == 1 && parts[0] == "outbox":
		value, err = server.operations.GetOutbox(request.Context(), actor)
	case len(parts) == 1 && parts[0] == "consumers":
		value, err = server.operations.ListConsumers(request.Context(), actor)
	case len(parts) == 1 && parts[0] == "jobs":
		value, err = server.operations.ListJobs(request.Context(), actor)
	case len(parts) == 2 && parts[0] == "jobs":
		value, err = server.operations.GetJob(request.Context(), actor, operationPathID(parts[1]))
	default:
		writeAPIError(writer, http.StatusNotFound, "not_found", "route not found")
		return true
	}
	if err != nil {
		switch {
		case errors.Is(err, operations.ErrPolicyDenied):
			writeAPIError(writer, http.StatusForbidden, "policy_denied", "Operations access is denied")
		case errors.Is(err, operations.ErrInvalidArgument):
			writeAPIError(writer, http.StatusBadRequest, "invalid_request", "Operations resource ID is invalid")
		case errors.Is(err, operations.ErrNotFound):
			writeAPIError(writer, http.StatusNotFound, "not_found", "Operations resource was not found")
		default:
			writeAPIError(writer, http.StatusInternalServerError, "internal_error", "Operations query failed")
		}
		return true
	}
	writeJSON(writer, http.StatusOK, value)
	return true
}

func operationPathID(encoded string) string {
	value, err := url.PathUnescape(encoded)
	if err != nil || value == "" || strings.Contains(value, "/") {
		return "!"
	}
	return value
}
