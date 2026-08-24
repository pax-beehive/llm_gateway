//go:build integration

package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/provider"
	gatewayruntime "github.com/toddzheng/llm-gateway/internal/runtime"
	"github.com/toddzheng/llm-gateway/internal/store"
)

func TestPostgresStoreFalseLeavesNoResponseContentOrOutboxSecret(t *testing.T) {
	db, ctx := integrationDatabase(t)
	responseStore := store.NewPostgresResponseStore(db)
	if err := responseStore.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	tenantID := fmt.Sprintf("store-false-%d", time.Now().UnixNano())
	if _, err := db.ExecContext(ctx, `INSERT INTO tenants (id, home_region) VALUES ($1, 'local')`, tenantID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanupTenant(t, db, tenantID) })

	engine := gatewayruntime.New(responseStore, provider.NewStaticRouter(provider.NewEchoExecutor()))
	const secret = "do-not-retain-this-secret"
	response, err := engine.Execute(ctx, core.Request{
		TenantID: tenantID, HomeRegion: "local", ExecutionEpoch: 1, Model: "echo-v1", Store: false,
		Input:             []core.Item{{Type: "message", Role: "user", Content: []core.Content{{Type: "input_text", Text: secret}}}},
		RequestedFeatures: []string{"text"}, Metadata: map[string]string{"secret": secret},
	})
	if err != nil || response.OutputText() != secret {
		t.Fatalf("response/error = %#v / %v", response, err)
	}
	if _, err := responseStore.Get(ctx, tenantID, response.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get error = %v, want not found", err)
	}
	var retained bool
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM responses WHERE tenant_id = $1 AND payload::text LIKE '%' || $2 || '%'
			UNION ALL
			SELECT 1 FROM transactional_outbox WHERE tenant_id = $1 AND payload::text LIKE '%' || $2 || '%'
		)`, tenantID, secret).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if retained {
		t.Fatal("store:false content was retained in a response or transactional outbox payload")
	}
}

func TestPostgresExecutionEpochFencesStaleWriter(t *testing.T) {
	db, ctx := integrationDatabase(t)
	responseStore := store.NewPostgresResponseStore(db)
	if err := responseStore.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	tenantID := fmt.Sprintf("epoch-fence-%d", time.Now().UnixNano())
	if _, err := db.ExecContext(ctx, `INSERT INTO tenants (id, home_region, execution_epoch) VALUES ($1, 'local', 1)`, tenantID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanupTenant(t, db, tenantID) })
	response := core.Response{
		ID: "resp-epoch", Object: "response", CreatedAt: time.Now().Unix(), Status: core.ResponseStatusInProgress,
		Model: "model", HomeRegion: "local", ExecutionEpoch: 1, Revision: 1, RetainContent: true, Output: []core.Item{},
	}
	if err := responseStore.Create(ctx, tenantID, response); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tenants SET execution_epoch = 2 WHERE id = $1`, tenantID); err != nil {
		t.Fatal(err)
	}
	response.Status = core.ResponseStatusFailed
	if err := responseStore.Update(ctx, tenantID, response, 1); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale update error = %v, want revision conflict", err)
	}
	newEpoch := response
	newEpoch.ID = "resp-epoch-2"
	newEpoch.ExecutionEpoch = 2
	if err := responseStore.Create(ctx, tenantID, newEpoch); err != nil {
		t.Fatalf("promoted writer create: %v", err)
	}
}

func TestPostgresConcurrentResponseQuotaIsGlobalAndLeased(t *testing.T) {
	db, ctx := integrationDatabase(t)
	responseStore := store.NewPostgresResponseStore(db)
	if err := responseStore.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	tenantID := fmt.Sprintf("global-quota-%d", time.Now().UnixNano())
	if _, err := db.ExecContext(ctx, `INSERT INTO tenants (id, home_region) VALUES ($1, 'local')`, tenantID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanupTenant(t, db, tenantID) })
	expiresAt := time.Now().Add(time.Minute)
	if err := responseStore.AcquireResponseSlot(ctx, tenantID, "lease-1", 1, expiresAt); err != nil {
		t.Fatal(err)
	}
	if err := responseStore.AcquireResponseSlot(ctx, tenantID, "lease-2", 1, expiresAt); !errors.Is(err, store.ErrQuotaExceeded) {
		t.Fatalf("second slot error = %v, want quota exceeded", err)
	}
	if err := responseStore.ReleaseResponseSlot(ctx, tenantID, "lease-1"); err != nil {
		t.Fatal(err)
	}
	if err := responseStore.AcquireResponseSlot(ctx, tenantID, "lease-2", 1, expiresAt); err != nil {
		t.Fatalf("slot after release: %v", err)
	}
}

