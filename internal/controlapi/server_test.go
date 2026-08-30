package controlapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/toddzheng/llm-gateway/internal/access"
	"github.com/toddzheng/llm-gateway/internal/controlapi"
	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/credentialadmin"
	"github.com/toddzheng/llm-gateway/internal/operations"
	"github.com/toddzheng/llm-gateway/internal/providerconnection"
	"github.com/toddzheng/llm-gateway/internal/routingcatalog"
	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

func TestMachineAuthenticatedGatewayObservationDoesNotUseHumanIAM(t *testing.T) {
	now := time.Date(2026, 8, 29, 22, 0, 0, 0, time.UTC)
	called := false
	handler := controlapi.New(controlapi.Config{
		GatewayVerifier: gatewayVerifierFunc(func(_ context.Context, authorization, method, path string, body []byte) (operations.GatewayIdentity, error) {
			if authorization != "Gateway-HMAC signed" || method != http.MethodPost || path != "/internal/v1/operations/gateway-observations" || len(body) == 0 {
				t.Fatalf("machine assertion request = %q %q %q", authorization, method, path)
			}
			return operations.GatewayIdentity{GatewayID: "gateway-a", Region: "us-west"}, nil
		}),
		GatewayObservations: observationIngestorFunc(func(_ context.Context, identity operations.GatewayIdentity, observation operations.GatewayObservation) error {
			called = true
			if identity.GatewayID != observation.GatewayID || identity.Region != observation.Region {
				t.Fatalf("identity/observation = %#v / %#v", identity, observation)
			}
			return nil
		}),
		Verifier: controlapi.IdentityVerifierFunc(func(context.Context, string) (controlapi.VerifiedIdentity, error) {
			t.Fatal("Gateway observation reached Human IAM")
			return controlapi.VerifiedIdentity{}, nil
		}),
	})
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/operations/gateway-observations", jsonBody(t, operations.GatewayObservation{
		EventSchemaVersion: 2, GatewayID: "gateway-a", Region: "us-west", BuildSHA: "abc1234",
		DatabaseSchemaVersion: 21, RoutingCatalogRevision: 7, AccessProjectionRevision: 9,
		ExecutionEpochFloor: 1, StartedAt: now.Add(-time.Hour), ObservedAt: now,
		Consumers: []operations.ConsumerObservation{},
	}))
	request.Header.Set("Authorization", "Gateway-HMAC signed")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || !called {
		t.Fatalf("status/called/body = %d/%v/%s", response.Code, called, response.Body.String())
	}
}

func TestGatewayObservationRejectsUnknownContentBearingFields(t *testing.T) {
	called := false
	handler := controlapi.New(controlapi.Config{
		GatewayVerifier: gatewayVerifierFunc(func(context.Context, string, string, string, []byte) (operations.GatewayIdentity, error) {
			return operations.GatewayIdentity{GatewayID: "gateway-a", Region: "us-west"}, nil
		}),
		GatewayObservations: observationIngestorFunc(func(context.Context, operations.GatewayIdentity, operations.GatewayObservation) error {
			called = true
			return nil
		}),
	})
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/operations/gateway-observations", bytes.NewBufferString(`{"gateway_id":"gateway-a","region":"us-west","prompt":"must not enter operations"}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || called {
		t.Fatalf("status/called/body = %d/%v/%s", response.Code, called, response.Body.String())
	}
}

