package controlapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/toddzheng/llm-gateway/internal/access"
	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/credentialadmin"
	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

type VerifiedIdentity struct {
	ActorType      string
	ActorID        string
	ActingTenantID string
	Scopes         []string
}

type IdentityVerifier interface {
	Verify(context.Context, string) (VerifiedIdentity, error)
}

type IdentityVerifierFunc func(context.Context, string) (VerifiedIdentity, error)

func (function IdentityVerifierFunc) Verify(ctx context.Context, assertion string) (VerifiedIdentity, error) {
	return function(ctx, assertion)
}

type Administration interface {
	CreateTenant(context.Context, tenantadmin.ActorEnvelope, string, tenantadmin.CreateTenantCommand) (tenantadmin.MutationResult, error)
	UpdateTenant(context.Context, tenantadmin.ActorEnvelope, string, tenantadmin.UpdateTenantCommand) (tenantadmin.MutationResult, error)
	TransitionTenant(context.Context, tenantadmin.ActorEnvelope, string, tenantadmin.TransitionTenantCommand) (tenantadmin.MutationResult, error)
	PublishTenantPolicy(context.Context, tenantadmin.ActorEnvelope, string, tenantadmin.PublishPolicyCommand) (tenantadmin.MutationResult, error)
	GetTenant(context.Context, tenantadmin.ActorEnvelope, string) (access.Tenant, error)
	ListTenants(context.Context, tenantadmin.ActorEnvelope, tenantadmin.TenantFilter) (tenantadmin.TenantPage, error)
	GetTenantPolicy(context.Context, tenantadmin.ActorEnvelope, string) (access.Tenant, error)
	ListTenantPolicyRevisions(context.Context, tenantadmin.ActorEnvelope, string, string, int) (tenantadmin.PolicyRevisionPage, error)
}

type Config struct {
	Administration Administration
	Credentials    CredentialAdministration
	Verifier       IdentityVerifier
}

type CredentialAdministration interface {
	Issue(context.Context, tenantadmin.ActorEnvelope, string, credentialadmin.IssueCommand) (credentialadmin.IssueResult, error)
	Get(context.Context, tenantadmin.ActorEnvelope, string, string) (credentialadmin.Credential, error)
	List(context.Context, tenantadmin.ActorEnvelope, credentialadmin.CredentialFilter) (credentialadmin.CredentialPage, error)
	Update(context.Context, tenantadmin.ActorEnvelope, string, credentialadmin.UpdateCommand) (credentialadmin.MutationResult, error)
	Revoke(context.Context, tenantadmin.ActorEnvelope, string, credentialadmin.RevokeCommand) (credentialadmin.MutationResult, error)
	Rotate(context.Context, tenantadmin.ActorEnvelope, string, credentialadmin.RotateCommand) (credentialadmin.RotationResult, error)
	GetPolicy(context.Context, tenantadmin.ActorEnvelope, string, string) (core.APIKeyPolicy, error)
	PublishPolicy(context.Context, tenantadmin.ActorEnvelope, string, credentialadmin.PublishPolicyCommand) (credentialadmin.MutationResult, error)
	ListPolicyRevisions(context.Context, tenantadmin.ActorEnvelope, string, string, string, int) (credentialadmin.PolicyRevisionPage, error)
	GetEffectivePolicy(context.Context, tenantadmin.ActorEnvelope, string, string) (credentialadmin.EffectivePolicy, error)
}

type Server struct {
	administration Administration
	credentials    CredentialAdministration
	verifier       IdentityVerifier
}

func New(config Config) http.Handler {
	return &Server{administration: config.Administration, credentials: config.Credentials, verifier: config.Verifier}
}

