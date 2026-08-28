package accessprojection

import (
	"context"
	"errors"
	"fmt"
)

type BatchResult struct {
	Scanned    int
	Applied    int
	Duplicates int
	Stale      int
	Gaps       int
}

// ConsumeControlOutboxBatch is the initial shared-PostgreSQL transport adapter.
// It reads immutable control events and applies them through the same inbox and
// revision checks used by a future external transport consumer.
func (store *Store) ConsumeControlOutboxBatch(ctx context.Context, limit int) (BatchResult, error) {
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 1000 {
		return BatchResult{}, fmt.Errorf("batch limit must be between 1 and 1000")
	}
	rows, err := store.database.QueryContext(ctx, `
		SELECT event_id, schema_version, aggregate_type, aggregate_id, aggregate_revision,
		       tenant_id, event_type, occurred_at, payload
		FROM control_outbox
		WHERE aggregate_type IN ('Tenant','GatewayAPIKey') AND schema_version = 2
		  AND NOT EXISTS (SELECT 1 FROM gateway_access_inbox i WHERE i.event_id = control_outbox.event_id)
		ORDER BY occurred_at, event_id
		LIMIT $1`, limit)
	if err != nil {
		return BatchResult{}, err
	}
	events := make([]ControlEvent, 0, limit)
	for rows.Next() {
		var event ControlEvent
		if err := rows.Scan(
			&event.EventID, &event.SchemaVersion, &event.AggregateType, &event.AggregateID,
			&event.AggregateRevision, &event.TenantID, &event.EventType, &event.OccurredAt, &event.Payload,
		); err != nil {
			_ = rows.Close()
			return BatchResult{}, err
		}
		events = append(events, event)
	}
	if err := rows.Close(); err != nil {
		return BatchResult{}, err
	}
	result := BatchResult{Scanned: len(events)}
	for _, event := range events {
		applied, err := store.Apply(ctx, event)
		switch applied.Disposition {
		case DispositionApplied:
			result.Applied++
		case DispositionDuplicate:
			result.Duplicates++
		case DispositionStale:
			result.Stale++
		case DispositionGap:
			result.Gaps++
		}
		if err != nil && !errors.Is(err, ErrRevisionGap) {
			return result, err
		}
	}
	return result, nil
}