func TestPostgresPersistsExperimentallyValidatedSavingFromStableCohorts(t *testing.T) {
	db, ctx := integrationDatabase(t)
	responseStore := store.NewPostgresResponseStore(db)
	if err := responseStore.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	tenantID := fmt.Sprintf("experiment-%d", time.Now().UnixNano())
	if _, err := db.ExecContext(ctx, `INSERT INTO tenants (id, home_region) VALUES ($1, 'local')`, tenantID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanupTenant(t, db, tenantID) })
	now := time.Now().UTC().Truncate(time.Second)
	price := core.PriceSnapshot{
		ID: "price-" + tenantID, Provider: "provider", Model: "model", Region: "local", Currency: "USD",
		InputPerMillionMicros: 1_000_000, EffectiveAt: now.Unix(), Source: "experiment-test",
	}
	for index, sample := range []struct {
		cohort string
		tokens int64
	}{{"holdout", 1_000_000}, {"treatment", 500_000}, {"treatment", 750_000}} {
		response := core.Response{
			ID: fmt.Sprintf("resp-%d", index), Object: "response", CreatedAt: now.Unix(), Status: core.ResponseStatusInProgress,
			Model: "model", HomeRegion: "local", ExecutionEpoch: 1, Revision: 1, RetainContent: true, Output: []core.Item{},
		}
		if err := responseStore.Create(ctx, tenantID, response); err != nil {
			t.Fatal(err)
		}
		response.Status = core.ResponseStatusCompleted
		usage := core.UsageRecord{
			ID: fmt.Sprintf("usage-%d", index), TenantID: tenantID, ResponseID: response.ID,
			AttemptID: fmt.Sprintf("attempt-%d", index), PriceSnapshot: price,
			ProviderUsage: []byte(`{}`), Usage: core.Usage{InputTokens: sample.tokens},
			AmountMicros: sample.tokens, Currency: "USD", HoldoutCohort: sample.cohort,
			ExperimentRevision: "experiment-v1", CreatedAt: now,
		}
		if err := responseStore.FinalizeWithUsage(ctx, tenantID, response, 1, usage); err != nil {
			t.Fatal(err)
		}
	}
	var rows int64
	var net int64
	if err := db.QueryRowContext(ctx, `
		SELECT count(*), COALESCE(sum(net_saving), 0)::bigint FROM savings_ledger
		WHERE tenant_id = $1 AND measure = 'experimentally_validated_saving'`, tenantID).Scan(&rows, &net); err != nil {
		t.Fatal(err)
	}
	if rows != 2 || net != 750_000 {
		t.Fatalf("experiment rows/net saving = %d/%d, want 2/750000", rows, net)
	}
}

