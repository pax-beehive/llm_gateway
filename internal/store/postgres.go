package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/quota"
)

type PostgresResponseStore struct {
	db *sql.DB
}

func NewPostgresResponseStore(db *sql.DB) *PostgresResponseStore {
	return &PostgresResponseStore{db: db}
}

func (s *PostgresResponseStore) RecordCapabilityUsage(ctx context.Context, usage core.CapabilityUsageRecord) error {
	if usage.ID == "" || usage.TenantID == "" || usage.APIKeyID == "" || usage.OperationID == "" ||
		usage.HomeRegion == "" || usage.ExecutionEpoch <= 0 || usage.RouteID == "" ||
		usage.Provider == "" || usage.Model == "" || usage.Currency == "" {
		return errors.New("capability usage requires complete identity and attribution")
	}
	if usage.Capability != core.CapabilityEmbeddings && usage.Capability != core.CapabilityModeration && usage.Capability != core.CapabilityRerank {
		return errors.New("capability usage has an unsupported capability")
	}
	if usage.InputUnits < 0 || usage.Dimensions < 0 || usage.Documents < 0 || usage.AmountMicros < 0 || usage.CreatedAt.IsZero() {
		return errors.New("capability usage amounts and creation time are invalid")
	}
	if usage.PriceSnapshot.Provider != usage.Provider || usage.PriceSnapshot.Currency != usage.Currency {
		return errors.New("capability usage price snapshot does not match attribution")
	}
	providerUsage := usage.ProviderUsage
	if len(providerUsage) == 0 {
		providerUsage = json.RawMessage(`{}`)
	}
	if err := core.ValidateCapabilityProviderUsage(providerUsage); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := assertTenantWriter(ctx, tx, usage.TenantID, usage.HomeRegion, usage.ExecutionEpoch); err != nil {
		return err
	}
	if err := ensurePriceSnapshot(ctx, tx, usage.PriceSnapshot); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO capability_usage_ledger (
			id, tenant_id, api_key_id, operation_id, home_region, execution_epoch, quota_reservation_id, capability,
			route_id, provider, model, price_snapshot_id, provider_usage,
			input_units, dimensions, documents, amount_micros, currency, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`,
		usage.ID, usage.TenantID, usage.APIKeyID, usage.OperationID, usage.HomeRegion, usage.ExecutionEpoch,
		usage.QuotaReservationID, usage.Capability,
		usage.RouteID, usage.Provider, usage.Model, usage.PriceSnapshot.ID, providerUsage,
		usage.InputUnits, usage.Dimensions, usage.Documents, usage.AmountMicros, usage.Currency, usage.CreatedAt,
	); err != nil {
		return fmt.Errorf("insert immutable capability usage: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO capability_usage_daily (
			usage_date, tenant_id, api_key_id, capability, provider, model, currency,
			operation_count, input_units, documents, amount_micros
		) VALUES (($1 AT TIME ZONE 'UTC')::date,$2,$3,$4,$5,$6,$7,1,$8,$9,$10)
		ON CONFLICT (usage_date, tenant_id, api_key_id, capability, provider, model, currency) DO UPDATE SET
			operation_count = capability_usage_daily.operation_count + 1,
			input_units = capability_usage_daily.input_units + EXCLUDED.input_units,
			documents = capability_usage_daily.documents + EXCLUDED.documents,
			amount_micros = capability_usage_daily.amount_micros + EXCLUDED.amount_micros,
			updated_at = now()`, usage.CreatedAt, usage.TenantID, usage.APIKeyID, usage.Capability,
		usage.Provider, usage.Model, usage.Currency, usage.InputUnits, usage.Documents, usage.AmountMicros,
	); err != nil {
		return fmt.Errorf("update capability usage projection: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO transactional_outbox (
			tenant_id, aggregate_type, aggregate_id, aggregate_revision, event_type, payload
		) VALUES ($1,'capability_usage',$2,1,'capability.usage_recorded',jsonb_build_object(
			'operation_id',$2::text,'api_key_id',$3::text,'capability',$4::text,'provider',$5::text,'model',$6::text,
			'input_units',$7::bigint,'dimensions',$8::bigint,'documents',$9::bigint,'amount_micros',$10::bigint,'currency',$11::text
		))`, usage.TenantID, usage.OperationID, usage.APIKeyID, usage.Capability, usage.Provider, usage.Model,
		usage.InputUnits, usage.Dimensions, usage.Documents, usage.AmountMicros, usage.Currency,
	); err != nil {
		return fmt.Errorf("record capability usage event: %w", err)
	}
	return tx.Commit()
}

func (s *PostgresResponseStore) AssertCapabilityWriter(ctx context.Context, tenantID, homeRegion string, executionEpoch int64) error {
	var exists bool
	if err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM tenants
			WHERE id = $1 AND home_region = $2 AND execution_epoch = $3
		)`, tenantID, homeRegion, executionEpoch).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%w: tenant home-region writer fencing conflict", ErrConflict)
	}
	return nil
}

