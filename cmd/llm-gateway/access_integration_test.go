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

	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/store"
)

func TestConfigureAuthenticatorBootstrapsThenUsesOnlyPersistentAPIKeyState(t *testing.T) {
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
	tenantID := fmt.Sprintf("main-access-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM tenants WHERE id = $1`, tenantID)
	})
	rawKey := "gw_test_main_access_" + tenantID
	t.Setenv("GATEWAY_API_KEY_PEPPER", "integration-test-pepper")
	t.Setenv("GATEWAY_BOOTSTRAP_ACCESS", "true")
	t.Setenv("GATEWAY_API_KEY_POLICIES_JSON", fmt.Sprintf(`{%q:{"revision":1,"allow_cache_protection":true,"limits":{"requests_per_minute":5}}}`, rawKey))
	t.Setenv("GATEWAY_API_KEY_METADATA_JSON", fmt.Sprintf(`{%q:{"environment":"integration"}}`, rawKey))
	configured, err := configureAuthenticator(ctx, db,
		map[string]string{rawKey: tenantID},
		map[string]string{tenantID: "us-west"},
		map[string]int64{tenantID: 2},
		map[string]core.TenantPolicy{tenantID: {Revision: 1}},
	)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := configured.Authenticate(ctx, rawKey)
	if err != nil || principal.TenantID != tenantID || principal.HomeRegion != "us-west" || principal.ExecutionEpoch != 2 ||
		!principal.APIKeyPolicy.AllowCacheProtection || principal.APIKeyPolicy.Limits.RequestsPerMinute == nil ||
		*principal.APIKeyPolicy.Limits.RequestsPerMinute != 5 {
		t.Fatalf("bootstrapped principal/error = %#v / %v", principal, err)
	}

	t.Setenv("GATEWAY_BOOTSTRAP_ACCESS", "false")
	persistentOnly, err := configureAuthenticator(ctx, db, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	principal, err = persistentOnly.Authenticate(ctx, rawKey)
	if err != nil || principal.TenantID != tenantID {
		t.Fatalf("persistent principal/error = %#v / %v", principal, err)
	}
}