func TestCreateTenantDerivesVerifiedActorAndReturnsVersionHeaders(t *testing.T) {
	service := &fakeAdministration{}
	service.create = func(_ context.Context, actor tenantadmin.ActorEnvelope, key string, command tenantadmin.CreateTenantCommand) (tenantadmin.MutationResult, error) {
		if actor.Type != "human" || actor.ID != "user-1" || actor.RequestID != "request-1" || actor.Reason != "provision for research" {
			t.Fatalf("Actor Envelope = %#v", actor)
		}
		if key != "create-1" || command.ID != "tenant-a" || command.HomeRegion != "us-west1" {
			t.Fatalf("command/key = %#v / %q", command, key)
		}
		return tenantadmin.MutationResult{Tenant: access.Tenant{
			ID: "tenant-a", Slug: "tenant-a", DisplayName: "Tenant A", Status: access.TenantActive,
			HomeRegion: "us-west1", ExecutionEpoch: 1, Policy: core.TenantPolicy{Revision: 1}, Revision: 1,
		}}, nil
	}
	handler := controlapi.New(controlapi.Config{
		Administration: service,
		Verifier: controlapi.IdentityVerifierFunc(func(_ context.Context, authorization string) (controlapi.VerifiedIdentity, error) {
			if authorization != "Bearer signed-human-assertion" {
				t.Fatalf("authorization = %q", authorization)
			}
			return controlapi.VerifiedIdentity{ActorType: "human", ActorID: "user-1", Scopes: []string{tenantadmin.ScopePlatformWrite}}, nil
		}),
	})
	request := httptest.NewRequest(http.MethodPost, "/control/v1/tenants", jsonBody(t, map[string]any{
		"id": "tenant-a", "slug": "tenant-a", "display_name": "Tenant A", "home_region": "us-west1",
		"reason": "provision for research", "initial_policy": map[string]any{"revision": 1},
	}))
	request.Header.Set("Authorization", "Bearer signed-human-assertion")
	request.Header.Set("Idempotency-Key", "create-1")
	request.Header.Set("X-Request-ID", "request-1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("status/body = %d / %s", response.Code, response.Body.String())
	}
	if response.Header().Get("ETag") != `"1"` || response.Header().Get("Location") != "/control/v1/tenants/tenant-a" {
		t.Fatalf("response headers = %#v", response.Header())
	}
}