func (s *PostgresResponseStore) ExecuteWithCapabilityWriterFence(
	ctx context.Context,
	tenantID string,
	homeRegion string,
	executionEpoch int64,
	execute func(context.Context) error,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := assertTenantWriter(ctx, tx, tenantID, homeRegion, executionEpoch); err != nil {
		return err
	}
	if err := execute(ctx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresResponseStore) AcquireResponseSlot(ctx context.Context, tenantID, leaseID string, limit int, expiresAt time.Time) error {
	if limit <= 0 || leaseID == "" {
		return errors.New("global quota requires a positive limit and lease identity")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "response-quota\x1f"+tenantID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tenant_response_slots WHERE tenant_id = $1 AND expires_at <= now()`, tenantID); err != nil {
		return err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tenant_response_slots WHERE tenant_id = $1`, tenantID).Scan(&count); err != nil {
		return err
	}
	if count >= limit {
		return quota.ErrExceeded
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tenant_response_slots (tenant_id, lease_id, expires_at)
		VALUES ($1,$2,$3)`, tenantID, leaseID, expiresAt); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresResponseStore) RenewResponseSlot(ctx context.Context, tenantID, leaseID string, expiresAt time.Time) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE tenant_response_slots SET expires_at = $3, updated_at = now()
		WHERE tenant_id = $1 AND lease_id = $2`, tenantID, leaseID, expiresAt)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresResponseStore) ReleaseResponseSlot(ctx context.Context, tenantID, leaseID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM tenant_response_slots WHERE tenant_id = $1 AND lease_id = $2`, tenantID, leaseID)
	return err
}

func (s *PostgresResponseStore) Create(ctx context.Context, tenantID string, response core.Response) error {
	payload, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("encode response: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := assertTenantWriter(ctx, tx, tenantID, response.HomeRegion, response.ExecutionEpoch); err != nil {
		return err
	}
	if err := s.beginConversationTx(ctx, tx, tenantID, response); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO responses (
			tenant_id, id, conversation_id, previous_response_id, status, home_region,
			execution_epoch, revision, retain_content, payload
		) VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), $5, $6, $7, $8, $9, $10)`,
		tenantID, response.ID, response.ConversationID, response.PreviousResponseID, response.Status,
		response.HomeRegion, response.ExecutionEpoch, response.Revision, response.RetainContent, payload,
	)
	if err != nil {
		return fmt.Errorf("insert response: %w", err)
	}
	if err := insertOutbox(ctx, tx, tenantID, response, "response.created", payload); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresResponseStore) CreateIdempotent(ctx context.Context, tenantID string, response core.Response, operation, key string, requestHash []byte) (core.Response, bool, error) {
	payload, err := json.Marshal(response)
	if err != nil {
		return core.Response{}, false, fmt.Errorf("encode response: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return core.Response{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := assertTenantWriter(ctx, tx, tenantID, response.HomeRegion, response.ExecutionEpoch); err != nil {
		return core.Response{}, false, err
	}
	lockIdentity := tenantID + "\x1f" + operation + "\x1f" + key
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lockIdentity); err != nil {
		return core.Response{}, false, fmt.Errorf("lock idempotency key: %w", err)
	}
	var existingHash, existingPayload []byte
	err = tx.QueryRowContext(ctx, `
		SELECT k.request_hash, r.payload
		FROM idempotency_keys k
		JOIN responses r ON r.tenant_id = k.tenant_id AND r.id = k.response_id
		WHERE k.tenant_id = $1 AND k.operation = $2 AND k.idempotency_key = $3
		  AND k.expires_at > now()`, tenantID, operation, key).Scan(&existingHash, &existingPayload)
	if err == nil {
		if !bytes.Equal(existingHash, requestHash) {
			return core.Response{}, false, ErrIdempotencyMismatch
		}
		var existing core.Response
		if err := json.Unmarshal(existingPayload, &existing); err != nil {
			return core.Response{}, false, err
		}
		if err := tx.Commit(); err != nil {
			return core.Response{}, false, err
		}
		return existing, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return core.Response{}, false, err
	}
	if err := s.beginConversationTx(ctx, tx, tenantID, response); err != nil {
		return core.Response{}, false, err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO responses (
			tenant_id, id, conversation_id, previous_response_id, status, home_region,
			execution_epoch, revision, retain_content, payload
		) VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), $5, $6, $7, $8, $9, $10)`,
		tenantID, response.ID, response.ConversationID, response.PreviousResponseID, response.Status,
		response.HomeRegion, response.ExecutionEpoch, response.Revision, response.RetainContent, payload,
	)
	if err != nil {
		return core.Response{}, false, fmt.Errorf("insert idempotent response: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO idempotency_keys (
			tenant_id, operation, idempotency_key, request_hash, response_id, expires_at
		) VALUES ($1, $2, $3, $4, $5, now() + interval '24 hours')
		ON CONFLICT (tenant_id, operation, idempotency_key) DO NOTHING`,
		tenantID, operation, key, requestHash, response.ID,
	)
	if err != nil {
		return core.Response{}, false, fmt.Errorf("reserve idempotency key: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return core.Response{}, false, err
	}
	if rows == 1 {
		if err := insertOutbox(ctx, tx, tenantID, response, "response.created", payload); err != nil {
			return core.Response{}, false, err
		}
		if err := tx.Commit(); err != nil {
			return core.Response{}, false, err
		}
		return response, true, nil
	}
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return core.Response{}, false, err
	}
	existingHash, existingPayload = nil, nil
	err = s.db.QueryRowContext(ctx, `
		SELECT k.request_hash, r.payload
		FROM idempotency_keys k
		JOIN responses r ON r.tenant_id = k.tenant_id AND r.id = k.response_id
		WHERE k.tenant_id = $1 AND k.operation = $2 AND k.idempotency_key = $3
		  AND k.expires_at > now()`, tenantID, operation, key).Scan(&existingHash, &existingPayload)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Response{}, false, ErrConflict
	}
	if err != nil {
		return core.Response{}, false, err
	}
	if !bytes.Equal(existingHash, requestHash) {
		return core.Response{}, false, ErrIdempotencyMismatch
	}
	var existing core.Response
	if err := json.Unmarshal(existingPayload, &existing); err != nil {
		return core.Response{}, false, err
	}
	return existing, false, nil
}

func (s *PostgresResponseStore) Get(ctx context.Context, tenantID, responseID string) (core.Response, error) {
	return s.getResponse(ctx, tenantID, responseID, false, "")
}

