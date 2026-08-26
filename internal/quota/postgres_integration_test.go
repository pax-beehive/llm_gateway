//go:build integration

package quota_test

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
	"github.com/toddzheng/llm-gateway/internal/quota"
	"github.com/toddzheng/llm-gateway/internal/store"
)

func TestPostgresQuotaReservationHonorsTenantAndAPIKeyLimits(t *testing.T) {
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

	tenantID := fmt.Sprintf("quota-%d", time.Now().UnixNano())
	accessService, err := access.NewPostgresService(db, []byte("integration-test-pepper"))
	if err != nil {
		t.Fatal(err)
	}
	tenantLimits := core.QuotaLimits{MonthlySpendMicros: limit(100), RefreshMonthlySpendMicros: limit(100), Currency: "USD"}
	keyLimits := core.QuotaLimits{MonthlySpendMicros: limit(60), RefreshMonthlySpendMicros: limit(60), Currency: "USD"}
	if err := accessService.CreateTenant(ctx, access.Tenant{
		ID: tenantID, Slug: tenantID, DisplayName: "Quota integration tenant", Status: access.TenantActive,
		HomeRegion: "local", ExecutionEpoch: 1,
		Policy: core.TenantPolicy{Revision: 1, Limits: tenantLimits},
	}, access.ChangeActor{Type: "test", ID: "integration"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM quota_reservations WHERE tenant_id = $1`, tenantID)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM cache_refresh_intents WHERE tenant_id = $1`, tenantID)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM cache_leases WHERE tenant_id = $1`, tenantID)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM tenants WHERE id = $1`, tenantID)
	})
	key, err := accessService.ImportAPIKey(ctx, access.APIKeySpec{
		TenantID: tenantID, Name: "quota key", RawKey: "gw_test_quota_" + tenantID,
		Policy: core.APIKeyPolicy{Revision: 1, Limits: keyLimits},
	})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 8, 26, 8, 15, 0, 0, time.UTC)
	currentTime := now
	controller := quota.NewPostgresController(db, func() time.Time { return currentTime })
	first, err := controller.Reserve(ctx, quota.ReservationRequest{
		TenantID: tenantID, APIKeyID: key.ID, ResponseID: "response-1",
		TenantPolicyRevision: 1, APIKeyPolicyRevision: 1,
		TenantLimits: tenantLimits, APIKeyLimits: keyLimits,
		Requests: 1, ReservedSpendMicros: 40, Currency: "USD", ExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = controller.Reserve(ctx, quota.ReservationRequest{
		TenantID: tenantID, APIKeyID: key.ID, ResponseID: "response-2",
		TenantPolicyRevision: 1, APIKeyPolicyRevision: 1,
		TenantLimits: tenantLimits, APIKeyLimits: keyLimits,
		Requests: 1, ReservedSpendMicros: 30, Currency: "USD", ExpiresAt: now.Add(time.Minute),
	})
	if !errors.Is(err, quota.ErrExceeded) {
		t.Fatalf("second reservation error = %v, want quota exceeded", err)
	}
	if err := controller.Release(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	second, err := controller.Reserve(ctx, quota.ReservationRequest{
		TenantID: tenantID, APIKeyID: key.ID, ResponseID: "response-2",
		TenantPolicyRevision: 1, APIKeyPolicyRevision: 1,
		TenantLimits: tenantLimits, APIKeyLimits: keyLimits,
		Requests: 1, ReservedSpendMicros: 30, Currency: "USD", ExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Commit(ctx, second.ID, quota.ActualUsage{Requests: 1, SpendMicros: 20}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := controller.APIKeySnapshot(ctx, tenantID, key.ID, "USD", now)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.MonthlySpend.Reserved != 0 || snapshot.MonthlySpend.Committed != 20 {
		t.Fatalf("monthly spend snapshot = %#v", snapshot.MonthlySpend)
	}
	results := make(chan struct {
		reservation quota.Reservation
		err         error
	}, 2)
	for index := 0; index < 2; index++ {
		go func(index int) {
			reservation, err := controller.Reserve(ctx, quota.ReservationRequest{
				TenantID: tenantID, APIKeyID: key.ID, ResponseID: fmt.Sprintf("concurrent-%d", index),
				TenantPolicyRevision: 1, APIKeyPolicyRevision: 1,
				TenantLimits: tenantLimits, APIKeyLimits: keyLimits,
				Requests: 1, ReservedSpendMicros: 30, Currency: "USD", ExpiresAt: now.Add(time.Minute),
			})
			results <- struct {
				reservation quota.Reservation
				err         error
			}{reservation, err}
		}(index)
	}
	var concurrentWinner quota.Reservation
	var successes, denials int
	for range 2 {
		result := <-results
		if result.err == nil {
			successes++
			concurrentWinner = result.reservation
		} else if errors.Is(result.err, quota.ErrExceeded) {
			denials++
		} else {
			t.Fatal(result.err)
		}
	}
	if successes != 1 || denials != 1 {
		t.Fatalf("concurrent reservation outcomes = %d successes/%d denials", successes, denials)
	}
	if err := controller.Release(ctx, concurrentWinner.ID); err != nil {
		t.Fatal(err)
	}

	for _, intentID := range []string{"refresh-1", "refresh-2"} {
		leaseID := "lease-" + intentID
		if _, err := db.ExecContext(ctx, `
			INSERT INTO cache_leases (
				tenant_id,id,revision,route_id,provider,model,credential_scope,region,
				cache_key,prefix_hash,estimated_expires_at,original_expires_at,fencing_token,status
			) VALUES ($1,$2,1,'route','provider','model','scope','local',$2,$2,$3,$3,1,'active')`,
			tenantID, leaseID, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO cache_refresh_intents (
				tenant_id,id,sponsor_api_key_id,cache_lease_id,cache_lease_revision,fencing_token,
				status,expected_net_saving,scheduled_for,candidate
			) VALUES ($1,$2,$3,$4,1,1,'running',1,$5,'{}'::jsonb)`,
			tenantID, intentID, key.ID, leaseID, now); err != nil {
			t.Fatal(err)
		}
	}
	refresh, err := controller.ReserveRefresh(ctx, quota.RefreshReservationRequest{
		TenantID: tenantID, APIKeyID: key.ID, CacheRefreshIntentID: "refresh-1",
		TenantPolicyRevision: 1, APIKeyPolicyRevision: 1, TenantLimits: tenantLimits, APIKeyLimits: keyLimits,
		ReservedSpendMicros: 40, Currency: "USD", ExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = controller.ReserveRefresh(ctx, quota.RefreshReservationRequest{
		TenantID: tenantID, APIKeyID: key.ID, CacheRefreshIntentID: "refresh-2",
		TenantPolicyRevision: 1, APIKeyPolicyRevision: 1, TenantLimits: tenantLimits, APIKeyLimits: keyLimits,
		ReservedSpendMicros: 30, Currency: "USD", ExpiresAt: now.Add(time.Minute),
	})
	if !errors.Is(err, quota.ErrExceeded) {
		t.Fatalf("second refresh reservation error = %v, want quota exceeded", err)
	}
	if err := controller.Uncertain(ctx, refresh.ID); err != nil {
		t.Fatal(err)
	}
	var uncertainMarked bool
	if err := db.QueryRowContext(ctx, `SELECT uncertain_at IS NOT NULL FROM quota_reservations WHERE id = $1`, refresh.ID).Scan(&uncertainMarked); err != nil || !uncertainMarked {
		t.Fatalf("uncertain reservation marker = %v / %v", uncertainMarked, err)
	}
	currentTime = now.Add(2 * time.Minute)
	settled, err := controller.Reconcile(ctx, 10)
	if err != nil || settled != 1 {
		t.Fatalf("reconcile expired refresh reservation = %d / %v", settled, err)
	}
	var status string
	if err := db.QueryRowContext(ctx, `SELECT status FROM quota_reservations WHERE id = $1`, refresh.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "uncertain" {
		t.Fatalf("expired reservation status = %q, want uncertain", status)
	}
	snapshot, err = controller.APIKeySnapshot(ctx, tenantID, key.ID, "USD", now)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.RefreshMonthlySpend.Reserved != 0 || snapshot.RefreshMonthlySpend.Committed != 40 {
		t.Fatalf("refresh monthly spend snapshot = %#v", snapshot.RefreshMonthlySpend)
	}
}
