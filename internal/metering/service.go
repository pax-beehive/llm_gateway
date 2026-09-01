package metering

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/toddzheng/llm-gateway/internal/quota"
)

type Service struct {
	database *sql.DB
	now      func() time.Time
}

func NewService(database *sql.DB, now func() time.Time) (*Service, error) {
	if database == nil {
		return nil, errors.New("Metering requires PostgreSQL")
	}
	if now == nil {
		now = time.Now
	}
	return &Service{database: database, now: now}, nil
}

type claimedEvent struct {
	OutboxID int64
	EventID  string
	Payload  []byte
	Created  time.Time
}

// ConsumeOutboxBatch uses durable leases so multiple Metering workers may share
// the source. Inbox insertion, projection effects, and source acknowledgement
// commit together while the processes share PostgreSQL; event identity remains
// stable for a future external at-least-once transport.
func (service *Service) ConsumeOutboxBatch(ctx context.Context, workerID string, limit int, lease time.Duration) (int, error) {
	if workerID == "" || limit < 1 || limit > 1000 || lease <= 0 || lease > 5*time.Minute {
		return 0, fmt.Errorf("%w: invalid relay claim", ErrInvalidArgument)
	}
	claimed, err := service.claim(ctx, workerID, limit, lease)
	if err != nil {
		return 0, err
	}
	completed := 0
	var firstError error
	for _, event := range claimed {
		if err := service.consumeOne(ctx, workerID, event); err != nil {
			code := stableError(err)
			_, _ = service.database.ExecContext(ctx, `UPDATE metering_outbox_claims
				SET lease_expires_at=CASE WHEN $4 THEN 'infinity'::timestamptz ELSE now() END,
				error_code=$2,poisoned=$4,updated_at=now() WHERE source_outbox_id=$1 AND worker_id=$3`,
				event.OutboxID, code, workerID, code == "invalid_event")
			if code != "invalid_event" && firstError == nil {
				firstError = err
			}
			continue
		}
		completed++
	}
	return completed, firstError
}

func (service *Service) claim(ctx context.Context, workerID string, limit int, lease time.Duration) ([]claimedEvent, error) {
	tx, err := service.database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `
		SELECT o.id,o.payload,o.created_at
		FROM transactional_outbox o
		LEFT JOIN metering_outbox_claims c ON c.source_outbox_id=o.id AND (c.lease_expires_at>now() OR c.poisoned)
		WHERE o.published_at IS NULL AND c.source_outbox_id IS NULL
		  AND o.event_type IN ('usage.recorded','capability.usage_recorded','cache_refresh.usage_recorded','quota.denied')
		ORDER BY o.id FOR UPDATE OF o SKIP LOCKED LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	var result []claimedEvent
	for rows.Next() {
		var event claimedEvent
		if err := rows.Scan(&event.OutboxID, &event.Payload, &event.Created); err != nil {
			_ = rows.Close()
			return nil, err
		}
		event.EventID = fmt.Sprintf("gateway-outbox:%d", event.OutboxID)
		result = append(result, event)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for _, event := range result {
		if _, err := tx.ExecContext(ctx, `INSERT INTO metering_outbox_claims(source_outbox_id,worker_id,lease_expires_at)
			VALUES($1,$2,now()+$3::interval)
			ON CONFLICT(source_outbox_id) DO UPDATE SET worker_id=EXCLUDED.worker_id,
			lease_expires_at=EXCLUDED.lease_expires_at,attempt_count=metering_outbox_claims.attempt_count+1,
			error_code=NULL,updated_at=now()`, event.OutboxID, workerID, intervalLiteral(lease)); err != nil {
			return nil, err
		}
	}
	return result, tx.Commit()
}

func intervalLiteral(duration time.Duration) string {
	return fmt.Sprintf("%f seconds", duration.Seconds())
}

func (service *Service) consumeOne(ctx context.Context, workerID string, claimed claimedEvent) error {
	tx, err := service.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var payload []byte
	var publishedAt sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT payload,published_at FROM transactional_outbox WHERE id=$1 FOR UPDATE`, claimed.OutboxID).Scan(&payload, &publishedAt); err != nil {
		return err
	}
	if publishedAt.Valid {
		_, err := tx.ExecContext(ctx, `DELETE FROM metering_outbox_claims WHERE source_outbox_id=$1 AND worker_id=$2`, claimed.OutboxID, workerID)
		if err != nil {
			return err
		}
		return tx.Commit()
	}
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return fmt.Errorf("decode Metering event envelope: %w", err)
	}
	if envelope.Type == quota.DenialEventType {
		return service.consumeQuotaDenial(ctx, tx, workerID, claimed, payload)
	}
	var event UsageEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return fmt.Errorf("decode Metering event: %w", err)
	}
	if event.EventID == "" {
		event.EventID = claimed.EventID
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = claimed.Created.UTC()
	}
	if err := event.Validate(); err != nil {
		return fmt.Errorf("validate Metering event: %w", err)
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return err
	}
	insert, err := tx.ExecContext(ctx, `INSERT INTO metering_inbox(
		event_id,source_outbox_id,schema_version,event_type,tenant_id,occurred_at,payload)
		VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(event_id) DO NOTHING`,
		event.EventID, claimed.OutboxID, event.SchemaVersion, event.Type, event.TenantID, event.OccurredAt, encoded)
	if err != nil {
		return err
	}
	rows, err := insert.RowsAffected()
	if err != nil {
		return err
	}
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
	return finishClaim(ctx, tx, workerID, claimed.OutboxID)
}