func (s *PostgresResponseStore) getResponse(ctx context.Context, tenantID, responseID string, scrubExpired bool, scrubRegion string) (core.Response, error) {
	var payload []byte
	var retainContent bool
	err := s.db.QueryRowContext(ctx, `
		SELECT payload, retain_content FROM responses
		WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL`, tenantID, responseID).Scan(&payload, &retainContent)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Response{}, ErrNotFound
	}
	if err != nil {
		return core.Response{}, err
	}
	var response core.Response
	if err := json.Unmarshal(payload, &response); err != nil {
		return core.Response{}, fmt.Errorf("decode response: %w", err)
	}
	response.RetainContent = retainContent
	if scrubExpired && isTerminal(response.Status) && response.ContentExpiresAt != nil && time.Now().Unix() >= *response.ContentExpiresAt {
		redacted := redactResponseContent(response)
		redacted.Revision = response.Revision + 1
		redactedPayload, err := json.Marshal(redacted)
		if err != nil {
			return core.Response{}, err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return core.Response{}, err
		}
		defer func() { _ = tx.Rollback() }()
		result, err := tx.ExecContext(ctx, `
			UPDATE responses SET revision = $4, payload = $5, updated_at = now()
			WHERE tenant_id = $1 AND id = $2 AND revision = $3 AND deleted_at IS NULL
			  AND EXISTS (SELECT 1 FROM tenants WHERE id = $1 AND home_region = $6)`,
			tenantID, responseID, response.Revision, redacted.Revision, redactedPayload, scrubRegion)
		if err != nil {
			return core.Response{}, err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return core.Response{}, err
		}
		if rows == 1 {
			redactionPayload, _ := json.Marshal(map[string]any{"id": response.ID, "content_expired": true})
			if _, err := tx.ExecContext(ctx, `
				UPDATE transactional_outbox SET payload = $3
				WHERE tenant_id = $1 AND aggregate_type = 'response' AND aggregate_id = $2`,
				tenantID, response.ID, redactionPayload); err != nil {
				return core.Response{}, err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM response_events WHERE tenant_id = $1 AND response_id = $2`, tenantID, response.ID); err != nil {
				return core.Response{}, err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM conversation_items WHERE tenant_id = $1 AND response_id = $2`, tenantID, response.ID); err != nil {
				return core.Response{}, err
			}
			if response.ConversationID != "" {
				var conversationRevision int64
				if err := tx.QueryRowContext(ctx, `
					UPDATE conversations SET revision = revision + 1, updated_at = now()
					WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL
					RETURNING revision`, tenantID, response.ConversationID).Scan(&conversationRevision); err != nil {
					return core.Response{}, err
				}
				if err := insertAggregateOutbox(
					ctx, tx, tenantID, "conversation", response.ConversationID, conversationRevision,
					"conversation.content_expired", redactionPayload,
				); err != nil {
					return core.Response{}, err
				}
			}
			if err := insertOutbox(ctx, tx, tenantID, redacted, "response.content_expired", redactedPayload); err != nil {
				return core.Response{}, err
			}
			if err := tx.Commit(); err != nil {
				return core.Response{}, err
			}
			response = redacted
		} else {
			_ = tx.Rollback()
			return core.Response{}, ErrConflict
		}
	}
	return response, nil
}

