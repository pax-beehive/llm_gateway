package cacheprotection

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/provider"
)

type PostgresIntentRepository struct {
	db *sql.DB
}

func NewPostgresIntentRepository(db *sql.DB) *PostgresIntentRepository {
	return &PostgresIntentRepository{db: db}
}

func (r *PostgresIntentRepository) CurrentLease(ctx context.Context, tenantID, leaseID string) (Lease, bool, error) {
	var lease Lease
	err := r.db.QueryRowContext(ctx, `
		SELECT revision, created_at, estimated_expires_at, refresh_count, spent_micros::bigint, fencing_token
		FROM cache_leases WHERE tenant_id = $1 AND id = $2`, tenantID, leaseID).Scan(
		&lease.Revision, &lease.CreatedAt, &lease.EstimatedExpiresAt,
		&lease.RefreshCount, &lease.SpentMicros, &lease.FencingToken,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Lease{}, false, nil
	}
	if err != nil {
		return Lease{}, false, err
	}
	lease.ID = leaseID
	return lease, true, nil
}

func (r *PostgresIntentRepository) Reserve(ctx context.Context, intent Intent) (Intent, bool, error) {
	candidate, err := json.Marshal(intent.Candidate)
	if err != nil {
		return Intent{}, false, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Intent{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	lease := intent.Candidate.Lease
	result, err := tx.ExecContext(ctx, `
		INSERT INTO cache_leases (
			tenant_id, id, revision, route_id, provider, model, credential_scope, region,
			cache_key, prefix_hash, estimated_expires_at, original_expires_at, fencing_token, status,
			created_at, refresh_count, spent_micros
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$11,$12,'active',$13,$14,$15)
		ON CONFLICT (tenant_id, id) DO UPDATE SET
			revision = EXCLUDED.revision,
			estimated_expires_at = EXCLUDED.estimated_expires_at,
			fencing_token = EXCLUDED.fencing_token,
			status = 'active',
			refresh_count = EXCLUDED.refresh_count,
			spent_micros = EXCLUDED.spent_micros,
			updated_at = now()
		WHERE cache_leases.revision <= EXCLUDED.revision
		  AND cache_leases.fencing_token <= EXCLUDED.fencing_token`,
		intent.TenantID, lease.ID, lease.Revision, lease.Anchor.RouteID, lease.Anchor.Provider,
		lease.Anchor.Model, lease.Anchor.CredentialScope, lease.Anchor.Region, lease.Anchor.CacheKey,
		lease.Anchor.PrefixHash, lease.EstimatedExpiresAt, lease.FencingToken, lease.CreatedAt,
		lease.RefreshCount, lease.SpentMicros,
	)
	if err != nil {
		return Intent{}, false, fmt.Errorf("upsert cache lease: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return Intent{}, false, errors.New("cache lease fencing conflict")
	}
	resultPayload, _ := json.Marshal(intent.ProviderResult)
	result, err = tx.ExecContext(ctx, `
		INSERT INTO cache_refresh_intents (
			tenant_id, id, cache_lease_id, cache_lease_revision, fencing_token, status,
			expected_net_saving, scheduled_for, provider_result, candidate, error, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,NULLIF($11,''),$12,$13)
		ON CONFLICT (tenant_id, cache_lease_id, cache_lease_revision) DO NOTHING`,
		intent.TenantID, intent.ID, intent.CacheLeaseID, intent.CacheLeaseRevision, intent.FencingToken,
		intent.Status, intent.ExpectedNetSavingMicros, intent.ScheduledFor, resultPayload, candidate,
		intent.Error, intent.CreatedAt, intent.UpdatedAt,
	)
	if err != nil {
		return Intent{}, false, err
	}
	rows, err = result.RowsAffected()
	if err != nil {
		return Intent{}, false, err
	}
	if rows == 0 {
		existing, err := getIntentTx(ctx, tx, intent.TenantID, intent.CacheLeaseID, intent.CacheLeaseRevision)
		if err != nil {
			return Intent{}, false, err
		}
		if err := tx.Commit(); err != nil {
			return Intent{}, false, err
		}
		return existing, false, nil
	}
	budgetReserved, err := reserveRefreshSessionBudget(ctx, tx, intent)
	if err != nil {
		return Intent{}, false, err
	}
	if !budgetReserved {
		intent.Status = IntentRejected
		intent.Error = "session_refresh_limit_reached"
		if _, err := tx.ExecContext(ctx, `
			UPDATE cache_refresh_intents
			SET status = 'rejected', error = $3, updated_at = now()
			WHERE tenant_id = $1 AND id = $2`, intent.TenantID, intent.ID, intent.Error); err != nil {
			return Intent{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Intent{}, false, err
	}
	return intent, true, nil
}

func (r *PostgresIntentRepository) Update(ctx context.Context, intent Intent, status IntentStatus) (Intent, error) {
	if !validTransition(intent.Status, status) {
		return Intent{}, fmt.Errorf("invalid cache intent transition %s -> %s", intent.Status, status)
	}
	providerResult, _ := json.Marshal(intent.ProviderResult)
	candidate, _ := json.Marshal(intent.Candidate)
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Intent{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if status == IntentPlanned {
		budgetReserved, reserveErr := reserveRefreshSessionBudget(ctx, tx, intent)
		if reserveErr != nil {
			return Intent{}, reserveErr
		}
		if !budgetReserved {
			status = IntentRejected
			intent.Error = "session_refresh_limit_reached"
		}
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE cache_refresh_intents
		SET status = $6, provider_result = $7, error = NULLIF($8,''), candidate = $10, updated_at = now()
		WHERE tenant_id = $1 AND id = $2 AND cache_lease_id = $3
		  AND cache_lease_revision = $4 AND fencing_token = $5 AND status = $9`,
		intent.TenantID, intent.ID, intent.CacheLeaseID, intent.CacheLeaseRevision,
		intent.FencingToken, status, providerResult, intent.Error, intent.Status, candidate,
	)
	if err != nil {
		return Intent{}, err
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return Intent{}, errors.New("cache refresh intent fencing or state conflict")
	}
	if status == IntentSucceeded {
		expiresAt := intent.ProviderResult.ExpiresAt
		if expiresAt.IsZero() {
			return Intent{}, errors.New("successful cache refresh did not report an expiry")
		}
		if err := ensureRefreshPriceSnapshot(ctx, tx, intent.Candidate.RefreshPriceSnapshot); err != nil {
			return Intent{}, err
		}
		refreshCost := actualRefreshCost(intent)
		providerUsage := intent.ProviderResult.ProviderUsage
		if len(providerUsage) == 0 || !json.Valid(providerUsage) {
			providerUsage = json.RawMessage(`{}`)
		}
		usageID := intent.ID + "_usage"
		usage := intent.ProviderResult.Usage
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO cache_refresh_usage_ledger (
				id, tenant_id, cache_refresh_intent_id, cache_lease_id, price_snapshot_id,
				provider_usage, input_tokens, cached_input_tokens, cache_write_input_tokens,
				output_tokens, amount, currency, usage_reliable, created_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
			usageID, intent.TenantID, intent.ID, intent.CacheLeaseID,
			intent.Candidate.RefreshPriceSnapshot.ID, providerUsage, usage.InputTokens,
			usage.CachedInputTokens, usage.CacheWriteInputTokens, usage.OutputTokens,
			refreshCost, intent.Candidate.RefreshPriceSnapshot.Currency, intent.ProviderResult.UsageReliable, intent.UpdatedAt,
		); err != nil {
			return Intent{}, fmt.Errorf("insert immutable refresh usage: %w", err)
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE cache_leases
			SET revision = revision + 1, fencing_token = fencing_token + 1,
			    original_expires_at = $11, estimated_expires_at = $6, status = 'refreshed',
			    refresh_count = refresh_count + 1, spent_micros = spent_micros + $7,
			    last_refresh_succeeded_at = $8, last_refresh_expires_at = $6,
			    last_refresh_cost_micros = $7, last_forecast_cost_micros = $9,
			    last_route_lock_cost_micros = $10, last_refresh_usage_id = $12,
			    last_refresh_provider_usage = $13, updated_at = $8
			WHERE tenant_id = $1 AND id = $2 AND revision = $3 AND fencing_token = $4
			  AND route_id = $5`,
			intent.TenantID, intent.CacheLeaseID, intent.CacheLeaseRevision, intent.FencingToken,
			intent.Anchor.RouteID, expiresAt, refreshCost,
			intent.UpdatedAt, intent.Candidate.Forecast.CostMicros,
			intent.Candidate.Economics.RouteLockOpportunityCostMicros, intent.Candidate.Lease.EstimatedExpiresAt,
			usageID, providerUsage,
		)
		if err != nil {
			return Intent{}, err
		}
		rows, _ := result.RowsAffected()
		if rows != 1 {
			return Intent{}, errors.New("cache lease fencing conflict after refresh")
		}
		outboxPayload, _ := json.Marshal(map[string]any{
			"usage_id": usageID, "cache_refresh_intent_id": intent.ID,
			"cache_lease_id": intent.CacheLeaseID, "amount_micros": refreshCost,
			"currency": intent.Candidate.RefreshPriceSnapshot.Currency,
		})
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO transactional_outbox (
				tenant_id, aggregate_type, aggregate_id, aggregate_revision, event_type, payload
			) VALUES ($1,'cache_refresh',$2,$3,'cache_refresh.usage_recorded',$4)
			ON CONFLICT DO NOTHING`, intent.TenantID, intent.ID, intent.CacheLeaseRevision, outboxPayload); err != nil {
			return Intent{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Intent{}, err
	}
	intent.Status = status
	intent.UpdatedAt = time.Now().UTC()
	return intent, nil
}

func reserveRefreshSessionBudget(ctx context.Context, tx *sql.Tx, intent Intent) (bool, error) {
	candidate := intent.Candidate
	if candidate.Policy.ShadowMode || candidate.HoldoutCohort != "treatment" ||
		candidate.RefreshBudgetRevision == "" || candidate.SessionIdentity == "" {
		return true, nil
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO cache_refresh_session_budgets (
			tenant_id, budget_revision, session_identity, refresh_intent_id
		) VALUES ($1,$2,$3,$4)
		ON CONFLICT (tenant_id, budget_revision, session_identity) DO NOTHING`,
		intent.TenantID, candidate.RefreshBudgetRevision, candidate.SessionIdentity, intent.ID,
	)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

func (r *PostgresIntentRepository) CustomerRequest(ctx context.Context, anchor provider.CacheAnchor, requestedAt time.Time) (CustomerRequestResult, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return CustomerRequestResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `
		UPDATE cache_refresh_intents i
		SET status = 'cancelled', updated_at = $9
		FROM cache_leases l
		WHERE i.tenant_id = l.tenant_id AND i.cache_lease_id = l.id
		  AND i.status = 'planned'
		  AND l.tenant_id = $1 AND l.route_id = $2 AND l.provider = $3 AND l.model = $4
		  AND l.credential_scope = $5 AND l.region = $6 AND l.cache_key = $7 AND l.prefix_hash = $8`,
		anchor.TenantID, anchor.RouteID, anchor.Provider, anchor.Model, anchor.CredentialScope,
		anchor.Region, anchor.CacheKey, anchor.PrefixHash, requestedAt,
	)
	if err != nil {
		return CustomerRequestResult{}, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return CustomerRequestResult{}, err
	}
	requestResult := CustomerRequestResult{Cancelled: int(rows)}
	var candidate ProtectedHitCandidate
	err = tx.QueryRowContext(ctx, `
		SELECT l.id, l.original_expires_at, l.last_refresh_succeeded_at,
		       l.last_refresh_expires_at, l.last_refresh_cost_micros,
		       l.last_forecast_cost_micros, l.last_storage_cost_micros,
		       l.last_route_lock_cost_micros, COALESCE(l.last_refresh_usage_id,''),
		       COALESCE(l.last_refresh_provider_usage,'{}'::jsonb),
		       COALESCE((
		           SELECT i.candidate->>'HoldoutCohort'
		           FROM cache_refresh_intents i
		           WHERE i.tenant_id = l.tenant_id AND i.cache_lease_id = l.id AND i.status = 'succeeded'
		           ORDER BY i.updated_at DESC LIMIT 1
		       ), '')
		FROM cache_leases l
		WHERE l.tenant_id = $1 AND l.route_id = $2 AND l.provider = $3 AND l.model = $4
		  AND l.credential_scope = $5 AND l.region = $6 AND l.cache_key = $7 AND l.prefix_hash = $8
		  AND l.last_refresh_succeeded_at < l.original_expires_at
		  AND $9 > l.original_expires_at AND $9 < l.last_refresh_expires_at
		ORDER BY l.last_refresh_succeeded_at DESC
		LIMIT 1`,
		anchor.TenantID, anchor.RouteID, anchor.Provider, anchor.Model, anchor.CredentialScope,
		anchor.Region, anchor.CacheKey, anchor.PrefixHash, requestedAt,
	).Scan(
		&candidate.CacheLeaseID, &candidate.OriginalLeaseExpiresAt, &candidate.RefreshSucceededAt,
		&candidate.RefreshExpiresAt, &candidate.RefreshCostMicros, &candidate.ForecastCostMicros,
		&candidate.StorageCostMicros, &candidate.RouteLockCostMicros,
		&candidate.RefreshUsageID, &candidate.RefreshProviderUsage, &candidate.HoldoutCohort,
	)
	if err == nil {
		requestResult.ProtectedHitCandidate = &candidate
	} else if !errors.Is(err, sql.ErrNoRows) {
		return CustomerRequestResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return CustomerRequestResult{}, err
	}
	return requestResult, nil
}

func ensureRefreshPriceSnapshot(ctx context.Context, tx *sql.Tx, snapshot core.PriceSnapshot) error {
	if snapshot.ID == "" || snapshot.Provider == "" || snapshot.Model == "" || snapshot.Region == "" ||
		snapshot.Currency == "" || snapshot.Source == "" {
		return errors.New("cache refresh requires an immutable price snapshot")
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO provider_price_snapshots (
			id, provider, model, region, currency, input_per_million, cached_input_per_million,
			cache_write_per_million, output_per_million, effective_at, source
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,to_timestamp($10),$11)
		ON CONFLICT (id) DO NOTHING`,
		snapshot.ID, snapshot.Provider, snapshot.Model, snapshot.Region, snapshot.Currency,
		snapshot.InputPerMillionMicros, snapshot.CachedInputPerMillionMicros,
		snapshot.CacheWritePerMillionMicros, snapshot.OutputPerMillionMicros,
		snapshot.EffectiveAt, snapshot.Source,
	); err != nil {
		return err
	}
	var existing core.PriceSnapshot
	if err := tx.QueryRowContext(ctx, `
		SELECT id, provider, model, region, currency, input_per_million::bigint,
		       cached_input_per_million::bigint, cache_write_per_million::bigint,
		       output_per_million::bigint, extract(epoch FROM effective_at)::bigint, source
		FROM provider_price_snapshots WHERE id = $1`, snapshot.ID).Scan(
		&existing.ID, &existing.Provider, &existing.Model, &existing.Region, &existing.Currency,
		&existing.InputPerMillionMicros, &existing.CachedInputPerMillionMicros,
		&existing.CacheWritePerMillionMicros, &existing.OutputPerMillionMicros,
		&existing.EffectiveAt, &existing.Source,
	); err != nil {
		return err
	}
	if existing != snapshot {
		return errors.New("refresh price snapshot ID already has different immutable values")
	}
	return nil
}

func (r *PostgresIntentRepository) ClaimDue(ctx context.Context, now time.Time, limit int) ([]Intent, error) {
	rows, err := r.db.QueryContext(ctx, `
		WITH due AS (
			SELECT tenant_id, id
			FROM cache_refresh_intents
			WHERE status = 'planned' AND scheduled_for <= $1
			ORDER BY scheduled_for, tenant_id, id
			FOR UPDATE SKIP LOCKED
			LIMIT $2
		)
		UPDATE cache_refresh_intents i
		SET status = 'running', updated_at = now()
		FROM due
		WHERE i.tenant_id = due.tenant_id AND i.id = due.id
		RETURNING i.tenant_id, i.id, i.cache_lease_id, i.cache_lease_revision,
		          i.fencing_token, i.status, i.expected_net_saving::bigint, i.scheduled_for,
		          i.provider_result, i.candidate, COALESCE(i.error,''), i.created_at, i.updated_at`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var intents []Intent
	for rows.Next() {
		intent, err := scanIntent(rows)
		if err != nil {
			return nil, err
		}
		intents = append(intents, intent)
	}
	return intents, rows.Err()
}

type rowScanner interface{ Scan(...any) error }

func getIntentTx(ctx context.Context, tx *sql.Tx, tenantID, leaseID string, revision int64) (Intent, error) {
	row := tx.QueryRowContext(ctx, `
		SELECT tenant_id, id, cache_lease_id, cache_lease_revision, fencing_token, status,
		       expected_net_saving::bigint, scheduled_for, provider_result, candidate,
		       COALESCE(error,''), created_at, updated_at
		FROM cache_refresh_intents
		WHERE tenant_id = $1 AND cache_lease_id = $2 AND cache_lease_revision = $3`, tenantID, leaseID, revision)
	return scanIntent(row)
}

func scanIntent(row rowScanner) (Intent, error) {
	var intent Intent
	var status string
	var providerResult, candidate []byte
	err := row.Scan(
		&intent.TenantID, &intent.ID, &intent.CacheLeaseID, &intent.CacheLeaseRevision,
		&intent.FencingToken, &status, &intent.ExpectedNetSavingMicros, &intent.ScheduledFor,
		&providerResult, &candidate, &intent.Error, &intent.CreatedAt, &intent.UpdatedAt,
	)
	if err != nil {
		return Intent{}, err
	}
	intent.Status = IntentStatus(status)
	if err := json.Unmarshal(providerResult, &intent.ProviderResult); err != nil {
		return Intent{}, err
	}
	if err := json.Unmarshal(candidate, &intent.Candidate); err != nil {
		return Intent{}, err
	}
	intent.Anchor = intent.Candidate.Lease.Anchor
	return intent, nil
}

func validTransition(from, to IntentStatus) bool {
	switch from {
	case IntentPlanned:
		return to == IntentRunning || to == IntentCancelled || to == IntentShadow
	case IntentRunning:
		return to == IntentSucceeded || to == IntentRejected || to == IntentUncertain
	case IntentShadow:
		return to == IntentPlanned
	default:
		return false
	}
}
