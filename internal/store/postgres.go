package store

import (
	"bytes"
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/toddzheng/llm-gateway/internal/core"
)

//go:embed migrations/000001_core.sql
var coreMigration string

//go:embed migrations/000002_conversation_items.sql
var conversationMigration string

//go:embed migrations/000003_versioned_configuration.sql
var configurationMigration string

//go:embed migrations/000004_cache_protection_runtime.sql
var cacheProtectionMigration string

//go:embed migrations/000005_protected_hit_evidence.sql
var protectedHitMigration string

//go:embed migrations/000006_cache_write_usage.sql
var cacheWriteUsageMigration string

type PostgresResponseStore struct {
	db *sql.DB
}

func NewPostgresResponseStore(db *sql.DB) *PostgresResponseStore {
	return &PostgresResponseStore{db: db}
}

func (s *PostgresResponseStore) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, coreMigration); err != nil {
		return fmt.Errorf("migrate response store: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, conversationMigration); err != nil {
		return fmt.Errorf("migrate conversation store: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, configurationMigration); err != nil {
		return fmt.Errorf("migrate configuration store: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, cacheProtectionMigration); err != nil {
		return fmt.Errorf("migrate cache protection store: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, protectedHitMigration); err != nil {
		return fmt.Errorf("migrate protected hit evidence: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, cacheWriteUsageMigration); err != nil {
		return fmt.Errorf("migrate cache write usage: %w", err)
	}
	return nil
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
	if err := s.beginConversationTx(ctx, tx, tenantID, response); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO responses (
			tenant_id, id, conversation_id, previous_response_id, status, home_region, revision, payload
		) VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), $5, $6, $7, $8)`,
		tenantID, response.ID, response.ConversationID, response.PreviousResponseID, response.Status,
		response.HomeRegion, response.Revision, payload,
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
			tenant_id, id, conversation_id, previous_response_id, status, home_region, revision, payload
		) VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), $5, $6, $7, $8)`,
		tenantID, response.ID, response.ConversationID, response.PreviousResponseID, response.Status,
		response.HomeRegion, response.Revision, payload,
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
	var payload []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT payload FROM responses
		WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL`, tenantID, responseID).Scan(&payload)
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
	return response, nil
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
		WHERE tenant_id = $1 AND id = $2 AND revision = $3 AND deleted_at IS NULL`,
		tenantID, response.ID, expectedRevision, response.Status, response.Revision, payload,
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

func (s *PostgresResponseStore) CompleteWithUsage(ctx context.Context, tenantID string, response core.Response, expectedRevision int64, usage core.UsageRecord) error {
	if response.Status != core.ResponseStatusCompleted {
		return errors.New("financial completion requires completed response")
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
		WHERE tenant_id = $1 AND id = $2 AND revision = $3 AND deleted_at IS NULL`,
		tenantID, response.ID, expectedRevision, response.Status, response.Revision, payload,
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
			id, tenant_id, response_id, attempt_id, price_snapshot_id, provider_usage,
			input_tokens, cached_input_tokens, cache_write_input_tokens, output_tokens, amount, currency, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		usage.ID, tenantID, response.ID, usage.AttemptID, usage.PriceSnapshot.ID, providerUsage,
		usage.Usage.InputTokens, usage.Usage.CachedInputTokens, usage.Usage.CacheWriteInputTokens, usage.Usage.OutputTokens,
		usage.AmountMicros, usage.Currency, usage.CreatedAt,
	); err != nil {
		return fmt.Errorf("insert immutable usage: %w", err)
	}
	grossSaving, attribution := observedDiscount(usage)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO savings_ledger (
			id, tenant_id, response_id, measure, attribution, price_snapshot_id, provider_usage,
			gross_saving, net_saving, currency, created_at
		) VALUES ($1, $2, $3, 'observed_discount', $4, $5, $6, $7, $7, $8, $9)`,
		usage.ID+"_observed", tenantID, response.ID, attribution, usage.PriceSnapshot.ID,
		providerUsage, grossSaving, usage.Currency, usage.CreatedAt,
	); err != nil {
		return fmt.Errorf("insert observed savings evidence: %w", err)
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
				storage_cost, route_lock_cost, net_saving, currency, created_at
			) VALUES ($1,$2,$3,$4,'estimated_protected_saving','estimated',$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
			usage.ID+"_protected", tenantID, response.ID, protected.CacheLeaseID,
			usage.PriceSnapshot.ID, providerUsage, grossSaving, protected.RefreshCostMicros,
			protected.ForecastCostMicros, protected.StorageCostMicros,
			protected.RouteLockCostMicros, netSaving, usage.Currency, usage.CreatedAt,
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

func protectedHitVerified(evidence core.ProtectedHitEvidence, reliable bool, cachedInputTokens int64) bool {
	return reliable && cachedInputTokens > 0 && evidence.CacheLeaseID != "" &&
		!evidence.RefreshSucceededAt.IsZero() &&
		evidence.RefreshSucceededAt.Before(evidence.OriginalLeaseExpiresAt) &&
		evidence.CustomerRequestAt.After(evidence.OriginalLeaseExpiresAt) &&
		evidence.CustomerRequestAt.Before(evidence.RefreshExpiresAt)
}

func ensurePriceSnapshot(ctx context.Context, tx *sql.Tx, snapshot core.PriceSnapshot) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO provider_price_snapshots (
			id, provider, model, region, currency, input_per_million, cached_input_per_million,
			cache_write_per_million, output_per_million, effective_at, source
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, to_timestamp($10), $11)
		ON CONFLICT (id) DO NOTHING`,
		snapshot.ID, snapshot.Provider, snapshot.Model, snapshot.Region, snapshot.Currency,
		snapshot.InputPerMillionMicros, snapshot.CachedInputPerMillionMicros,
		snapshot.CacheWritePerMillionMicros, snapshot.OutputPerMillionMicros, snapshot.EffectiveAt, snapshot.Source,
	); err != nil {
		return fmt.Errorf("insert price snapshot: %w", err)
	}
	var existing core.PriceSnapshot
	err := tx.QueryRowContext(ctx, `
		SELECT id, provider, model, region, currency,
		       input_per_million::bigint, cached_input_per_million::bigint, cache_write_per_million::bigint,
		       output_per_million::bigint,
		       extract(epoch FROM effective_at)::bigint, source
		FROM provider_price_snapshots WHERE id = $1`, snapshot.ID).Scan(
		&existing.ID, &existing.Provider, &existing.Model, &existing.Region, &existing.Currency,
		&existing.InputPerMillionMicros, &existing.CachedInputPerMillionMicros,
		&existing.CacheWritePerMillionMicros, &existing.OutputPerMillionMicros,
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
		WHERE tenant_id = $1 AND id = $2 AND revision = $3 AND deleted_at IS NULL`,
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
	_, err = tx.ExecContext(ctx, `
		INSERT INTO conversations (
			tenant_id, id, home_region, revision, metadata, active_response_id, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, NULL, to_timestamp($6), to_timestamp($6))`,
		tenantID, conversation.ID, conversation.HomeRegion, conversation.Revision, metadata, conversation.CreatedAt,
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
		SELECT id, extract(epoch FROM created_at)::bigint, home_region, revision,
		       COALESCE(active_response_id, ''), metadata
		FROM conversations
		WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL`, tenantID, conversationID).Scan(
		&conversation.ID, &conversation.CreatedAt, &conversation.HomeRegion, &conversation.Revision,
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
		SELECT id, extract(epoch FROM created_at)::bigint, home_region, revision,
		       COALESCE(active_response_id, ''), metadata
		FROM conversations
		WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL
		FOR UPDATE`, tenantID, conversationID).Scan(
		&conversation.ID, &conversation.CreatedAt, &conversation.HomeRegion, &conversation.Revision,
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