func (s *PostgresResponseStore) ScrubExpiredContent(ctx context.Context, homeRegion string, limit int) (int, error) {
	if homeRegion == "" || limit <= 0 {
		return 0, errors.New("retention scrub requires a Home Region and positive limit")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.tenant_id, r.id
		FROM responses r
		JOIN tenants t ON t.id = r.tenant_id AND t.home_region = $1
		WHERE r.deleted_at IS NULL
		  AND r.status IN ('completed', 'failed', 'cancelled')
		  AND r.payload ? 'content_expires_at'
		  AND (r.payload->>'content_expires_at')::bigint <= extract(epoch FROM now())::bigint
		ORDER BY r.updated_at, r.tenant_id, r.id
		LIMIT $2`, homeRegion, limit)
	if err != nil {
		return 0, err
	}
	var identities [][2]string
	for rows.Next() {
		var identity [2]string
		if err := rows.Scan(&identity[0], &identity[1]); err != nil {
			_ = rows.Close()
			return 0, err
		}
		identities = append(identities, identity)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	scrubbed := 0
	for _, identity := range identities {
		if _, err := s.getResponse(ctx, identity[0], identity[1], true, homeRegion); err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return scrubbed, err
		}
		scrubbed++
	}
	return scrubbed, nil
}

func (s *PostgresResponseStore) Update(ctx context.Context, tenantID string, response core.Response, expectedRevision int64) error {
	response.Revision = expectedRevision + 1
	payload, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("encode response: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `
		UPDATE responses
		SET status = $4, revision = $5, payload = $6, updated_at = now()
		WHERE tenant_id = $1 AND id = $2 AND revision = $3 AND deleted_at IS NULL
		  AND execution_epoch = $7
		  AND EXISTS (SELECT 1 FROM tenants t WHERE t.id = $1 AND t.home_region = $8 AND t.execution_epoch = $7)`,
		tenantID, response.ID, expectedRevision, response.Status, response.Revision, payload,
		response.ExecutionEpoch, response.HomeRegion,
	)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return s.classifyWriteMiss(ctx, tx, tenantID, response.ID)
	}
	if isTerminal(response.Status) {
		if err := s.finishConversationTx(ctx, tx, tenantID, response); err != nil {
			return err
		}
	}
	eventType := "response.updated"
	if response.Status == core.ResponseStatusCompleted {
		eventType = "response.completed"
	} else if response.Status == core.ResponseStatusFailed {
		eventType = "response.failed"
	} else if response.Status == core.ResponseStatusCancelled {
		eventType = "response.cancelled"
	}
	if err := insertOutbox(ctx, tx, tenantID, response, eventType, payload); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresResponseStore) FinalizeWithUsage(ctx context.Context, tenantID string, response core.Response, expectedRevision int64, usage core.UsageRecord) error {
	if response.Status != core.ResponseStatusCompleted {
		return errors.New("financial finalization requires a completed Response")
	}
	response.Revision = expectedRevision + 1
	payload, err := json.Marshal(response)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `
		UPDATE responses
		SET status = $4, revision = $5, payload = $6, updated_at = now()
		WHERE tenant_id = $1 AND id = $2 AND revision = $3 AND deleted_at IS NULL
		  AND execution_epoch = $7
		  AND EXISTS (SELECT 1 FROM tenants t WHERE t.id = $1 AND t.home_region = $8 AND t.execution_epoch = $7)`,
		tenantID, response.ID, expectedRevision, response.Status, response.Revision, payload,
		response.ExecutionEpoch, response.HomeRegion,
	)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return s.classifyWriteMiss(ctx, tx, tenantID, response.ID)
	}
	if err := s.finishConversationTx(ctx, tx, tenantID, response); err != nil {
		return err
	}
	if err := ensurePriceSnapshot(ctx, tx, usage.PriceSnapshot); err != nil {
		return err
	}
	providerUsage := usage.ProviderUsage
	if len(providerUsage) == 0 {
		providerUsage = json.RawMessage(`{}`)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO usage_ledger (
			id, tenant_id, api_key_id, quota_reservation_id, response_id, attempt_id, price_snapshot_id, provider_usage,
			input_tokens, cached_input_tokens, cache_write_input_tokens, output_tokens, amount, currency, created_at
		) VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
		usage.ID, tenantID, usage.APIKeyID, usage.QuotaReservationID, response.ID, usage.AttemptID, usage.PriceSnapshot.ID, providerUsage,
		usage.Usage.InputTokens, usage.Usage.CachedInputTokens, usage.Usage.CacheWriteInputTokens, usage.Usage.OutputTokens,
		usage.AmountMicros, usage.Currency, usage.CreatedAt,
	); err != nil {
		return fmt.Errorf("insert immutable usage: %w", err)
	}
	if err := insertUsageRollups(ctx, tx, tenantID, usage); err != nil {
		return err
	}
	grossSaving, attribution := observedDiscount(usage)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO savings_ledger (
			id, tenant_id, response_id, measure, attribution, price_snapshot_id, provider_usage,
			gross_saving, net_saving, currency, holdout_cohort, experiment_revision, created_at
		) VALUES ($1, $2, $3, 'observed_discount', $4, $5, $6, $7, $7, $8, NULLIF($9,''), NULLIF($10,''), $11)`,
		usage.ID+"_observed", tenantID, response.ID, attribution, usage.PriceSnapshot.ID,
		providerUsage, grossSaving, usage.Currency, usage.HoldoutCohort, usage.ExperimentRevision, usage.CreatedAt,
	); err != nil {
		return fmt.Errorf("insert observed savings evidence: %w", err)
	}
	if err := insertExperimentEvidence(ctx, tx, tenantID, response.ID, usage, providerUsage); err != nil {
		return err
	}
	if protected := usage.ProtectedHit; protected != nil &&
		protectedHitVerified(*protected, usage.CacheUsageReliable, usage.Usage.CachedInputTokens) {
		costs := protected.RefreshCostMicros + protected.ForecastCostMicros +
			protected.StorageCostMicros + protected.RouteLockCostMicros
		netSaving := grossSaving - costs
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO savings_ledger (
				id, tenant_id, response_id, cache_lease_id, measure, attribution,
				price_snapshot_id, provider_usage, gross_saving, refresh_cost, forecast_cost,
				storage_cost, route_lock_cost, net_saving, currency, holdout_cohort, created_at,
				refresh_usage_id, refresh_provider_usage
			) VALUES ($1,$2,$3,$4,'estimated_protected_saving','estimated',$5,$6,$7,$8,$9,$10,$11,$12,$13,NULLIF($14,''),$15,NULLIF($16,''),$17)`,
			usage.ID+"_protected", tenantID, response.ID, protected.CacheLeaseID,
			usage.PriceSnapshot.ID, providerUsage, grossSaving, protected.RefreshCostMicros,
			protected.ForecastCostMicros, protected.StorageCostMicros,
			protected.RouteLockCostMicros, netSaving, usage.Currency, protected.HoldoutCohort, usage.CreatedAt,
			protected.RefreshUsageID, nullJSON(protected.RefreshProviderUsage),
		); err != nil {
			return fmt.Errorf("insert protected hit evidence: %w", err)
		}
	}
	if err := insertOutbox(ctx, tx, tenantID, response, "response.completed", payload); err != nil {
		return err
	}
	usagePayload, _ := json.Marshal(usage)
	if err := insertAggregateOutbox(ctx, tx, tenantID, "usage", usage.ID, 1, "usage.recorded", usagePayload); err != nil {
		return err
	}
	return tx.Commit()
}

type DailyUsage struct {
	Date                  time.Time
	Provider              string
	Model                 string
	Currency              string
	ResponseCount         int64
	InputTokens           int64
	CachedInputTokens     int64
	CacheWriteInputTokens int64
	OutputTokens          int64
	AmountMicros          int64
}

func (s *PostgresResponseStore) APIKeyDailyUsage(
	ctx context.Context,
	tenantID, apiKeyID string,
	from, through time.Time,
) ([]DailyUsage, error) {
	if tenantID == "" || apiKeyID == "" {
		return nil, errors.New("API key usage requires Tenant and API key identities")
	}
	from = from.UTC()
	through = through.UTC()
	if through.Before(from) {
		return nil, errors.New("usage range end precedes start")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT usage_date, provider, model, currency, response_count,
		       input_tokens, cached_input_tokens, cache_write_input_tokens,
		       output_tokens, amount_micros
		FROM api_key_usage_daily
		WHERE tenant_id = $1 AND api_key_id = $2
		  AND usage_date >= $3::date AND usage_date <= $4::date
		ORDER BY usage_date, provider, model, currency`, tenantID, apiKeyID, from, through)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []DailyUsage
	for rows.Next() {
		var usage DailyUsage
		if err := rows.Scan(
			&usage.Date, &usage.Provider, &usage.Model, &usage.Currency, &usage.ResponseCount,
			&usage.InputTokens, &usage.CachedInputTokens, &usage.CacheWriteInputTokens,
			&usage.OutputTokens, &usage.AmountMicros,
		); err != nil {
			return nil, err
		}
		result = append(result, usage)
	}
	return result, rows.Err()
}

