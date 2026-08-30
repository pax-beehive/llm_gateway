//go:build integration

package controlrelay_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/toddzheng/llm-gateway/internal/controlevent"
	"github.com/toddzheng/llm-gateway/internal/controlrelay"
	"github.com/toddzheng/llm-gateway/internal/migrations"
	"github.com/toddzheng/llm-gateway/internal/providerconnection"
	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

func TestPostgresPublisherScopesEventsAndAdvancesPastOtherRegions(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	database, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	for _, migrate := range []func(context.Context, *sql.DB) error{migrations.Migrate, tenantadmin.Migrate, providerconnection.Migrate} {
		if err := migrate(ctx, database); err != nil {
			t.Fatal(err)
		}
	}
	prefix := fmt.Sprintf("relay-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = database.ExecContext(context.Background(), `DELETE FROM control_outbox WHERE event_id LIKE $1`, prefix+"%")
	})
	type inserted struct {
		id        string
		aggregate string
		eventType string
		schema    int
		payload   map[string]any
	}
	events := []inserted{
		{prefix + "-west", "Tenant", "TenantCreated", 2, map[string]any{"home_region": "us-west1"}},
		{prefix + "-east", "GatewayAPIKey", "GatewayAPIKeyIssued", 2, map[string]any{"home_region": "us-east1"}},
		{prefix + "-catalog", "RoutingCatalog", "RoutingCatalogPublished", 1, map[string]any{}},
		{prefix + "-provider-east", "ProviderConnection", "ProviderConnectionEnabled", 3, map[string]any{"region": "us-east1"}},
		{prefix + "-unsupported", "Other", "OtherChanged", 1, map[string]any{}},
	}
	var first, last int64
	for index, event := range events {
		payload, _ := json.Marshal(event.payload)
		var sequence int64
		if err := database.QueryRowContext(ctx, `INSERT INTO control_outbox (
			event_id,schema_version,aggregate_type,aggregate_id,aggregate_revision,tenant_id,event_type,occurred_at,payload
		) VALUES ($1,$2,$3,$4,1,NULL,$5,now(),$6) RETURNING delivery_sequence`,
			event.id, event.schema, event.aggregate, prefix, event.eventType, payload).Scan(&sequence); err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			first = sequence
		}
		last = sequence
	}
	publisher, err := controlrelay.NewPostgresPublisher(database)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := publisher.Publish(ctx, controlevent.Audience{GatewayID: "gateway-a", Region: "us-west1"}, first-1, len(events))
	if err != nil {
		t.Fatal(err)
	}
	if batch.NextCursor != last || batch.SourceHead < batch.NextCursor {
		t.Fatalf("next cursor/source head = %d/%d want cursor %d", batch.NextCursor, batch.SourceHead, last)
	}
	if len(batch.Events) != 2 || batch.Events[0].EventID != prefix+"-west" || batch.Events[1].EventID != prefix+"-catalog" {
		t.Fatalf("events = %#v", batch.Events)
	}
}
