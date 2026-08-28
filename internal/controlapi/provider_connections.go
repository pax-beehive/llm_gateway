package controlapi

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/toddzheng/llm-gateway/internal/provider"
	"github.com/toddzheng/llm-gateway/internal/providerconnection"
	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

func (server *Server) routeProviderConnections(writer http.ResponseWriter, request *http.Request, actor tenantadmin.ActorEnvelope) bool {
	const connections = "/control/v1/provider-connections"
	const operations = "/control/v1/provider-operations"
	if request.URL.Path == operations || request.URL.Path == operations+"/" {
		return false
	}
	if strings.HasPrefix(request.URL.Path, operations+"/") {
		if server.providerConnections == nil {
			writeAPIError(writer, http.StatusServiceUnavailable, "service_unavailable", "Provider Connection Registry is not configured")
			return true
		}
		if request.Method != http.MethodGet {
			methodNotAllowed(writer, http.MethodGet)
			return true
		}
		operationID, err := onePathID(strings.TrimPrefix(request.URL.Path, operations+"/"))
		if err != nil {
			writeAPIError(writer, http.StatusBadRequest, "invalid_request", "invalid Provider operation ID")
			return true
		}
		operation, err := server.providerConnections.GetOperation(request.Context(), actor, operationID)
		if err != nil {
			writeProviderConnectionError(writer, err)
			return true
		}
		writeJSON(writer, http.StatusOK, operation)
		return true
	}
	if request.URL.Path != connections && request.URL.Path != connections+"/" && !strings.HasPrefix(request.URL.Path, connections+"/") {
		return false
	}
	if server.providerConnections == nil {
		writeAPIError(writer, http.StatusServiceUnavailable, "service_unavailable", "Provider Connection Registry is not configured")
		return true
	}
	if request.URL.Path == connections || request.URL.Path == connections+"/" {
		switch request.Method {
		case http.MethodPost:
			server.registerProviderConnection(writer, request, actor)
		case http.MethodGet:
			server.listProviderConnections(writer, request, actor)
		default:
			methodNotAllowed(writer, http.MethodGet, http.MethodPost)
		}
		return true
	}
	tail := strings.Split(strings.TrimPrefix(request.URL.Path, connections+"/"), "/")
	connectionID, err := onePathID(tail[0])
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", "invalid Provider Connection ID")
		return true
	}
	if len(tail) == 1 {
		switch request.Method {
		case http.MethodGet:
			server.getProviderConnection(writer, request, actor, connectionID)
		case http.MethodPatch:
			server.updateProviderConnection(writer, request, actor, connectionID)
		default:
			methodNotAllowed(writer, http.MethodGet, http.MethodPatch)
		}
		return true
	}
	if len(tail) != 2 || request.Method != http.MethodPost {
		if len(tail) == 2 {
			methodNotAllowed(writer, http.MethodPost)
		} else {
			writeAPIError(writer, http.StatusNotFound, "not_found", "route not found")
		}
		return true
	}
	switch tail[1] {
	case "enable", "disable":
		server.changeProviderConnectionStatus(writer, request, actor, connectionID, tail[1] == "enable")
	case "probes":
		server.requestProviderProbe(writer, request, actor, connectionID)
	case "model-discoveries":
		server.requestProviderDiscovery(writer, request, actor, connectionID)
	case "credential-rotations":
		server.requestProviderRotation(writer, request, actor, connectionID)
	default:
		writeAPIError(writer, http.StatusNotFound, "not_found", "route not found")
	}
	return true
}

type registerProviderConnectionRequest struct {
	ID                    string                     `json:"id"`
	Provider              string                     `json:"provider"`
	DisplayName           string                     `json:"display_name"`
	BaseURL               string                     `json:"base_url"`
	Region                string                     `json:"region"`
	CredentialScope       string                     `json:"credential_scope"`
	Secret                string                     `json:"secret"`
	CapabilityDeclaration provider.CapabilityProfile `json:"capability_declaration"`
	Reason                string                     `json:"reason"`
}

