//go:build integration

package credentialadmin_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/credentialadmin"
	"github.com/toddzheng/llm-gateway/internal/store"
	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

func TestIssueGatewayAPIKeyReturnsSecretOnceAndPersistsOnlyVersionedDigest(t *testing.T) {
	db, ctx := credentialIntegrationDatabase(t)
	if err := store.NewPostgresResponseStore(db).Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tenantadmin.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := credentialadmin.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	tenantService, err := tenantadmin.NewService(db, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	tenantID := fmt.Sprintf("credential-issue-%d", time.Now().UnixNano())
	actor := tenantadmin.ActorEnvelope{
		Type: "human", ID: "credential-operator", Scopes: []string{tenantadmin.ScopePlatformWrite},
		RequestID: "request-credential-issue", Reason: "integration test",
	}
	if _, err := tenantService.CreateTenant(ctx, actor, "create-"+tenantID, tenantadmin.CreateTenantCommand{
		ID: tenantID, Slug: tenantID, DisplayName: "Credential Tenant", HomeRegion: "us-test",
		InitialPolicy: core.TenantPolicy{Revision: 1},
	}); err != nil {
		t.Fatal(err)
	}
	seed := sha256.Sum256([]byte(tenantID))
	secretMaterial := bytes.Repeat(seed[:], 4)
	service, err := credentialadmin.NewService(db, credentialadmin.PepperRing{
		CurrentVersion: 2,
		Peppers:        map[int16][]byte{1: []byte("old-integration-pepper"), 2: []byte("current-integration-pepper")},
	}, time.Now, bytes.NewReader(secretMaterial))
	if err != nil {
		t.Fatal(err)
	}
	command := credentialadmin.IssueCommand{
		TenantID: tenantID, Name: "production workload", Metadata: map[string]any{"environment": "test"},
		Policy: core.APIKeyPolicy{Revision: 1},
	}
	issued, err := service.Issue(ctx, actor, "issue-"+tenantID, command)
	if err != nil {
		t.Fatal(err)
	}
	if issued.Replay || issued.RawSecret == "" || !strings.HasPrefix(issued.RawSecret, "gw_") || issued.Credential.DigestVersion != 2 {
		t.Fatalf("issued credential = %#v", issued)
	}
	replayed, err := service.Issue(ctx, actor, "issue-"+tenantID, command)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replay || replayed.RawSecret != "" || replayed.Credential.ID != issued.Credential.ID {
		t.Fatalf("replayed credential = %#v", replayed)
	}
	changedReason := actor
	changedReason.Reason = "different reason"
	if _, err := service.Issue(ctx, changedReason, "issue-"+tenantID, command); !errors.Is(err, credentialadmin.ErrIdempotencyConflict) {
		t.Fatalf("changed reason error = %v", err)
	}
	var digest []byte
	var digestVersion int16
	if err := db.QueryRowContext(ctx, `SELECT secret_digest, digest_version FROM api_keys WHERE id = $1`, issued.Credential.ID).Scan(&digest, &digestVersion); err != nil {
		t.Fatal(err)
	}
	if digestVersion != 2 || len(digest) != 32 || bytes.Contains(digest, []byte(issued.RawSecret)) {
		t.Fatalf("persisted digest version/length = %d/%d", digestVersion, len(digest))
	}
	var auditCount, outboxCount int
	var outboxPayload []byte
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM control_audit_events WHERE tenant_id = $1 AND action = 'gateway_api_key.issue'`, tenantID).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM control_outbox WHERE tenant_id = $1 AND event_type = 'GatewayAPIKeyIssued'`, tenantID).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT payload FROM control_outbox WHERE tenant_id = $1 AND event_type = 'GatewayAPIKeyIssued'`, tenantID).Scan(&outboxPayload); err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 || outboxCount != 1 || bytes.Contains(outboxPayload, []byte(issued.RawSecret)) {
		t.Fatalf("audit/outbox = %d/%d payload=%s", auditCount, outboxCount, outboxPayload)
	}
	var event map[string]any
	if err := json.Unmarshal(outboxPayload, &event); err != nil {
		t.Fatal(err)
	}
	if event["digest_version"] != float64(2) || event["secret_digest"] == "" {
		t.Fatalf("projection event = %#v", event)
	}
}

func credentialIntegrationDatabase(t *testing.T) (*sql.DB, context.Context) {
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