func insertUsageRollups(ctx context.Context, tx *sql.Tx, tenantID string, usage core.UsageRecord) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tenant_usage_daily (
			usage_date, tenant_id, provider, model, currency, response_count,
			input_tokens, cached_input_tokens, cache_write_input_tokens, output_tokens, amount_micros
		) VALUES (($1 AT TIME ZONE 'UTC')::date,$2,$3,$4,$5,1,$6,$7,$8,$9,$10)
		ON CONFLICT (usage_date, tenant_id, provider, model, currency) DO UPDATE SET
			response_count = tenant_usage_daily.response_count + 1,
			input_tokens = tenant_usage_daily.input_tokens + EXCLUDED.input_tokens,
			cached_input_tokens = tenant_usage_daily.cached_input_tokens + EXCLUDED.cached_input_tokens,
			cache_write_input_tokens = tenant_usage_daily.cache_write_input_tokens + EXCLUDED.cache_write_input_tokens,
			output_tokens = tenant_usage_daily.output_tokens + EXCLUDED.output_tokens,
			amount_micros = tenant_usage_daily.amount_micros + EXCLUDED.amount_micros,
			updated_at = now()`, usage.CreatedAt, tenantID, usage.PriceSnapshot.Provider, usage.PriceSnapshot.Model,
		usage.Currency, usage.Usage.InputTokens, usage.Usage.CachedInputTokens,
		usage.Usage.CacheWriteInputTokens, usage.Usage.OutputTokens, usage.AmountMicros); err != nil {
		return fmt.Errorf("update Tenant usage rollup: %w", err)
	}
	if usage.APIKeyID == "" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO api_key_usage_daily (
			usage_date, tenant_id, api_key_id, provider, model, currency, response_count,
			input_tokens, cached_input_tokens, cache_write_input_tokens, output_tokens, amount_micros
		) VALUES (($1 AT TIME ZONE 'UTC')::date,$2,$3,$4,$5,$6,1,$7,$8,$9,$10,$11)
		ON CONFLICT (usage_date, tenant_id, api_key_id, provider, model, currency) DO UPDATE SET
			response_count = api_key_usage_daily.response_count + 1,
			input_tokens = api_key_usage_daily.input_tokens + EXCLUDED.input_tokens,
			cached_input_tokens = api_key_usage_daily.cached_input_tokens + EXCLUDED.cached_input_tokens,
			cache_write_input_tokens = api_key_usage_daily.cache_write_input_tokens + EXCLUDED.cache_write_input_tokens,
			output_tokens = api_key_usage_daily.output_tokens + EXCLUDED.output_tokens,
			amount_micros = api_key_usage_daily.amount_micros + EXCLUDED.amount_micros,
			updated_at = now()`, usage.CreatedAt, tenantID, usage.APIKeyID, usage.PriceSnapshot.Provider,
		usage.PriceSnapshot.Model, usage.Currency, usage.Usage.InputTokens, usage.Usage.CachedInputTokens,
		usage.Usage.CacheWriteInputTokens, usage.Usage.OutputTokens, usage.AmountMicros); err != nil {
		return fmt.Errorf("update API key usage rollup: %w", err)
	}
	return nil
}