func (service *Service) consumeQuotaDenial(ctx context.Context, tx *sql.Tx, workerID string, claimed claimedEvent, payload []byte) error {
	var event quota.DenialEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return fmt.Errorf("decode quota denial event: %w", err)
	}
	if event.EventID == "" {
		event.EventID = claimed.EventID
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = claimed.Created.UTC()
	}
	if err := event.Validate(); err != nil {
		return fmt.Errorf("validate quota denial event: %w", err)
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return err
	}
	insert, err := tx.ExecContext(ctx, `INSERT INTO metering_inbox(
		event_id,source_outbox_id,schema_version,event_type,tenant_id,occurred_at,payload)
		VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(event_id) DO NOTHING`,
		event.EventID, claimed.OutboxID, event.SchemaVersion, event.Type, event.TenantID, event.OccurredAt, encoded)
	if err != nil {
		return err
	}
	rows, err := insert.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 1 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO metering_quota_denials(
			event_id,tenant_id,api_key_id,response_id,attempt_id,operation_id,capability,public_model,route_id,region,
			denial_scope,dimension,currency,tenant_policy_revision,api_key_policy_revision,occurred_at)
			VALUES($1,$2,NULLIF($3,''),NULLIF($4,''),NULLIF($5,''),NULLIF($6,''),NULLIF($7,''),NULLIF($8,''),
			NULLIF($9,''),NULLIF($10,''),$11,$12,NULLIF($13,''),NULLIF($14,0),NULLIF($15,0),$16)`,
			event.EventID, event.TenantID, event.APIKeyID, event.ResponseID, event.AttemptID, event.OperationID,
			event.Capability, event.PublicModel, event.RouteID, event.Region, event.Scope, event.Dimension,
			event.Currency, event.TenantPolicyRevision, event.APIKeyPolicyRevision, event.OccurredAt); err != nil {
			return err
		}
	}
	return finishClaim(ctx, tx, workerID, claimed.OutboxID)
}

func finishClaim(ctx context.Context, tx *sql.Tx, workerID string, outboxID int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE transactional_outbox SET published_at=COALESCE(published_at,now()) WHERE id=$1`, outboxID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM metering_outbox_claims WHERE source_outbox_id=$1 AND worker_id=$2`, outboxID, workerID); err != nil {
		return err
	}
	return tx.Commit()
}

func insertFact(ctx context.Context, tx *sql.Tx, event UsageEvent) (bool, error) {
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "metering-usage\x1f"+event.TenantID+"\x1f"+event.UsageID); err != nil {
		return false, err
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM metering_usage_facts WHERE tenant_id=$1 AND usage_id=$2)`, event.TenantID, event.UsageID).Scan(&exists); err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO metering_usage_facts(
		event_id,usage_id,tenant_id,api_key_id,response_id,attempt_id,operation_id,capability,route_id,
		provider,public_model,provider_model,region,price_snapshot_id,input_tokens,cached_input_tokens,
		cache_write_input_tokens,output_tokens,input_units,documents,amount_micros,currency,outcome,
		corrects_event_id,correction_actor_id,reason,occurred_at)
		VALUES($1,$2,$3,NULLIF($4,''),NULLIF($5,''),NULLIF($6,''),NULLIF($7,''),NULLIF($8,''),NULLIF($9,''),
		$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,NULLIF($24,''),NULLIF($25,''),NULLIF($26,''),$27)`,
		event.EventID, event.UsageID, event.TenantID, event.APIKeyID, event.ResponseID, event.AttemptID,
		event.OperationID, event.Capability, event.RouteID, event.Provider, event.PublicModel, event.ProviderModel,
		event.Region, event.PriceSnapshotID, event.InputTokens, event.CachedInputTokens,
		event.CacheWriteInputTokens, event.OutputTokens, event.InputUnits, event.Documents,
		event.AmountMicros, event.Currency, event.Outcome, event.CorrectsEventID, event.CorrectionActorID, event.Reason, event.OccurredAt)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

func applyActiveProjection(ctx context.Context, tx *sql.Tx, event UsageEvent) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO metering_usage_daily(
		generation_id,usage_date,tenant_id,api_key_id,provider,public_model,provider_model,route_id,outcome,currency,
		operation_count,input_tokens,cached_input_tokens,cache_write_input_tokens,output_tokens,input_units,documents,amount_micros)
		SELECT id,($1 AT TIME ZONE 'UTC')::date,$2,$3,$4,$5,$6,$7,$8,$9,1,$10,$11,$12,$13,$14,$15,$16
		FROM metering_projection_generations WHERE status='active'
		ON CONFLICT(generation_id,usage_date,tenant_id,api_key_id,provider,public_model,provider_model,route_id,outcome,currency)
		DO UPDATE SET operation_count=metering_usage_daily.operation_count+1,
		input_tokens=metering_usage_daily.input_tokens+EXCLUDED.input_tokens,
		cached_input_tokens=metering_usage_daily.cached_input_tokens+EXCLUDED.cached_input_tokens,
		cache_write_input_tokens=metering_usage_daily.cache_write_input_tokens+EXCLUDED.cache_write_input_tokens,
		output_tokens=metering_usage_daily.output_tokens+EXCLUDED.output_tokens,
		input_units=metering_usage_daily.input_units+EXCLUDED.input_units,
		documents=metering_usage_daily.documents+EXCLUDED.documents,
		amount_micros=metering_usage_daily.amount_micros+EXCLUDED.amount_micros`,
		event.OccurredAt, event.TenantID, event.APIKeyID, event.Provider, event.PublicModel,
		event.ProviderModel, event.RouteID, event.Outcome, event.Currency, event.InputTokens,
		event.CachedInputTokens, event.CacheWriteInputTokens, event.OutputTokens, event.InputUnits,
		event.Documents, event.AmountMicros)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE metering_projection_generations SET source_cutoff=GREATEST(source_cutoff,$1),completed_at=now() WHERE status='active'`, event.OccurredAt)
	return err
}

func (service *Service) Summary(ctx context.Context, filter Filter) (Summary, error) {
	where, args, err := filterSQL(filter, "", 1)
	if err != nil {
		return Summary{}, err
	}
	rows, err := service.database.QueryContext(ctx, `SELECT currency,count(*),COALESCE(sum(input_tokens),0),
		COALESCE(sum(cached_input_tokens),0),COALESCE(sum(cache_write_input_tokens),0),COALESCE(sum(output_tokens),0),
		COALESCE(sum(input_units),0),COALESCE(sum(documents),0),COALESCE(sum(amount_micros),0)
		FROM metering_usage_facts WHERE `+where+` GROUP BY currency ORDER BY currency`, args...)
	if err != nil {
		return Summary{}, err
	}
	defer rows.Close()
	result := Summary{Totals: []Totals{}}
	for rows.Next() {
		var total Totals
		if err := rows.Scan(&total.Currency, &total.OperationCount, &total.InputTokens, &total.CachedInputTokens,
			&total.CacheWriteInputTokens, &total.OutputTokens, &total.InputUnits, &total.Documents, &total.AmountMicros); err != nil {
			return Summary{}, err
		}
		result.Totals = append(result.Totals, total)
	}
	if err := rows.Err(); err != nil {
		return Summary{}, err
	}
	_ = service.database.QueryRowContext(ctx, `SELECT COALESCE(max(occurred_at),'epoch') FROM metering_inbox`).Scan(&result.DataCutoff)
	return result, nil
}

func (service *Service) TimeSeries(ctx context.Context, filter Filter, granularity string) ([]TimePoint, error) {
	if granularity != "hour" && granularity != "day" {
		return nil, fmt.Errorf("%w: granularity must be hour or day", ErrInvalidArgument)
	}
	if filter.From.IsZero() || filter.Through.IsZero() || filter.Through.Before(filter.From) ||
		(granularity == "hour" && filter.Through.Sub(filter.From) > 31*24*time.Hour) ||
		(granularity == "day" && filter.Through.Sub(filter.From) > 366*24*time.Hour) {
		return nil, fmt.Errorf("%w: time range is missing, reversed, or too large", ErrInvalidArgument)
	}
	where, args, err := filterSQL(filter, "", 2)
	if err != nil {
		return nil, err
	}
	rows, err := service.database.QueryContext(ctx, `SELECT date_trunc($1,occurred_at),currency,count(*),
		COALESCE(sum(input_tokens),0),COALESCE(sum(cached_input_tokens),0),COALESCE(sum(cache_write_input_tokens),0),
		COALESCE(sum(output_tokens),0),COALESCE(sum(input_units),0),COALESCE(sum(documents),0),COALESCE(sum(amount_micros),0)
		FROM metering_usage_facts WHERE `+where+` GROUP BY 1,currency ORDER BY 1,currency`, append([]any{granularity}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []TimePoint
	for rows.Next() {
		var point TimePoint
		if err := rows.Scan(&point.Start, &point.Totals.Currency, &point.Totals.OperationCount,
			&point.Totals.InputTokens, &point.Totals.CachedInputTokens, &point.Totals.CacheWriteInputTokens,
			&point.Totals.OutputTokens, &point.Totals.InputUnits, &point.Totals.Documents, &point.Totals.AmountMicros); err != nil {
			return nil, err
		}
		result = append(result, point)
	}
	return result, rows.Err()
}

func (service *Service) Events(ctx context.Context, filter Filter, cursor string, limit int) (EventPage, error) {
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 200 {
		return EventPage{}, fmt.Errorf("%w: limit must be 1..200", ErrInvalidArgument)
	}
	where, args, err := filterSQL(filter, "f.", 1)
	if err != nil {
		return EventPage{}, err
	}
	if cursor != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil {
			return EventPage{}, fmt.Errorf("%w: invalid cursor", ErrInvalidArgument)
		}
		parts := strings.SplitN(string(decoded), "\x1f", 2)
		if len(parts) != 2 {
			return EventPage{}, fmt.Errorf("%w: invalid cursor", ErrInvalidArgument)
		}
		at, err := time.Parse(time.RFC3339Nano, parts[0])
		if err != nil {
			return EventPage{}, fmt.Errorf("%w: invalid cursor", ErrInvalidArgument)
		}
		args = append(args, at, parts[1])
		where += fmt.Sprintf(" AND (f.occurred_at,f.event_id) > ($%d,$%d)", len(args)-1, len(args))
	}
	args = append(args, limit+1)
	rows, err := service.database.QueryContext(ctx, `SELECT i.payload FROM metering_inbox i JOIN metering_usage_facts f USING(event_id)
		WHERE `+where+fmt.Sprintf(" ORDER BY f.occurred_at,f.event_id LIMIT $%d", len(args)), args...)
	if err != nil {
		return EventPage{}, err
	}
	defer rows.Close()
	page := EventPage{Data: []UsageEvent{}}
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return EventPage{}, err
		}
		var event UsageEvent
		if err := json.Unmarshal(payload, &event); err != nil {
			return EventPage{}, err
		}
		page.Data = append(page.Data, event)
	}
	if len(page.Data) > limit {
		page.Data = page.Data[:limit]
		last := page.Data[len(page.Data)-1]
		page.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(last.OccurredAt.Format(time.RFC3339Nano) + "\x1f" + last.EventID))
	}
	_ = service.database.QueryRowContext(ctx, `SELECT COALESCE(max(occurred_at),'epoch') FROM metering_inbox`).Scan(&page.DataCutoff)
	return page, rows.Err()
}

func filterSQL(filter Filter, prefix string, firstPlaceholder int) (string, []any, error) {
	if filter.TenantID == "" && !filter.AllTenants {
		return "", nil, fmt.Errorf("%w: Tenant scope is required", ErrInvalidArgument)
	}
	if !filter.From.IsZero() && !filter.Through.IsZero() && filter.Through.Before(filter.From) {
		return "", nil, fmt.Errorf("%w: invalid time range", ErrInvalidArgument)
	}
	where := []string{"TRUE"}
	args := []any{}
	if filter.TenantID != "" {
		args = append(args, filter.TenantID)
		where = append(where, fmt.Sprintf("%stenant_id=$%d", prefix, firstPlaceholder+len(args)-1))
	}
	add := func(column, value string) {
		if value != "" {
			args = append(args, value)
			where = append(where, fmt.Sprintf("%s%s=$%d", prefix, column, firstPlaceholder+len(args)-1))
		}
	}
	add("api_key_id", filter.APIKeyID)
	add("response_id", filter.ResponseID)
	add("provider", filter.Provider)
	add("public_model", filter.PublicModel)
	add("provider_model", filter.ProviderModel)
	add("route_id", filter.RouteID)
	add("outcome", filter.Outcome)
	add("currency", filter.Currency)
	if !filter.From.IsZero() {
		args = append(args, filter.From.UTC())
		where = append(where, fmt.Sprintf("%soccurred_at >= $%d", prefix, firstPlaceholder+len(args)-1))
	}
	if !filter.Through.IsZero() {
		args = append(args, filter.Through.UTC())
		where = append(where, fmt.Sprintf("%soccurred_at <= $%d", prefix, firstPlaceholder+len(args)-1))
	}
	return strings.Join(where, " AND "), args, nil
}

func (service *Service) Correct(ctx context.Context, actorID, reason, idempotencyKey, targetEventID string, delta UsageEvent) (UsageEvent, error) {
	if actorID == "" || reason == "" || idempotencyKey == "" || targetEventID == "" {
		return UsageEvent{}, fmt.Errorf("%w: correction requires actor, reason, idempotency, and target", ErrInvalidArgument)
	}
	tx, err := service.database.BeginTx(ctx, nil)
	if err != nil {
		return UsageEvent{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var original UsageEvent
	var payload []byte
	if err := tx.QueryRowContext(ctx, `SELECT payload FROM metering_inbox WHERE event_id=$1`, targetEventID).Scan(&payload); errors.Is(err, sql.ErrNoRows) {
		return UsageEvent{}, ErrNotFound
	} else if err != nil {
		return UsageEvent{}, err
	}
	if err := json.Unmarshal(payload, &original); err != nil {
		return UsageEvent{}, err
	}
	id := "correction:" + original.TenantID + ":" + idempotencyKey
	delta.EventID = id
	delta.UsageID = id
	delta.SchemaVersion = CurrentEventSchemaVersion
	delta.Type = EventUsageCorrected
	delta.TenantID = original.TenantID
	delta.APIKeyID = original.APIKeyID
	delta.ResponseID = original.ResponseID
	delta.AttemptID = original.AttemptID
	delta.OperationID = original.OperationID
	delta.Capability = original.Capability
	delta.RouteID = original.RouteID
	delta.Provider = original.Provider
	delta.PublicModel = original.PublicModel
	delta.ProviderModel = original.ProviderModel
	delta.Region = original.Region
	delta.PriceSnapshotID = original.PriceSnapshotID
	delta.Currency = original.Currency
	delta.Outcome = original.Outcome
	delta.CorrectsEventID = targetEventID
	delta.CorrectionActorID = actorID
	delta.Reason = reason
	delta.OccurredAt = service.now().UTC()
	if err := delta.Validate(); err != nil {
		return UsageEvent{}, err
	}
	encoded, _ := json.Marshal(delta)
	result, err := tx.ExecContext(ctx, `INSERT INTO metering_inbox(event_id,schema_version,event_type,tenant_id,occurred_at,payload)
		VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(event_id) DO NOTHING`, id, delta.SchemaVersion, delta.Type, delta.TenantID, delta.OccurredAt, encoded)
	if err != nil {
		return UsageEvent{}, err
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		var existing []byte
		if err := tx.QueryRowContext(ctx, `SELECT payload FROM metering_inbox WHERE event_id=$1`, id).Scan(&existing); err != nil {
			return UsageEvent{}, err
		}
		var replay UsageEvent
		_ = json.Unmarshal(existing, &replay)
		return replay, tx.Commit()
	}
	inserted, err := insertFact(ctx, tx, delta)
	if err != nil {
		return UsageEvent{}, err
	}
	if !inserted {
		return UsageEvent{}, errors.New("Metering correction identity conflict")
	}
	if err := applyActiveProjection(ctx, tx, delta); err != nil {
		return UsageEvent{}, err
	}
	if err := tx.Commit(); err != nil {
		return UsageEvent{}, err
	}
	return delta, nil
}

func (service *Service) Rebuild(ctx context.Context) (int64, error) {
	tx, err := service.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('metering-projection-rebuild',0))`); err != nil {
		return 0, err
	}
	var cutoff time.Time
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(max(occurred_at),'epoch') FROM metering_usage_facts`).Scan(&cutoff); err != nil {
		return 0, err
	}
	var generation int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO metering_projection_generations(status,source_cutoff) VALUES('building',$1) RETURNING id`, cutoff).Scan(&generation); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO metering_usage_daily(generation_id,usage_date,tenant_id,api_key_id,provider,public_model,provider_model,route_id,outcome,currency,
		operation_count,input_tokens,cached_input_tokens,cache_write_input_tokens,output_tokens,input_units,documents,amount_micros)
		SELECT $1,(occurred_at AT TIME ZONE 'UTC')::date,tenant_id,COALESCE(api_key_id,''),provider,public_model,provider_model,COALESCE(route_id,''),outcome,currency,
		count(*),sum(input_tokens),sum(cached_input_tokens),sum(cache_write_input_tokens),sum(output_tokens),sum(input_units),sum(documents),sum(amount_micros)
		FROM metering_usage_facts WHERE occurred_at<=$2 GROUP BY 2,3,4,5,6,7,8,9,10`, generation, cutoff); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE metering_projection_generations SET status='superseded' WHERE status='active'`); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE metering_projection_generations SET status='active',completed_at=now() WHERE id=$1 AND status='building'`, generation); err != nil {
		return 0, err
	}
	return generation, tx.Commit()
}

func (service *Service) Status(ctx context.Context) (Status, error) {
	var result Status
	if err := service.database.QueryRowContext(ctx, `SELECT id,source_cutoff FROM metering_projection_generations WHERE status='active'`).Scan(&result.ProjectionGeneration, &result.ProjectionCutoff); err != nil {
		return Status{}, err
	}
	if err := service.database.QueryRowContext(ctx, `SELECT count(*),min(created_at) FROM transactional_outbox WHERE published_at IS NULL AND event_type IN ('usage.recorded','capability.usage_recorded','cache_refresh.usage_recorded','quota.denied')`).Scan(&result.PendingEvents, &result.OldestPendingAt); err != nil {
		return Status{}, err
	}
	if err := service.database.QueryRowContext(ctx, `SELECT count(*) FROM metering_outbox_claims WHERE poisoned`).Scan(&result.PoisonEvents); err != nil {
		return Status{}, err
	}
	if err := service.database.QueryRowContext(ctx, `SELECT count(*) FROM metering_exports WHERE status IN ('queued','running')`).Scan(&result.QueuedExports); err != nil {
		return Status{}, err
	}
	return result, nil
}

func stableError(err error) string {
	if errors.Is(err, ErrInvalidArgument) {
		return "invalid_event"
	}
	if strings.Contains(err.Error(), "decode Metering event") || strings.Contains(err.Error(), "validate Metering event") {
		return "invalid_event"
	}
	return "consumer_failed"
}

func newID(prefix string) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return prefix + "_" + hex.EncodeToString(raw[:]), nil
}