func (server *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path == "/healthz" && request.Method == http.MethodGet {
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	if server.administration == nil || server.verifier == nil {
		writeAPIError(writer, http.StatusServiceUnavailable, "service_unavailable", "control plane is not configured")
		return
	}
	identity, err := server.verifier.Verify(request.Context(), request.Header.Get("Authorization"))
	if err != nil || identity.ActorType == "" || identity.ActorID == "" {
		writer.Header().Set("WWW-Authenticate", "Bearer")
		writeAPIError(writer, http.StatusUnauthorized, "invalid_identity_assertion", "Human IAM assertion is missing or invalid")
		return
	}
	actor := tenantadmin.ActorEnvelope{
		Type: identity.ActorType, ID: identity.ActorID, ActingTenantID: identity.ActingTenantID,
		Scopes: append([]string(nil), identity.Scopes...), RequestID: requestID(request),
	}
	server.route(writer, request, actor)
}

func (server *Server) route(writer http.ResponseWriter, request *http.Request, actor tenantadmin.ActorEnvelope) {
	const collection = "/control/v1/tenants"
	if request.URL.Path == collection || request.URL.Path == collection+"/" {
		switch request.Method {
		case http.MethodPost:
			server.createTenant(writer, request, actor)
		case http.MethodGet:
			server.listTenants(writer, request, actor)
		default:
			methodNotAllowed(writer, http.MethodGet, http.MethodPost)
		}
		return
	}
	if !strings.HasPrefix(request.URL.Path, collection+"/") {
		writeAPIError(writer, http.StatusNotFound, "not_found", "route not found")
		return
	}
	remainder := strings.TrimPrefix(request.URL.Path, collection+"/")
	parts := strings.Split(remainder, "/")
	if len(parts) == 0 || parts[0] == "" {
		writeAPIError(writer, http.StatusNotFound, "not_found", "route not found")
		return
	}
	tenantID, err := url.PathUnescape(parts[0])
	if err != nil || tenantID == "" || strings.Contains(tenantID, "/") {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", "invalid Tenant ID")
		return
	}
	if len(parts) == 1 {
		switch request.Method {
		case http.MethodGet:
			server.getTenant(writer, request, actor, tenantID)
		case http.MethodPatch:
			server.updateTenant(writer, request, actor, tenantID)
		default:
			methodNotAllowed(writer, http.MethodGet, http.MethodPatch)
		}
		return
	}
	if parts[1] == "gateway-api-keys" {
		server.routeGatewayAPIKeys(writer, request, actor, tenantID, parts[2:])
		return
	}
	if len(parts) != 2 {
		writeAPIError(writer, http.StatusNotFound, "not_found", "route not found")
		return
	}
	switch parts[1] {
	case "transitions":
		if request.Method != http.MethodPost {
			methodNotAllowed(writer, http.MethodPost)
			return
		}
		server.transitionTenant(writer, request, actor, tenantID)
	case "policy":
		switch request.Method {
		case http.MethodGet:
			server.getPolicy(writer, request, actor, tenantID)
		case http.MethodPut:
			server.putPolicy(writer, request, actor, tenantID)
		default:
			methodNotAllowed(writer, http.MethodGet, http.MethodPut)
		}
	case "policy-revisions":
		if request.Method != http.MethodGet {
			methodNotAllowed(writer, http.MethodGet)
			return
		}
		server.listPolicyRevisions(writer, request, actor, tenantID)
	default:
		writeAPIError(writer, http.StatusNotFound, "not_found", "route not found")
	}
}

func (server *Server) routeGatewayAPIKeys(
	writer http.ResponseWriter,
	request *http.Request,
	actor tenantadmin.ActorEnvelope,
	tenantID string,
	tail []string,
) {
	if len(tail) == 0 {
		switch request.Method {
		case http.MethodPost:
			server.issueGatewayAPIKey(writer, request, actor, tenantID)
		case http.MethodGet:
			server.listGatewayAPIKeys(writer, request, actor, tenantID)
		default:
			methodNotAllowed(writer, http.MethodGet, http.MethodPost)
		}
		return
	}
	credentialID, err := url.PathUnescape(tail[0])
	if err != nil || credentialID == "" || strings.Contains(credentialID, "/") {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", "invalid Gateway API Key ID")
		return
	}
	if len(tail) == 1 {
		switch request.Method {
		case http.MethodGet:
			server.getGatewayAPIKey(writer, request, actor, tenantID, credentialID)
		case http.MethodPatch:
			server.updateGatewayAPIKey(writer, request, actor, tenantID, credentialID)
		default:
			methodNotAllowed(writer, http.MethodGet, http.MethodPatch)
		}
		return
	}
	if len(tail) != 2 {
		writeAPIError(writer, http.StatusNotFound, "not_found", "route not found")
		return
	}
	switch tail[1] {
	case "revoke":
		if request.Method != http.MethodPost {
			methodNotAllowed(writer, http.MethodPost)
			return
		}
		server.revokeGatewayAPIKey(writer, request, actor, tenantID, credentialID)
	case "rotate":
		if request.Method != http.MethodPost {
			methodNotAllowed(writer, http.MethodPost)
			return
		}
		server.rotateGatewayAPIKey(writer, request, actor, tenantID, credentialID)
	case "policy":
		switch request.Method {
		case http.MethodGet:
			server.getGatewayAPIKeyPolicy(writer, request, actor, tenantID, credentialID)
		case http.MethodPut:
			server.putGatewayAPIKeyPolicy(writer, request, actor, tenantID, credentialID)
		default:
			methodNotAllowed(writer, http.MethodGet, http.MethodPut)
		}
	case "policy-revisions":
		if request.Method != http.MethodGet {
			methodNotAllowed(writer, http.MethodGet)
			return
		}
		server.listGatewayAPIKeyPolicyRevisions(writer, request, actor, tenantID, credentialID)
	case "effective-policy":
		if request.Method != http.MethodGet {
			methodNotAllowed(writer, http.MethodGet)
			return
		}
		server.getGatewayAPIKeyEffectivePolicy(writer, request, actor, tenantID, credentialID)
	default:
		writeAPIError(writer, http.StatusNotFound, "not_found", "route not found")
	}
}

type issueGatewayAPIKeyRequest struct {
	Name      string            `json:"name"`
	Metadata  map[string]any    `json:"metadata"`
	ExpiresAt *time.Time        `json:"expires_at"`
	Policy    core.APIKeyPolicy `json:"policy"`
	Reason    string            `json:"reason"`
}

func (server *Server) issueGatewayAPIKey(writer http.ResponseWriter, request *http.Request, actor tenantadmin.ActorEnvelope, tenantID string) {
	if server.credentials == nil {
		writeAPIError(writer, http.StatusServiceUnavailable, "service_unavailable", "Gateway Credential Administration is not configured")
		return
	}
	var input issueGatewayAPIKeyRequest
	if !decodeBody(writer, request, &input) {
		return
	}
	actor.Reason = input.Reason
	result, err := server.credentials.Issue(request.Context(), actor, request.Header.Get("Idempotency-Key"), credentialadmin.IssueCommand{
		TenantID: tenantID, Name: input.Name, Metadata: input.Metadata, ExpiresAt: input.ExpiresAt, Policy: input.Policy,
	})
	if err != nil {
		writeCredentialError(writer, err)
		return
	}
	writer.Header().Set("ETag", etag(result.Credential.Revision))
	writer.Header().Set("Location", "/control/v1/tenants/"+url.PathEscape(tenantID)+"/gateway-api-keys/"+url.PathEscape(result.Credential.ID))
	if result.Replay {
		writer.Header().Set("Idempotency-Replayed", "true")
	}
	response := credentialResource(result.Credential)
	if result.RawSecret != "" {
		response["secret"] = result.RawSecret
	}
	writeJSON(writer, http.StatusCreated, response)
}

func credentialResource(credential credentialadmin.Credential) map[string]any {
	return map[string]any{
		"id": credential.ID, "tenant_id": credential.TenantID, "name": credential.Name,
		"prefix": credential.Prefix, "digest_version": credential.DigestVersion, "status": credential.Status,
		"revision": credential.Revision, "policy": credential.Policy, "metadata": credential.Metadata,
		"expires_at": credential.ExpiresAt, "revoked_at": credential.RevokedAt,
		"predecessor_id": credential.PredecessorID, "replacement_id": credential.ReplacementID,
		"grace_expires_at": credential.GraceExpiresAt,
	}
}

func (server *Server) getGatewayAPIKey(writer http.ResponseWriter, request *http.Request, actor tenantadmin.ActorEnvelope, tenantID, credentialID string) {
	credential, err := server.credentials.Get(request.Context(), actor, tenantID, credentialID)
	if err != nil {
		writeCredentialError(writer, err)
		return
	}
	writer.Header().Set("ETag", etag(credential.Revision))
	writeJSON(writer, http.StatusOK, credentialResource(credential))
}

func (server *Server) listGatewayAPIKeys(writer http.ResponseWriter, request *http.Request, actor tenantadmin.ActorEnvelope, tenantID string) {
	limit, err := optionalInt(request.URL.Query().Get("limit"))
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", "limit must be an integer")
		return
	}
	page, err := server.credentials.List(request.Context(), actor, credentialadmin.CredentialFilter{
		TenantID: tenantID, Status: access.APIKeyStatus(request.URL.Query().Get("status")),
		Cursor: request.URL.Query().Get("cursor"), Limit: limit,
	})
	if err != nil {
		writeCredentialError(writer, err)
		return
	}
	data := make([]map[string]any, 0, len(page.Data))
	for _, credential := range page.Data {
		data = append(data, credentialResource(credential))
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": data, "next_cursor": page.NextCursor})
}

type updateGatewayAPIKeyRequest struct {
	Name             *string         `json:"name"`
	Metadata         *map[string]any `json:"metadata"`
	ExpiresAt        *time.Time      `json:"expires_at"`
	ClearExpiresAt   bool            `json:"clear_expires_at"`
	ExpectedRevision int64           `json:"expected_revision"`
	Reason           string          `json:"reason"`
}

func (server *Server) updateGatewayAPIKey(writer http.ResponseWriter, request *http.Request, actor tenantadmin.ActorEnvelope, tenantID, credentialID string) {
	var input updateGatewayAPIKeyRequest
	if !decodeBody(writer, request, &input) {
		return
	}
	revision, ok := expectedRevision(writer, request, input.ExpectedRevision)
	if !ok {
		return
	}
	actor.Reason = input.Reason
	result, err := server.credentials.Update(request.Context(), actor, request.Header.Get("Idempotency-Key"), credentialadmin.UpdateCommand{
		TenantID: tenantID, CredentialID: credentialID, ExpectedRevision: revision,
		Name: input.Name, Metadata: input.Metadata, ExpiresAt: input.ExpiresAt, ClearExpiresAt: input.ClearExpiresAt,
	})
	writeCredentialMutation(writer, result, err)
}

type revokeGatewayAPIKeyRequest struct {
	ExpectedRevision int64  `json:"expected_revision"`
	Reason           string `json:"reason"`
}

func (server *Server) revokeGatewayAPIKey(writer http.ResponseWriter, request *http.Request, actor tenantadmin.ActorEnvelope, tenantID, credentialID string) {
	var input revokeGatewayAPIKeyRequest
	if !decodeBody(writer, request, &input) {
		return
	}
	revision, ok := expectedRevision(writer, request, input.ExpectedRevision)
	if !ok {
		return
	}
	actor.Reason = input.Reason
	result, err := server.credentials.Revoke(request.Context(), actor, request.Header.Get("Idempotency-Key"), credentialadmin.RevokeCommand{
		TenantID: tenantID, CredentialID: credentialID, ExpectedRevision: revision,
	})
	writeCredentialMutation(writer, result, err)
}

type rotateGatewayAPIKeyRequest struct {
	ExpectedRevision  int64      `json:"expected_revision"`
	RevokeImmediately bool       `json:"revoke_immediately"`
	GraceExpiresAt    *time.Time `json:"grace_expires_at"`
	Reason            string     `json:"reason"`
}

func (server *Server) rotateGatewayAPIKey(writer http.ResponseWriter, request *http.Request, actor tenantadmin.ActorEnvelope, tenantID, credentialID string) {
	var input rotateGatewayAPIKeyRequest
	if !decodeBody(writer, request, &input) {
		return
	}
	revision, ok := expectedRevision(writer, request, input.ExpectedRevision)
	if !ok {
		return
	}
	actor.Reason = input.Reason
	result, err := server.credentials.Rotate(request.Context(), actor, request.Header.Get("Idempotency-Key"), credentialadmin.RotateCommand{
		TenantID: tenantID, CredentialID: credentialID, ExpectedRevision: revision,
		RevokeImmediately: input.RevokeImmediately, GraceExpiresAt: input.GraceExpiresAt,
	})
	if err != nil {
		writeCredentialError(writer, err)
		return
	}
	writer.Header().Set("ETag", etag(result.Replacement.Revision))
	writer.Header().Set("Location", "/control/v1/tenants/"+url.PathEscape(tenantID)+"/gateway-api-keys/"+url.PathEscape(result.Replacement.ID))
	if result.Replay {
		writer.Header().Set("Idempotency-Replayed", "true")
	}
	response := map[string]any{"predecessor": credentialResource(result.Predecessor), "replacement": credentialResource(result.Replacement)}
	if result.RawSecret != "" {
		response["secret"] = result.RawSecret
	}
	writeJSON(writer, http.StatusCreated, response)
}

func (server *Server) getGatewayAPIKeyPolicy(writer http.ResponseWriter, request *http.Request, actor tenantadmin.ActorEnvelope, tenantID, credentialID string) {
	policy, err := server.credentials.GetPolicy(request.Context(), actor, tenantID, credentialID)
	if err != nil {
		writeCredentialError(writer, err)
		return
	}
	writer.Header().Set("ETag", etag(policy.Revision))
	writeJSON(writer, http.StatusOK, policy)
}

type putGatewayAPIKeyPolicyRequest struct {
	Policy           *core.APIKeyPolicy `json:"policy"`
	RestoreRevision  *int64             `json:"restore_revision"`
	ExpectedRevision int64              `json:"expected_revision"`
	Reason           string             `json:"reason"`
}

func (server *Server) putGatewayAPIKeyPolicy(writer http.ResponseWriter, request *http.Request, actor tenantadmin.ActorEnvelope, tenantID, credentialID string) {
	var input putGatewayAPIKeyPolicyRequest
	if !decodeBody(writer, request, &input) {
		return
	}
	revision, ok := expectedRevision(writer, request, input.ExpectedRevision)
	if !ok {
		return
	}
	actor.Reason = input.Reason
	result, err := server.credentials.PublishPolicy(request.Context(), actor, request.Header.Get("Idempotency-Key"), credentialadmin.PublishPolicyCommand{
		TenantID: tenantID, CredentialID: credentialID, ExpectedRevision: revision,
		Policy: input.Policy, RestoreRevision: input.RestoreRevision,
	})
	if err != nil {
		writeCredentialError(writer, err)
		return
	}
	writer.Header().Set("ETag", etag(result.Credential.Policy.Revision))
	if result.Replay {
		writer.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(writer, http.StatusOK, result.Credential.Policy)
}

func (server *Server) listGatewayAPIKeyPolicyRevisions(writer http.ResponseWriter, request *http.Request, actor tenantadmin.ActorEnvelope, tenantID, credentialID string) {
	limit, err := optionalInt(request.URL.Query().Get("limit"))
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", "limit must be an integer")
		return
	}
	page, err := server.credentials.ListPolicyRevisions(request.Context(), actor, tenantID, credentialID, request.URL.Query().Get("cursor"), limit)
	if err != nil {
		writeCredentialError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": page.Data, "next_cursor": page.NextCursor})
}

func (server *Server) getGatewayAPIKeyEffectivePolicy(writer http.ResponseWriter, request *http.Request, actor tenantadmin.ActorEnvelope, tenantID, credentialID string) {
	policy, err := server.credentials.GetEffectivePolicy(request.Context(), actor, tenantID, credentialID)
	if err != nil {
		writeCredentialError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, policy)
}

func writeCredentialMutation(writer http.ResponseWriter, result credentialadmin.MutationResult, err error) {
	if err != nil {
		writeCredentialError(writer, err)
		return
	}
	writer.Header().Set("ETag", etag(result.Credential.Revision))
	if result.Replay {
		writer.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(writer, http.StatusOK, credentialResource(result.Credential))
}

type createTenantRequest struct {
	ID            string            `json:"id"`
	Slug          string            `json:"slug"`
	DisplayName   string            `json:"display_name"`
	HomeRegion    string            `json:"home_region"`
	Metadata      map[string]any    `json:"metadata"`
	InitialPolicy core.TenantPolicy `json:"initial_policy"`
	Reason        string            `json:"reason"`
}

func (server *Server) createTenant(writer http.ResponseWriter, request *http.Request, actor tenantadmin.ActorEnvelope) {
	var input createTenantRequest
	if !decodeBody(writer, request, &input) {
		return
	}
	actor.Reason = input.Reason
	result, err := server.administration.CreateTenant(request.Context(), actor, request.Header.Get("Idempotency-Key"), tenantadmin.CreateTenantCommand{
		ID: input.ID, Slug: input.Slug, DisplayName: input.DisplayName, HomeRegion: input.HomeRegion,
		Metadata: input.Metadata, InitialPolicy: input.InitialPolicy,
	})
	if err != nil {
		writeServiceError(writer, err)
		return
	}
	setTenantHeaders(writer, result.Tenant)
	writer.Header().Set("Location", "/control/v1/tenants/"+url.PathEscape(result.Tenant.ID))
	if result.Replay {
		writer.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(writer, http.StatusCreated, tenantResource(result.Tenant))
}

func (server *Server) getTenant(writer http.ResponseWriter, request *http.Request, actor tenantadmin.ActorEnvelope, tenantID string) {
	tenant, err := server.administration.GetTenant(request.Context(), actor, tenantID)
	if err != nil {
		writeServiceError(writer, err)
		return
	}
	setTenantHeaders(writer, tenant)
	writeJSON(writer, http.StatusOK, tenantResource(tenant))
}

func (server *Server) listTenants(writer http.ResponseWriter, request *http.Request, actor tenantadmin.ActorEnvelope) {
	query := request.URL.Query()
	limit, err := optionalInt(query.Get("limit"))
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", "limit must be an integer")
		return
	}
	includeClosed, err := optionalBool(query.Get("include_closed"))
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", "include_closed must be a boolean")
		return
	}
	page, err := server.administration.ListTenants(request.Context(), actor, tenantadmin.TenantFilter{
		ID: query.Get("id"), Slug: query.Get("slug"), Status: access.TenantStatus(query.Get("status")),
		HomeRegion: query.Get("home_region"), IncludeClosed: includeClosed, Cursor: query.Get("cursor"), Limit: limit,
	})
	if err != nil {
		writeServiceError(writer, err)
		return
	}
	data := make([]tenantResponse, 0, len(page.Data))
	for _, tenant := range page.Data {
		data = append(data, tenantResource(tenant))
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": data, "next_cursor": page.NextCursor})
}

type updateTenantRequest struct {
	DisplayName      *string         `json:"display_name"`
	Metadata         *map[string]any `json:"metadata"`
	ExpectedRevision int64           `json:"expected_revision"`
	Reason           string          `json:"reason"`
}

func (server *Server) updateTenant(writer http.ResponseWriter, request *http.Request, actor tenantadmin.ActorEnvelope, tenantID string) {
	var input updateTenantRequest
	if !decodeBody(writer, request, &input) {
		return
	}
	revision, ok := expectedRevision(writer, request, input.ExpectedRevision)
	if !ok {
		return
	}
	actor.Reason = input.Reason
	result, err := server.administration.UpdateTenant(request.Context(), actor, request.Header.Get("Idempotency-Key"), tenantadmin.UpdateTenantCommand{
		TenantID: tenantID, ExpectedRevision: revision, DisplayName: input.DisplayName, Metadata: input.Metadata,
	})
	writeMutationResult(writer, result, err)
}

type transitionRequest struct {
	Target           access.TenantStatus `json:"target"`
	ExpectedRevision int64               `json:"expected_revision"`
	Reason           string              `json:"reason"`
}

func (server *Server) transitionTenant(writer http.ResponseWriter, request *http.Request, actor tenantadmin.ActorEnvelope, tenantID string) {
	var input transitionRequest
	if !decodeBody(writer, request, &input) {
		return
	}
	revision, ok := expectedRevision(writer, request, input.ExpectedRevision)
	if !ok {
		return
	}
	actor.Reason = input.Reason
	result, err := server.administration.TransitionTenant(request.Context(), actor, request.Header.Get("Idempotency-Key"), tenantadmin.TransitionTenantCommand{
		TenantID: tenantID, ExpectedRevision: revision, Target: input.Target,
	})
	writeMutationResult(writer, result, err)
}

func (server *Server) getPolicy(writer http.ResponseWriter, request *http.Request, actor tenantadmin.ActorEnvelope, tenantID string) {
	tenant, err := server.administration.GetTenantPolicy(request.Context(), actor, tenantID)
	if err != nil {
		writeServiceError(writer, err)
		return
	}
	writer.Header().Set("ETag", etag(tenant.Policy.Revision))
	writeJSON(writer, http.StatusOK, tenant.Policy)
}

type putPolicyRequest struct {
	Policy           *core.TenantPolicy `json:"policy"`
	RestoreRevision  *int64             `json:"restore_revision"`
	ExpectedRevision int64              `json:"expected_revision"`
	Reason           string             `json:"reason"`
}

func (server *Server) putPolicy(writer http.ResponseWriter, request *http.Request, actor tenantadmin.ActorEnvelope, tenantID string) {
	var input putPolicyRequest
	if !decodeBody(writer, request, &input) {
		return
	}
	revision, ok := expectedRevision(writer, request, input.ExpectedRevision)
	if !ok {
		return
	}
	actor.Reason = input.Reason
	result, err := server.administration.PublishTenantPolicy(request.Context(), actor, request.Header.Get("Idempotency-Key"), tenantadmin.PublishPolicyCommand{
		TenantID: tenantID, ExpectedRevision: revision, Policy: input.Policy, RestoreRevision: input.RestoreRevision,
	})
	if err != nil {
		writeServiceError(writer, err)
		return
	}
	writer.Header().Set("ETag", etag(result.Tenant.Policy.Revision))
	if result.Replay {
		writer.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(writer, http.StatusOK, result.Tenant.Policy)
}

func (server *Server) listPolicyRevisions(writer http.ResponseWriter, request *http.Request, actor tenantadmin.ActorEnvelope, tenantID string) {
	limit, err := optionalInt(request.URL.Query().Get("limit"))
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", "limit must be an integer")
		return
	}
	page, err := server.administration.ListTenantPolicyRevisions(request.Context(), actor, tenantID, request.URL.Query().Get("cursor"), limit)
	if err != nil {
		writeServiceError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": page.Data, "next_cursor": page.NextCursor})
}

func writeMutationResult(writer http.ResponseWriter, result tenantadmin.MutationResult, err error) {
	if err != nil {
		writeServiceError(writer, err)
		return
	}
	setTenantHeaders(writer, result.Tenant)
	if result.Replay {
		writer.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(writer, http.StatusOK, tenantResource(result.Tenant))
}

type tenantResponse struct {
	ID             string              `json:"id"`
	Slug           string              `json:"slug"`
	DisplayName    string              `json:"display_name"`
	Status         access.TenantStatus `json:"status"`
	HomeRegion     string              `json:"home_region"`
	ExecutionEpoch int64               `json:"execution_epoch"`
	Policy         core.TenantPolicy   `json:"policy"`
	Metadata       map[string]any      `json:"metadata"`
	Revision       int64               `json:"revision"`
}

func tenantResource(tenant access.Tenant) tenantResponse {
	return tenantResponse{
		ID: tenant.ID, Slug: tenant.Slug, DisplayName: tenant.DisplayName, Status: tenant.Status,
		HomeRegion: tenant.HomeRegion, ExecutionEpoch: tenant.ExecutionEpoch, Policy: tenant.Policy,
		Metadata: tenant.Metadata, Revision: tenant.Revision,
	}
}

func expectedRevision(writer http.ResponseWriter, request *http.Request, bodyRevision int64) (int64, bool) {
	header := strings.TrimSpace(request.Header.Get("If-Match"))
	headerRevision := int64(0)
	if header != "" {
		if len(header) < 3 || header[0] != '"' || header[len(header)-1] != '"' {
			writeAPIError(writer, http.StatusBadRequest, "invalid_request", "If-Match must be a quoted positive revision")
			return 0, false
		}
		value, err := strconv.ParseInt(header[1:len(header)-1], 10, 64)
		if err != nil || value <= 0 {
			writeAPIError(writer, http.StatusBadRequest, "invalid_request", "If-Match must be a quoted positive revision")
			return 0, false
		}
		headerRevision = value
	}
	if bodyRevision > 0 && headerRevision > 0 && bodyRevision != headerRevision {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", "expected_revision and If-Match disagree")
		return 0, false
	}
	if bodyRevision > 0 {
		return bodyRevision, true
	}
	if headerRevision > 0 {
		return headerRevision, true
	}
	writeAPIError(writer, http.StatusPreconditionRequired, "expected_revision_required", "If-Match or expected_revision is required")
	return 0, false
}

func decodeBody(writer http.ResponseWriter, request *http.Request, destination any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", "request body must be valid JSON: "+safeDecodeError(err))
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", "request body must contain one JSON value")
		return false
	}
	return true
}

func safeDecodeError(err error) string {
	var syntaxError *json.SyntaxError
	var typeError *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntaxError):
		return fmt.Sprintf("syntax error at byte %d", syntaxError.Offset)
	case errors.As(err, &typeError):
		return "invalid value for " + typeError.Field
	case errors.Is(err, io.EOF):
		return "empty body"
	default:
		return err.Error()
	}
}

func writeServiceError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, tenantadmin.ErrNotFound):
		writeAPIError(writer, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, tenantadmin.ErrAlreadyExists):
		writeAPIError(writer, http.StatusConflict, "already_exists", err.Error())
	case errors.Is(err, tenantadmin.ErrRevisionConflict):
		writeAPIError(writer, http.StatusConflict, "revision_conflict", err.Error())
	case errors.Is(err, tenantadmin.ErrInvalidTransition):
		writeAPIError(writer, http.StatusConflict, "invalid_transition", err.Error())
	case errors.Is(err, tenantadmin.ErrIdempotencyConflict):
		writeAPIError(writer, http.StatusConflict, "idempotency_conflict", err.Error())
	case errors.Is(err, tenantadmin.ErrPolicyDenied):
		writeAPIError(writer, http.StatusForbidden, "policy_denied", err.Error())
	case errors.Is(err, tenantadmin.ErrInvalidArgument):
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", err.Error())
	default:
		writeAPIError(writer, http.StatusInternalServerError, "internal_error", "control-plane operation failed")
	}
}