func insertExperimentEvidence(ctx context.Context, tx *sql.Tx, tenantID, responseID string, usage core.UsageRecord, providerUsage json.RawMessage) error {
	if (usage.HoldoutCohort != "treatment" && usage.HoldoutCohort != "holdout") || usage.ExperimentRevision == "" {
		return nil
	}
	lockIdentity := tenantID + "\x1f" + usage.PriceSnapshot.ID + "\x1f" + usage.ExperimentRevision
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lockIdentity); err != nil {
		return fmt.Errorf("lock experiment evidence revision: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT s.holdout_cohort, count(*), COALESCE(sum(u.amount), 0)::bigint
		FROM savings_ledger s
		JOIN usage_ledger u ON u.tenant_id = s.tenant_id AND u.response_id = s.response_id
		WHERE s.tenant_id = $1 AND s.measure = 'observed_discount'
		  AND s.price_snapshot_id = $2 AND s.experiment_revision = $3
		  AND s.holdout_cohort IN ('treatment', 'holdout')
		GROUP BY s.holdout_cohort`, tenantID, usage.PriceSnapshot.ID, usage.ExperimentRevision)
	if err != nil {
		return err
	}
	defer rows.Close()
	type cohort struct{ responses, cost int64 }
	cohorts := map[string]cohort{}
	for rows.Next() {
		var name string
		var value cohort
		if err := rows.Scan(&name, &value.responses, &value.cost); err != nil {
			return err
		}
		cohorts[name] = value
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	treatment, holdout := cohorts["treatment"], cohorts["holdout"]
	if treatment.responses == 0 || holdout.responses == 0 {
		return nil
	}
	var refreshCost, unreliableRefreshes int64
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(sum(r.amount), 0)::bigint,
		       count(*) FILTER (WHERE NOT r.usage_reliable)
		FROM cache_refresh_usage_ledger r
		JOIN cache_refresh_intents i
		  ON i.tenant_id = r.tenant_id AND i.id = r.cache_refresh_intent_id
		WHERE r.tenant_id = $1 AND r.price_snapshot_id = $2
		  AND i.candidate->>'HoldoutCohort' = 'treatment'
		  AND i.candidate->>'ExperimentRevision' = $3`, tenantID, usage.PriceSnapshot.ID, usage.ExperimentRevision).Scan(
		&refreshCost, &unreliableRefreshes,
	); err != nil {
		return err
	}
	if unreliableRefreshes > 0 {
		return nil
	}
	treatment.cost += refreshCost
	saving := (holdout.cost/holdout.responses - treatment.cost/treatment.responses) * treatment.responses
	var previouslyRecorded int64
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(sum(net_saving), 0)::bigint
		FROM savings_ledger
		WHERE tenant_id = $1 AND measure = 'experimentally_validated_saving'
		  AND price_snapshot_id = $2 AND experiment_revision = $3`,
		tenantID, usage.PriceSnapshot.ID, usage.ExperimentRevision,
	).Scan(&previouslyRecorded); err != nil {
		return err
	}
	incrementalSaving := saving - previouslyRecorded
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO savings_ledger (
			id, tenant_id, response_id, measure, attribution, price_snapshot_id, provider_usage,
			gross_saving, net_saving, currency, holdout_cohort, experiment_revision, created_at
		) VALUES ($1,$2,$3,'experimentally_validated_saving','experiment',$4,$5,$6,$6,$7,'treatment_vs_holdout',$8,$9)`,
		usage.ID+"_experiment", tenantID, responseID, usage.PriceSnapshot.ID, providerUsage,
		incrementalSaving, usage.Currency, usage.ExperimentRevision, usage.CreatedAt,
	); err != nil {
		return fmt.Errorf("insert experiment savings evidence: %w", err)
	}
	return nil
}

func protectedHitVerified(evidence core.ProtectedHitEvidence, reliable bool, cachedInputTokens int64) bool {
	return reliable && cachedInputTokens > 0 && evidence.CacheLeaseID != "" &&
		evidence.RefreshUsageID != "" && len(evidence.RefreshProviderUsage) > 0 && json.Valid(evidence.RefreshProviderUsage) &&
		!evidence.RefreshSucceededAt.IsZero() &&
		evidence.RefreshSucceededAt.Before(evidence.OriginalLeaseExpiresAt) &&
		evidence.CustomerRequestAt.After(evidence.OriginalLeaseExpiresAt) &&
		evidence.CustomerRequestAt.Before(evidence.RefreshExpiresAt)
}

func nullJSON(value json.RawMessage) json.RawMessage {
	if len(value) == 0 || !json.Valid(value) {
		return json.RawMessage(`{}`)
	}
	return value
}

func ensurePriceSnapshot(ctx context.Context, tx *sql.Tx, snapshot core.PriceSnapshot) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO provider_price_snapshots (
			id, provider, model, region, currency, input_per_million, cached_input_per_million,
			cache_write_per_million, output_per_million, embedding_input_per_million,
			moderation_input_per_million, rerank_document_per_thousand, effective_at, source
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, to_timestamp($13), $14)
		ON CONFLICT (id) DO NOTHING`,
		snapshot.ID, snapshot.Provider, snapshot.Model, snapshot.Region, snapshot.Currency,
		snapshot.InputPerMillionMicros, snapshot.CachedInputPerMillionMicros,
		snapshot.CacheWritePerMillionMicros, snapshot.OutputPerMillionMicros,
		snapshot.EmbeddingInputPerMillionMicros, snapshot.ModerationInputPerMillionMicros,
		snapshot.RerankDocumentPerThousandMicros, snapshot.EffectiveAt, snapshot.Source,
	); err != nil {
		return fmt.Errorf("insert price snapshot: %w", err)
	}
	var existing core.PriceSnapshot
	err := tx.QueryRowContext(ctx, `
		SELECT id, provider, model, region, currency,
		       input_per_million::bigint, cached_input_per_million::bigint, cache_write_per_million::bigint,
		       output_per_million::bigint, embedding_input_per_million, moderation_input_per_million,
		       rerank_document_per_thousand,
		       extract(epoch FROM effective_at)::bigint, source
		FROM provider_price_snapshots WHERE id = $1`, snapshot.ID).Scan(
		&existing.ID, &existing.Provider, &existing.Model, &existing.Region, &existing.Currency,
		&existing.InputPerMillionMicros, &existing.CachedInputPerMillionMicros,
		&existing.CacheWritePerMillionMicros, &existing.OutputPerMillionMicros,
		&existing.EmbeddingInputPerMillionMicros, &existing.ModerationInputPerMillionMicros,
		&existing.RerankDocumentPerThousandMicros,
		&existing.EffectiveAt, &existing.Source,
	)
	if err != nil {
		return err
	}
	if existing != snapshot {
		return errors.New("price snapshot ID is immutable and already has different values")
	}
	return nil
}

func observedDiscount(usage core.UsageRecord) (int64, string) {
	if !usage.CacheUsageReliable || usage.Usage.CachedInputTokens < 0 ||
		usage.PriceSnapshot.InputPerMillionMicros < usage.PriceSnapshot.CachedInputPerMillionMicros {
		return 0, "unavailable"
	}
	difference := usage.PriceSnapshot.InputPerMillionMicros - usage.PriceSnapshot.CachedInputPerMillionMicros
	return perMillionAmount(usage.Usage.CachedInputTokens, difference), "observed"
}

func perMillionAmount(tokens, rate int64) int64 {
	return (tokens/1_000_000)*rate + (tokens%1_000_000)*rate/1_000_000
}

func (s *PostgresResponseStore) Delete(ctx context.Context, tenantID, responseID string, expectedRevision int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	newRevision := expectedRevision + 1
	tombstone := core.Response{ID: responseID, Object: "response", Status: core.ResponseStatusDeleted, Revision: newRevision}
	payload, err := json.Marshal(tombstone)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE responses
		SET status = $4, revision = $5, payload = $6, deleted_at = now(), updated_at = now()
		WHERE tenant_id = $1 AND id = $2 AND revision = $3 AND deleted_at IS NULL
		  AND execution_epoch = (SELECT execution_epoch FROM tenants WHERE id = $1)`,
		tenantID, responseID, expectedRevision, core.ResponseStatusDeleted, newRevision, payload,
	)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return s.classifyWriteMiss(ctx, tx, tenantID, responseID)
	}
	if err := insertOutbox(ctx, tx, tenantID, tombstone, "response.deleted", payload); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresResponseStore) ListInputItems(ctx context.Context, tenantID, responseID string) ([]core.Item, error) {
	response, err := s.Get(ctx, tenantID, responseID)
	if err != nil {
		return nil, err
	}
	return response.Input, nil
}

