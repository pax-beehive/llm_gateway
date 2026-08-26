//go:build integration

package tenantadmin_test

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
	"github.com/toddzheng/llm-gateway/internal/store"
	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

func TestCreateTenantIsIdempotentAndCommitsAuditAndOutboxAtomically(t *testing.T) {
	db, ctx := adminIntegrationDatabase(t)
	if err := store.NewPostgresResponseStore(db).Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tenantadmin.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	service, err := tenantadmin.NewService(db, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	tenantID := fmt.Sprintf("admin-create-%d", time.Now().UnixNano())
	idempotencyKey := "create-" + tenantID
	actor := tenantadmin.ActorEnvelope{
		Type: "human", ID: "operator-1", Scopes: []string{tenantadmin.ScopePlatformWrite},
		RequestID: "request-create-1", Reason: "integration test",
	}
	command := tenantadmin.CreateTenantCommand{
		ID: tenantID, Slug: tenantID, DisplayName: "Created Tenant", HomeRegion: "local",
		Metadata: map[string]any{"environment": "test"}, InitialPolicy: core.TenantPolicy{Revision: 1},
	}
	created, err := service.CreateTenant(ctx, actor, idempotencyKey, command)
	if err != nil {
		t.Fatal(err)
	}
	if created.Replay || created.Tenant.ID != tenantID || created.Tenant.Status != access.TenantActive ||
		created.Tenant.Revision != 1 || created.Tenant.Policy.Revision != 1 {
		t.Fatalf("created Tenant = %#v", created)
	}
	replayed, err := service.CreateTenant(ctx, actor, idempotencyKey, command)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replay || replayed.Tenant.ID != created.Tenant.ID || replayed.Tenant.Revision != created.Tenant.Revision {
		t.Fatalf("replayed Tenant = %#v", replayed)
	}
	changed := command
	changed.DisplayName = "Different request"
	if _, err := service.CreateTenant(ctx, actor, idempotencyKey, changed); !errors.Is(err, tenantadmin.ErrIdempotencyConflict) {
		t.Fatalf("idempotency mismatch error = %v", err)
	}
	var auditCount, outboxCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM control_audit_events WHERE tenant_id = $1`, tenantID).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM control_outbox WHERE tenant_id = $1`, tenantID).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 || outboxCount != 1 {
		t.Fatalf("audit/outbox count = %d/%d, want 1/1", auditCount, outboxCount)
	}
}

