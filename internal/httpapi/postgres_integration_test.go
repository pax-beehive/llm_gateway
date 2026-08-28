//go:build integration

package httpapi_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/toddzheng/llm-gateway/internal/access"
	"github.com/toddzheng/llm-gateway/internal/accessprojection"
	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/httpapi"
	"github.com/toddzheng/llm-gateway/internal/provider"
	"github.com/toddzheng/llm-gateway/internal/quota"
	"github.com/toddzheng/llm-gateway/internal/runtime"
	"github.com/toddzheng/llm-gateway/internal/store"
)

func TestPersistedAPIKeyRequestIsAdmittedAndRateLimited(t *testing.T) {
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
	responseStore := store.NewPostgresResponseStore(db)
	if err := responseStore.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	tenantID := fmt.Sprintf("http-quota-%d", time.Now().UnixNano())
	accessService, err := access.NewPostgresService(db, []byte("integration-test-pepper"))
	if err != nil {
		t.Fatal(err)
	}
	tenantPolicy := core.TenantPolicy{
		Revision: 1,
		Limits:   core.QuotaLimits{RequestsPerMinute: httpLimit(1), Currency: "USD"},
	}
	if err := accessService.CreateTenant(ctx, access.Tenant{
		ID: tenantID, Slug: tenantID, DisplayName: "HTTP quota tenant", Status: access.TenantActive,
		HomeRegion: "local", ExecutionEpoch: 1, Policy: tenantPolicy,
	}, access.ChangeActor{Type: "test", ID: "integration"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM tenants WHERE id = $1`, tenantID)
	})
	rawKey := "gw_test_http_quota_" + tenantID
	key, err := accessService.ImportAPIKey(ctx, access.APIKeySpec{
		TenantID: tenantID, Name: "HTTP key", RawKey: rawKey,
		Policy: core.APIKeyPolicy{Revision: 1},
	})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 8, 26, 8, 30, 0, 0, time.UTC)
	quotaController := quota.NewPostgresController(db, func() time.Time { return now })
	route := provider.Route{
		ID: "echo", Provider: "echo", Model: "echo-v1", Region: "local", HomeRegion: "local", Healthy: true,
		Profile:  provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{"text": provider.CapabilityNative}},
		Executor: provider.NewEchoExecutor(),
		PriceSnapshot: core.PriceSnapshot{
			ID: "echo-price", Provider: "echo", Model: "echo-v1", Region: "local", Currency: "USD",
			EffectiveAt: 1, Source: "integration-test",
		},
	}
	engine := runtime.NewWithOptions(responseStore, provider.NewRouter(route), runtime.Options{QuotaController: quotaController})
	handler := httpapi.New(httpapi.Config{
		Runtime: engine, ModelCatalog: provider.NewRouter(route), Authenticator: accessService, LocalRegion: "local",
	})

	first := performJSON(t, handler, rawKey, http.MethodPost, "/v1/responses", map[string]any{
		"model": "echo-v1", "input": "first",
	})
	if first.Code != http.StatusOK {
		t.Fatalf("first status/body = %d / %s", first.Code, first.Body.String())
	}
	second := performJSON(t, handler, rawKey, http.MethodPost, "/v1/responses", map[string]any{
		"model": "echo-v1", "input": "second",
	})
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second status/body = %d / %s", second.Code, second.Body.String())
	}
	snapshot, err := quotaController.APIKeySnapshot(ctx, tenantID, key.ID, "USD", now)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.MinuteRequests.Reserved != 0 || snapshot.MinuteRequests.Committed != 1 {
		t.Fatalf("request quota snapshot = %#v", snapshot.MinuteRequests)
	}
}

func TestPublicGatewayAuthenticatesFromLocalProjectionWithoutControlPlane(t *testing.T) {
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
	if err := accessprojection.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}

	tenantID := fmt.Sprintf("http-projection-%d", time.Now().UnixNano())
	apiKeyID := "gak_" + tenantID
	rawKey := "gw_http_projection_" + tenantID
	pepper := []byte("http-projection-integration-pepper")
	digest := hmac.New(sha256.New, pepper)
	_, _ = digest.Write([]byte(rawKey))
	projection, err := accessprojection.New(db, accessprojection.PepperRing{
		CurrentVersion: 1, Peppers: map[int16][]byte{1: pepper},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if err := projection.ReplaceSnapshot(ctx, accessprojection.Snapshot{
		Tenant: accessprojection.TenantSnapshot{
			ID: tenantID, Status: access.TenantActive, Revision: 1, HomeRegion: "local", ExecutionEpoch: 1,
			Policy: core.TenantPolicy{Revision: 1},
		},
		Keys: []accessprojection.KeySnapshot{{
			ID: apiKeyID, Prefix: "gw_http_proj", SecretDigest: digest.Sum(nil), DigestVersion: 1,
			Status: access.APIKeyActive, Revision: 1, Policy: core.APIKeyPolicy{Revision: 1},
		}},
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM gateway_access_projection WHERE tenant_id = $1`, tenantID)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM gateway_access_heads WHERE aggregate_id IN ($1, $2)`, tenantID, apiKeyID)
	})

	route := provider.Route{
		ID: "echo", Provider: "echo", Model: "echo-v1", Region: "local", HomeRegion: "local", Healthy: true,
		Profile: provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{
			"text": provider.CapabilityNative,
		}},
		Executor: provider.NewEchoExecutor(),
		PriceSnapshot: core.PriceSnapshot{
			ID: "echo-price", Provider: "echo", Model: "echo-v1", Region: "local", Currency: "USD",
			EffectiveAt: 1, Source: "integration-test",
		},
	}
	responseStore := store.NewMemoryResponseStore()
	engine := runtime.New(responseStore, provider.NewRouter(route))
	handler := httpapi.New(httpapi.Config{
		Runtime: engine, ModelCatalog: provider.NewRouter(route), Authenticator: projection, LocalRegion: "local",
	})

	response := performJSON(t, handler, rawKey, http.MethodPost, "/v1/responses", map[string]any{
		"model": "echo-v1", "input": "control plane is offline",
	})
	if response.Code != http.StatusOK {
		t.Fatalf("projection-authenticated public request status/body = %d / %s", response.Code, response.Body.String())
	}
}

func httpLimit(value int64) *int64 { return &value }
