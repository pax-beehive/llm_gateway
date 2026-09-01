//go:build integration

package quota_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/toddzheng/llm-gateway/internal/access"
	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/migrations"
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
	responseStore := store.NewPostgresResponseStore(db)
	if err := migrations.Migrate(ctx, db); err != nil {
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
		_, _ = db.ExecContext(context.Background(), `DELETE FROM capability_usage_daily WHERE tenant_id = $1`, tenantID)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM capability_usage_ledger WHERE tenant_id = $1`, tenantID)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM quota_reservations WHERE tenant_id = $1`, tenantID)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM cache_refresh_intents WHERE tenant_id = $1`, tenantID)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM cache_leases WHERE tenant_id = $1`, tenantID)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM transactional_outbox WHERE tenant_id = $1`, tenantID)
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
		TenantID: tenantID, APIKeyID: key.ID, ResponseID: "response-1", ResponseAttemptID: "attempt-1",
		TenantPolicyRevision: 1, APIKeyPolicyRevision: 1,
		TenantLimits: tenantLimits, APIKeyLimits: keyLimits,
		Requests: 1, ReservedSpendMicros: 40, Currency: "USD", ExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	siblingAttempt, err := controller.Reserve(ctx, quota.ReservationRequest{
		TenantID: tenantID, APIKeyID: key.ID, ResponseID: "response-1", ResponseAttemptID: "attempt-1b",
		TenantPolicyRevision: 1, APIKeyPolicyRevision: 1,
		TenantLimits: tenantLimits, APIKeyLimits: keyLimits,
		Requests: 1, ReservedSpendMicros: 10, Currency: "USD", ExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("second Provider Attempt reservation for one Response: %v", err)
	}
	_, err = controller.Reserve(ctx, quota.ReservationRequest{
		TenantID: tenantID, APIKeyID: key.ID, ResponseID: "response-2", ResponseAttemptID: "attempt-2",
		PublicModel: "gateway-model", RouteID: "route-a", Region: "local",
		TenantPolicyRevision: 1, APIKeyPolicyRevision: 1,
		TenantLimits: tenantLimits, APIKeyLimits: keyLimits,
		Requests: 1, ReservedSpendMicros: 30, Currency: "USD", ExpiresAt: now.Add(time.Minute),
	})
	if !errors.Is(err, quota.ErrExceeded) {
		t.Fatalf("second reservation error = %v, want quota exceeded", err)
	}
	var denialPayload []byte
	if err := db.QueryRowContext(ctx, `SELECT payload FROM transactional_outbox WHERE tenant_id=$1 AND event_type='quota.denied' ORDER BY id DESC LIMIT 1`, tenantID).Scan(&denialPayload); err != nil {
		t.Fatal(err)
	}
	var denial quota.DenialEvent
	if err := json.Unmarshal(denialPayload, &denial); err != nil {
		t.Fatal(err)
	}
	if denial.Scope != "api_key" || denial.Dimension != "spend_micros_usd" || denial.PublicModel != "gateway-model" || denial.RouteID != "route-a" || denial.ResponseID != "response-2" {
		t.Fatalf("quota denial evidence = %#v", denial)
	}
	if err := controller.Release(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if err := controller.Release(ctx, siblingAttempt.ID); err != nil {
		t.Fatal(err)
	}
	second, err := controller.Reserve(ctx, quota.ReservationRequest{
		TenantID: tenantID, APIKeyID: key.ID, ResponseID: "response-2", ResponseAttemptID: "attempt-2",
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
	enforcement, err := controller.EnforcementSnapshot(ctx, tenantID, key.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	monthly := enforcement.Balances["monthly_spend_micros"]
	if enforcement.TenantPolicyRevision != 1 || enforcement.APIKeyPolicyRevision != 1 ||
		monthly.Committed != 20 || monthly.Reserved != 0 || monthly.Uncertain != 0 ||
		monthly.Remaining == nil || *monthly.Remaining != 40 {
		t.Fatalf("enforcement snapshot = %#v", enforcement)
	}
	capabilityReservation, err := controller.Reserve(ctx, quota.ReservationRequest{
		TenantID: tenantID, APIKeyID: key.ID, CapabilityOperationID: "embedding-operation", Capability: core.CapabilityEmbeddings,
		HomeRegion: "local", ExecutionEpoch: 1,
		TenantPolicyRevision: 1, APIKeyPolicyRevision: 1,
		TenantLimits: tenantLimits, APIKeyLimits: keyLimits,
		Requests: 1, ReservedSpendMicros: 10, ReservedEmbeddingInputUnits: 7,
		Currency: "USD", ExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := responseStore.RecordCapabilityUsage(ctx, core.CapabilityUsageRecord{
		ID: "embedding-usage", TenantID: tenantID, APIKeyID: key.ID, OperationID: "embedding-operation",
		HomeRegion: "local", ExecutionEpoch: 1,
		QuotaReservationID: capabilityReservation.ID, Capability: core.CapabilityEmbeddings,
		RouteID: "embedding-route", Provider: "openai-" + tenantID, Model: "embedding-model-" + tenantID,
		PriceSnapshot: core.PriceSnapshot{
			ID: "embedding-price-" + tenantID, Provider: "openai-" + tenantID, Model: "embedding-model-" + tenantID, Region: "local", Currency: "USD",
			EmbeddingInputPerMillionMicros: 20_000, EffectiveAt: now.Unix(), Source: "integration-test",
		},
		ProviderUsage: []byte(`{"prompt_tokens":7}`), InputUnits: 7, Dimensions: 1536,
		AmountMicros: 8, Currency: "USD", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	var capabilityStatus string
	if err := db.QueryRowContext(ctx, `SELECT status FROM quota_reservations WHERE id = $1`, capabilityReservation.ID).Scan(&capabilityStatus); err != nil {
		t.Fatal(err)
	}
	if capabilityStatus != "committed" {
		t.Fatalf("capability reservation status after usage transaction = %q", capabilityStatus)
	}
	settled, err := controller.Reconcile(ctx, 10)
	if err != nil || settled != 0 {
		t.Fatalf("reconcile already committed capability usage = %d / %v", settled, err)
	}
	snapshot, err = controller.APIKeySnapshot(ctx, tenantID, key.ID, "USD", now)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.MonthlySpend.Reserved != 0 || snapshot.MonthlySpend.Committed != 28 {
		t.Fatalf("monthly spend after capability = %#v", snapshot.MonthlySpend)
	}
	var committedEmbeddingUnits int64
	if err := db.QueryRowContext(ctx, `SELECT committed_embedding_input_units FROM quota_reservations WHERE id = $1`, capabilityReservation.ID).Scan(&committedEmbeddingUnits); err != nil {
		t.Fatal(err)
	}
	if committedEmbeddingUnits != 7 {
		t.Fatalf("committed embedding units = %d, want 7", committedEmbeddingUnits)
	}
	results := make(chan struct {
		reservation quota.Reservation
		err         error
	}, 2)
	for index := 0; index < 2; index++ {
		go func(index int) {
			reservation, err := controller.Reserve(ctx, quota.ReservationRequest{
				TenantID: tenantID, APIKeyID: key.ID, ResponseID: fmt.Sprintf("concurrent-%d", index), ResponseAttemptID: fmt.Sprintf("attempt-%d", index),
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
	settled, err = controller.Reconcile(ctx, 10)
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
	fallbackResponseID := "fallback-response-" + tenantID
	failedAttempt, err := controller.Reserve(ctx, quota.ReservationRequest{
		TenantID: tenantID, APIKeyID: key.ID, ResponseID: fallbackResponseID, ResponseAttemptID: "failed-attempt",
		TenantPolicyRevision: 1, APIKeyPolicyRevision: 1, TenantLimits: tenantLimits, APIKeyLimits: keyLimits,
		Requests: 1, ReservedSpendMicros: 5, Currency: "USD", ExpiresAt: currentTime.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	successAttempt, err := controller.Reserve(ctx, quota.ReservationRequest{
		TenantID: tenantID, APIKeyID: key.ID, ResponseID: fallbackResponseID, ResponseAttemptID: "success-attempt",
		TenantPolicyRevision: 1, APIKeyPolicyRevision: 1, TenantLimits: tenantLimits, APIKeyLimits: keyLimits,
		Requests: 1, ReservedSpendMicros: 5, Currency: "USD", ExpiresAt: currentTime.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	response := core.Response{ID: fallbackResponseID, Object: "response", CreatedAt: now.Unix(), Status: core.ResponseStatusInProgress, Model: "model", HomeRegion: "local", ExecutionEpoch: 1, Revision: 1, RetainContent: true, Output: []core.Item{}}
	if err := responseStore.Create(ctx, tenantID, response); err != nil {
		t.Fatal(err)
	}
	response.Status = core.ResponseStatusCompleted
	if err := responseStore.FinalizeWithUsage(ctx, tenantID, response, 1, core.UsageRecord{
		ID: "fallback-usage-" + tenantID, TenantID: tenantID, APIKeyID: key.ID, ResponseID: response.ID, AttemptID: "success-attempt", QuotaReservationID: successAttempt.ID,
		PriceSnapshot: core.PriceSnapshot{ID: "fallback-price-" + tenantID, Provider: "provider-" + tenantID, Model: "model-" + tenantID, Region: "local", Currency: "USD", EffectiveAt: now.Unix(), Source: "integration-test"},
		ProviderUsage: []byte(`{}`), Usage: core.Usage{InputTokens: 2, OutputTokens: 1, TotalTokens: 3}, AmountMicros: 4, Currency: "USD", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if reconciled, err := controller.Reconcile(ctx, 10); err != nil || reconciled != 0 {
		t.Fatalf("fallback reconciliation = %d / %v, successful attempt must already be committed", reconciled, err)
	}
	var failedStatus, successStatus string
	if err := db.QueryRowContext(ctx, `SELECT status FROM quota_reservations WHERE id = $1`, failedAttempt.ID).Scan(&failedStatus); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT status FROM quota_reservations WHERE id = $1`, successAttempt.ID).Scan(&successStatus); err != nil {
		t.Fatal(err)
	}
	if failedStatus != "reserved" || successStatus != "committed" {
		t.Fatalf("fallback reservation statuses = %q/%q", failedStatus, successStatus)
	}
}