func TestPostgresRetentionExpiryScrubsResponseAndOutboxContent(t *testing.T) {
	db, ctx := integrationDatabase(t)
	responseStore := store.NewPostgresResponseStore(db)
	if err := responseStore.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	tenantID := fmt.Sprintf("retention-%d", time.Now().UnixNano())
	if _, err := db.ExecContext(ctx, `INSERT INTO tenants (id, home_region) VALUES ($1, 'local')`, tenantID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanupTenant(t, db, tenantID) })
	const secret = "expired-content-secret"
	expired := time.Now().Add(-time.Second).Unix()
	response := core.Response{
		ID: "resp-expired", Object: "response", CreatedAt: time.Now().Unix(), Status: core.ResponseStatusCompleted,
		Model: "model", HomeRegion: "local", ExecutionEpoch: 1, Revision: 1, RetainContent: true,
		Input:            []core.Item{{Type: "message", Content: []core.Content{{Type: "input_text", Text: secret}}}},
		Output:           []core.Item{{Type: "message", Content: []core.Content{{Type: "output_text", Text: secret}}}},
		ContentExpiresAt: &expired,
	}
	if err := responseStore.Create(ctx, tenantID, response); err != nil {
		t.Fatal(err)
	}
	if scrubbed, err := responseStore.ScrubExpiredContent(ctx, "local", 10); err != nil || scrubbed != 1 {
		t.Fatalf("retention scrub count/error = %d / %v", scrubbed, err)
	}
	got, err := responseStore.Get(ctx, tenantID, response.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Input) != 0 || len(got.Output) != 0 || got.Revision != 2 {
		t.Fatalf("expired Response retained content: %#v", got)
	}
	var retained bool
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM responses WHERE tenant_id = $1 AND payload::text LIKE '%' || $2 || '%'
			UNION ALL
			SELECT 1 FROM transactional_outbox WHERE tenant_id = $1 AND payload::text LIKE '%' || $2 || '%'
		)`, tenantID, secret).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if retained {
		t.Fatal("expired content remained in PostgreSQL payloads")
	}
}

func TestPostgresConversationAndResponseCommitTogether(t *testing.T) {
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
	responseStore := store.NewPostgresResponseStore(db)
	if err := responseStore.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	tenantID := fmt.Sprintf("integration-%d", time.Now().UnixNano())
	if _, err := db.ExecContext(ctx, `INSERT INTO tenants (id, home_region) VALUES ($1, 'local')`, tenantID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanupTenant(t, db, tenantID) })

	engine := gatewayruntime.New(responseStore, provider.NewStaticRouter(provider.NewEchoExecutor()))
	conversation, err := engine.CreateConversation(ctx, tenantID, "local", 1, []core.Item{{
		Type: "message", Role: "system", Content: []core.Content{{Type: "input_text", Text: "system:"}},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := engine.Execute(ctx, core.Request{
		TenantID: tenantID, HomeRegion: "local", Model: "echo-v1", Store: true,
		ConversationID:    conversation.ID,
		Input:             []core.Item{{Type: "message", Role: "user", Content: []core.Content{{Type: "input_text", Text: "user"}}}},
		RequestedFeatures: []string{"text"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.OutputText() != "system:user" {
		t.Fatalf("output = %q", response.OutputText())
	}
	persisted, err := engine.GetConversation(ctx, tenantID, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Revision != 3 || persisted.ActiveResponseID != "" || len(persisted.Items) != 3 {
		t.Fatalf("persisted conversation = %#v", persisted)
	}
	var responseEvents, conversationEvents, usageEvents int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FILTER (WHERE aggregate_type = 'response'),
		       count(*) FILTER (WHERE aggregate_type = 'conversation'),
		       count(*) FILTER (WHERE aggregate_type = 'usage')
		FROM transactional_outbox WHERE tenant_id = $1`, tenantID).Scan(&responseEvents, &conversationEvents, &usageEvents); err != nil {
		t.Fatal(err)
	}
	if responseEvents != 2 || conversationEvents != 1 || usageEvents != 1 {
		t.Fatalf("outbox response/conversation/usage events = %d/%d/%d, want 2/1/1", responseEvents, conversationEvents, usageEvents)
	}
	var usageRows, savingsRows int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM usage_ledger WHERE tenant_id = $1`, tenantID).Scan(&usageRows); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM savings_ledger WHERE tenant_id = $1`, tenantID).Scan(&savingsRows); err != nil {
		t.Fatal(err)
	}
	if usageRows != 1 || savingsRows != 1 {
		t.Fatalf("usage/savings rows = %d/%d, want 1/1", usageRows, savingsRows)
	}
}

