package metering

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFilterFromRequestEnforcesTenantScope(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/metering/v1/usage/summary?tenant_id=tenant-b", nil)
	_, err := filterFromRequest(request, Identity{ActorID: "user", TenantID: "tenant-a", Scopes: []string{ScopeTenantRead}})
	if err == nil {
		t.Fatal("Tenant actor crossed Tenant boundary")
	}
	request = httptest.NewRequest(http.MethodGet, "/metering/v1/usage/summary", nil)
	filter, err := filterFromRequest(request, Identity{ActorID: "user", TenantID: "tenant-a", Scopes: []string{ScopeTenantRead}})
	if err != nil || filter.TenantID != "tenant-a" {
		t.Fatalf("filter/error=%#v/%v", filter, err)
	}
}

func TestFilterFromRequestAllowsAuthorizedPlatformAggregation(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/metering/v1/usage/summary", nil)
	filter, err := filterFromRequest(request, Identity{ActorID: "operator", Scopes: []string{ScopePlatformRead}})
	if err != nil || !filter.AllTenants || filter.TenantID != "" {
		t.Fatalf("platform filter/error=%#v/%v", filter, err)
	}
	request = httptest.NewRequest(http.MethodGet, "/metering/v1/usage/summary?tenant_id=tenant-a", nil)
	filter, err = filterFromRequest(request, Identity{ActorID: "operator", Scopes: []string{ScopePlatformRead}})
	if err != nil || filter.AllTenants || filter.TenantID != "tenant-a" {
		t.Fatalf("scoped platform filter/error=%#v/%v", filter, err)
	}
}

func TestHandlerRejectsUnauthenticatedQueries(t *testing.T) {
	service := &Service{}
	handler, err := NewHandler(service, IdentityVerifierFunc(func(context.Context, string) (Identity, error) { return Identity{}, ErrPolicyDenied }), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metering/v1/usage/summary", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d", response.Code)
	}
}
