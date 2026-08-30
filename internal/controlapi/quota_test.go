package controlapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/toddzheng/llm-gateway/internal/access"
	"github.com/toddzheng/llm-gateway/internal/controlapi"
	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/quota"
	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

type quotaAdministration struct{ *fakeAdministration }

func (quotaAdministration) GetTenantPolicy(_ context.Context, actor tenantadmin.ActorEnvelope, tenantID string) (access.Tenant, error) {
	if actor.ID != "operator" || tenantID != "tenant-a" {
		panic("unexpected authorization")
	}
	return access.Tenant{ID: tenantID, Policy: core.TenantPolicy{Revision: 7, Limits: core.QuotaLimits{Currency: "USD"}}}, nil
}

type quotaQueryFunc func(context.Context, string, string, time.Time) (quota.EnforcementSnapshot, error)

func (function quotaQueryFunc) EnforcementSnapshot(ctx context.Context, tenantID, keyID string, at time.Time) (quota.EnforcementSnapshot, error) {
	return function(ctx, tenantID, keyID, at)
}

func TestTenantQuotaSnapshotUsesAuthorizedTenantQuery(t *testing.T) {
	handler := controlapi.New(controlapi.Config{Administration: quotaAdministration{&fakeAdministration{}}, Verifier: fixedVerifier(controlapi.VerifiedIdentity{ActorType: "human", ActorID: "operator", Scopes: []string{tenantadmin.ScopePlatformRead}}), QuotaSnapshots: quotaQueryFunc(func(_ context.Context, tenantID, keyID string, _ time.Time) (quota.EnforcementSnapshot, error) {
		if tenantID != "tenant-a" || keyID != "" {
			t.Fatalf("tenant/key=%s/%s", tenantID, keyID)
		}
		remaining := int64(80)
		return quota.EnforcementSnapshot{TenantID: tenantID, TenantPolicyRevision: 7, Balances: map[string]quota.Balance{"monthly_spend_micros": {Committed: 20, Remaining: &remaining}}}, nil
	})})
	request := httptest.NewRequest(http.MethodGet, "/control/v1/tenants/tenant-a/quota-snapshot", nil)
	request.Header.Set("Authorization", "Bearer valid")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status/body=%d/%s", response.Code, response.Body.String())
	}
	var body quota.EnforcementSnapshot
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.TenantPolicyRevision != 7 || body.Balances["monthly_spend_micros"].Committed != 20 {
		t.Fatalf("snapshot=%#v", body)
	}
}