func TestPostgresResponseFinalizationRecordsVerifiedProtectedHitNetSaving(t *testing.T) {
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
	responseStore := store.NewPostgresResponseStore(db)
	if err := responseStore.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	tenantID := fmt.Sprintf("protected-hit-%d", time.Now().UnixNano())
	if _, err := db.ExecContext(ctx, `INSERT INTO tenants (id, home_region) VALUES ($1, 'local')`, tenantID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanupTenant(t, db, tenantID) })

	now := time.Now().UTC()
	response := core.Response{
		ID: "resp-protected", Object: "response", CreatedAt: now.Unix(), Status: core.ResponseStatusInProgress,
		Model: "model", HomeRegion: "local", ExecutionEpoch: 1, Revision: 1, RetainContent: true, Output: []core.Item{},
	}
	if err := responseStore.Create(ctx, tenantID, response); err != nil {
		t.Fatal(err)
	}
	response.Status = core.ResponseStatusCompleted
	response.CompletedAt = func() *int64 { value := now.Unix(); return &value }()
	usage := core.UsageRecord{
		ID: "usage-protected", TenantID: tenantID, ResponseID: response.ID, AttemptID: "attempt-protected",
		PriceSnapshot: core.PriceSnapshot{
			ID: "price-" + tenantID, Provider: "anthropic", Model: "model", Region: "local", Currency: "USD",
			InputPerMillionMicros: 10_000_000, CachedInputPerMillionMicros: 1_000_000,
			OutputPerMillionMicros: 20_000_000, EffectiveAt: now.Unix(), Source: "integration-test",
		},
		ProviderUsage: []byte(`{"cache_read_input_tokens":800000}`),
		Usage:         core.Usage{InputTokens: 1_000_000, CachedInputTokens: 800_000, OutputTokens: 10},
		AmountMicros:  2_800_200, Currency: "USD", CacheUsageReliable: true, CreatedAt: now,
		ProtectedHit: &core.ProtectedHitEvidence{
			CacheLeaseID: "lease-protected", OriginalLeaseExpiresAt: now.Add(-time.Minute),
			RefreshSucceededAt: now.Add(-2 * time.Minute), RefreshExpiresAt: now.Add(3 * time.Minute),
			CustomerRequestAt: now, RefreshCostMicros: 100_000, ForecastCostMicros: 10_000,
			RefreshUsageID: "refresh-usage-protected", RefreshProviderUsage: []byte(`{"cache_creation_input_tokens":1000}`),
			StorageCostMicros: 5_000, RouteLockCostMicros: 15_000,
		},
	}
	if err := responseStore.FinalizeWithUsage(ctx, tenantID, response, 1, usage); err != nil {
		t.Fatal(err)
	}
	var gross, net int64
	var attribution string
	if err := db.QueryRowContext(ctx, `
		SELECT gross_saving::bigint, net_saving::bigint, attribution
		FROM savings_ledger
		WHERE tenant_id = $1 AND response_id = $2 AND measure = 'estimated_protected_saving'`,
		tenantID, response.ID,
	).Scan(&gross, &net, &attribution); err != nil {
		t.Fatal(err)
	}
	if gross != 7_200_000 || net != 7_070_000 || attribution != "estimated" {
		t.Fatalf("protected saving gross/net/attribution = %d/%d/%s", gross, net, attribution)
	}
}

func cleanupTenant(t *testing.T, db *sql.DB, tenantID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, statement := range []string{
		`DELETE FROM tenant_response_slots WHERE tenant_id = $1`,
		`DELETE FROM transactional_outbox WHERE tenant_id = $1`,
		`DELETE FROM savings_ledger WHERE tenant_id = $1`,
		`DELETE FROM usage_ledger WHERE tenant_id = $1`,
		`DELETE FROM idempotency_keys WHERE tenant_id = $1`,
		`DELETE FROM conversation_items WHERE tenant_id = $1`,
		`DELETE FROM responses WHERE tenant_id = $1`,
		`DELETE FROM conversations WHERE tenant_id = $1`,
		`DELETE FROM tenants WHERE id = $1`,
	} {
		if _, err := db.ExecContext(ctx, statement, tenantID); err != nil {
			t.Errorf("cleanup tenant: %v", err)
		}
	}
}

func integrationDatabase(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
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
	return db, ctx
}
