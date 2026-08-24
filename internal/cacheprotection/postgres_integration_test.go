//go:build integration

package cacheprotection_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/toddzheng/llm-gateway/internal/cacheprotection"
	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/provider"
	"github.com/toddzheng/llm-gateway/internal/store"
)

func TestPostgresWorkerClaimsAndRefreshesIntentExactlyOnce(t *testing.T) {
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
	defer cancel()
	if err := store.NewPostgresResponseStore(db).Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	tenantID := fmt.Sprintf("cache-integration-%d", time.Now().UnixNano())
	if _, err := db.ExecContext(ctx, `INSERT INTO tenants (id, home_region) VALUES ($1, 'local')`, tenantID); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	candidate := eligibleCandidate(now)
	candidate.Lease.Anchor.TenantID = tenantID
	candidate.Lease.EstimatedExpiresAt = now.Add(5 * time.Minute)
	candidate.Forecast.ExpectedAt = now.Add(30 * time.Minute)
	candidate.HoldoutCohort = "treatment"
	candidate.ExperimentRevision = "experiment-v1"
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM transactional_outbox WHERE tenant_id = $1`, tenantID)
		_, _ = db.Exec(`DELETE FROM cache_refresh_usage_ledger WHERE tenant_id = $1`, tenantID)
		_, _ = db.Exec(`DELETE FROM cache_refresh_intents WHERE tenant_id = $1`, tenantID)
		_, _ = db.Exec(`DELETE FROM cache_leases WHERE tenant_id = $1`, tenantID)
		_, _ = db.Exec(`DELETE FROM tenants WHERE id = $1`, tenantID)
		_, _ = db.Exec(`DELETE FROM provider_price_snapshots WHERE id = $1`, candidate.RefreshPriceSnapshot.ID)
	})
	repository := cacheprotection.NewPostgresIntentRepository(db)
	planner := cacheprotection.NewCoordinator(repository, func() time.Time { return now })
	var refreshCalls atomic.Int64
	protector := cacheProtectorStub{
		inspect: provider.CacheCapability{Supported: true},
		refresh: func(context.Context, provider.CacheAnchor) (provider.RefreshResult, error) {
			refreshCalls.Add(1)
			return provider.RefreshResult{
				Status: "succeeded", ExpiresAt: now.Add(10 * time.Minute),
				Usage: core.Usage{InputTokens: 100_000, CacheWriteInputTokens: 100_000}, UsageReliable: true,
				ProviderUsage: []byte(`{"cache_creation_input_tokens":100000}`),
			}, nil
		},
	}
	candidate.Policy.ShadowMode = true
	shadow, err := planner.Run(ctx, candidate, protector)
	if err != nil || shadow.Status != cacheprotection.IntentShadow {
		t.Fatalf("shadow intent = %#v, error = %v", shadow, err)
	}
	candidate.Policy.ShadowMode = false
	planned, err := planner.Run(ctx, candidate, protector)
	if err != nil {
		t.Fatal(err)
	}
	if planned.Status != cacheprotection.IntentPlanned {
		t.Fatalf("planned status = %q", planned.Status)
	}
	worker := cacheprotection.NewCoordinator(repository, func() time.Time { return now.Add(4*time.Minute + 55*time.Second) })
	completed, err := worker.RunDue(ctx, 10, func(provider.CacheAnchor) provider.CacheProtector { return protector })
	if err != nil {
		t.Fatal(err)
	}
	if len(completed) != 1 || completed[0].Status != cacheprotection.IntentSucceeded {
		t.Fatalf("completed intents = %#v", completed)
	}
	again, err := worker.RunDue(ctx, 10, func(provider.CacheAnchor) provider.CacheProtector { return protector })
	if err != nil || len(again) != 0 || refreshCalls.Load() != 1 {
		t.Fatalf("second claim = %#v, err %v, refresh calls %d", again, err, refreshCalls.Load())
	}
	var revision, fencingToken int64
	var status string
	if err := db.QueryRowContext(ctx, `
		SELECT revision, fencing_token, status FROM cache_leases WHERE tenant_id = $1 AND id = $2`,
		tenantID, candidate.Lease.ID).Scan(&revision, &fencingToken, &status); err != nil {
		t.Fatal(err)
	}
	if revision != candidate.Lease.Revision+1 || fencingToken != candidate.Lease.FencingToken+1 || status != "refreshed" {
		t.Fatalf("lease revision/fence/status = %d/%d/%s", revision, fencingToken, status)
	}
	var refreshAmount int64
	var refreshProviderUsage []byte
	var refreshUsageReliable bool
	if err := db.QueryRowContext(ctx, `
		SELECT amount::bigint, provider_usage, usage_reliable
		FROM cache_refresh_usage_ledger
		WHERE tenant_id = $1 AND cache_refresh_intent_id = $2`, tenantID, completed[0].ID,
	).Scan(&refreshAmount, &refreshProviderUsage, &refreshUsageReliable); err != nil {
		t.Fatal(err)
	}
	if refreshAmount != 1_250_000 || !refreshUsageReliable || string(refreshProviderUsage) != `{"cache_creation_input_tokens": 100000}` {
		t.Fatalf("refresh financial evidence = %d / %s", refreshAmount, refreshProviderUsage)
	}
	requestObserver := cacheprotection.NewCoordinator(repository, func() time.Time { return now.Add(6 * time.Minute) })
	requestResult, err := requestObserver.CustomerRequest(ctx, candidate.Lease.Anchor)
	if err != nil {
		t.Fatal(err)
	}
	evidence := requestResult.ProtectedHitCandidate
	if evidence == nil || evidence.CacheLeaseID != candidate.Lease.ID ||
		evidence.RefreshUsageID != completed[0].ID+"_usage" || evidence.HoldoutCohort != "treatment" ||
		!evidence.OriginalLeaseExpiresAt.Equal(now.Add(5*time.Minute)) ||
		!evidence.RefreshExpiresAt.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("protected hit candidate = %#v", evidence)
	}
}
