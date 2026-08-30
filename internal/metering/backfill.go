package metering

import (
	"context"
	"encoding/json"
	"fmt"
)

// BackfillGatewayLedger is an explicit, bounded bootstrap from authoritative,
// content-free financial ledgers. It never reads Response input/output fields.
// Repeated calls are safe because each ledger fact has a stable inbox identity.
func (service *Service) BackfillGatewayLedger(ctx context.Context, limit int) (int, error) {
	return service.backfillGatewayLedger(ctx, "", limit)
}

func (service *Service) BackfillTenantLedger(ctx context.Context, tenantID string, limit int) (int, error) {
	if tenantID == "" {
		return 0, ErrInvalidArgument
	}
	return service.backfillGatewayLedger(ctx, tenantID, limit)
}

func (service *Service) backfillGatewayLedger(ctx context.Context, tenantID string, limit int) (int, error) {
	if limit < 1 || limit > 10000 {
		return 0, fmt.Errorf("%w: backfill limit must be 1..10000", ErrInvalidArgument)
	}
	completed := 0
	queries := []string{
		`SELECT 'ledger:response:'||u.id,u.id,u.tenant_id,COALESCE(u.api_key_id,''),u.response_id,u.attempt_id,'','',
		 COALESCE((SELECT item->>'route_id' FROM jsonb_array_elements(COALESCE(r.payload->'attempts','[]'::jsonb)) item WHERE item->>'id'=u.attempt_id LIMIT 1),''),
		 p.provider,COALESCE(NULLIF(r.payload->>'model',''),p.model),p.model,p.region,u.price_snapshot_id,
		 u.input_tokens,u.cached_input_tokens,u.cache_write_input_tokens,u.output_tokens,0,0,u.amount::bigint,u.currency,'committed','', '',u.created_at
		 FROM usage_ledger u JOIN provider_price_snapshots p ON p.id=u.price_snapshot_id
		 LEFT JOIN responses r ON r.tenant_id=u.tenant_id AND r.id=u.response_id
		 WHERE ($2='' OR u.tenant_id=$2) AND NOT EXISTS(SELECT 1 FROM metering_inbox i WHERE i.event_id='ledger:response:'||u.id)
		 ORDER BY u.created_at,u.id LIMIT $1`,
		`SELECT 'ledger:capability:'||u.id,u.id,u.tenant_id,COALESCE(u.api_key_id,''),'','',u.operation_id,u.capability,u.route_id,
		 u.provider,COALESCE(NULLIF(u.public_model,''),u.model),p.model,p.region,u.price_snapshot_id,0,0,0,0,u.input_units,u.documents,u.amount_micros,u.currency,'committed','','',u.created_at
		 FROM capability_usage_ledger u JOIN provider_price_snapshots p ON p.id=u.price_snapshot_id
		 WHERE ($2='' OR u.tenant_id=$2) AND NOT EXISTS(SELECT 1 FROM metering_inbox i WHERE i.event_id='ledger:capability:'||u.id)
		 ORDER BY u.created_at,u.id LIMIT $1`,
		`SELECT 'ledger:refresh:'||u.id,u.id,u.tenant_id,COALESCE(u.api_key_id,''),'','',u.cache_refresh_intent_id,'cache_refresh',l.route_id,
		 l.provider,l.model,p.model,p.region,u.price_snapshot_id,u.input_tokens,u.cached_input_tokens,u.cache_write_input_tokens,u.output_tokens,0,0,u.amount::bigint,u.currency,'committed','','',u.created_at
		 FROM cache_refresh_usage_ledger u JOIN provider_price_snapshots p ON p.id=u.price_snapshot_id
		 JOIN cache_leases l ON l.tenant_id=u.tenant_id AND l.id=u.cache_lease_id
		 WHERE ($2='' OR u.tenant_id=$2) AND NOT EXISTS(SELECT 1 FROM metering_inbox i WHERE i.event_id='ledger:refresh:'||u.id)
		 ORDER BY u.created_at,u.id LIMIT $1`,
	}
	for _, query := range queries {
		events, err := service.readBackfillEvents(ctx, query, tenantID, limit-completed)
		if err != nil {
			return completed, err
		}
		for _, event := range events {
			if err := service.insertBackfill(ctx, event); err != nil {
				return completed, err
			}
			completed++
			if completed >= limit {
				return completed, nil
			}
		}
	}
	return completed, nil
}

func (service *Service) readBackfillEvents(ctx context.Context, query, tenantID string, limit int) ([]UsageEvent, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := service.database.QueryContext(ctx, query, limit, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []UsageEvent
	for rows.Next() {
		var event UsageEvent
		if err := rows.Scan(&event.EventID, &event.UsageID, &event.TenantID, &event.APIKeyID, &event.ResponseID, &event.AttemptID, &event.OperationID, &event.Capability, &event.RouteID, &event.Provider, &event.PublicModel, &event.ProviderModel, &event.Region, &event.PriceSnapshotID, &event.InputTokens, &event.CachedInputTokens, &event.CacheWriteInputTokens, &event.OutputTokens, &event.InputUnits, &event.Documents, &event.AmountMicros, &event.Currency, &event.Outcome, &event.CorrectsEventID, &event.Reason, &event.OccurredAt); err != nil {
			return nil, err
		}
		event.SchemaVersion = CurrentEventSchemaVersion
		switch {
		case event.ResponseID != "":
			event.Type = EventUsageRecorded
		case event.Capability == "cache_refresh":
			event.Type = EventCacheRefreshRecorded
		default:
			event.Type = EventCapabilityUsageRecorded
		}
		result = append(result, event)
	}
	return result, rows.Err()
}

func (service *Service) insertBackfill(ctx context.Context, event UsageEvent) error {
	if err := event.Validate(); err != nil {
		return err
	}
	encoded, err := jsonMarshal(event)
	if err != nil {
		return err
	}
	tx, err := service.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `INSERT INTO metering_inbox(event_id,schema_version,event_type,tenant_id,occurred_at,payload) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(event_id) DO NOTHING`, event.EventID, event.SchemaVersion, event.Type, event.TenantID, event.OccurredAt, encoded)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows == 1 {
		inserted, err := insertFact(ctx, tx, event)
		if err != nil {
			return err
		}
		if inserted {
			if err := applyActiveProjection(ctx, tx, event); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func jsonMarshal(value any) ([]byte, error) { return json.Marshal(value) }