func TestTenantQueriesUseStablePaginationAndEnforceActingTenantScope(t *testing.T) {
	db, ctx := adminIntegrationDatabase(t)
	if err := store.NewPostgresResponseStore(db).Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tenantadmin.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	service, err := tenantadmin.NewService(db, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("admin-list-%d", time.Now().UnixNano())
	homeRegion := "us-test-" + prefix
	writer := tenantadmin.ActorEnvelope{
		Type: "human", ID: "operator-list", Scopes: []string{tenantadmin.ScopePlatformWrite},
		RequestID: "request-list-create", Reason: "integration test setup",
	}
	for _, suffix := range []string{"a", "b", "c"} {
		command := tenantadmin.CreateTenantCommand{
			ID: prefix + "-" + suffix, Slug: prefix + "-" + suffix, DisplayName: "List " + suffix,
			HomeRegion: homeRegion, InitialPolicy: core.TenantPolicy{Revision: 1},
		}
		if _, err := service.CreateTenant(ctx, writer, "create-"+prefix+"-"+suffix, command); err != nil {
			t.Fatal(err)
		}
	}
	reader := tenantadmin.ActorEnvelope{Type: "human", ID: "reader", Scopes: []string{tenantadmin.ScopePlatformRead}}
	first, err := service.ListTenants(ctx, reader, tenantadmin.TenantFilter{HomeRegion: homeRegion, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Data) != 2 || first.NextCursor == "" || first.Data[0].ID >= first.Data[1].ID {
		t.Fatalf("first page = %#v", first)
	}
	second, err := service.ListTenants(ctx, reader, tenantadmin.TenantFilter{HomeRegion: homeRegion, Limit: 2, Cursor: first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Data) != 1 || second.NextCursor != "" || second.Data[0].ID != prefix+"-c" {
		t.Fatalf("second page = %#v", second)
	}
	tenantReader := tenantadmin.ActorEnvelope{
		Type: "human", ID: "tenant-reader", ActingTenantID: prefix + "-a", Scopes: []string{tenantadmin.ScopeTenantRead},
	}
	if _, err := service.GetTenant(ctx, tenantReader, prefix+"-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetTenant(ctx, tenantReader, prefix+"-b"); !errors.Is(err, tenantadmin.ErrPolicyDenied) {
		t.Fatalf("cross-Tenant read error = %v", err)
	}
}

func TestTenantProfileAndLifecycleMutationsRequireCurrentRevision(t *testing.T) {
	db, ctx := adminIntegrationDatabase(t)
	if err := store.NewPostgresResponseStore(db).Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tenantadmin.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	service, err := tenantadmin.NewService(db, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	tenantID := fmt.Sprintf("admin-lifecycle-%d", time.Now().UnixNano())
	actor := tenantadmin.ActorEnvelope{
		Type: "human", ID: "operator-lifecycle", Scopes: []string{tenantadmin.ScopePlatformWrite},
		RequestID: "request-lifecycle", Reason: "integration test",
	}
	created, err := service.CreateTenant(ctx, actor, "create-"+tenantID, tenantadmin.CreateTenantCommand{
		ID: tenantID, Slug: tenantID, DisplayName: "Before", HomeRegion: "us-test",
		InitialPolicy: core.TenantPolicy{Revision: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	displayName := "After"
	metadata := map[string]any{"cost_center": "research"}
	updated, err := service.UpdateTenant(ctx, actor, "update-"+tenantID, tenantadmin.UpdateTenantCommand{
		TenantID: tenantID, ExpectedRevision: created.Tenant.Revision,
		DisplayName: &displayName, Metadata: &metadata,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Tenant.DisplayName != displayName || updated.Tenant.Metadata["cost_center"] != "research" || updated.Tenant.Revision != 2 {
		t.Fatalf("updated Tenant = %#v", updated.Tenant)
	}
	if _, err := service.UpdateTenant(ctx, actor, "stale-update-"+tenantID, tenantadmin.UpdateTenantCommand{
		TenantID: tenantID, ExpectedRevision: 1, DisplayName: &displayName,
	}); !errors.Is(err, tenantadmin.ErrRevisionConflict) {
		t.Fatalf("stale update error = %v", err)
	}
	reserved := map[string]any{"home_region": "elsewhere"}
	if _, err := service.UpdateTenant(ctx, actor, "reserved-update-"+tenantID, tenantadmin.UpdateTenantCommand{
		TenantID: tenantID, ExpectedRevision: 2, Metadata: &reserved,
	}); !errors.Is(err, tenantadmin.ErrInvalidArgument) {
		t.Fatalf("reserved metadata error = %v", err)
	}
	suspended, err := service.TransitionTenant(ctx, actor, "suspend-"+tenantID, tenantadmin.TransitionTenantCommand{
		TenantID: tenantID, ExpectedRevision: 2, Target: access.TenantSuspended,
	})
	if err != nil {
		t.Fatal(err)
	}
	if suspended.Tenant.Status != access.TenantSuspended || suspended.Tenant.Revision != 3 {
		t.Fatalf("suspended Tenant = %#v", suspended.Tenant)
	}
	reactivated, err := service.TransitionTenant(ctx, actor, "reactivate-"+tenantID, tenantadmin.TransitionTenantCommand{
		TenantID: tenantID, ExpectedRevision: 3, Target: access.TenantActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	closed, err := service.TransitionTenant(ctx, actor, "close-"+tenantID, tenantadmin.TransitionTenantCommand{
		TenantID: tenantID, ExpectedRevision: reactivated.Tenant.Revision, Target: access.TenantClosed,
	})
	if err != nil {
		t.Fatal(err)
	}
	if closed.Tenant.Status != access.TenantClosed || closed.Tenant.Revision != 5 {
		t.Fatalf("closed Tenant = %#v", closed.Tenant)
	}
	if _, err := service.TransitionTenant(ctx, actor, "reopen-"+tenantID, tenantadmin.TransitionTenantCommand{
		TenantID: tenantID, ExpectedRevision: 5, Target: access.TenantActive,
	}); !errors.Is(err, tenantadmin.ErrRevisionConflict) {
		t.Fatalf("closed Tenant reopen error = %v", err)
	}
	var auditCount, outboxCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM control_audit_events WHERE tenant_id = $1`, tenantID).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM control_outbox WHERE tenant_id = $1`, tenantID).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 5 || outboxCount != 5 {
		t.Fatalf("audit/outbox count = %d/%d, want 5/5", auditCount, outboxCount)
	}
}

func TestTenantPolicyPublicationAndRestoreAppendImmutableRevisions(t *testing.T) {
	db, ctx := adminIntegrationDatabase(t)
	if err := store.NewPostgresResponseStore(db).Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tenantadmin.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	service, err := tenantadmin.NewService(db, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	tenantID := fmt.Sprintf("admin-policy-%d", time.Now().UnixNano())
	actor := tenantadmin.ActorEnvelope{
		Type: "human", ID: "operator-policy", Scopes: []string{tenantadmin.ScopePlatformWrite},
		RequestID: "request-policy", Reason: "integration test",
	}
	created, err := service.CreateTenant(ctx, actor, "create-"+tenantID, tenantadmin.CreateTenantCommand{
		ID: tenantID, Slug: tenantID, DisplayName: "Policy Tenant", HomeRegion: "us-test",
		InitialPolicy: core.TenantPolicy{Revision: 1, MaxConcurrentResponses: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	nextPolicy := core.TenantPolicy{Revision: 2, MaxConcurrentResponses: 3}
	published, err := service.PublishTenantPolicy(ctx, actor, "publish-"+tenantID, tenantadmin.PublishPolicyCommand{
		TenantID: tenantID, ExpectedRevision: 1, Policy: &nextPolicy,
	})
	if err != nil {
		t.Fatal(err)
	}
	if published.Tenant.Policy.Revision != 2 || published.Tenant.Policy.MaxConcurrentResponses != 3 || published.Tenant.Revision != created.Tenant.Revision+1 {
		t.Fatalf("published Tenant = %#v", published.Tenant)
	}
	if _, err := service.PublishTenantPolicy(ctx, actor, "stale-policy-"+tenantID, tenantadmin.PublishPolicyCommand{
		TenantID: tenantID, ExpectedRevision: 1, Policy: &nextPolicy,
	}); !errors.Is(err, tenantadmin.ErrRevisionConflict) {
		t.Fatalf("stale policy publication error = %v", err)
	}
	restoreRevision := int64(1)
	restored, err := service.PublishTenantPolicy(ctx, actor, "restore-"+tenantID, tenantadmin.PublishPolicyCommand{
		TenantID: tenantID, ExpectedRevision: 2, RestoreRevision: &restoreRevision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if restored.Tenant.Policy.Revision != 3 || restored.Tenant.Policy.MaxConcurrentResponses != 1 {
		t.Fatalf("restored Tenant = %#v", restored.Tenant)
	}
	revisions, err := service.ListTenantPolicyRevisions(ctx, actor, tenantID, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(revisions.Data) != 3 || revisions.Data[0].Revision != 1 || revisions.Data[1].Revision != 2 || revisions.Data[2].Revision != 3 {
		t.Fatalf("policy revisions = %#v", revisions)
	}
	if revisions.Data[2].Policy.MaxConcurrentResponses != revisions.Data[0].Policy.MaxConcurrentResponses {
		t.Fatalf("restored revision content = %#v, original = %#v", revisions.Data[2], revisions.Data[0])
	}
	if _, err := db.ExecContext(ctx, `UPDATE tenant_policy_revisions SET change_reason = 'tampered' WHERE tenant_id = $1 AND revision = 1`, tenantID); err == nil {
		t.Fatal("policy revision update unexpectedly succeeded")
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM tenant_policy_revisions WHERE tenant_id = $1 AND revision = 1`, tenantID); err == nil {
		t.Fatal("policy revision deletion unexpectedly succeeded")
	}
}

func adminIntegrationDatabase(t *testing.T) (*sql.DB, context.Context) {
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
