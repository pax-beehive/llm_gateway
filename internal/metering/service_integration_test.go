//go:build integration

package metering_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/metering"
	"github.com/toddzheng/llm-gateway/internal/migrations"
	"github.com/toddzheng/llm-gateway/internal/quota"
	"github.com/toddzheng/llm-gateway/internal/store"
)

func TestRelayDeduplicatesCorrectsRebuildsAndIsolatesTenants(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("TEST_DATABASE_URL is required")
	}
	database, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	if err := migrations.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	if err := metering.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("metering-%d", time.Now().UnixNano())
	tenantA := prefix + "-a"
	tenantB := prefix + "-b"
	for _, tenant := range []string{tenantA, tenantB} {
		if _, err := database.ExecContext(ctx, `INSERT INTO tenants(id,home_region,policy_revision,policy) VALUES($1,'test',1,'{"revision":1}')`, tenant); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, tenant := range []string{tenantA, tenantB} {
			_, _ = database.ExecContext(context.Background(), `DELETE FROM metering_exports WHERE tenant_id=$1`, tenant)
			_, _ = database.ExecContext(context.Background(), `DELETE FROM metering_quota_denials WHERE tenant_id=$1`, tenant)
			_, _ = database.ExecContext(context.Background(), `DELETE FROM metering_usage_daily WHERE tenant_id=$1`, tenant)
			_, _ = database.ExecContext(context.Background(), `DELETE FROM metering_usage_facts WHERE tenant_id=$1`, tenant)
			_, _ = database.ExecContext(context.Background(), `DELETE FROM metering_inbox WHERE tenant_id=$1`, tenant)
			_, _ = database.ExecContext(context.Background(), `DELETE FROM transactional_outbox WHERE tenant_id=$1`, tenant)
			_, _ = database.ExecContext(context.Background(), `DELETE FROM tenants WHERE id=$1`, tenant)
		}
	})
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	event := metering.UsageEvent{SchemaVersion: 1, Type: metering.EventUsageRecorded, UsageID: prefix + "-usage", TenantID: tenantA, APIKeyID: "key-a", ResponseID: "response-a", AttemptID: "attempt-a", RouteID: prefix + "-route", Provider: "openai", PublicModel: "gateway-model", ProviderModel: "gpt-test", Region: "test", PriceSnapshotID: "price-a", InputTokens: 10, OutputTokens: 5, AmountMicros: 25, Currency: "USD", Outcome: "committed", OccurredAt: now}
	payload, _ := json.Marshal(event)
	var outboxID int64
	if err := database.QueryRowContext(ctx, `INSERT INTO transactional_outbox(tenant_id,aggregate_type,aggregate_id,aggregate_revision,event_type,payload,created_at) VALUES($1,'usage',$2,1,'usage.recorded',$3,$4) RETURNING id`, tenantA, event.UsageID, payload, now).Scan(&outboxID); err != nil {
		t.Fatal(err)
	}
	service, _ := metering.NewService(database, func() time.Time { return now.Add(time.Hour) })
	processed, err := service.ConsumeOutboxBatch(ctx, prefix+"-worker", 1000, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if processed < 1 {
		t.Fatalf("processed=%d", processed)
	}
	// Simulate ambiguous transport acknowledgement. The same stable event ID must
	// not apply a second projection effect.
	if _, err := database.ExecContext(ctx, `UPDATE transactional_outbox SET published_at=NULL WHERE id=$1`, outboxID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ConsumeOutboxBatch(ctx, prefix+"-worker", 1000, time.Minute); err != nil {
		t.Fatal(err)
	}
	summary, err := service.Summary(ctx, metering.Filter{TenantID: tenantA})
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Totals) != 1 || summary.Totals[0].OperationCount != 1 || summary.Totals[0].AmountMicros != 25 {
		t.Fatalf("deduplicated summary=%#v", summary)
	}
	other, err := service.Summary(ctx, metering.Filter{TenantID: tenantB})
	if err != nil {
		t.Fatal(err)
	}
	if len(other.Totals) != 0 {
		t.Fatalf("cross-Tenant usage=%#v", other)
	}
	tenantBEvent := event
	tenantBEvent.UsageID = prefix + "-usage-tenant-b"
	tenantBEvent.TenantID = tenantB
	tenantBEvent.APIKeyID = "key-b"
	tenantBEvent.ResponseID = "response-tenant-b"
	tenantBEvent.AttemptID = "attempt-tenant-b"
	tenantBPayload, _ := json.Marshal(tenantBEvent)
	if _, err := database.ExecContext(ctx, `INSERT INTO transactional_outbox(tenant_id,aggregate_type,aggregate_id,aggregate_revision,event_type,payload,created_at) VALUES($1,'usage',$2,1,'usage.recorded',$3,$4)`, tenantB, tenantBEvent.UsageID, tenantBPayload, now); err != nil {
		t.Fatal(err)
	}
	if processed, err := service.ConsumeOutboxBatch(ctx, prefix+"-worker", 1000, time.Minute); err != nil || processed < 1 {
		t.Fatalf("consume Tenant B usage=%d/%v", processed, err)
	}
	platformSummary, err := service.Summary(ctx, metering.Filter{AllTenants: true, RouteID: event.RouteID})
	if err != nil || len(platformSummary.Totals) != 1 || platformSummary.Totals[0].OperationCount != 2 || platformSummary.Totals[0].AmountMicros != 50 {
		t.Fatalf("platform summary=%#v/%v", platformSummary, err)
	}
	platformSeries, err := service.TimeSeries(ctx, metering.Filter{AllTenants: true, RouteID: event.RouteID, From: now.Add(-time.Hour), Through: now.Add(time.Hour)}, "hour")
	if err != nil || len(platformSeries) != 1 || platformSeries[0].Totals.OperationCount != 2 {
		t.Fatalf("platform timeseries=%#v/%v", platformSeries, err)
	}
	denialRoute := prefix + "-denied-route"
	quotaController := quota.NewPostgresController(database, func() time.Time { return now.Add(2 * time.Hour) })
	if err := quotaController.RecordDenial(ctx, quota.DenialEvent{
		TenantID: tenantA, APIKeyID: "key-a", ResponseID: "response-denied", AttemptID: "attempt-denied",
		PublicModel: "gateway-model", RouteID: denialRoute, Region: "test", Scope: "api_key", Dimension: "requests",
		TenantPolicyRevision: 1, APIKeyPolicyRevision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if processed, err := service.ConsumeOutboxBatch(ctx, prefix+"-worker", 1000, time.Minute); err != nil || processed < 1 {
		t.Fatalf("consume quota denial=%d/%v", processed, err)
	}
	denials, err := service.QuotaDenials(ctx, metering.QuotaDenialFilter{Filter: metering.Filter{AllTenants: true, RouteID: denialRoute}, Scope: "api_key", Limit: 10})
	if err != nil || len(denials.Data) != 1 || denials.Data[0].Dimension != "requests" || denials.Data[0].TenantID != tenantA {
		t.Fatalf("quota denial query=%#v/%v", denials, err)
	}
	events, err := service.Events(ctx, metering.Filter{TenantID: tenantA}, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events.Data) != 1 {
		t.Fatalf("events=%#v", events)
	}
	correction, err := service.Correct(ctx, "operator", "Provider reconciliation", prefix+"-correction", events.Data[0].EventID, metering.UsageEvent{AmountMicros: -5})
	if err != nil {
		t.Fatal(err)
	}
	if correction.CorrectsEventID == "" || correction.CorrectionActorID != "operator" {
		t.Fatal("correction lost source or actor identity")
	}
	corrected, err := service.Summary(ctx, metering.Filter{TenantID: tenantA})
	if err != nil {
		t.Fatal(err)
	}
	if corrected.Totals[0].AmountMicros != 20 {
		t.Fatalf("corrected summary=%#v", corrected)
	}
	var before int64
	if err := database.QueryRowContext(ctx, `SELECT amount_micros FROM metering_usage_daily d JOIN metering_projection_generations g ON g.id=d.generation_id WHERE g.status='active' AND tenant_id=$1`, tenantA).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	var after int64
	if err := database.QueryRowContext(ctx, `SELECT amount_micros FROM metering_usage_daily d JOIN metering_projection_generations g ON g.id=d.generation_id WHERE g.status='active' AND tenant_id=$1`, tenantA).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after || after != 20 {
		t.Fatalf("projection before/after=%d/%d", before, after)
	}
	second := event
	second.UsageID = prefix + "-usage-2"
	second.ResponseID = "response-b"
	second.AttemptID = "attempt-b"
	second.Currency = "EUR"
	second.AmountMicros = 7
	secondPayload, _ := json.Marshal(second)
	if _, err := database.ExecContext(ctx, `INSERT INTO transactional_outbox(tenant_id,aggregate_type,aggregate_id,aggregate_revision,event_type,payload,created_at) VALUES($1,'usage',$2,1,'usage.recorded',$3,$4)`, tenantA, second.UsageID, secondPayload, now); err != nil {
		t.Fatal(err)
	}
	if processed, err := service.ConsumeOutboxBatch(ctx, prefix+"-worker", 1000, time.Minute); err != nil || processed < 1 {
		t.Fatalf("consume second currency=%d/%v", processed, err)
	}
	mixed, err := service.Summary(ctx, metering.Filter{TenantID: tenantA})
	if err != nil || len(mixed.Totals) != 2 || mixed.Totals[0].Currency == mixed.Totals[1].Currency {
		t.Fatalf("explicit currency totals=%#v/%v", mixed, err)
	}
	firstPage, err := service.Events(ctx, metering.Filter{TenantID: tenantA}, "", 1)
	if err != nil || len(firstPage.Data) != 1 || firstPage.NextCursor == "" {
		t.Fatalf("first cursor page=%#v/%v", firstPage, err)
	}
	secondPage, err := service.Events(ctx, metering.Filter{TenantID: tenantA}, firstPage.NextCursor, 1)
	if err != nil || len(secondPage.Data) != 1 || secondPage.Data[0].EventID == firstPage.Data[0].EventID {
		t.Fatalf("second cursor page=%#v/%v", secondPage, err)
	}
	if _, err := service.TimeSeries(ctx, metering.Filter{TenantID: tenantA, From: now, Through: now.Add(32 * 24 * time.Hour)}, "hour"); err == nil {
		t.Fatal("unbounded hourly time series unexpectedly succeeded")
	}
	handler, err := metering.NewHandler(service, metering.IdentityVerifierFunc(func(context.Context, string) (metering.Identity, error) {
		return metering.Identity{ActorID: "operator", Scopes: []string{metering.ScopePlatformRead}}, nil
	}), nil, nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/metering/v1/responses/response-a/usage?tenant_id="+tenantA, nil)
	responseRecorder := httptest.NewRecorder()
	handler.ServeHTTP(responseRecorder, request)
	if responseRecorder.Code != http.StatusOK {
		t.Fatalf("Response usage status/body=%d/%s", responseRecorder.Code, responseRecorder.Body.String())
	}
	var responseUsage metering.EventPage
	if err := json.Unmarshal(responseRecorder.Body.Bytes(), &responseUsage); err != nil || len(responseUsage.Data) != 2 || responseUsage.Data[0].ResponseID != "response-a" || responseUsage.Data[1].ResponseID != "response-a" {
		t.Fatalf("Response usage=%#v/%v", responseUsage, err)
	}
	exportCutoff := now.Add(2 * time.Hour)
	// Keep the test independent from the database server's wall clock. Events
	// already consumed before the request belong to the immutable export; the
	// late event below is explicitly moved beyond the ingestion cutoff.
	if _, err := database.ExecContext(ctx, `UPDATE metering_inbox SET consumed_at=$2 WHERE tenant_id=$1`, tenantA, exportCutoff.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	exportService, _ := metering.NewService(database, func() time.Time { return exportCutoff })
	exportJob, err := exportService.RequestExport(ctx, metering.Filter{TenantID: tenantA})
	if err != nil {
		t.Fatal(err)
	}
	late := event
	late.UsageID = prefix + "-late-usage"
	late.ResponseID = "response-late"
	late.AttemptID = "attempt-late"
	late.OccurredAt = now.Add(-time.Hour)
	latePayload, _ := json.Marshal(late)
	if _, err := database.ExecContext(ctx, `INSERT INTO transactional_outbox(tenant_id,aggregate_type,aggregate_id,aggregate_revision,event_type,payload,created_at) VALUES($1,'usage',$2,1,'usage.recorded',$3,$4)`, tenantA, late.UsageID, latePayload, late.OccurredAt); err != nil {
		t.Fatal(err)
	}
	if processed, err := exportService.ConsumeOutboxBatch(ctx, prefix+"-worker", 1000, time.Minute); err != nil || processed < 1 {
		t.Fatalf("consume late usage=%d/%v", processed, err)
	}
	if _, err := database.ExecContext(ctx, `UPDATE metering_inbox i SET consumed_at=$3 FROM metering_usage_facts f WHERE f.event_id=i.event_id AND f.tenant_id=$1 AND f.usage_id=$2`, tenantA, late.UsageID, exportCutoff.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	exportStore, err := metering.NewFileExportStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := exportService.RunNextExport(ctx, exportStore); err != nil || !worked {
		t.Fatalf("run export=%v/%v", worked, err)
	}
	completedExport, err := exportService.GetExport(ctx, tenantA, exportJob.ID)
	if err != nil || completedExport.Status != "succeeded" {
		t.Fatalf("completed export=%#v/%v", completedExport, err)
	}
	exported, err := exportStore.Get(ctx, completedExport.ObjectKey)
	if err != nil || !strings.Contains(string(exported), event.UsageID) || strings.Contains(string(exported), late.UsageID) {
		t.Fatalf("immutable export cutoff payload=%s/error=%v", exported, err)
	}
}

func TestControlledLedgerBackfillIsIdempotent(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("TEST_DATABASE_URL is required")
	}
	database, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	if err := migrations.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	if err := metering.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	tenantID := fmt.Sprintf("metering-backfill-%d", time.Now().UnixNano())
	if _, err := database.ExecContext(ctx, `INSERT INTO tenants(id,home_region,policy_revision,policy) VALUES($1,'local',1,'{"revision":1}')`, tenantID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = database.ExecContext(context.Background(), `DELETE FROM metering_usage_daily WHERE tenant_id=$1`, tenantID)
		_, _ = database.ExecContext(context.Background(), `DELETE FROM metering_usage_facts WHERE tenant_id=$1`, tenantID)
		_, _ = database.ExecContext(context.Background(), `DELETE FROM metering_inbox WHERE tenant_id=$1`, tenantID)
		_, _ = database.ExecContext(context.Background(), `DELETE FROM transactional_outbox WHERE tenant_id=$1`, tenantID)
		_, _ = database.ExecContext(context.Background(), `DELETE FROM savings_ledger WHERE tenant_id=$1`, tenantID)
		_, _ = database.ExecContext(context.Background(), `DELETE FROM usage_ledger WHERE tenant_id=$1`, tenantID)
		_, _ = database.ExecContext(context.Background(), `DELETE FROM responses WHERE tenant_id=$1`, tenantID)
		_, _ = database.ExecContext(context.Background(), `DELETE FROM tenants WHERE id=$1`, tenantID)
	})
	now := time.Date(2026, 8, 30, 13, 0, 0, 0, time.UTC)
	response := core.Response{ID: tenantID + "-response", Object: "response", Model: "gateway-model", Status: core.ResponseStatusInProgress, HomeRegion: "local", ExecutionEpoch: 1, Revision: 1, RetainContent: true, CreatedAt: now.Unix()}
	responseStore := store.NewPostgresResponseStore(database)
	if err := responseStore.Create(ctx, tenantID, response); err != nil {
		t.Fatal(err)
	}
	response.Status = core.ResponseStatusCompleted
	usage := core.UsageRecord{ID: tenantID + "-usage", TenantID: tenantID, ResponseID: response.ID, AttemptID: "attempt-a", RouteID: "route-a", PriceSnapshot: core.PriceSnapshot{ID: tenantID + "-price", Provider: "openai-" + tenantID, Model: "gpt-" + tenantID, Region: "local", Currency: "USD", InputPerMillionMicros: 1, OutputPerMillionMicros: 1, EffectiveAt: now.Unix(), Source: "test"}, Usage: core.Usage{InputTokens: 3, OutputTokens: 2}, AmountMicros: 9, Currency: "USD", CreatedAt: now}
	if err := responseStore.FinalizeWithUsage(ctx, tenantID, response, 1, usage); err != nil {
		t.Fatal(err)
	}
	service, _ := metering.NewService(database, func() time.Time { return now })
	count, err := service.BackfillTenantLedger(ctx, tenantID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("backfilled=%d", count)
	}
	processed, err := service.ConsumeOutboxBatch(ctx, tenantID+"-worker", 100, time.Minute)
	if err != nil || processed < 1 {
		t.Fatalf("relay after ledger bootstrap=%d/%v", processed, err)
	}
	count, err = service.BackfillTenantLedger(ctx, tenantID, 100)
	if err != nil || count != 0 {
		t.Fatalf("replay backfill=%d/%v", count, err)
	}
	summary, err := service.Summary(ctx, metering.Filter{TenantID: tenantID})
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Totals) != 1 || summary.Totals[0].AmountMicros != 9 {
		t.Fatalf("summary=%#v", summary)
	}
}
