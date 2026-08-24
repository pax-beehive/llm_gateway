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
	var existingHash, existingPayload []byte
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
	_, err := tx.ExecContext(ctx, `
		INSERT INTO transactional_outbox (
			tenant_id, aggregate_type, aggregate_id, aggregate_revision, event_type, payload
		) VALUES ($1, 'response', $2, $3, $4, $5)`,
		tenantID, response.ID, response.Revision, eventType, payload,
	)
	if err != nil {
		return fmt.Errorf("insert transactional outbox: %w", err)
	}
	return nil
}