func (s *PostgresResponseStore) CreateConversation(ctx context.Context, tenantID string, conversation core.Conversation) error {
	metadata, err := json.Marshal(conversation.Metadata)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := assertTenantWriter(ctx, tx, tenantID, conversation.HomeRegion, conversation.ExecutionEpoch); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO conversations (
			tenant_id, id, home_region, execution_epoch, revision, metadata, active_response_id, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, NULL, to_timestamp($7), to_timestamp($7))`,
		tenantID, conversation.ID, conversation.HomeRegion, conversation.ExecutionEpoch,
		conversation.Revision, metadata, conversation.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert conversation: %w", err)
	}
	if err := insertConversationItems(ctx, tx, tenantID, conversation.ID, "", "initial", 0, conversation.Items); err != nil {
		return err
	}
	payload, _ := json.Marshal(conversation)
	if err := insertAggregateOutbox(ctx, tx, tenantID, "conversation", conversation.ID, conversation.Revision, "conversation.created", payload); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresResponseStore) GetConversation(ctx context.Context, tenantID, conversationID string) (core.Conversation, error) {
	var conversation core.Conversation
	var metadata []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT id, extract(epoch FROM created_at)::bigint, home_region, execution_epoch, revision,
		       COALESCE(active_response_id, ''), metadata
		FROM conversations
		WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL`, tenantID, conversationID).Scan(
		&conversation.ID, &conversation.CreatedAt, &conversation.HomeRegion, &conversation.ExecutionEpoch, &conversation.Revision,
		&conversation.ActiveResponseID, &metadata,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Conversation{}, ErrNotFound
	}
	if err != nil {
		return core.Conversation{}, err
	}
	conversation.Object = "conversation"
	if err := json.Unmarshal(metadata, &conversation.Metadata); err != nil {
		return core.Conversation{}, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT payload FROM conversation_items
		WHERE tenant_id = $1 AND conversation_id = $2
		ORDER BY position`, tenantID, conversationID)
	if err != nil {
		return core.Conversation{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return core.Conversation{}, err
		}
		var item core.Item
		if err := json.Unmarshal(payload, &item); err != nil {
			return core.Conversation{}, err
		}
		conversation.Items = append(conversation.Items, item)
	}
	return conversation, rows.Err()
}

func (s *PostgresResponseStore) AppendConversationItems(ctx context.Context, tenantID, conversationID string, items []core.Item, expectedRevision int64) (core.Conversation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return core.Conversation{}, err
	}
	defer func() { _ = tx.Rollback() }()
	conversation, err := lockConversation(ctx, tx, tenantID, conversationID)
	if err != nil {
		return core.Conversation{}, err
	}
	if conversation.Revision != expectedRevision {
		return core.Conversation{}, ErrConflict
	}
	if conversation.ActiveResponseID != "" {
		return core.Conversation{}, ErrConversationBusy
	}
	if err := insertConversationItems(ctx, tx, tenantID, conversationID, "", "manual", int64(len(conversation.Items)), items); err != nil {
		return core.Conversation{}, err
	}
	conversation.Items = append(conversation.Items, items...)
	conversation.Revision++
	if _, err := tx.ExecContext(ctx, `
		UPDATE conversations SET revision = $3, updated_at = now()
		WHERE tenant_id = $1 AND id = $2`, tenantID, conversationID, conversation.Revision); err != nil {
		return core.Conversation{}, err
	}
	payload, _ := json.Marshal(conversation)
	if err := insertAggregateOutbox(ctx, tx, tenantID, "conversation", conversationID, conversation.Revision, "conversation.items.appended", payload); err != nil {
		return core.Conversation{}, err
	}
	if err := tx.Commit(); err != nil {
		return core.Conversation{}, err
	}
	return conversation, nil
}

func (s *PostgresResponseStore) DeleteConversation(ctx context.Context, tenantID, conversationID string, expectedRevision int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	conversation, err := lockConversation(ctx, tx, tenantID, conversationID)
	if err != nil {
		return err
	}
	if conversation.Revision != expectedRevision {
		return ErrConflict
	}
	if conversation.ActiveResponseID != "" {
		return ErrConversationBusy
	}
	newRevision := conversation.Revision + 1
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM conversation_items WHERE tenant_id = $1 AND conversation_id = $2`, tenantID, conversationID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE conversations
		SET revision = $3, metadata = '{}'::jsonb, deleted_at = now(), updated_at = now()
		WHERE tenant_id = $1 AND id = $2`, tenantID, conversationID, newRevision); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{"id": conversationID, "deleted": true})
	if err := insertAggregateOutbox(ctx, tx, tenantID, "conversation", conversationID, newRevision, "conversation.deleted", payload); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresResponseStore) beginConversationTx(ctx context.Context, tx *sql.Tx, tenantID string, response core.Response) error {
	if response.ConversationID == "" {
		return nil
	}
	conversation, err := lockConversation(ctx, tx, tenantID, response.ConversationID)
	if err != nil {
		return err
	}
	if conversation.HomeRegion != response.HomeRegion {
		return ErrConflict
	}
	if conversation.ExecutionEpoch != response.ExecutionEpoch {
		return ErrConflict
	}
	if conversation.ActiveResponseID != "" {
		return ErrConversationBusy
	}
	if err := insertConversationItems(ctx, tx, tenantID, response.ConversationID, response.ID, "input", int64(len(conversation.Items)), response.Input); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE conversations
		SET revision = revision + 1, active_response_id = $3, updated_at = now()
		WHERE tenant_id = $1 AND id = $2`, tenantID, response.ConversationID, response.ID)
	return err
}

