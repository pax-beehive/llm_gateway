//go:build integration

package store_test

import (
	"context"
	"database/sql"
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
	conversation, err := engine.CreateConversation(ctx, tenantID, "local", []core.Item{{
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

func cleanupTenant(t *testing.T, db *sql.DB, tenantID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, statement := range []string{
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
