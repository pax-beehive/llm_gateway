package accessprojection

import (
	"context"
	"errors"
	"time"

	"github.com/toddzheng/llm-gateway/internal/quota"
)

var ErrConcurrencyExceeded = quota.ErrExceeded

func (store *Store) AcquireAPIKeyResponseSlot(ctx context.Context, apiKeyID, leaseID string, limit int, expiresAt time.Time) error {
	if apiKeyID == "" || leaseID == "" || limit <= 0 {
		return errors.New("Gateway API Key concurrency requires an API key, lease, and positive limit")
	}
	transaction, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	if _, err := transaction.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "gateway-api-key-response-quota\x1f"+apiKeyID); err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx, `DELETE FROM gateway_access_response_slots WHERE api_key_id = $1 AND expires_at <= now()`, apiKeyID); err != nil {
		return err
	}
	var count int
	if err := transaction.QueryRowContext(ctx, `SELECT count(*) FROM gateway_access_response_slots WHERE api_key_id = $1`, apiKeyID).Scan(&count); err != nil {
		return err
	}
	if count >= limit {
		return ErrConcurrencyExceeded
	}
	if _, err := transaction.ExecContext(ctx, `
		INSERT INTO gateway_access_response_slots (api_key_id, lease_id, expires_at)
		VALUES ($1,$2,$3)`, apiKeyID, leaseID, expiresAt); err != nil {
		return err
	}
	return transaction.Commit()
}

func (store *Store) RenewAPIKeyResponseSlot(ctx context.Context, apiKeyID, leaseID string, expiresAt time.Time) error {
	result, err := store.database.ExecContext(ctx, `
		UPDATE gateway_access_response_slots SET expires_at = $3, updated_at = now()
		WHERE api_key_id = $1 AND lease_id = $2`, apiKeyID, leaseID, expiresAt)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return errors.New("Gateway API Key concurrency lease not found")
	}
	return nil
}

func (store *Store) ReleaseAPIKeyResponseSlot(ctx context.Context, apiKeyID, leaseID string) error {
	_, err := store.database.ExecContext(ctx, `DELETE FROM gateway_access_response_slots WHERE api_key_id = $1 AND lease_id = $2`, apiKeyID, leaseID)
	return err
}