func TestMutationRequiresIdempotencyAndCurrentETag(t *testing.T) {
	service := &fakeAdministration{}
	service.update = func(_ context.Context, _ tenantadmin.ActorEnvelope, _ string, command tenantadmin.UpdateTenantCommand) (tenantadmin.MutationResult, error) {
		if command.ExpectedRevision != 7 {
			t.Fatalf("expected revision = %d", command.ExpectedRevision)
		}
		return tenantadmin.MutationResult{}, tenantadmin.ErrRevisionConflict
	}
	handler := controlapi.New(controlapi.Config{
		Administration: service,
		Verifier:       fixedVerifier(controlapi.VerifiedIdentity{ActorType: "human", ActorID: "user-1", Scopes: []string{tenantadmin.ScopePlatformWrite}}),
	})
	request := httptest.NewRequest(http.MethodPatch, "/control/v1/tenants/tenant-a", jsonBody(t, map[string]any{
		"display_name": "Changed", "reason": "operator request",
	}))
	request.Header.Set("Authorization", "Bearer valid")
	request.Header.Set("Idempotency-Key", "update-1")
	request.Header.Set("If-Match", `"7"`)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("status/body = %d / %s", response.Code, response.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "revision_conflict" {
		t.Fatalf("error = %#v", body.Error)
	}
}

func TestInvalidHumanAssertionIsRejectedBeforeAdministration(t *testing.T) {
	service := &fakeAdministration{}
	handler := controlapi.New(controlapi.Config{
		Administration: service,
		Verifier: controlapi.IdentityVerifierFunc(func(context.Context, string) (controlapi.VerifiedIdentity, error) {
			return controlapi.VerifiedIdentity{}, errors.New("signature rejected")
		}),
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/control/v1/tenants", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status/body = %d / %s", response.Code, response.Body.String())
	}
}

func TestIssueGatewayAPIKeyReturnsOneTimeSecretFromCredentialModule(t *testing.T) {
	credentials := credentialAdministrationFunc(func(_ context.Context, actor tenantadmin.ActorEnvelope, key string, command credentialadmin.IssueCommand) (credentialadmin.IssueResult, error) {
		if actor.ID != "user-1" || actor.Reason != "new workload" || key != "issue-1" || command.TenantID != "tenant-a" {
			t.Fatalf("actor/key/command = %#v / %q / %#v", actor, key, command)
		}
		return credentialadmin.IssueResult{
			Credential: credentialadmin.Credential{
				ID: "gak_123", TenantID: "tenant-a", Name: command.Name, Prefix: "gw_prefix",
				DigestVersion: 2, Status: access.APIKeyActive, Revision: 1, Policy: core.APIKeyPolicy{Revision: 1},
			},
			RawSecret: "gw_one_time_secret",
		}, nil
	})
	handler := controlapi.New(controlapi.Config{
		Administration: &fakeAdministration{}, Credentials: credentials,
		Verifier: fixedVerifier(controlapi.VerifiedIdentity{ActorType: "human", ActorID: "user-1", Scopes: []string{tenantadmin.ScopePlatformWrite}}),
	})
	request := httptest.NewRequest(http.MethodPost, "/control/v1/tenants/tenant-a/gateway-api-keys", jsonBody(t, map[string]any{
		"name": "workload", "policy": map[string]any{"revision": 1}, "reason": "new workload",
	}))
	request.Header.Set("Authorization", "Bearer valid")
	request.Header.Set("Idempotency-Key", "issue-1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || response.Header().Get("ETag") != `"1"` {
		t.Fatalf("status/headers/body = %d / %#v / %s", response.Code, response.Header(), response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["secret"] != "gw_one_time_secret" || body["id"] != "gak_123" {
		t.Fatalf("issued body = %#v", body)
	}
}

func TestListGatewayAPIKeysReturnsMetadataWithoutSecrets(t *testing.T) {
	credentials := &fakeCredentialAdministration{}
	credentials.list = func(_ context.Context, actor tenantadmin.ActorEnvelope, filter credentialadmin.CredentialFilter) (credentialadmin.CredentialPage, error) {
		if actor.ID != "user-1" || filter.TenantID != "tenant-a" || filter.Limit != 2 {
			t.Fatalf("actor/filter = %#v / %#v", actor, filter)
		}
		return credentialadmin.CredentialPage{Data: []credentialadmin.Credential{{
			ID: "gak_123", TenantID: "tenant-a", Name: "workload", Prefix: "gw_prefix",
			DigestVersion: 2, Status: access.APIKeyActive, Revision: 1, Policy: core.APIKeyPolicy{Revision: 1},
		}}, NextCursor: "next"}, nil
	}
	handler := controlapi.New(controlapi.Config{
		Administration: &fakeAdministration{}, Credentials: credentials,
		Verifier: fixedVerifier(controlapi.VerifiedIdentity{ActorType: "human", ActorID: "user-1", Scopes: []string{tenantadmin.ScopePlatformRead}}),
	})
	request := httptest.NewRequest(http.MethodGet, "/control/v1/tenants/tenant-a/gateway-api-keys?limit=2", nil)
	request.Header.Set("Authorization", "Bearer valid")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status/body = %d / %s", response.Code, response.Body.String())
	}
	if bytes.Contains(response.Body.Bytes(), []byte("secret")) || !bytes.Contains(response.Body.Bytes(), []byte(`"next_cursor":"next"`)) {
		t.Fatalf("list body = %s", response.Body.String())
	}
}

func TestProviderConnectionSensitiveMutationsNeverEchoSecrets(t *testing.T) {
	providers := &fakeProviderConnectionAdministration{}
	providers.register = func(_ context.Context, actor tenantadmin.ActorEnvelope, key string, command providerconnection.RegisterCommand) (providerconnection.MutationResult, error) {
		if actor.ID != "user-1" || actor.Reason != "configure provider" || key != "register-provider" || string(command.Secret) != "provider-secret" {
			t.Fatalf("actor/key/command = %#v / %q / %#v", actor, key, command)
		}
		return providerconnection.MutationResult{Connection: providerconnection.ProviderConnection{
			ID: command.ID, Provider: command.Provider, DisplayName: command.DisplayName, BaseURL: command.BaseURL,
			Region: command.Region, CredentialScope: command.CredentialScope,
			AdministrativeStatus: providerconnection.StatusDisabled, CapabilityDeclaration: command.CapabilityDeclaration,
			CredentialVersion: 1, Revision: 1,
		}}, nil
	}
	providers.rotation = func(_ context.Context, actor tenantadmin.ActorEnvelope, key string, command providerconnection.RotationCommand) (providerconnection.OperationResult, error) {
		if actor.Reason != "rotate provider" || key != "rotate-provider" || command.ExpectedRevision != 1 || string(command.Secret) != "rotated-secret" {
			t.Fatalf("rotation actor/key/command = %#v / %q / %#v", actor, key, command)
		}
		return providerconnection.OperationResult{Operation: providerconnection.Operation{
			ID: "pop_rotation", Type: providerconnection.OperationCredentialRotation,
			ConnectionID: command.ConnectionID, ExpectedRevision: command.ExpectedRevision,
			Status: providerconnection.OperationQueued,
		}}, nil
	}
	handler := controlapi.New(controlapi.Config{
		Administration: &fakeAdministration{}, ProviderConnections: providers,
		Verifier: fixedVerifier(controlapi.VerifiedIdentity{ActorType: "human", ActorID: "user-1", Scopes: []string{tenantadmin.ScopePlatformWrite}}),
	})
	register := httptest.NewRequest(http.MethodPost, "/control/v1/provider-connections", jsonBody(t, map[string]any{
		"id": "pc-openai", "provider": "openai", "display_name": "OpenAI", "base_url": "https://api.openai.test/v1",
		"region": "us-west1", "credential_scope": "organization-a", "secret": "provider-secret",
		"capability_declaration": map[string]any{"revision": 1, "features": map[string]string{"text": "native"}},
		"reason":                 "configure provider",
	}))
	register.Header.Set("Authorization", "Bearer valid")
	register.Header.Set("Idempotency-Key", "register-provider")
	registerResponse := httptest.NewRecorder()
	handler.ServeHTTP(registerResponse, register)
	if registerResponse.Code != http.StatusCreated || bytes.Contains(registerResponse.Body.Bytes(), []byte("provider-secret")) ||
		bytes.Contains(registerResponse.Body.Bytes(), []byte("secret_ref")) {
		t.Fatalf("register status/body = %d/%s", registerResponse.Code, registerResponse.Body.String())
	}
	rotation := httptest.NewRequest(http.MethodPost, "/control/v1/provider-connections/pc-openai/credential-rotations", jsonBody(t, map[string]any{
		"expected_revision": 1, "secret": "rotated-secret", "reason": "rotate provider",
	}))
	rotation.Header.Set("Authorization", "Bearer valid")
	rotation.Header.Set("Idempotency-Key", "rotate-provider")
	rotationResponse := httptest.NewRecorder()
	handler.ServeHTTP(rotationResponse, rotation)
	if rotationResponse.Code != http.StatusAccepted || rotationResponse.Header().Get("Location") != "/control/v1/provider-operations/pop_rotation" ||
		bytes.Contains(rotationResponse.Body.Bytes(), []byte("rotated-secret")) {
		t.Fatalf("rotation status/headers/body = %d/%#v/%s", rotationResponse.Code, rotationResponse.Header(), rotationResponse.Body.String())
	}
	callerAuthorized := httptest.NewRequest(http.MethodPost, "/control/v1/provider-connections/pc-openai/probes", jsonBody(t, map[string]any{
		"expected_revision": 1, "live_authorized": true, "reason": "caller assertion must not authorize live work",
	}))
	callerAuthorized.Header.Set("Authorization", "Bearer valid")
	callerAuthorized.Header.Set("Idempotency-Key", "probe-provider")
	callerAuthorizedResponse := httptest.NewRecorder()
	handler.ServeHTTP(callerAuthorizedResponse, callerAuthorized)
	if callerAuthorizedResponse.Code != http.StatusBadRequest {
		t.Fatalf("caller live authorization status/body = %d/%s", callerAuthorizedResponse.Code, callerAuthorizedResponse.Body.String())
	}
}

func TestCreateRoutingCatalogDraftUsesVerifiedActorAndVersionHeaders(t *testing.T) {
	routing := &fakeRoutingCatalogAdministration{}
	routing.create = func(_ context.Context, actor tenantadmin.ActorEnvelope, key string, command routingcatalog.CreateDraftCommand) (routingcatalog.DraftResult, error) {
		if actor.ID != "user-1" || actor.Reason != "prepare routing" || key != "create-routing-draft" || command.ID != "draft-a" {
			t.Fatalf("actor/key/command = %#v / %q / %#v", actor, key, command)
		}
		return routingcatalog.DraftResult{Draft: routingcatalog.Draft{ID: command.ID, BaseRevision: command.BaseRevision, Document: command.Document, Status: routingcatalog.DraftOpen, Revision: 1}}, nil
	}
	handler := controlapi.New(controlapi.Config{
		Administration: &fakeAdministration{}, RoutingCatalog: routing,
		Verifier: fixedVerifier(controlapi.VerifiedIdentity{ActorType: "human", ActorID: "user-1", Scopes: []string{tenantadmin.ScopePlatformWrite}}),
	})
	request := httptest.NewRequest(http.MethodPost, "/control/v1/routing-catalog/drafts", jsonBody(t, map[string]any{
		"id": "draft-a", "base_revision": 3, "document": map[string]any{"routes": []any{}}, "reason": "prepare routing",
	}))
	request.Header.Set("Authorization", "Bearer valid")
	request.Header.Set("Idempotency-Key", "create-routing-draft")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || response.Header().Get("ETag") != `"1"` || response.Header().Get("Location") != "/control/v1/routing-catalog/drafts/draft-a" {
		t.Fatalf("status/headers/body = %d/%#v/%s", response.Code, response.Header(), response.Body.String())
	}
}

func TestRoutingCatalogProbeQueuesOneBudgetedOperationPerProviderConnection(t *testing.T) {
	routing := &fakeRoutingCatalogAdministration{}
	routing.getDraft = func(_ context.Context, actor tenantadmin.ActorEnvelope, draftID string) (routingcatalog.Draft, error) {
		if actor.ID != "user-1" || draftID != "draft-a" {
			t.Fatalf("actor/draft = %#v / %q", actor, draftID)
		}
		route := routingcatalog.ManagedRoute{ProviderConnectionID: "pc-openai"}
		return routingcatalog.Draft{ID: draftID, Revision: 4, Document: routingcatalog.Document{Routes: []routingcatalog.ManagedRoute{route, route}}}, nil
	}
	providers := &fakeProviderConnectionAdministration{}
	providers.get = func(context.Context, tenantadmin.ActorEnvelope, string) (providerconnection.ProviderConnection, error) {
		return providerconnection.ProviderConnection{ID: "pc-openai", Revision: 7}, nil
	}
	providers.probe = func(_ context.Context, actor tenantadmin.ActorEnvelope, key string, command providerconnection.OperationCommand) (providerconnection.OperationResult, error) {
		if actor.Reason != "verify draft providers" || key == "probe-routing-draft" || command.ConnectionID != "pc-openai" || command.ExpectedRevision != 7 {
			t.Fatalf("actor/key/command = %#v / %q / %#v", actor, key, command)
		}
		return providerconnection.OperationResult{Operation: providerconnection.Operation{ID: "pop-probe", ConnectionID: command.ConnectionID, Status: providerconnection.OperationQueued}}, nil
	}
	handler := controlapi.New(controlapi.Config{
		Administration: &fakeAdministration{}, RoutingCatalog: routing, ProviderConnections: providers,
		Verifier: fixedVerifier(controlapi.VerifiedIdentity{ActorType: "human", ActorID: "user-1", Scopes: []string{tenantadmin.ScopePlatformWrite}}),
	})
	request := httptest.NewRequest(http.MethodPost, "/control/v1/routing-catalog/drafts/draft-a/probe", jsonBody(t, map[string]any{
		"expected_revision": 4, "reason": "verify draft providers",
	}))
	request.Header.Set("Authorization", "Bearer valid")
	request.Header.Set("Idempotency-Key", "probe-routing-draft")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status/body = %d/%s", response.Code, response.Body.String())
	}
	var body struct {
		Data []providerconnection.Operation `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || len(body.Data) != 1 || body.Data[0].ID != "pop-probe" {
		t.Fatalf("probe response = %#v / %v", body, err)
	}
}

type fakeAdministration struct {
	create func(context.Context, tenantadmin.ActorEnvelope, string, tenantadmin.CreateTenantCommand) (tenantadmin.MutationResult, error)
	update func(context.Context, tenantadmin.ActorEnvelope, string, tenantadmin.UpdateTenantCommand) (tenantadmin.MutationResult, error)
}

type credentialAdministrationFunc func(context.Context, tenantadmin.ActorEnvelope, string, credentialadmin.IssueCommand) (credentialadmin.IssueResult, error)

type fakeCredentialAdministration struct {
	list func(context.Context, tenantadmin.ActorEnvelope, credentialadmin.CredentialFilter) (credentialadmin.CredentialPage, error)
}

type fakeProviderConnectionAdministration struct {
	register func(context.Context, tenantadmin.ActorEnvelope, string, providerconnection.RegisterCommand) (providerconnection.MutationResult, error)
	rotation func(context.Context, tenantadmin.ActorEnvelope, string, providerconnection.RotationCommand) (providerconnection.OperationResult, error)
	get      func(context.Context, tenantadmin.ActorEnvelope, string) (providerconnection.ProviderConnection, error)
	probe    func(context.Context, tenantadmin.ActorEnvelope, string, providerconnection.OperationCommand) (providerconnection.OperationResult, error)
}

type fakeRoutingCatalogAdministration struct {
	create   func(context.Context, tenantadmin.ActorEnvelope, string, routingcatalog.CreateDraftCommand) (routingcatalog.DraftResult, error)
	getDraft func(context.Context, tenantadmin.ActorEnvelope, string) (routingcatalog.Draft, error)
}

func (fake *fakeRoutingCatalogAdministration) CreateDraft(ctx context.Context, actor tenantadmin.ActorEnvelope, key string, command routingcatalog.CreateDraftCommand) (routingcatalog.DraftResult, error) {
	return fake.create(ctx, actor, key, command)
}
func (fake *fakeRoutingCatalogAdministration) GetDraft(ctx context.Context, actor tenantadmin.ActorEnvelope, draftID string) (routingcatalog.Draft, error) {
	return fake.getDraft(ctx, actor, draftID)
}
func (*fakeRoutingCatalogAdministration) UpdateDraft(context.Context, tenantadmin.ActorEnvelope, string, routingcatalog.UpdateDraftCommand) (routingcatalog.DraftResult, error) {
	panic("unexpected UpdateDraft")
}
func (*fakeRoutingCatalogAdministration) ValidateDraft(context.Context, tenantadmin.ActorEnvelope, routingcatalog.ValidateDraftCommand) (routingcatalog.DraftResult, error) {
	panic("unexpected ValidateDraft")
}
func (*fakeRoutingCatalogAdministration) PublishDraft(context.Context, tenantadmin.ActorEnvelope, string, routingcatalog.PublishDraftCommand) (routingcatalog.PublicationResult, error) {
	panic("unexpected PublishDraft")
}
func (*fakeRoutingCatalogAdministration) Current(context.Context, tenantadmin.ActorEnvelope) (routingcatalog.Revision, error) {
	panic("unexpected Current")
}
func (*fakeRoutingCatalogAdministration) ListRevisions(context.Context, tenantadmin.ActorEnvelope, int64, int) (routingcatalog.RevisionPage, error) {
	panic("unexpected ListRevisions")
}
func (*fakeRoutingCatalogAdministration) GetRevision(context.Context, tenantadmin.ActorEnvelope, int64) (routingcatalog.Revision, error) {
	panic("unexpected GetRevision")
}
func (*fakeRoutingCatalogAdministration) Restore(context.Context, tenantadmin.ActorEnvelope, string, routingcatalog.RestoreCommand) (routingcatalog.PublicationResult, error) {
	panic("unexpected Restore")
}
func (*fakeRoutingCatalogAdministration) GetPublication(context.Context, tenantadmin.ActorEnvelope, string) (routingcatalog.Publication, error) {
	panic("unexpected GetPublication")
}

func (fake *fakeProviderConnectionAdministration) Register(ctx context.Context, actor tenantadmin.ActorEnvelope, key string, command providerconnection.RegisterCommand) (providerconnection.MutationResult, error) {
	return fake.register(ctx, actor, key, command)
}
func (fake *fakeProviderConnectionAdministration) Get(ctx context.Context, actor tenantadmin.ActorEnvelope, connectionID string) (providerconnection.ProviderConnection, error) {
	return fake.get(ctx, actor, connectionID)
}
func (*fakeProviderConnectionAdministration) List(context.Context, tenantadmin.ActorEnvelope, providerconnection.ConnectionFilter) (providerconnection.ConnectionPage, error) {
	panic("unexpected List")
}
func (*fakeProviderConnectionAdministration) Update(context.Context, tenantadmin.ActorEnvelope, string, providerconnection.UpdateCommand) (providerconnection.MutationResult, error) {
	panic("unexpected Update")
}
func (*fakeProviderConnectionAdministration) Enable(context.Context, tenantadmin.ActorEnvelope, string, providerconnection.StatusCommand) (providerconnection.MutationResult, error) {
	panic("unexpected Enable")
}
func (*fakeProviderConnectionAdministration) Disable(context.Context, tenantadmin.ActorEnvelope, string, providerconnection.StatusCommand) (providerconnection.MutationResult, error) {
	panic("unexpected Disable")
}
func (fake *fakeProviderConnectionAdministration) RequestProbe(ctx context.Context, actor tenantadmin.ActorEnvelope, key string, command providerconnection.OperationCommand) (providerconnection.OperationResult, error) {
	return fake.probe(ctx, actor, key, command)
}
func (*fakeProviderConnectionAdministration) RequestDiscovery(context.Context, tenantadmin.ActorEnvelope, string, providerconnection.OperationCommand) (providerconnection.OperationResult, error) {
	panic("unexpected RequestDiscovery")
}
func (fake *fakeProviderConnectionAdministration) RequestRotation(ctx context.Context, actor tenantadmin.ActorEnvelope, key string, command providerconnection.RotationCommand) (providerconnection.OperationResult, error) {
	return fake.rotation(ctx, actor, key, command)
}
func (*fakeProviderConnectionAdministration) GetOperation(context.Context, tenantadmin.ActorEnvelope, string) (providerconnection.Operation, error) {
	panic("unexpected GetOperation")
}

func (f *fakeCredentialAdministration) Issue(context.Context, tenantadmin.ActorEnvelope, string, credentialadmin.IssueCommand) (credentialadmin.IssueResult, error) {
	panic("unexpected Issue")
}

func (f *fakeCredentialAdministration) List(ctx context.Context, actor tenantadmin.ActorEnvelope, filter credentialadmin.CredentialFilter) (credentialadmin.CredentialPage, error) {
	return f.list(ctx, actor, filter)
}

func (function credentialAdministrationFunc) Issue(ctx context.Context, actor tenantadmin.ActorEnvelope, key string, command credentialadmin.IssueCommand) (credentialadmin.IssueResult, error) {
	return function(ctx, actor, key, command)
}

func (credentialAdministrationFunc) Get(context.Context, tenantadmin.ActorEnvelope, string, string) (credentialadmin.Credential, error) {
	panic("unexpected Get")
}
func (credentialAdministrationFunc) List(context.Context, tenantadmin.ActorEnvelope, credentialadmin.CredentialFilter) (credentialadmin.CredentialPage, error) {
	panic("unexpected List")
}
func (credentialAdministrationFunc) Update(context.Context, tenantadmin.ActorEnvelope, string, credentialadmin.UpdateCommand) (credentialadmin.MutationResult, error) {
	panic("unexpected Update")
}
func (credentialAdministrationFunc) Revoke(context.Context, tenantadmin.ActorEnvelope, string, credentialadmin.RevokeCommand) (credentialadmin.MutationResult, error) {
	panic("unexpected Revoke")
}
func (credentialAdministrationFunc) Rotate(context.Context, tenantadmin.ActorEnvelope, string, credentialadmin.RotateCommand) (credentialadmin.RotationResult, error) {
	panic("unexpected Rotate")
}
func (credentialAdministrationFunc) GetPolicy(context.Context, tenantadmin.ActorEnvelope, string, string) (core.APIKeyPolicy, error) {
	panic("unexpected GetPolicy")
}
func (credentialAdministrationFunc) PublishPolicy(context.Context, tenantadmin.ActorEnvelope, string, credentialadmin.PublishPolicyCommand) (credentialadmin.MutationResult, error) {
	panic("unexpected PublishPolicy")
}
func (credentialAdministrationFunc) ListPolicyRevisions(context.Context, tenantadmin.ActorEnvelope, string, string, string, int) (credentialadmin.PolicyRevisionPage, error) {
	panic("unexpected ListPolicyRevisions")
}
func (credentialAdministrationFunc) GetEffectivePolicy(context.Context, tenantadmin.ActorEnvelope, string, string) (credentialadmin.EffectivePolicy, error) {
	panic("unexpected GetEffectivePolicy")
}

func (*fakeCredentialAdministration) Get(context.Context, tenantadmin.ActorEnvelope, string, string) (credentialadmin.Credential, error) {
	panic("unexpected Get")
}
func (*fakeCredentialAdministration) Update(context.Context, tenantadmin.ActorEnvelope, string, credentialadmin.UpdateCommand) (credentialadmin.MutationResult, error) {
	panic("unexpected Update")
}
func (*fakeCredentialAdministration) Revoke(context.Context, tenantadmin.ActorEnvelope, string, credentialadmin.RevokeCommand) (credentialadmin.MutationResult, error) {
	panic("unexpected Revoke")
}
func (*fakeCredentialAdministration) Rotate(context.Context, tenantadmin.ActorEnvelope, string, credentialadmin.RotateCommand) (credentialadmin.RotationResult, error) {
	panic("unexpected Rotate")
}
func (*fakeCredentialAdministration) GetPolicy(context.Context, tenantadmin.ActorEnvelope, string, string) (core.APIKeyPolicy, error) {
	panic("unexpected GetPolicy")
}
func (*fakeCredentialAdministration) PublishPolicy(context.Context, tenantadmin.ActorEnvelope, string, credentialadmin.PublishPolicyCommand) (credentialadmin.MutationResult, error) {
	panic("unexpected PublishPolicy")
}
func (*fakeCredentialAdministration) ListPolicyRevisions(context.Context, tenantadmin.ActorEnvelope, string, string, string, int) (credentialadmin.PolicyRevisionPage, error) {
	panic("unexpected ListPolicyRevisions")
}
func (*fakeCredentialAdministration) GetEffectivePolicy(context.Context, tenantadmin.ActorEnvelope, string, string) (credentialadmin.EffectivePolicy, error) {
	panic("unexpected GetEffectivePolicy")
}

func (f *fakeAdministration) CreateTenant(ctx context.Context, actor tenantadmin.ActorEnvelope, key string, command tenantadmin.CreateTenantCommand) (tenantadmin.MutationResult, error) {
	return f.create(ctx, actor, key, command)
}
func (f *fakeAdministration) UpdateTenant(ctx context.Context, actor tenantadmin.ActorEnvelope, key string, command tenantadmin.UpdateTenantCommand) (tenantadmin.MutationResult, error) {
	return f.update(ctx, actor, key, command)
}
func (f *fakeAdministration) TransitionTenant(context.Context, tenantadmin.ActorEnvelope, string, tenantadmin.TransitionTenantCommand) (tenantadmin.MutationResult, error) {
	panic("unexpected TransitionTenant")
}
func (f *fakeAdministration) PublishTenantPolicy(context.Context, tenantadmin.ActorEnvelope, string, tenantadmin.PublishPolicyCommand) (tenantadmin.MutationResult, error) {
	panic("unexpected PublishTenantPolicy")
}
func (f *fakeAdministration) GetTenant(context.Context, tenantadmin.ActorEnvelope, string) (access.Tenant, error) {
	panic("unexpected GetTenant")
}
func (f *fakeAdministration) ListTenants(context.Context, tenantadmin.ActorEnvelope, tenantadmin.TenantFilter) (tenantadmin.TenantPage, error) {
	panic("unexpected ListTenants")
}
func (f *fakeAdministration) GetTenantPolicy(context.Context, tenantadmin.ActorEnvelope, string) (access.Tenant, error) {
	panic("unexpected GetTenantPolicy")
}
func (f *fakeAdministration) ListTenantPolicyRevisions(context.Context, tenantadmin.ActorEnvelope, string, string, int) (tenantadmin.PolicyRevisionPage, error) {
	panic("unexpected ListTenantPolicyRevisions")
}

func fixedVerifier(identity controlapi.VerifiedIdentity) controlapi.IdentityVerifier {
	return controlapi.IdentityVerifierFunc(func(context.Context, string) (controlapi.VerifiedIdentity, error) { return identity, nil })
}

type gatewayVerifierFunc func(context.Context, string, string, string, []byte) (operations.GatewayIdentity, error)

func (function gatewayVerifierFunc) Verify(ctx context.Context, authorization, method, path string, body []byte) (operations.GatewayIdentity, error) {
	return function(ctx, authorization, method, path, body)
}

type observationIngestorFunc func(context.Context, operations.GatewayIdentity, operations.GatewayObservation) error

func (function observationIngestorFunc) RecordGatewayObservation(ctx context.Context, identity operations.GatewayIdentity, observation operations.GatewayObservation) error {
	return function(ctx, identity, observation)
}

func jsonBody(t *testing.T, value any) *bytes.Reader {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(payload)
}
