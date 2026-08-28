//go:build integration

package accessprojection_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/toddzheng/llm-gateway/internal/access"
	"github.com/toddzheng/llm-gateway/internal/accessprojection"
	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/credentialadmin"
	"github.com/toddzheng/llm-gateway/internal/store"
	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

func TestProjectionDeduplicatesRejectsStaleDetectsGapAndAuthenticatesLocally(t *testing.T) {
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
	now := time.Now().UTC().Truncate(time.Microsecond)
	pepper := []byte("projection-integration-pepper")
	store, err := accessprojection.New(db, accessprojection.PepperRing{
		CurrentVersion: 1, Peppers: map[int16][]byte{1: pepper},
	}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	tenantID := fmt.Sprintf("projection-tenant-%d", time.Now().UnixNano())
	keyID := "gak_" + tenantID
	rawKey := "gw_projection_" + tenantID
	digest := hmac.New(sha256.New, pepper)
	_, _ = digest.Write([]byte(rawKey))
	eventWithTenant := func(
		id string,
		revision int64,
		status access.APIKeyStatus,
		tenantRevision int64,
		tenantStatus access.TenantStatus,
	) accessprojection.ControlEvent {
		payload, marshalErr := json.Marshal(map[string]any{
			"tenant_id": tenantID, "tenant_status": tenantStatus, "tenant_revision": tenantRevision,
			"home_region": "us-test", "execution_epoch": 1,
			"tenant_policy_revision": 1, "tenant_policy": core.TenantPolicy{},
			"api_key_id": keyID, "prefix": "gw_project", "secret_digest": digest.Sum(nil), "digest_version": 1,
			"status": status, "key_revision": revision, "policy_revision": 1, "policy": core.APIKeyPolicy{},
		})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return accessprojection.ControlEvent{
			EventID: id, SchemaVersion: 2, AggregateType: "GatewayAPIKey", AggregateID: keyID,
			AggregateRevision: revision, TenantID: tenantID, EventType: "GatewayAPIKeyChanged",
			OccurredAt: now.Add(-time.Second), Payload: payload,
		}
	}
	event := func(id string, revision int64, status access.APIKeyStatus) accessprojection.ControlEvent {
		return eventWithTenant(id, revision, status, 1, access.TenantActive)
	}
	tenantEvent := func(id string, revision int64, status access.TenantStatus) accessprojection.ControlEvent {
		payload, marshalErr := json.Marshal(map[string]any{
			"tenant_id": tenantID, "status": status, "home_region": "us-test",
			"tenant_revision": revision, "policy_revision": 1, "tenant_policy": core.TenantPolicy{},
			"execution_epoch": 1,
		})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return accessprojection.ControlEvent{
			EventID: id, SchemaVersion: 2, AggregateType: "Tenant", AggregateID: tenantID,
			AggregateRevision: revision, TenantID: tenantID, EventType: "TenantChanged",
			OccurredAt: now.Add(-time.Second), Payload: payload,
		}
	}
	applied, err := store.Apply(ctx, eventWithTenant("event-1-"+tenantID, 1, access.APIKeyActive, 2, access.TenantSuspended))
	if err != nil || applied.Disposition != accessprojection.DispositionApplied || applied.Lag != time.Second {
		t.Fatalf("apply = %#v err=%v", applied, err)
	}
	if _, err := store.Apply(ctx, tenantEvent("tenant-event-1-"+tenantID, 1, access.TenantActive)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Authenticate(ctx, rawKey); !errors.Is(err, access.ErrInvalidAPIKey) {
		t.Fatalf("delayed Tenant event regressed newer embedded Tenant state: %v", err)
	}
	if _, err := store.Apply(ctx, tenantEvent("tenant-event-2-"+tenantID, 2, access.TenantActive)); err != nil {
		t.Fatal(err)
	}
	duplicate, err := store.Apply(ctx, eventWithTenant("event-1-"+tenantID, 1, access.APIKeyActive, 2, access.TenantSuspended))
	if err != nil || duplicate.Disposition != accessprojection.DispositionDuplicate {
		t.Fatalf("duplicate = %#v err=%v", duplicate, err)
	}
	stale, err := store.Apply(ctx, event("event-stale-"+tenantID, 1, access.APIKeyActive))
	if err != nil || stale.Disposition != accessprojection.DispositionStale {
		t.Fatalf("stale = %#v err=%v", stale, err)
	}
	gap, err := store.Apply(ctx, event("event-4-"+tenantID, 4, access.APIKeyRevoked))
	if !errors.Is(err, accessprojection.ErrRevisionGap) || gap.Disposition != accessprojection.DispositionGap {
		t.Fatalf("gap = %#v err=%v", gap, err)
	}
	status, err := store.Status(ctx)
	if err != nil || status.GapCount < 1 || status.OldestGapAt == nil {
		t.Fatalf("status = %#v err=%v", status, err)
	}
	if _, err := store.Apply(ctx, event("event-2-"+tenantID, 2, access.APIKeyActive)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Apply(ctx, tenantEvent("tenant-event-3-"+tenantID, 3, access.TenantSuspended)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Apply(ctx, event("event-3-"+tenantID, 3, access.APIKeyActive)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Authenticate(ctx, rawKey); !errors.Is(err, access.ErrInvalidAPIKey) {
		t.Fatalf("stale embedded Tenant state reactivated a suspended Tenant: %v", err)
	}
	if _, err := store.Apply(ctx, tenantEvent("tenant-event-4-"+tenantID, 4, access.TenantActive)); err != nil {
		t.Fatal(err)
	}
	principal, err := store.Authenticate(ctx, rawKey)
	if err != nil || principal.TenantID != tenantID || principal.APIKeyID != keyID || principal.HomeRegion != "us-test" {
		t.Fatalf("principal = %#v err=%v", principal, err)
	}
	if _, err := store.Apply(ctx, event("event-4-"+tenantID, 4, access.APIKeyRevoked)); err != nil {
		t.Fatal(err)
	}
	revocationStatus, err := store.Status(ctx)
	if err != nil || revocationStatus.LastRevocationAppliedAt == nil || revocationStatus.MaxRevocationApplyLag < time.Second {
		t.Fatalf("revocation projection status = %#v err=%v", revocationStatus, err)
	}
	if _, err := store.Authenticate(ctx, rawKey); !errors.Is(err, access.ErrInvalidAPIKey) {
		t.Fatalf("revoked authentication error = %v", err)
	}
}

func TestSnapshotAtomicallyRepairsProjectionAndGatesPepperRetirement(t *testing.T) {
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
	now := time.Now().UTC().Truncate(time.Microsecond)
	pepper := []byte("snapshot-integration-pepper")
	store, err := accessprojection.New(db, accessprojection.PepperRing{CurrentVersion: 2, Peppers: map[int16][]byte{
		1: []byte("old-snapshot-pepper"), 2: pepper,
	}}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	tenantID := fmt.Sprintf("snapshot-tenant-%d", time.Now().UnixNano())
	currentBefore, err := store.ActiveDigestVersionCount(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	oldBefore, err := store.ActiveDigestVersionCount(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM gateway_access_projection WHERE tenant_id = $1`, tenantID)
	})
	rawKey := "gw_snapshot_" + tenantID
	keyID := "gak_" + tenantID
	digest := hmac.New(sha256.New, pepper)
	_, _ = digest.Write([]byte(rawKey))
	oldRawKey := "gw_snapshot_old_" + tenantID
	oldDigest := hmac.New(sha256.New, []byte("old-snapshot-pepper"))
	_, _ = oldDigest.Write([]byte(oldRawKey))
	snapshot := accessprojection.Snapshot{
		Tenant: accessprojection.TenantSnapshot{
			ID: tenantID, Status: access.TenantActive, Revision: 7, HomeRegion: "us-test", ExecutionEpoch: 3,
			Policy: core.TenantPolicy{Revision: 4},
		},
		Keys: []accessprojection.KeySnapshot{{
			ID: keyID, Prefix: "gw_snapshot", SecretDigest: digest.Sum(nil), DigestVersion: 2,
			Status: access.APIKeyActive, Revision: 5, Policy: core.APIKeyPolicy{Revision: 2},
		}, {
			ID: "gak_old_" + tenantID, Prefix: "gw_snapshot", SecretDigest: oldDigest.Sum(nil), DigestVersion: 1,
			Status: access.APIKeyActive, Revision: 1, Policy: core.APIKeyPolicy{Revision: 1},
		}},
		CreatedAt: now,
	}
	gapPayload, err := json.Marshal(map[string]any{
		"tenant_id": tenantID, "tenant_status": access.TenantActive, "tenant_revision": 7,
		"home_region": "us-test", "execution_epoch": 3,
		"tenant_policy_revision": 4, "tenant_policy": core.TenantPolicy{},
		"api_key_id": keyID, "prefix": "gw_snapshot", "secret_digest": digest.Sum(nil), "digest_version": 2,
		"status": access.APIKeyActive, "key_revision": 5, "policy_revision": 2, "policy": core.APIKeyPolicy{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Apply(ctx, accessprojection.ControlEvent{
		EventID: "snapshot-gap-" + tenantID, SchemaVersion: 2, AggregateType: "GatewayAPIKey", AggregateID: keyID,
		AggregateRevision: 5, TenantID: tenantID, EventType: "GatewayAPIKeyChanged", OccurredAt: now, Payload: gapPayload,
	}); !errors.Is(err, accessprojection.ErrRevisionGap) {
		t.Fatalf("seed snapshot gap error = %v", err)
	}
	if err := store.ReplaceSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	var remainingGap int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM gateway_access_gaps WHERE aggregate_id = $1`, keyID).Scan(&remainingGap); err != nil || remainingGap != 0 {
		t.Fatalf("snapshot gap count = %d err=%v", remainingGap, err)
	}
	principal, err := store.Authenticate(ctx, rawKey)
	if err != nil || principal.ExecutionEpoch != 3 || principal.TenantPolicy.Revision != 4 || principal.APIKeyPolicy.Revision != 2 {
		t.Fatalf("snapshot principal = %#v err=%v", principal, err)
	}
	flushed, err := store.FlushLastUsed(ctx, 100)
	if err != nil || flushed != 1 {
		t.Fatalf("last-used flush = %d err=%v", flushed, err)
	}
	var lastUsedAt time.Time
	if err := db.QueryRowContext(ctx, `
		SELECT last_used_at FROM gateway_access_projection WHERE tenant_id = $1 AND api_key_id = $2`, tenantID, keyID).Scan(&lastUsedAt); err != nil {
		t.Fatal(err)
	}
	count, err := store.ActiveDigestVersionCount(ctx, 2)
	if err != nil || count != currentBefore+1 {
		t.Fatalf("active digest version count = %d err=%v", count, err)
	}
	oldCount, err := store.ActiveDigestVersionCount(ctx, 1)
	if err != nil || oldCount != oldBefore+1 {
		t.Fatalf("old digest version count = %d err=%v", oldCount, err)
	}
	suspendedSnapshot := snapshot
	suspendedSnapshot.Tenant.Status = access.TenantSuspended
	suspendedSnapshot.Tenant.Revision = 8
	if err := store.ReplaceSnapshot(ctx, suspendedSnapshot); err != nil {
		t.Fatal(err)
	}
	withoutOld, err := accessprojection.New(db, accessprojection.PepperRing{
		CurrentVersion: 2, Peppers: map[int16][]byte{2: pepper},
	}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := withoutOld.ValidatePepperCoverage(ctx); err == nil {
		t.Fatal("pepper coverage accepted removal of a digest version used by a reactivatable suspended Tenant")
	}
	snapshot.Tenant.Revision = 9
	if err := store.ReplaceSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := store.AcquireAPIKeyResponseSlot(ctx, keyID, "snapshot-lease-1", 1, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	var repairedLastUsedAt time.Time
	if err := db.QueryRowContext(ctx, `
		SELECT last_used_at FROM gateway_access_projection WHERE tenant_id = $1 AND api_key_id = $2`, tenantID, keyID).Scan(&repairedLastUsedAt); err != nil {
		t.Fatal(err)
	}
	if !repairedLastUsedAt.Equal(lastUsedAt) {
		t.Fatalf("snapshot repair regressed last_used_at: before=%s after=%s", lastUsedAt, repairedLastUsedAt)
	}
	if err := store.AcquireAPIKeyResponseSlot(ctx, keyID, "snapshot-lease-2", 1, now.Add(time.Minute)); !errors.Is(err, accessprojection.ErrConcurrencyExceeded) {
		t.Fatalf("snapshot repair discarded active concurrency lease: %v", err)
	}
	if err := store.ReleaseAPIKeyResponseSlot(ctx, keyID, "snapshot-lease-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE gateway_access_heads SET revision = 6 WHERE aggregate_type = 'GatewayAPIKey' AND aggregate_id = $1`, keyID); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceSnapshot(ctx, snapshot); !errors.Is(err, accessprojection.ErrInvalidEvent) {
		t.Fatalf("stale snapshot replacement error = %v", err)
	}
	if _, err := store.Authenticate(ctx, rawKey); err != nil {
		t.Fatalf("stale snapshot damaged last valid projection: %v", err)
	}
}

func TestAPIKeyConcurrencyLeaseIsHardAcrossStoreInstances(t *testing.T) {
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
	pepper := []byte("concurrency-integration-pepper")
	first, err := accessprojection.New(db, accessprojection.PepperRing{
		CurrentVersion: 1, Peppers: map[int16][]byte{1: pepper},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := accessprojection.New(db, accessprojection.PepperRing{
		CurrentVersion: 1, Peppers: map[int16][]byte{1: pepper},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	apiKeyID := fmt.Sprintf("concurrency-key-%d", time.Now().UnixNano())
	expiresAt := time.Now().UTC().Add(time.Minute)
	if err := first.AcquireAPIKeyResponseSlot(ctx, apiKeyID, "lease-1", 1, expiresAt); err != nil {
		t.Fatal(err)
	}
	if err := second.AcquireAPIKeyResponseSlot(ctx, apiKeyID, "lease-2", 1, expiresAt); !errors.Is(err, accessprojection.ErrConcurrencyExceeded) {
		t.Fatalf("second store acquire error = %v", err)
	}
	if err := first.ReleaseAPIKeyResponseSlot(ctx, apiKeyID, "lease-1"); err != nil {
		t.Fatal(err)
	}
	if err := second.AcquireAPIKeyResponseSlot(ctx, apiKeyID, "lease-2", 1, expiresAt); err != nil {
		t.Fatal(err)
	}
}

func TestControlOutboxEventsBuildLocalProjectionWithoutRuntimeControlPlaneCalls(t *testing.T) {
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
	if err := tenantadmin.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := credentialadmin.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := accessprojection.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	pepper := []byte("outbox-projection-pepper")
	tenantID := fmt.Sprintf("outbox-projection-%d", time.Now().UnixNano())
	actor := tenantadmin.ActorEnvelope{
		Type: "human", ID: "outbox-test", Scopes: []string{tenantadmin.ScopePlatformRead, tenantadmin.ScopePlatformWrite},
		RequestID: "outbox-projection-request", Reason: "integration test",
	}
	tenantService, _ := tenantadmin.NewService(db, time.Now)
	if _, err := tenantService.CreateTenant(ctx, actor, "create-"+tenantID, tenantadmin.CreateTenantCommand{
		ID: tenantID, Slug: tenantID, DisplayName: "Outbox Projection", HomeRegion: "us-test",
		InitialPolicy: core.TenantPolicy{Revision: 1},
	}); err != nil {
		t.Fatal(err)
	}
	credentialService, err := credentialadmin.NewService(db, credentialadmin.PepperRing{
		CurrentVersion: 1, Peppers: map[int16][]byte{1: pepper},
	}, time.Now, nil)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := credentialService.Issue(ctx, actor, "issue-"+tenantID, credentialadmin.IssueCommand{
		TenantID: tenantID, Name: "outbox workload", Policy: core.APIKeyPolicy{Revision: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	projection, err := accessprojection.New(db, accessprojection.PepperRing{
		CurrentVersion: 1, Peppers: map[int16][]byte{1: pepper},
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	for attempts := 0; attempts < 5; attempts++ {
		result, err := projection.ConsumeControlOutboxBatch(ctx, 1000)
		if err != nil {
			t.Fatal(err)
		}
		if result.Scanned == 0 {
			break
		}
	}
	principal, err := projection.Authenticate(ctx, issued.RawSecret)
	if err != nil || principal.TenantID != tenantID || principal.APIKeyID != issued.Credential.ID {
		t.Fatalf("projected principal = %#v err=%v", principal, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE api_keys SET status = 'revoked', revoked_at = now() WHERE tenant_id = $1 AND id = $2`, tenantID, issued.Credential.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := projection.Authenticate(ctx, issued.RawSecret); err != nil {
		t.Fatalf("local projection unexpectedly depended on authoritative control tables: %v", err)
	}
}
