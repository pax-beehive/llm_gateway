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

	"github.com/toddzheng/llm-gateway/internal/access"
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
		InitialPolicy: core.TenantPolicy{Revision: 1, MaxConcurrentResponses: 1},
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
	badCIDRs := []string{"not-a-cidr"}
	if _, err := service.Issue(ctx, actor, "issue-invalid-cidr-"+tenantID, credentialadmin.IssueCommand{
		TenantID: tenantID, Name: "invalid CIDR", Policy: core.APIKeyPolicy{AllowedCIDRs: &badCIDRs},
	}); !errors.Is(err, credentialadmin.ErrInvalidArgument) {
		t.Fatalf("invalid issuance CIDR error = %v", err)
	}
	if _, err := service.Issue(ctx, actor, "issue-typed-metadata-"+tenantID, credentialadmin.IssueCommand{
		TenantID: tenantID, Name: "typed metadata", Metadata: map[string]any{"home_region": "elsewhere"},
	}); !errors.Is(err, credentialadmin.ErrInvalidArgument) {
		t.Fatalf("typed metadata issuance error = %v", err)
	}
	tooHigh := 2
	if _, err := service.Issue(ctx, actor, "issue-expanded-concurrency-"+tenantID, credentialadmin.IssueCommand{
		TenantID: tenantID, Name: "expanded concurrency", Policy: core.APIKeyPolicy{MaxConcurrentResponses: &tooHigh},
	}); !errors.Is(err, credentialadmin.ErrPolicyDenied) {
		t.Fatalf("expanded issuance concurrency error = %v", err)
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

func TestGatewayAPIKeyQueriesAndProfileUpdateUseStablePaginationAndCAS(t *testing.T) {
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
	tenantID := fmt.Sprintf("credential-query-%d", time.Now().UnixNano())
	actor := tenantadmin.ActorEnvelope{
		Type: "human", ID: "credential-operator", Scopes: []string{tenantadmin.ScopePlatformRead, tenantadmin.ScopePlatformWrite},
		RequestID: "request-credential-query", Reason: "integration test",
	}
	if _, err := tenantService.CreateTenant(ctx, actor, "create-"+tenantID, tenantadmin.CreateTenantCommand{
		ID: tenantID, Slug: tenantID, DisplayName: "Credential Query Tenant", HomeRegion: "us-test",
		InitialPolicy: core.TenantPolicy{Revision: 1},
	}); err != nil {
		t.Fatal(err)
	}
	service, err := credentialadmin.NewService(db, credentialadmin.PepperRing{
		CurrentVersion: 1, Peppers: map[int16][]byte{1: []byte("query-integration-pepper")},
	}, time.Now, nil)
	if err != nil {
		t.Fatal(err)
	}
	issued := make([]credentialadmin.IssueResult, 0, 3)
	for _, name := range []string{"alpha", "beta", "gamma"} {
		result, err := service.Issue(ctx, actor, "issue-"+tenantID+"-"+name, credentialadmin.IssueCommand{
			TenantID: tenantID, Name: name, Policy: core.APIKeyPolicy{Revision: 1},
		})
		if err != nil {
			t.Fatal(err)
		}
		issued = append(issued, result)
	}
	first, err := service.List(ctx, actor, credentialadmin.CredentialFilter{TenantID: tenantID, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Data) != 2 || first.NextCursor == "" || first.Data[0].ID >= first.Data[1].ID {
		t.Fatalf("first credential page = %#v", first)
	}
	second, err := service.List(ctx, actor, credentialadmin.CredentialFilter{TenantID: tenantID, Limit: 2, Cursor: first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Data) != 1 || second.NextCursor != "" {
		t.Fatalf("second credential page = %#v", second)
	}
	observed, err := service.Get(ctx, actor, tenantID, issued[0].Credential.ID)
	if err != nil || observed.ID != issued[0].Credential.ID {
		t.Fatalf("observed credential = %#v err=%v", observed, err)
	}
	name := "renamed"
	metadata := map[string]any{"owner": "platform"}
	expiresAt := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	updated, err := service.Update(ctx, actor, "update-"+tenantID, credentialadmin.UpdateCommand{
		TenantID: tenantID, CredentialID: observed.ID, ExpectedRevision: observed.Revision,
		Name: &name, Metadata: &metadata, ExpiresAt: &expiresAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Credential.Name != name || updated.Credential.Metadata["owner"] != "platform" ||
		updated.Credential.ExpiresAt == nil || !updated.Credential.ExpiresAt.Equal(expiresAt) || updated.Credential.Revision != observed.Revision+1 {
		t.Fatalf("updated credential = %#v", updated.Credential)
	}
	if _, err := service.Update(ctx, actor, "stale-update-"+tenantID, credentialadmin.UpdateCommand{
		TenantID: tenantID, CredentialID: observed.ID, ExpectedRevision: observed.Revision, Name: &name,
	}); !errors.Is(err, credentialadmin.ErrRevisionConflict) {
		t.Fatalf("stale update error = %v", err)
	}
	var auditCount, outboxCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM control_audit_events WHERE tenant_id = $1 AND action = 'gateway_api_key.update'`, tenantID).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM control_outbox WHERE tenant_id = $1 AND event_type = 'GatewayAPIKeyChanged'`, tenantID).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 || outboxCount != 1 {
		t.Fatalf("update audit/outbox = %d/%d", auditCount, outboxCount)
	}
}

func TestGatewayAPIKeyRotationReturnsSecretOnceAndGraceReconciliationRevokesPredecessor(t *testing.T) {
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
	now := time.Now().UTC().Truncate(time.Microsecond)
	tenantID := fmt.Sprintf("credential-rotate-%d", time.Now().UnixNano())
	actor := tenantadmin.ActorEnvelope{
		Type: "human", ID: "credential-operator", Scopes: []string{tenantadmin.ScopePlatformRead, tenantadmin.ScopePlatformWrite},
		RequestID: "request-credential-rotate", Reason: "integration test",
	}
	tenantService, _ := tenantadmin.NewService(db, func() time.Time { return now })
	if _, err := tenantService.CreateTenant(ctx, actor, "create-"+tenantID, tenantadmin.CreateTenantCommand{
		ID: tenantID, Slug: tenantID, DisplayName: "Credential Rotate Tenant", HomeRegion: "us-test",
		InitialPolicy: core.TenantPolicy{Revision: 1},
	}); err != nil {
		t.Fatal(err)
	}
	service, err := credentialadmin.NewService(db, credentialadmin.PepperRing{
		CurrentVersion: 2, Peppers: map[int16][]byte{1: []byte("old-rotation-pepper"), 2: []byte("new-rotation-pepper")},
	}, func() time.Time { return now }, nil)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := service.Issue(ctx, actor, "issue-"+tenantID, credentialadmin.IssueCommand{
		TenantID: tenantID, Name: "rotating", Policy: core.APIKeyPolicy{Revision: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	grace := now.Add(5 * time.Minute)
	rotated, err := service.Rotate(ctx, actor, "rotate-"+tenantID, credentialadmin.RotateCommand{
		TenantID: tenantID, CredentialID: issued.Credential.ID, ExpectedRevision: issued.Credential.Revision,
		GraceExpiresAt: &grace,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rotated.RawSecret == "" || rotated.Replacement.PredecessorID != issued.Credential.ID ||
		rotated.Predecessor.ReplacementID != rotated.Replacement.ID || rotated.Predecessor.GraceExpiresAt == nil ||
		rotated.Predecessor.Status != access.APIKeyActive || rotated.Replacement.DigestVersion != 2 {
		t.Fatalf("rotation = %#v", rotated)
	}
	replayed, err := service.Rotate(ctx, actor, "rotate-"+tenantID, credentialadmin.RotateCommand{
		TenantID: tenantID, CredentialID: issued.Credential.ID, ExpectedRevision: issued.Credential.Revision,
		GraceExpiresAt: &grace,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replay || replayed.RawSecret != "" || replayed.Replacement.ID != rotated.Replacement.ID {
		t.Fatalf("rotation replay = %#v", replayed)
	}
	now = grace.Add(time.Second)
	count, err := service.ReconcileExpiredGrace(ctx, 10)
	if err != nil || count != 1 {
		t.Fatalf("reconcile count=%d err=%v", count, err)
	}
	predecessor, err := service.Get(ctx, actor, tenantID, issued.Credential.ID)
	if err != nil || predecessor.Status != access.APIKeyRevoked || predecessor.RevokedAt == nil {
		t.Fatalf("reconciled predecessor = %#v err=%v", predecessor, err)
	}
	if _, err := service.Revoke(ctx, actor, "revoke-again-"+tenantID, credentialadmin.RevokeCommand{
		TenantID: tenantID, CredentialID: predecessor.ID, ExpectedRevision: predecessor.Revision,
	}); err != nil {
		t.Fatalf("terminal revoke must be idempotent: %v", err)
	}
	var issuedEvents, revokedEvents int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM control_outbox WHERE tenant_id = $1 AND event_type = 'GatewayAPIKeyIssued'`, tenantID).Scan(&issuedEvents); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM control_outbox WHERE tenant_id = $1 AND event_type = 'GatewayAPIKeyRevoked'`, tenantID).Scan(&revokedEvents); err != nil {
		t.Fatal(err)
	}
	if issuedEvents != 2 || revokedEvents != 1 {
		t.Fatalf("issued/revoked events = %d/%d", issuedEvents, revokedEvents)
	}
}

func TestGatewayAPIKeyPolicyPublicationHistoryRestoreAndEffectiveIntersection(t *testing.T) {
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
	tenantID := fmt.Sprintf("credential-policy-%d", time.Now().UnixNano())
	actor := tenantadmin.ActorEnvelope{
		Type: "human", ID: "credential-operator", Scopes: []string{tenantadmin.ScopePlatformRead, tenantadmin.ScopePlatformWrite},
		RequestID: "request-credential-policy", Reason: "integration test",
	}
	tenantService, _ := tenantadmin.NewService(db, time.Now)
	if _, err := tenantService.CreateTenant(ctx, actor, "create-"+tenantID, tenantadmin.CreateTenantCommand{
		ID: tenantID, Slug: tenantID, DisplayName: "Credential Policy Tenant", HomeRegion: "us-test",
		InitialPolicy: core.TenantPolicy{Revision: 1, MaxConcurrentResponses: 5},
	}); err != nil {
		t.Fatal(err)
	}
	service, err := credentialadmin.NewService(db, credentialadmin.PepperRing{
		CurrentVersion: 1, Peppers: map[int16][]byte{1: []byte("policy-integration-pepper")},
	}, time.Now, nil)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := service.Issue(ctx, actor, "issue-"+tenantID, credentialadmin.IssueCommand{
		TenantID: tenantID, Name: "policy", Policy: core.APIKeyPolicy{Revision: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	models := []string{"gpt-5.6", "deepseek-chat"}
	operations := []string{"responses", "embeddings"}
	cidrs := []string{"203.0.113.0/24"}
	regions := []string{"us-test"}
	concurrency := 2
	policy := core.APIKeyPolicy{
		Revision: 2, AllowedPublicModels: &models, AllowedOperations: &operations,
		AllowedCIDRs: &cidrs, AllowedRegions: &regions, MaxConcurrentResponses: &concurrency,
	}
	published, err := service.PublishPolicy(ctx, actor, "policy-"+tenantID, credentialadmin.PublishPolicyCommand{
		TenantID: tenantID, CredentialID: issued.Credential.ID, ExpectedRevision: 1, Policy: &policy,
	})
	if err != nil {
		t.Fatal(err)
	}
	if published.Credential.Policy.Revision != 2 || published.Credential.Revision != 2 {
		t.Fatalf("published policy credential = %#v", published.Credential)
	}
	effective, err := service.GetEffectivePolicy(ctx, actor, tenantID, issued.Credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if effective.MaxConcurrentResponses != 2 || effective.TenantPolicy.Revision != 1 || effective.APIKeyPolicy.Revision != 2 {
		t.Fatalf("effective policy = %#v", effective)
	}
	revisions, err := service.ListPolicyRevisions(ctx, actor, tenantID, issued.Credential.ID, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(revisions.Data) != 2 || revisions.Data[0].Revision != 1 || revisions.Data[1].Revision != 2 {
		t.Fatalf("policy revisions = %#v", revisions)
	}
	restoreRevision := int64(1)
	restored, err := service.PublishPolicy(ctx, actor, "restore-"+tenantID, credentialadmin.PublishPolicyCommand{
		TenantID: tenantID, CredentialID: issued.Credential.ID, ExpectedRevision: 2, RestoreRevision: &restoreRevision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if restored.Credential.Policy.Revision != 3 || restored.Credential.Policy.AllowedPublicModels != nil {
		t.Fatalf("restored policy = %#v", restored.Credential.Policy)
	}
	badCIDRs := []string{"not-a-cidr"}
	badPolicy := core.APIKeyPolicy{Revision: 4, AllowedCIDRs: &badCIDRs}
	if _, err := service.PublishPolicy(ctx, actor, "invalid-policy-"+tenantID, credentialadmin.PublishPolicyCommand{
		TenantID: tenantID, CredentialID: issued.Credential.ID, ExpectedRevision: 3, Policy: &badPolicy,
	}); !errors.Is(err, credentialadmin.ErrInvalidArgument) {
		t.Fatalf("invalid CIDR policy error = %v", err)
	}
	var policyEvents int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM control_outbox WHERE tenant_id = $1 AND event_type = 'GatewayAPIKeyPolicyPublished'`, tenantID).Scan(&policyEvents); err != nil {
		t.Fatal(err)
	}
	if policyEvents != 2 {
		t.Fatalf("policy events = %d", policyEvents)
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