func (server *Server) registerProviderConnection(writer http.ResponseWriter, request *http.Request, actor tenantadmin.ActorEnvelope) {
	var input registerProviderConnectionRequest
	if !decodeBody(writer, request, &input) {
		return
	}
	actor.Reason = input.Reason
	result, err := server.providerConnections.Register(request.Context(), actor, request.Header.Get("Idempotency-Key"), providerconnection.RegisterCommand{
		ID: input.ID, Provider: input.Provider, DisplayName: input.DisplayName, BaseURL: input.BaseURL,
		Region: input.Region, CredentialScope: input.CredentialScope, Secret: []byte(input.Secret),
		CapabilityDeclaration: input.CapabilityDeclaration,
	})
	if err != nil {
		writeProviderConnectionError(writer, err)
		return
	}
	writeProviderConnectionMutationHeaders(writer, result)
	writer.Header().Set("Location", "/control/v1/provider-connections/"+url.PathEscape(result.Connection.ID))
	writeJSON(writer, http.StatusCreated, result.Connection)
}

func (server *Server) getProviderConnection(writer http.ResponseWriter, request *http.Request, actor tenantadmin.ActorEnvelope, connectionID string) {
	connection, err := server.providerConnections.Get(request.Context(), actor, connectionID)
	if err != nil {
		writeProviderConnectionError(writer, err)
		return
	}
	writer.Header().Set("ETag", etag(connection.Revision))
	writeJSON(writer, http.StatusOK, connection)
}

func (server *Server) listProviderConnections(writer http.ResponseWriter, request *http.Request, actor tenantadmin.ActorEnvelope) {
	limit, err := optionalInt(request.URL.Query().Get("limit"))
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", "limit must be an integer")
		return
	}
	page, err := server.providerConnections.List(request.Context(), actor, providerconnection.ConnectionFilter{
		Provider: request.URL.Query().Get("provider"), Region: request.URL.Query().Get("region"),
		Status: providerconnection.AdministrativeStatus(request.URL.Query().Get("status")),
		Cursor: request.URL.Query().Get("cursor"), Limit: limit,
	})
	if err != nil {
		writeProviderConnectionError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, page)
}

type updateProviderConnectionRequest struct {
	DisplayName           *string                     `json:"display_name"`
	BaseURL               *string                     `json:"base_url"`
	Region                *string                     `json:"region"`
	CredentialScope       *string                     `json:"credential_scope"`
	CapabilityDeclaration *provider.CapabilityProfile `json:"capability_declaration"`
	ExpectedRevision      int64                       `json:"expected_revision"`
	Reason                string                      `json:"reason"`
}

func (server *Server) updateProviderConnection(writer http.ResponseWriter, request *http.Request, actor tenantadmin.ActorEnvelope, connectionID string) {
	var input updateProviderConnectionRequest
	if !decodeBody(writer, request, &input) {
		return
	}
	revision, ok := expectedRevision(writer, request, input.ExpectedRevision)
	if !ok {
		return
	}
	actor.Reason = input.Reason
	result, err := server.providerConnections.Update(request.Context(), actor, request.Header.Get("Idempotency-Key"), providerconnection.UpdateCommand{
		ConnectionID: connectionID, ExpectedRevision: revision, DisplayName: input.DisplayName,
		BaseURL: input.BaseURL, Region: input.Region, CredentialScope: input.CredentialScope,
		CapabilityDeclaration: input.CapabilityDeclaration,
	})
	writeProviderConnectionMutation(writer, result, err)
}

type providerConnectionStatusRequest struct {
	ExpectedRevision int64  `json:"expected_revision"`
	Reason           string `json:"reason"`
}

func (server *Server) changeProviderConnectionStatus(writer http.ResponseWriter, request *http.Request, actor tenantadmin.ActorEnvelope, connectionID string, enable bool) {
	var input providerConnectionStatusRequest
	if !decodeBody(writer, request, &input) {
		return
	}
	revision, ok := expectedRevision(writer, request, input.ExpectedRevision)
	if !ok {
		return
	}
	actor.Reason = input.Reason
	command := providerconnection.StatusCommand{ConnectionID: connectionID, ExpectedRevision: revision}
	var result providerconnection.MutationResult
	var err error
	if enable {
		result, err = server.providerConnections.Enable(request.Context(), actor, request.Header.Get("Idempotency-Key"), command)
	} else {
		result, err = server.providerConnections.Disable(request.Context(), actor, request.Header.Get("Idempotency-Key"), command)
	}
	writeProviderConnectionMutation(writer, result, err)
}

func (server *Server) requestProviderProbe(writer http.ResponseWriter, request *http.Request, actor tenantadmin.ActorEnvelope, connectionID string) {
	server.requestProviderOperation(writer, request, actor, connectionID, providerconnection.OperationProbe)
}

