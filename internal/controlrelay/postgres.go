package controlrelay

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/toddzheng/llm-gateway/internal/controlevent"
)

type PostgresPublisher struct {
	database *sql.DB
}

func NewPostgresPublisher(database *sql.DB) (*PostgresPublisher, error) {
	if database == nil {
		return nil, errors.New("PostgreSQL Control Event publisher requires a database")
	}
	return &PostgresPublisher{database: database}, nil
}

func (publisher *PostgresPublisher) Publish(ctx context.Context, audience controlevent.Audience, after int64, limit int) (controlevent.Batch, error) {
	if strings.TrimSpace(audience.GatewayID) == "" || strings.TrimSpace(audience.Region) == "" || after < 0 || limit < 1 || limit > 256 {
		return controlevent.Batch{}, errors.New("invalid Control Event publication request")
	}
	var sourceHead int64
	if err := publisher.database.QueryRowContext(ctx, `SELECT GREATEST(COALESCE(max(delivery_sequence),0),$1) FROM control_outbox`, after).Scan(&sourceHead); err != nil {
		return controlevent.Batch{}, fmt.Errorf("read Control Event source head: %w", err)
	}
	rows, err := publisher.database.QueryContext(ctx, `WITH event_window AS (
		SELECT event_id,delivery_sequence,schema_version,aggregate_type,aggregate_id,aggregate_revision,
		       COALESCE(tenant_id,'') AS tenant_id,event_type,occurred_at,payload
		FROM control_outbox WHERE delivery_sequence>$1 AND delivery_sequence<=$4 ORDER BY delivery_sequence LIMIT $2
	)
	SELECT event_id,delivery_sequence,schema_version,aggregate_type,aggregate_id,aggregate_revision,
	       COALESCE(tenant_id,''),event_type,occurred_at,payload,
	       CASE
		         WHEN aggregate_type IN ('Tenant','GatewayAPIKey') AND schema_version=2
		           THEN COALESCE(payload->>'home_region'=$3,false)
	         WHEN aggregate_type='RoutingCatalog' AND event_type='RoutingCatalogPublished' AND schema_version=1
	           THEN true
		 WHEN aggregate_type='ProviderConnection' AND schema_version=3
		   AND event_type IN ('ProviderConnectionRegistered','ProviderConnectionChanged','ProviderConnectionEnabled',
		     'ProviderConnectionDisabled','ProviderCredentialRotated','ProviderConnectionExecutionProjected')
		           THEN COALESCE(payload->>'region'=$3,false) OR COALESCE(payload->>'previous_region'=$3,false)
	         WHEN aggregate_type='ProviderOperation' AND event_type='ProviderConnectionHealthObserved' AND schema_version=2
	           THEN EXISTS (SELECT 1 FROM provider_connections c WHERE c.id=payload->>'connection_id' AND c.region=$3)
	         ELSE false
	       END AS eligible
	FROM event_window ORDER BY delivery_sequence`, after, limit, audience.Region, sourceHead)
	if err != nil {
		return controlevent.Batch{}, fmt.Errorf("read Control Event publication window: %w", err)
	}
	defer rows.Close()
	batch := controlevent.Batch{Events: []controlevent.Event{}, NextCursor: after, SourceHead: sourceHead}
	for rows.Next() {
		var event controlevent.Event
		var eligible bool
		if err := rows.Scan(&event.EventID, &event.DeliverySequence, &event.SchemaVersion, &event.AggregateType,
			&event.AggregateID, &event.AggregateRevision, &event.TenantID, &event.EventType, &event.OccurredAt,
			&event.Payload, &eligible); err != nil {
			return controlevent.Batch{}, err
		}
		batch.NextCursor = event.DeliverySequence
		if eligible {
			batch.Events = append(batch.Events, event)
		}
	}
	if err := rows.Err(); err != nil {
		return controlevent.Batch{}, err
	}
	return batch, nil
}
