package controlapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/toddzheng/llm-gateway/internal/access"
	"github.com/toddzheng/llm-gateway/internal/controlapi"
	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/credentialadmin"
	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

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

type fakeAdministration struct {
	create func(context.Context, tenantadmin.ActorEnvelope, string, tenantadmin.CreateTenantCommand) (tenantadmin.MutationResult, error)
	update func(context.Context, tenantadmin.ActorEnvelope, string, tenantadmin.UpdateTenantCommand) (tenantadmin.MutationResult, error)
}

type credentialAdministrationFunc func(context.Context, tenantadmin.ActorEnvelope, string, credentialadmin.IssueCommand) (credentialadmin.IssueResult, error)

func (function credentialAdministrationFunc) Issue(ctx context.Context, actor tenantadmin.ActorEnvelope, key string, command credentialadmin.IssueCommand) (credentialadmin.IssueResult, error) {
	return function(ctx, actor, key, command)
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

func jsonBody(t *testing.T, value any) *bytes.Reader {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(payload)
}