func (server *Server) requestProviderDiscovery(writer http.ResponseWriter, request *http.Request, actor tenantadmin.ActorEnvelope, connectionID string) {
	server.requestProviderOperation(writer, request, actor, connectionID, providerconnection.OperationModelDiscovery)
}

func (server *Server) requestProviderOperation(writer http.ResponseWriter, request *http.Request, actor tenantadmin.ActorEnvelope, connectionID string, operationType providerconnection.OperationType) {
	var input providerConnectionStatusRequest
	if !decodeBody(writer, request, &input) {
		return
	}
	revision, ok := expectedRevision(writer, request, input.ExpectedRevision)
	if !ok {
		return
	}
	actor.Reason = input.Reason
	command := providerconnection.OperationCommand{ConnectionID: connectionID, ExpectedRevision: revision}
	var result providerconnection.OperationResult
	var err error
	if operationType == providerconnection.OperationProbe {
		result, err = server.providerConnections.RequestProbe(request.Context(), actor, request.Header.Get("Idempotency-Key"), command)
	} else {
		result, err = server.providerConnections.RequestDiscovery(request.Context(), actor, request.Header.Get("Idempotency-Key"), command)
	}
	writeProviderOperation(writer, result, err)
}

func (server *Server) requestProviderRotation(writer http.ResponseWriter, request *http.Request, actor tenantadmin.ActorEnvelope, connectionID string) {
	var input struct {
		ExpectedRevision int64  `json:"expected_revision"`
		Secret           string `json:"secret"`
		Reason           string `json:"reason"`
	}
	if !decodeBody(writer, request, &input) {
		return
	}
	revision, ok := expectedRevision(writer, request, input.ExpectedRevision)
	if !ok {
		return
	}
	actor.Reason = input.Reason
	result, err := server.providerConnections.RequestRotation(request.Context(), actor, request.Header.Get("Idempotency-Key"), providerconnection.RotationCommand{
		ConnectionID: connectionID, ExpectedRevision: revision, Secret: []byte(input.Secret),
	})
	writeProviderOperation(writer, result, err)
}

func writeProviderOperation(writer http.ResponseWriter, result providerconnection.OperationResult, err error) {
	if err != nil {
		writeProviderConnectionError(writer, err)
		return
	}
	if result.Replay {
		writer.Header().Set("Idempotency-Replayed", "true")
	}
	writer.Header().Set("Location", "/control/v1/provider-operations/"+url.PathEscape(result.Operation.ID))
	writeJSON(writer, http.StatusAccepted, result.Operation)
}

func writeProviderConnectionMutation(writer http.ResponseWriter, result providerconnection.MutationResult, err error) {
	if err != nil {
		writeProviderConnectionError(writer, err)
		return
	}
	writeProviderConnectionMutationHeaders(writer, result)
	writeJSON(writer, http.StatusOK, result.Connection)
}

func writeProviderConnectionMutationHeaders(writer http.ResponseWriter, result providerconnection.MutationResult) {
	writer.Header().Set("ETag", etag(result.Connection.Revision))
	if result.Replay {
		writer.Header().Set("Idempotency-Replayed", "true")
	}
}

func writeProviderConnectionError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, providerconnection.ErrNotFound):
		writeAPIError(writer, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, providerconnection.ErrAlreadyExists):
		writeAPIError(writer, http.StatusConflict, "already_exists", err.Error())
	case errors.Is(err, providerconnection.ErrRevisionConflict):
		writeAPIError(writer, http.StatusConflict, "revision_conflict", err.Error())
	case errors.Is(err, providerconnection.ErrIdempotencyConflict):
		writeAPIError(writer, http.StatusConflict, "idempotency_conflict", err.Error())
	case errors.Is(err, providerconnection.ErrPolicyDenied):
		writeAPIError(writer, http.StatusForbidden, "policy_denied", err.Error())
	case errors.Is(err, providerconnection.ErrInvalidArgument):
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", err.Error())
	default:
		writeAPIError(writer, http.StatusInternalServerError, "internal_error", "control-plane operation failed")
	}
}

func onePathID(value string) (string, error) {
	if value == "" || strings.Contains(value, "/") {
		return "", errors.New("invalid path ID")
	}
	decoded, err := url.PathUnescape(value)
	if err != nil || decoded == "" || strings.Contains(decoded, "/") {
		return "", errors.New("invalid path ID")
	}
	return decoded, nil
}