func (s *PostgresResponseStore) finishConversationTx(ctx context.Context, tx *sql.Tx, tenantID string, response core.Response) error {
	if response.ConversationID == "" {
		return nil
	}
	conversation, err := lockConversation(ctx, tx, tenantID, response.ConversationID)
	if err != nil {
		return err
	}
	if conversation.ActiveResponseID != response.ID {
		return ErrConflict
	}
	if err := insertConversationItems(ctx, tx, tenantID, response.ConversationID, response.ID, "output", int64(len(conversation.Items)), response.Output); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE conversations
		SET revision = revision + 1, active_response_id = NULL, updated_at = now()
		WHERE tenant_id = $1 AND id = $2`, tenantID, response.ConversationID)
	return err
}

func lockConversation(ctx context.Context, tx *sql.Tx, tenantID, conversationID string) (core.Conversation, error) {
	var conversation core.Conversation
	var metadata []byte
	err := tx.QueryRowContext(ctx, `
		SELECT c.id, extract(epoch FROM c.created_at)::bigint, c.home_region, c.execution_epoch, c.revision,
		       COALESCE(c.active_response_id, ''), c.metadata
		FROM conversations c
		JOIN tenants t ON t.id = c.tenant_id AND t.home_region = c.home_region AND t.execution_epoch = c.execution_epoch
		WHERE c.tenant_id = $1 AND c.id = $2 AND c.deleted_at IS NULL
		FOR UPDATE`, tenantID, conversationID).Scan(
		&conversation.ID, &conversation.CreatedAt, &conversation.HomeRegion, &conversation.ExecutionEpoch, &conversation.Revision,
		&conversation.ActiveResponseID, &metadata,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Conversation{}, ErrNotFound
	}
	if err != nil {
		return core.Conversation{}, err
	}
	conversation.Object = "conversation"
	if err := json.Unmarshal(metadata, &conversation.Metadata); err != nil {
		return core.Conversation{}, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT payload FROM conversation_items
		WHERE tenant_id = $1 AND conversation_id = $2 ORDER BY position`, tenantID, conversationID)
	if err != nil {
		return core.Conversation{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return core.Conversation{}, err
		}
		var item core.Item
		if err := json.Unmarshal(payload, &item); err != nil {
			return core.Conversation{}, err
		}
		conversation.Items = append(conversation.Items, item)
	}
	return conversation, rows.Err()
}

func assertTenantWriter(ctx context.Context, tx *sql.Tx, tenantID, homeRegion string, executionEpoch int64) error {
	var currentHomeRegion string
	var currentExecutionEpoch int64
	if err := tx.QueryRowContext(ctx, `
		SELECT home_region, execution_epoch FROM tenants WHERE id = $1 FOR SHARE`, tenantID).Scan(
		&currentHomeRegion, &currentExecutionEpoch,
	); errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: tenant home-region writer fencing conflict", ErrConflict)
	} else if err != nil {
		return err
	}
	if currentHomeRegion != homeRegion || currentExecutionEpoch != executionEpoch {
		return fmt.Errorf("%w: tenant home-region writer fencing conflict", ErrConflict)
	}
	return nil
}

func insertConversationItems(ctx context.Context, tx *sql.Tx, tenantID, conversationID, responseID, direction string, offset int64, items []core.Item) error {
	for index, item := range items {
		payload, err := json.Marshal(item)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO conversation_items (
				tenant_id, conversation_id, position, item_id, response_id, direction, payload
			) VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, $7)`,
			tenantID, conversationID, offset+int64(index)+1, item.ID, responseID, direction, payload,
		); err != nil {
			return fmt.Errorf("insert conversation item: %w", err)
		}
	}
	return nil
}

func (s *PostgresResponseStore) classifyWriteMiss(ctx context.Context, tx *sql.Tx, tenantID, responseID string) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM responses WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL
		)`, tenantID, responseID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	return ErrConflict
}

func insertOutbox(ctx context.Context, tx *sql.Tx, tenantID string, response core.Response, eventType string, payload []byte) error {
	return insertAggregateOutbox(ctx, tx, tenantID, "response", response.ID, response.Revision, eventType, payload)
}

func insertAggregateOutbox(ctx context.Context, tx *sql.Tx, tenantID, aggregateType, aggregateID string, revision int64, eventType string, payload []byte) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO transactional_outbox (
			tenant_id, aggregate_type, aggregate_id, aggregate_revision, event_type, payload
		) VALUES ($1, $2, $3, $4, $5, $6)`,
		tenantID, aggregateType, aggregateID, revision, eventType, payload,
	)
	if err != nil {
		return fmt.Errorf("insert transactional outbox: %w", err)
	}
	return nil
}
