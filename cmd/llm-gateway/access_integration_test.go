//go:build integration

package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/toddzheng/llm-gateway/internal/access"
	"github.com/toddzheng/llm-gateway/internal/accessbootstrap"
	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/migrations"
)

func TestAccessBootstrapIsSeparateFromGatewayAuthentication(t *testing.T) {
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
	if err := migrations.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	tenantID := fmt.Sprintf("main-access-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM tenants WHERE id = $1`, tenantID)
	})
	rawKey := "gw_test_main_access_" + tenantID
	t.Setenv("GATEWAY_API_KEY_PEPPER", "integration-test-pepper")
	service, err := access.NewPostgresServiceWithPeppers(db, 1, map[int16][]byte{1: []byte("integration-test-pepper")})
	if err != nil {
		t.Fatal(err)
	}
	requests := int64(5)
	if _, err := accessbootstrap.Bootstrap(ctx, service, accessbootstrap.Input{
		APIKeys: map[string]string{rawKey: tenantID}, HomeRegions: map[string]string{tenantID: "us-west"},
		ExecutionEpochs: map[string]int64{tenantID: 2}, TenantPolicies: map[string]core.TenantPolicy{tenantID: {Revision: 1}},
		APIKeyPolicies: map[string]core.APIKeyPolicy{rawKey: {Revision: 1, AllowCacheProtection: true, Limits: core.QuotaLimits{RequestsPerMinute: &requests}}},
		APIKeyMetadata: map[string]map[string]any{rawKey: {"environment": "integration"}},
	}); err != nil {
		t.Fatal(err)
	}
	configured, err := configureAuthenticator(ctx, db, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := configured.Authenticate(ctx, rawKey)
	if err != nil || principal.TenantID != tenantID || principal.HomeRegion != "us-west" || principal.ExecutionEpoch != 2 ||
		!principal.APIKeyPolicy.AllowCacheProtection || principal.APIKeyPolicy.Limits.RequestsPerMinute == nil ||
		*principal.APIKeyPolicy.Limits.RequestsPerMinute != 5 {
		t.Fatalf("bootstrapped principal/error = %#v / %v", principal, err)
	}

	persistentOnly, err := configureAuthenticator(ctx, db, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	principal, err = persistentOnly.Authenticate(ctx, rawKey)
	if err != nil || principal.TenantID != tenantID {
		t.Fatalf("persistent principal/error = %#v / %v", principal, err)
	}
}
