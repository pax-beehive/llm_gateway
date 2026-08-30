//go:build integration

package operations_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/toddzheng/llm-gateway/internal/controlapi"
	"github.com/toddzheng/llm-gateway/internal/migrations"
	"github.com/toddzheng/llm-gateway/internal/operations"
	"github.com/toddzheng/llm-gateway/internal/routingcatalog"
	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

func TestControlPlaneFirstRollingObservationVersionsOverHTTP(t *testing.T) {
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
	if err := migrations.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := tenantadmin.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := routingcatalog.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := operations.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	service, _ := operations.NewService(db, nil, func() time.Time { return now })
	key := []byte("rolling-observation-hmac-key-0000001")
	verifier, err := operations.NewHMACVerifier(map[string]string{"gateway-rolling": string(key)}, map[string]string{"gateway-rolling": "us-west"}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	handler := controlapi.New(controlapi.Config{GatewayObservations: service, GatewayVerifier: verifier})
	for _, version := range []int{operations.MinimumObservationVersion, operations.CurrentObservationVersion} {
		observation := operations.GatewayObservation{EventSchemaVersion: version, GatewayID: "gateway-rolling", Region: "us-west",
			BuildSHA: fmt.Sprintf("rolling-v%d", version), DatabaseSchemaVersion: operations.CurrentDatabaseSchema,
			RoutingCatalogRevision: int64(version), AccessProjectionRevision: int64(version), StartedAt: now.Add(-time.Minute),
			ObservedAt: now.Add(time.Duration(version) * time.Millisecond), Consumers: []operations.ConsumerObservation{}}
		if version == operations.CurrentObservationVersion {
			observation.Backlogs.MeteringProjectionStatus = "unavailable"
		}
		body, _ := json.Marshal(observation)
		authorization, err := operations.GatewayAuthorization(key, observation.GatewayID, now, http.MethodPost, "/internal/v1/operations/gateway-observations", body)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/internal/v1/operations/gateway-observations", bytes.NewReader(body))
		request.Header.Set("Authorization", authorization)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusAccepted {
			t.Fatalf("observation v%d status/body = %d/%s", version, response.Code, response.Body.String())
		}
	}
}

func TestGatewayObservationsAreIdempotentAndCannotMoveRevisionsBackward(t *testing.T) {
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
	if err := migrations.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := tenantadmin.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := routingcatalog.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := operations.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	service, err := operations.NewService(db, nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	gatewayID := fmt.Sprintf("gateway-operations-%d", time.Now().UnixNano())
	identity := operations.GatewayIdentity{GatewayID: gatewayID, Region: "us-west"}
	current := operations.GatewayObservation{EventSchemaVersion: operations.CurrentObservationVersion, GatewayID: gatewayID, Region: "us-west", BuildSHA: "abcdef1", DatabaseSchemaVersion: 21,
		RoutingCatalogRevision: 9, AccessProjectionRevision: 12, ExecutionEpochFloor: 3, LastUsageOutboxID: 14,
		StartedAt: now.Add(-time.Hour), ObservedAt: now, Consumers: []operations.ConsumerObservation{}, Backlogs: operations.BacklogSignals{MeteringProjectionStatus: "unavailable"},
		AccessReceipts: []operations.AccessRolloutReceipt{{EventID: "access-event-1", DeliverySequence: 12, AggregateType: "GatewayAPIKey",
			AggregateID: "key-a", AggregateRevision: 2, Status: "applied", OccurredAt: now.Add(-time.Second), ObservedAt: now}}}
	if err := service.RecordGatewayObservation(ctx, identity, current); err != nil {
		t.Fatal(err)
	}
	futureReceipt := current
	futureReceipt.ObservedAt = now.Add(2 * time.Second)
	futureReceipt.RoutingReceipts = []routingcatalog.RolloutReceipt{{PublicationID: "publication-a", GatewayID: gatewayID, Region: identity.Region,
		CatalogRevision: 9, Status: routingcatalog.ReceiptApplied, ObservedAt: futureReceipt.ObservedAt.Add(time.Second)}}
	if err := service.RecordGatewayObservation(ctx, identity, futureReceipt); !errors.Is(err, operations.ErrInvalidArgument) {
		t.Fatalf("future rollout receipt error = %v", err)
	}
	if err := service.RecordGatewayObservation(ctx, identity, current); err != nil {
		t.Fatal(err)
	}
	stale := current
	stale.ObservedAt = now.Add(-time.Second)
	stale.RoutingCatalogRevision = 2
	stale.AccessProjectionRevision = 3
	stale.AccessReceipts = nil
	if err := service.RecordGatewayObservation(ctx, identity, stale); err != nil {
		t.Fatal(err)
	}
	newerButBehind := current
	newerButBehind.ObservedAt = now.Add(time.Second)
	newerButBehind.RoutingCatalogRevision = 1
	newerButBehind.AccessProjectionRevision = 2
	newerButBehind.LastUsageOutboxID = 3
	newerButBehind.AccessReceipts = []operations.AccessRolloutReceipt{{EventID: "access-event-after-restart", DeliverySequence: 13,
		AggregateType: "GatewayAPIKey", AggregateID: "key-a", AggregateRevision: 3, Status: "rejected", ErrorCode: "revision_gap",
		OccurredAt: now, ObservedAt: newerButBehind.ObservedAt}}
	if err := service.RecordGatewayObservation(ctx, identity, newerButBehind); err != nil {
		t.Fatal(err)
	}
	actor := tenantadmin.ActorEnvelope{Type: "human", ID: "operator", Scopes: []string{tenantadmin.ScopePlatformRead}}
	got, err := service.GetGateway(ctx, actor, gatewayID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RoutingCatalogRevision != 9 || got.AccessProjectionRevision != 12 || got.LastUsageOutboxID != 14 || got.HeartbeatStatus != "current" || len(got.AccessReceipts) != 2 {
		t.Fatalf("Gateway summary = %#v", got)
	}
	var accessReceipts int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM operations_access_rollout_receipts WHERE gateway_id=$1 AND event_id='access-event-1'`, gatewayID).Scan(&accessReceipts); err != nil || accessReceipts != 1 {
		t.Fatalf("access receipts/error = %d/%v", accessReceipts, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM operations_access_rollout_receipts WHERE gateway_id=$1 AND event_id='access-event-after-restart'`, gatewayID).Scan(&accessReceipts); err != nil || accessReceipts != 1 {
		t.Fatalf("restart access receipts/error = %d/%v", accessReceipts, err)
	}
	for _, version := range []int{operations.MinimumObservationVersion, operations.CurrentObservationVersion} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			versioned := current
			versioned.EventSchemaVersion = version
			versioned.GatewayID = fmt.Sprintf("%s-v%d", gatewayID, version)
			versioned.ObservedAt = now.Add(time.Duration(version) * time.Millisecond)
			if version < operations.CurrentObservationVersion {
				versioned.AccessReceipts = nil
				versioned.Backlogs.MeteringProjectionStatus = ""
			}
			if err := service.RecordGatewayObservation(ctx, operations.GatewayIdentity{GatewayID: versioned.GatewayID, Region: "us-west"}, versioned); err != nil {
				t.Fatalf("version %d: %v", version, err)
			}
		})
	}
}