func writeCredentialError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, credentialadmin.ErrNotFound):
		writeAPIError(writer, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, credentialadmin.ErrAlreadyExists):
		writeAPIError(writer, http.StatusConflict, "already_exists", err.Error())
	case errors.Is(err, credentialadmin.ErrRevisionConflict):
		writeAPIError(writer, http.StatusConflict, "revision_conflict", err.Error())
	case errors.Is(err, credentialadmin.ErrIdempotencyConflict):
		writeAPIError(writer, http.StatusConflict, "idempotency_conflict", err.Error())
	case errors.Is(err, credentialadmin.ErrPolicyDenied):
		writeAPIError(writer, http.StatusForbidden, "policy_denied", err.Error())
	case errors.Is(err, credentialadmin.ErrInvalidArgument):
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", err.Error())
	default:
		writeAPIError(writer, http.StatusInternalServerError, "internal_error", "control-plane operation failed")
	}
}

func methodNotAllowed(writer http.ResponseWriter, allowed ...string) {
	writer.Header().Set("Allow", strings.Join(allowed, ", "))
	writeAPIError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
}

func writeAPIError(writer http.ResponseWriter, status int, code, message string) {
	writeJSON(writer, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func setTenantHeaders(writer http.ResponseWriter, tenant access.Tenant) {
	writer.Header().Set("ETag", etag(tenant.Revision))
}

func etag(revision int64) string {
	return `"` + strconv.FormatInt(revision, 10) + `"`
}

func optionalInt(value string) (int, error) {
	if value == "" {
		return 0, nil
	}
	return strconv.Atoi(value)
}

func optionalBool(value string) (bool, error) {
	if value == "" {
		return false, nil
	}
	return strconv.ParseBool(value)
}

func requestID(request *http.Request) string {
	if value := strings.TrimSpace(request.Header.Get("X-Request-ID")); value != "" && len(value) <= 255 {
		return value
	}
	var value [16]byte
	if _, err := rand.Read(value[:]); err == nil {
		return "creq_" + hex.EncodeToString(value[:])
	}
	return "creq_unavailable"
}
