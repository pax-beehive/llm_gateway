package controlapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/toddzheng/llm-gateway/internal/controlapi"
	"github.com/toddzheng/llm-gateway/internal/controlaudit"
	"github.com/toddzheng/llm-gateway/internal/providerconnection"
	"github.com/toddzheng/llm-gateway/internal/routingcatalog"
	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

func TestRoutingDraftListPassesStableQueryFilter(t *testing.T) {
	called := false
	handler := controlapi.New(controlapi.Config{
		Administration: &fakeAdministration{},
		RoutingCatalog: &fakeRoutingCatalogAdministration{listDrafts: func(_ context.Context, _ tenantadmin.ActorEnvelope, filter routingcatalog.DraftFilter) (routingcatalog.DraftPage, error) {
			called = true
			if filter.Status != routingcatalog.DraftValidated || filter.Cursor != "cursor-a" || filter.Limit != 25 {
				t.Fatalf("filter = %#v", filter)
			}
			return routingcatalog.DraftPage{Data: []routingcatalog.Draft{{ID: "draft-a"}}, NextCursor: "cursor-b"}, nil
		}},
		Verifier: platformReadVerifier(),
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/control/v1/routing-catalog/drafts?status=validated&cursor=cursor-a&limit=25", nil))
	if response.Code != http.StatusOK || !called || response.Body.String() == "" {
		t.Fatalf("status/called/body = %d/%v/%s", response.Code, called, response.Body.String())
	}
}

func TestProviderOperationListPassesConnectionAndStatusFilter(t *testing.T) {
	called := false
	handler := controlapi.New(controlapi.Config{
		Administration: &fakeAdministration{},
		ProviderConnections: &fakeProviderConnectionAdministration{listOperations: func(_ context.Context, _ tenantadmin.ActorEnvelope, filter providerconnection.OperationFilter) (providerconnection.OperationPage, error) {
			called = true
			if filter.ConnectionID != "pc-a" || filter.Type != providerconnection.OperationProbe || filter.Status != providerconnection.OperationFailed || filter.Limit != 10 {
				t.Fatalf("filter = %#v", filter)
			}
			return providerconnection.OperationPage{Data: []providerconnection.Operation{{ID: "op-a"}}}, nil
		}},
		Verifier: platformReadVerifier(),
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/control/v1/provider-operations?connection_id=pc-a&type=probe&status=failed&limit=10", nil))
	if response.Code != http.StatusOK || !called {
		t.Fatalf("status/called/body = %d/%v/%s", response.Code, called, response.Body.String())
	}
}

func TestControlAuditQueryAcceptsTypedAndCompatibilityFilters(t *testing.T) {
	called := false
	handler := controlapi.New(controlapi.Config{
		Administration: &fakeAdministration{},
		Audit: auditQueryFunc(func(_ context.Context, _ tenantadmin.ActorEnvelope, filter controlaudit.Filter) (controlaudit.Page, error) {
			called = true
			if filter.TenantID != "tenant-a" || filter.AggregateType != "Tenant" || filter.AggregateID != "tenant-a" ||
				filter.ActorID != "operator-a" || filter.Action != "tenant.update" || filter.Limit != 20 || filter.From.IsZero() || filter.Through.IsZero() {
				t.Fatalf("filter = %#v", filter)
			}
			return controlaudit.Page{Data: []controlaudit.Event{{ID: "audit-a", OccurredAt: time.Now().UTC()}}}, nil
		}),
		Verifier: platformReadVerifier(),
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet,
		"/control/v1/audit?tenant_id=tenant-a&aggregate_type=Tenant&resource=tenant-a&actor=operator-a&action=tenant.update&from=2026-08-01T00:00:00Z&through=2026-09-01T00:00:00Z&limit=20", nil))
	if response.Code != http.StatusOK || !called {
		t.Fatalf("status/called/body = %d/%v/%s", response.Code, called, response.Body.String())
	}
}

type auditQueryFunc func(context.Context, tenantadmin.ActorEnvelope, controlaudit.Filter) (controlaudit.Page, error)

func (function auditQueryFunc) List(ctx context.Context, actor tenantadmin.ActorEnvelope, filter controlaudit.Filter) (controlaudit.Page, error) {
	return function(ctx, actor, filter)
}

func platformReadVerifier() controlapi.IdentityVerifier {
	return fixedVerifier(controlapi.VerifiedIdentity{ActorType: "human", ActorID: "operator", Scopes: []string{tenantadmin.ScopePlatformRead}})
}
