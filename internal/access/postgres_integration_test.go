//go:build integration

package access_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/toddzheng/llm-gateway/internal/access"
	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/store"
)

func TestPersistedAPIKeyAuthenticatesCurrentTenantAndKeyPolicy(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	if err := store.NewPostgresResponseStore(db).Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	tenantID := fmt.Sprintf("access-%d", time.Now().UnixNano())
	service, err := access.NewPostgresService(db, []byte("integration-test-pepper"))
	if err != nil {
		t.Fatal(err)
	}
	tenantPolicy := core.TenantPolicy{
		Revision: 1, MaxConcurrentResponses: 7, MaxInputItems: 99,
		Limits: core.QuotaLimits{RequestsPerMinute: int64Pointer(12), MonthlySpendMicros: int64Pointer(9_000_000), Currency: "USD"},
	}
	tenant := access.Tenant{
		ID: tenantID, Slug: tenantID, DisplayName: "Access integration tenant",
		Status: access.TenantActive, HomeRegion: "us-west", ExecutionEpoch: 3,
		Policy: tenantPolicy,
	}
	actor := access.ChangeActor{Type: "test", ID: "integration"}
	if err := service.CreateTenant(ctx, tenant, actor); err != nil {
		t.Fatal(err)
	}
	if err := service.CreateTenant(ctx, tenant, actor); err != nil {
		t.Fatalf("idempotent Tenant bootstrap: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM tenants WHERE id = $1`, tenantID)
	})

	rawKey := "gw_test_persisted_" + tenantID
	keyPolicy := core.APIKeyPolicy{
		Revision: 1, AllowCacheProtection: false,
		Limits: core.QuotaLimits{RequestsPerMinute: int64Pointer(5), MonthlySpendMicros: int64Pointer(1_000_000), Currency: "USD"},
	}
	keySpec := access.APIKeySpec{
		TenantID: tenantID, Name: "integration key", RawKey: rawKey, Policy: keyPolicy,
	}
	key, err := service.ImportAPIKey(ctx, keySpec)
	if err != nil {
		t.Fatal(err)
	}
	if key.ID == "" || key.Prefix == "" {
		t.Fatalf("issued key metadata = %#v", key)
	}
	repeated, err := service.ImportAPIKey(ctx, keySpec)
	if err != nil || repeated.ID != key.ID {
		t.Fatalf("idempotent API key bootstrap = %#v / %v", repeated, err)
	}
	updatedKey, err := service.UpdateAPIKeyMetadata(ctx, tenantID, key.ID, key.Revision, map[string]any{
		"environment": "integration", "owner": "platform",
	})
	if err != nil || updatedKey.Revision != key.Revision+1 || updatedKey.Metadata["environment"] != "integration" {
		t.Fatalf("updated API key metadata = %#v / %v", updatedKey, err)
	}
	if _, err := service.UpdateAPIKeyMetadata(ctx, tenantID, key.ID, key.Revision, map[string]any{"stale": true}); !errors.Is(err, access.ErrConflict) {
		t.Fatalf("stale API key metadata update = %v, want conflict", err)
	}

	principal, err := service.Authenticate(ctx, rawKey)
	if err != nil {
		t.Fatal(err)
	}
	if principal.TenantID != tenantID || principal.APIKeyID != key.ID || principal.HomeRegion != "us-west" || principal.ExecutionEpoch != 3 {
		t.Fatalf("principal = %#v", principal)
	}
	if principal.TenantPolicy.Revision != 1 || principal.APIKeyPolicy.Revision != 1 {
		t.Fatalf("principal policies = %#v / %#v", principal.TenantPolicy, principal.APIKeyPolicy)
	}
	tenantPolicy.Revision = 2
	tenantPolicy.MaxConcurrentResponses = 3
	if err := service.PublishTenantPolicy(ctx, tenantID, 1, tenantPolicy, access.ChangeActor{
		Type: "test", ID: "tenant-policy-publisher",
	}); err != nil {
		t.Fatal(err)
	}
	principal, err = service.Authenticate(ctx, rawKey)
	if err != nil || principal.TenantPolicy.Revision != 2 || principal.TenantPolicy.MaxConcurrentResponses != 3 {
		t.Fatalf("principal after Tenant policy publication = %#v / %v", principal, err)
	}
	staleTenantPolicy := tenantPolicy
	if err := service.PublishTenantPolicy(ctx, tenantID, 1, staleTenantPolicy, access.ChangeActor{
		Type: "test", ID: "stale-tenant-publisher",
	}); !errors.Is(err, access.ErrConflict) {
		t.Fatalf("stale Tenant policy publication = %v, want conflict", err)
	}

	keyPolicy.Revision = 2
	keyPolicy.AllowCacheProtection = true
	if err := service.PublishAPIKeyPolicy(ctx, tenantID, key.ID, 1, keyPolicy, access.ChangeActor{
		Type: "test", ID: "policy-publisher",
	}); err != nil {
		t.Fatal(err)
	}
	principal, err = service.Authenticate(ctx, rawKey)
	if err != nil || principal.APIKeyPolicy.Revision != 2 || !principal.APIKeyPolicy.AllowCacheProtection {
		t.Fatalf("principal after policy publication = %#v / %v", principal, err)
	}
	stale := keyPolicy
	stale.Revision = 2
	if err := service.PublishAPIKeyPolicy(ctx, tenantID, key.ID, 1, stale, access.ChangeActor{
		Type: "test", ID: "stale-publisher",
	}); !errors.Is(err, access.ErrConflict) {
		t.Fatalf("stale API key policy publication = %v, want conflict", err)
	}
	lookedUp, err := service.LookupPrincipal(ctx, tenantID, key.ID)
	if err != nil || lookedUp.APIKeyPolicy.Revision != 2 {
		t.Fatalf("lookup current principal = %#v / %v", lookedUp, err)
	}
	suspended, err := service.TransitionTenant(ctx, tenantID, 2, access.TenantSuspended, time.Now().UTC())
	if err != nil || suspended.Status != access.TenantSuspended || suspended.Revision != 3 {
		t.Fatalf("suspended Tenant = %#v / %v", suspended, err)
	}
	if _, err := service.LookupPrincipal(ctx, tenantID, key.ID); !errors.Is(err, access.ErrInvalidAPIKey) {
		t.Fatalf("principal lookup while Tenant suspended = %v", err)
	}
	reactivated, err := service.TransitionTenant(ctx, tenantID, 3, access.TenantActive, time.Now().UTC())
	if err != nil || reactivated.Status != access.TenantActive || reactivated.Revision != 4 {
		t.Fatalf("reactivated Tenant = %#v / %v", reactivated, err)
	}
	invalidPolicy := keyPolicy
	invalidPolicy.Revision = 3
	invalidPolicy.Limits.RequestsPerMinute = int64Pointer(-1)
	if err := service.PublishAPIKeyPolicy(ctx, tenantID, key.ID, 2, invalidPolicy, access.ChangeActor{
		Type: "test", ID: "invalid-policy-publisher",
	}); err == nil {
		t.Fatal("negative API key quota policy was accepted")
	}

	if err := service.RevokeAPIKey(ctx, tenantID, key.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authenticate(ctx, rawKey); !errors.Is(err, access.ErrInvalidAPIKey) {
		t.Fatalf("authentication after revoke = %v, want invalid API key", err)
	}
	expiredAt := time.Now().UTC().Add(-time.Minute)
	expiredRawKey := "gw_test_expired_" + tenantID
	if _, err := service.ImportAPIKey(ctx, access.APIKeySpec{
		TenantID: tenantID, Name: "expired key", RawKey: expiredRawKey,
		Policy: core.APIKeyPolicy{Revision: 1}, ExpiresAt: &expiredAt,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authenticate(ctx, expiredRawKey); !errors.Is(err, access.ErrInvalidAPIKey) {
		t.Fatalf("expired API key authentication = %v, want invalid API key", err)
	}
}

func int64Pointer(value int64) *int64 { return &value }
