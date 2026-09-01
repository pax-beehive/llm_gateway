package quota

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/toddzheng/llm-gateway/internal/core"
)

var ErrReservationNotFound = errors.New("quota reservation not found")

type ReservationRequest struct {
	TenantID                    string
	APIKeyID                    string
	ResponseID                  string
	ResponseAttemptID           string
	CapabilityOperationID       string
	Capability                  core.Capability
	PublicModel                 string
	RouteID                     string
	Region                      string
	HomeRegion                  string
	ExecutionEpoch              int64
	TenantPolicyRevision        int64
	APIKeyPolicyRevision        int64
	TenantLimits                core.QuotaLimits
	APIKeyLimits                core.QuotaLimits
	Requests                    int64
	ReservedInputTokens         int64
	ReservedOutputTokens        int64
	ReservedSpendMicros         int64
	ReservedEmbeddingInputUnits int64
	ReservedRerankDocuments     int64
	Currency                    string
	ExpiresAt                   time.Time
}

type Reservation struct {
	ID          string
	TenantID    string
	APIKeyID    string
	ResponseID  string
	AttemptID   string
	OperationID string
}

type RefreshReservationRequest struct {
	TenantID             string
	APIKeyID             string
	CacheRefreshIntentID string
	TenantPolicyRevision int64
	APIKeyPolicyRevision int64
	TenantLimits         core.QuotaLimits
	APIKeyLimits         core.QuotaLimits
	ReservedSpendMicros  int64
	Currency             string
	ExpiresAt            time.Time
}

type ActualUsage struct {
	Requests            int64
	InputTokens         int64
	OutputTokens        int64
	SpendMicros         int64
	EmbeddingInputUnits int64
	RerankDocuments     int64
}

type Counter struct {
	Reserved  int64
	Committed int64
}

type Snapshot struct {
	MinuteRequests      Counter
	MinuteTokens        Counter
	DailySpend          Counter
	MonthlySpend        Counter
	RefreshDailySpend   Counter
	RefreshMonthlySpend Counter
}

type PostgresController struct {
	db  *sql.DB
	now func() time.Time
}

type Controller interface {
	Reserve(context.Context, ReservationRequest) (Reservation, error)
	Commit(context.Context, string, ActualUsage) error
	Release(context.Context, string) error
	Uncertain(context.Context, string) error
	ReserveRefresh(context.Context, RefreshReservationRequest) (Reservation, error)
	Reconcile(context.Context, int) (int, error)
}

func NewPostgresController(db *sql.DB, now func() time.Time) *PostgresController {
	if now == nil {
		now = time.Now
	}
	return &PostgresController{db: db, now: now}
}

func (c *PostgresController) Reserve(ctx context.Context, request ReservationRequest) (Reservation, error) {
	if err := validateReservationRequest(request); err != nil {
		return Reservation{}, err
	}
	effective, err := EffectiveLimits(request.TenantLimits, request.APIKeyLimits)
	if err != nil {
		return Reservation{}, err
	}
	if request.ReservedSpendMicros > 0 && effective.Currency != request.Currency {
		return Reservation{}, errors.New("quota reservation currency does not match policy")
	}
	if exceeds(effective.MaxInputTokens, request.ReservedInputTokens) {
		return c.rejectReservation(ctx, request, "effective_policy", fmt.Errorf("%w: max input tokens", ErrExceeded))
	}
	if exceeds(effective.MaxOutputTokens, request.ReservedOutputTokens) {
		return c.rejectReservation(ctx, request, "effective_policy", fmt.Errorf("%w: max output tokens", ErrExceeded))
	}
	if exceeds(effective.MaxCostMicros, request.ReservedSpendMicros) {
		return c.rejectReservation(ctx, request, "effective_policy", fmt.Errorf("%w: max request cost", ErrExceeded))
	}
	if exceeds(effective.EmbeddingInputUnits, request.ReservedEmbeddingInputUnits) {
		return c.rejectReservation(ctx, request, "effective_policy", fmt.Errorf("%w: embedding input units", ErrExceeded))
	}
	if exceeds(effective.RerankDocuments, request.ReservedRerankDocuments) {
		return c.rejectReservation(ctx, request, "effective_policy", fmt.Errorf("%w: rerank documents", ErrExceeded))
	}
	now := c.now().UTC()
	minuteStart := now.Truncate(time.Minute)
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	id, err := reservationID()
	if err != nil {
		return Reservation{}, err
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return Reservation{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if request.CapabilityOperationID != "" {
		var homeRegion string
		var executionEpoch int64
		if err := tx.QueryRowContext(ctx, `
			SELECT home_region, execution_epoch FROM gateway_lock_tenant_fence($1)`, request.TenantID).Scan(
			&homeRegion, &executionEpoch,
		); errors.Is(err, sql.ErrNoRows) {
			return Reservation{}, errors.New("capability quota writer fencing conflict")
		} else if err != nil {
			return Reservation{}, err
		}
		if homeRegion != request.HomeRegion || executionEpoch != request.ExecutionEpoch {
			return Reservation{}, errors.New("capability quota writer fencing conflict")
		}
	}
	if err := lockQuotaScopes(ctx, tx, request.TenantID, request.APIKeyID); err != nil {
		return Reservation{}, err
	}
	tokens := request.ReservedInputTokens + request.ReservedOutputTokens
	if err := reserveScope(ctx, tx, counterScope{tenantID: request.TenantID}, request.TenantLimits,
		request.Requests, tokens, request.ReservedSpendMicros, request.Currency, minuteStart, dayStart, monthStart); err != nil {
		if errors.Is(err, ErrExceeded) {
			_ = tx.Rollback()
			return c.rejectReservation(ctx, request, "tenant", err)
		}
		return Reservation{}, err
	}
	if err := reserveScope(ctx, tx, counterScope{tenantID: request.TenantID, apiKeyID: request.APIKeyID}, effective,
		request.Requests, tokens, request.ReservedSpendMicros, request.Currency, minuteStart, dayStart, monthStart); err != nil {
		if errors.Is(err, ErrExceeded) {
			_ = tx.Rollback()
			return c.rejectReservation(ctx, request, "api_key", err)
		}
		return Reservation{}, err
	}
	kind := "response"
	if request.CapabilityOperationID != "" {
		kind = "capability"
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO quota_reservations (
			id, tenant_id, api_key_id, response_id, response_attempt_id, capability_operation_id, capability,
			capability_home_region, capability_execution_epoch, kind,
			tenant_policy_revision, api_key_policy_revision,
			currency, reserved_requests, reserved_input_tokens, reserved_output_tokens,
			reserved_spend_micros, reserved_embedding_input_units, reserved_rerank_documents,
			minute_window_start, day_window_start, month_window_start,
			status, expires_at
		) VALUES ($1,$2,$3,NULLIF($4,''),NULLIF($5,''),NULLIF($6,''),NULLIF($7,''),NULLIF($8,''),NULLIF($9,0),$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,'reserved',$23)`,
		id, request.TenantID, request.APIKeyID, request.ResponseID, request.ResponseAttemptID, request.CapabilityOperationID, request.Capability,
		request.HomeRegion, request.ExecutionEpoch, kind,
		request.TenantPolicyRevision, request.APIKeyPolicyRevision, request.Currency,
		request.Requests, request.ReservedInputTokens, request.ReservedOutputTokens,
		request.ReservedSpendMicros, request.ReservedEmbeddingInputUnits, request.ReservedRerankDocuments,
		minuteStart, dayStart, monthStart, request.ExpiresAt,
	); err != nil {
		return Reservation{}, fmt.Errorf("persist quota reservation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Reservation{}, err
	}
	return Reservation{
		ID: id, TenantID: request.TenantID, APIKeyID: request.APIKeyID,
		ResponseID: request.ResponseID, AttemptID: request.ResponseAttemptID, OperationID: request.CapabilityOperationID,
	}, nil
}

func (c *PostgresController) ReserveRefresh(ctx context.Context, request RefreshReservationRequest) (Reservation, error) {
	if request.TenantID == "" || request.APIKeyID == "" || request.CacheRefreshIntentID == "" {
		return Reservation{}, errors.New("refresh quota reservation requires Tenant, API key, and intent identities")
	}
	if request.TenantPolicyRevision <= 0 || request.APIKeyPolicyRevision <= 0 {
		return Reservation{}, errors.New("refresh quota reservation requires positive policy revisions")
	}
	if request.ReservedSpendMicros < 0 || request.ExpiresAt.IsZero() {
		return Reservation{}, errors.New("refresh quota reservation requires non-negative spend and an expiry")
	}
	effective, err := EffectiveLimits(request.TenantLimits, request.APIKeyLimits)
	if err != nil {
		return Reservation{}, err
	}
	if request.ReservedSpendMicros > 0 && (request.Currency == "" || effective.Currency != request.Currency) {
		return Reservation{}, errors.New("refresh quota reservation currency does not match policy")
	}
	now := c.now().UTC()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	id, err := reservationID()
	if err != nil {
		return Reservation{}, err
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return Reservation{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockQuotaScopes(ctx, tx, request.TenantID, request.APIKeyID); err != nil {
		return Reservation{}, err
	}
	if err := reserveRefreshScope(ctx, tx, counterScope{tenantID: request.TenantID}, request.TenantLimits,
		request.ReservedSpendMicros, request.Currency, dayStart, monthStart); err != nil {
		if errors.Is(err, ErrExceeded) {
			_ = tx.Rollback()
			return c.rejectRefreshReservation(ctx, request, "tenant", err)
		}
		return Reservation{}, err
	}
	if err := reserveRefreshScope(ctx, tx, counterScope{tenantID: request.TenantID, apiKeyID: request.APIKeyID}, effective,
		request.ReservedSpendMicros, request.Currency, dayStart, monthStart); err != nil {
		if errors.Is(err, ErrExceeded) {
			_ = tx.Rollback()
			return c.rejectRefreshReservation(ctx, request, "api_key", err)
		}
		return Reservation{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO quota_reservations (
			id, tenant_id, api_key_id, response_id, cache_refresh_intent_id, kind,
			tenant_policy_revision, api_key_policy_revision, currency,
			reserved_requests, reserved_input_tokens, reserved_output_tokens, reserved_spend_micros,
			minute_window_start, day_window_start, month_window_start, status, expires_at
		) VALUES ($1,$2,$3,NULL,$4,'cache_refresh',$5,$6,$7,0,0,0,$8,$9,$10,$11,'reserved',$12)`,
		id, request.TenantID, request.APIKeyID, request.CacheRefreshIntentID,
		request.TenantPolicyRevision, request.APIKeyPolicyRevision, request.Currency, request.ReservedSpendMicros,
		now.Truncate(time.Minute), dayStart, monthStart, request.ExpiresAt); err != nil {
		return Reservation{}, fmt.Errorf("persist refresh quota reservation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Reservation{}, err
	}
	return Reservation{ID: id, TenantID: request.TenantID, APIKeyID: request.APIKeyID}, nil
}

func (c *PostgresController) Commit(ctx context.Context, reservationID string, actual ActualUsage) error {
	if actual.Requests < 0 || actual.InputTokens < 0 || actual.OutputTokens < 0 || actual.SpendMicros < 0 ||
		actual.EmbeddingInputUnits < 0 || actual.RerankDocuments < 0 {
		return errors.New("actual quota usage cannot be negative")
	}
	if actual.InputTokens > int64(^uint64(0)>>1)-actual.OutputTokens {
		return errors.New("actual quota token total overflows int64")
	}
	return c.finish(ctx, reservationID, "committed", actual)
}

func (c *PostgresController) Release(ctx context.Context, reservationID string) error {
	return c.finish(ctx, reservationID, "released", ActualUsage{})
}

func (c *PostgresController) Uncertain(ctx context.Context, reservationID string) error {
	result, err := c.db.ExecContext(ctx, `
		UPDATE quota_reservations SET uncertain_at = COALESCE(uncertain_at, now()), updated_at = now()
		WHERE id = $1 AND status = 'reserved'`, reservationID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		var status string
		if scanErr := c.db.QueryRowContext(ctx, `SELECT status FROM quota_reservations WHERE id = $1`, reservationID).Scan(&status); errors.Is(scanErr, sql.ErrNoRows) {
			return ErrReservationNotFound
		} else if scanErr != nil {
			return scanErr
		}
		if status == "committed" || status == "uncertain" {
			return nil
		}
		return fmt.Errorf("quota reservation is already %s", status)
	}
	return nil
}

func (c *PostgresController) APIKeySnapshot(ctx context.Context, tenantID, apiKeyID, currency string, at time.Time) (Snapshot, error) {
	at = at.UTC()
	minuteStart := at.Truncate(time.Minute)
	dayStart := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
	monthStart := time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, time.UTC)
	var snapshot Snapshot
	queries := []struct {
		metric string
		start  time.Time
		result *Counter
	}{
		{"requests", minuteStart, &snapshot.MinuteRequests},
		{"tokens", minuteStart, &snapshot.MinuteTokens},
		{financialMetric("spend_micros", currency), dayStart, &snapshot.DailySpend},
		{financialMetric("spend_micros", currency), monthStart, &snapshot.MonthlySpend},
		{financialMetric("refresh_spend_micros", currency), dayStart, &snapshot.RefreshDailySpend},
		{financialMetric("refresh_spend_micros", currency), monthStart, &snapshot.RefreshMonthlySpend},
	}
	for _, query := range queries {
		err := c.db.QueryRowContext(ctx, `
			SELECT reserved_amount, committed_amount
			FROM api_key_quota_counters
			WHERE tenant_id = $1 AND api_key_id = $2 AND metric = $3 AND window_start = $4`,
			tenantID, apiKeyID, query.metric, query.start,
		).Scan(&query.result.Reserved, &query.result.Committed)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return Snapshot{}, err
		}
	}
	return snapshot, nil
}

type reservationRecord struct {
	id                          string
	tenantID                    string
	apiKeyID                    string
	status                      string
	kind                        string
	responseID                  string
	refreshIntentID             string
	capabilityOperationID       string
	capability                  core.Capability
	currency                    string
	reservedRequests            int64
	reservedInputTokens         int64
	reservedOutputTokens        int64
	reservedSpendMicros         int64
	reservedEmbeddingInputUnits int64
	reservedRerankDocuments     int64
	minuteStart                 time.Time
	dayStart                    time.Time
	monthStart                  time.Time
}

func (c *PostgresController) finish(ctx context.Context, reservationID, targetStatus string, actual ActualUsage) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := finishReservationTx(ctx, tx, reservationID, "", "", "", targetStatus, actual); err != nil {
		return err
	}
	return tx.Commit()
}

// CommitReservationTx settles quota inside the caller's authoritative usage
// transaction. The caller must commit or roll back the transaction.
func CommitReservationTx(ctx context.Context, tx *sql.Tx, reservationID, tenantID, apiKeyID, kind string, actual ActualUsage) error {
	if tx == nil || reservationID == "" || tenantID == "" || apiKeyID == "" || kind == "" {
		return ErrReservationNotFound
	}
	if actual.Requests < 0 || actual.InputTokens < 0 || actual.OutputTokens < 0 || actual.SpendMicros < 0 || actual.EmbeddingInputUnits < 0 || actual.RerankDocuments < 0 {
		return errors.New("actual quota usage cannot be negative")
	}
	if actual.InputTokens > int64(^uint64(0)>>1)-actual.OutputTokens {
		return errors.New("actual quota token total overflows int64")
	}
	return finishReservationTx(ctx, tx, reservationID, tenantID, apiKeyID, kind, "committed", actual)
}

func finishReservationTx(ctx context.Context, tx *sql.Tx, reservationID, tenantID, apiKeyID, kind, targetStatus string, actual ActualUsage) error {
	var record reservationRecord
	err := tx.QueryRowContext(ctx, `
		SELECT id, tenant_id, api_key_id, status, kind, COALESCE(response_id,''), COALESCE(cache_refresh_intent_id,''),
		       COALESCE(capability_operation_id,''), COALESCE(capability,''), currency, reserved_requests,
		       reserved_input_tokens, reserved_output_tokens, reserved_spend_micros,
		       reserved_embedding_input_units, reserved_rerank_documents,
		       minute_window_start, day_window_start, month_window_start
		FROM quota_reservations
		WHERE id = $1 AND ($2 = '' OR tenant_id = $2) AND ($3 = '' OR api_key_id = $3) AND ($4 = '' OR kind = $4)
		FOR UPDATE`, reservationID, tenantID, apiKeyID, kind).Scan(
		&record.id, &record.tenantID, &record.apiKeyID, &record.status, &record.kind, &record.responseID, &record.refreshIntentID,
		&record.capabilityOperationID, &record.capability, &record.currency, &record.reservedRequests,
		&record.reservedInputTokens, &record.reservedOutputTokens, &record.reservedSpendMicros,
		&record.reservedEmbeddingInputUnits, &record.reservedRerankDocuments,
		&record.minuteStart, &record.dayStart, &record.monthStart,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrReservationNotFound
	}
	if err != nil {
		return err
	}
	if record.status == targetStatus {
		return nil
	}
	if record.status != "reserved" {
		return fmt.Errorf("quota reservation is already %s", record.status)
	}
	if err := settleReservationTx(ctx, tx, record, targetStatus, actual); err != nil {
		return err
	}
	return nil
}

func (c *PostgresController) Reconcile(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		return 0, errors.New("quota reconciliation limit must be positive")
	}
	rows, err := c.db.QueryContext(ctx, `
		SELECT id FROM quota_reservations
		WHERE status = 'reserved' AND (
			expires_at <= $1
			OR (kind = 'response' AND EXISTS (
				SELECT 1 FROM usage_ledger u WHERE u.tenant_id = quota_reservations.tenant_id
				  AND u.quota_reservation_id = quota_reservations.id))
			OR (kind = 'cache_refresh' AND EXISTS (
				SELECT 1 FROM cache_refresh_usage_ledger u WHERE u.tenant_id = quota_reservations.tenant_id
				  AND u.cache_refresh_intent_id = quota_reservations.cache_refresh_intent_id))
			OR (kind = 'capability' AND EXISTS (
				SELECT 1 FROM capability_usage_ledger u WHERE u.tenant_id = quota_reservations.tenant_id
				  AND u.quota_reservation_id = quota_reservations.id))
		)
		ORDER BY expires_at, id LIMIT $2`, c.now().UTC(), limit)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	completed := 0
	for _, id := range ids {
		settled, err := c.reconcileOne(ctx, id)
		if err != nil {
			return completed, err
		}
		if settled {
			completed++
		}
	}
	return completed, nil
}

func (c *PostgresController) reconcileOne(ctx context.Context, id string) (bool, error) {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var record reservationRecord
	err = tx.QueryRowContext(ctx, `
		SELECT id, tenant_id, api_key_id, status, kind, COALESCE(response_id,''), COALESCE(cache_refresh_intent_id,''),
		       COALESCE(capability_operation_id,''), COALESCE(capability,''), currency,
		       reserved_requests, reserved_input_tokens, reserved_output_tokens, reserved_spend_micros,
		       reserved_embedding_input_units, reserved_rerank_documents,
		       minute_window_start, day_window_start, month_window_start
		FROM quota_reservations WHERE id = $1 FOR UPDATE`, id).Scan(
		&record.id, &record.tenantID, &record.apiKeyID, &record.status, &record.kind, &record.responseID, &record.refreshIntentID,
		&record.capabilityOperationID, &record.capability, &record.currency,
		&record.reservedRequests, &record.reservedInputTokens, &record.reservedOutputTokens, &record.reservedSpendMicros,
		&record.reservedEmbeddingInputUnits, &record.reservedRerankDocuments,
		&record.minuteStart, &record.dayStart, &record.monthStart)
	if errors.Is(err, sql.ErrNoRows) {
		return false, tx.Commit()
	}
	if err != nil {
		return false, err
	}
	if record.status != "reserved" {
		return false, tx.Commit()
	}
	actual, found, err := reservationUsageTx(ctx, tx, record)
	if err != nil {
		return false, err
	}
	target := "committed"
	if !found {
		var expired bool
		if err := tx.QueryRowContext(ctx, `SELECT expires_at <= $2 FROM quota_reservations WHERE id = $1`, id, c.now().UTC()).Scan(&expired); err != nil {
			return false, err
		}
		if !expired {
			return false, tx.Commit()
		}
		target = "uncertain"
		actual = ActualUsage{Requests: record.reservedRequests, InputTokens: record.reservedInputTokens,
			OutputTokens: record.reservedOutputTokens, SpendMicros: record.reservedSpendMicros,
			EmbeddingInputUnits: record.reservedEmbeddingInputUnits, RerankDocuments: record.reservedRerankDocuments}
	}
	if err := settleReservationTx(ctx, tx, record, target, actual); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func reservationUsageTx(ctx context.Context, tx *sql.Tx, record reservationRecord) (ActualUsage, bool, error) {
	var actual ActualUsage
	var err error
	if record.kind == "cache_refresh" {
		err = tx.QueryRowContext(ctx, `
			SELECT 0, input_tokens, output_tokens, amount::bigint
			FROM cache_refresh_usage_ledger
			WHERE tenant_id = $1 AND cache_refresh_intent_id = $2`, record.tenantID, record.refreshIntentID).Scan(
			&actual.Requests, &actual.InputTokens, &actual.OutputTokens, &actual.SpendMicros)
	} else if record.kind == "capability" {
		err = tx.QueryRowContext(ctx, `
			SELECT 1, 0, 0, amount_micros,
			       CASE WHEN capability = 'embeddings' THEN input_units ELSE 0 END,
			       CASE WHEN capability = 'rerank' THEN documents ELSE 0 END
			FROM capability_usage_ledger
			WHERE tenant_id = $1 AND quota_reservation_id = $2`, record.tenantID, record.id).Scan(
			&actual.Requests, &actual.InputTokens, &actual.OutputTokens, &actual.SpendMicros,
			&actual.EmbeddingInputUnits, &actual.RerankDocuments)
	} else {
		err = tx.QueryRowContext(ctx, `
			SELECT 1, input_tokens, output_tokens, amount::bigint
			FROM usage_ledger WHERE tenant_id = $1 AND quota_reservation_id = $2`, record.tenantID, record.id).Scan(
			&actual.Requests, &actual.InputTokens, &actual.OutputTokens, &actual.SpendMicros)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ActualUsage{}, false, nil
	}
	return actual, err == nil, err
}

func settleReservationTx(ctx context.Context, tx *sql.Tx, record reservationRecord, targetStatus string, actual ActualUsage) error {
	if err := lockQuotaScopes(ctx, tx, record.tenantID, record.apiKeyID); err != nil {
		return err
	}
	if targetStatus == "released" {
		actual = ActualUsage{}
	}
	for _, scope := range []counterScope{{tenantID: record.tenantID}, {tenantID: record.tenantID, apiKeyID: record.apiKeyID}} {
		if record.kind == "cache_refresh" {
			if err := settleCounter(ctx, tx, scope, financialMetric("refresh_spend_micros", record.currency), record.dayStart, record.reservedSpendMicros, actual.SpendMicros); err != nil {
				return err
			}
			if err := settleCounter(ctx, tx, scope, financialMetric("refresh_spend_micros", record.currency), record.monthStart, record.reservedSpendMicros, actual.SpendMicros); err != nil {
				return err
			}
			continue
		}
		reservedTokens := record.reservedInputTokens + record.reservedOutputTokens
		actualTokens := actual.InputTokens + actual.OutputTokens
		if err := settleCounter(ctx, tx, scope, "requests", record.minuteStart, record.reservedRequests, actual.Requests); err != nil {
			return err
		}
		if err := settleCounter(ctx, tx, scope, "tokens", record.minuteStart, reservedTokens, actualTokens); err != nil {
			return err
		}
		if err := settleCounter(ctx, tx, scope, financialMetric("spend_micros", record.currency), record.dayStart, record.reservedSpendMicros, actual.SpendMicros); err != nil {
			return err
		}
		if err := settleCounter(ctx, tx, scope, financialMetric("spend_micros", record.currency), record.monthStart, record.reservedSpendMicros, actual.SpendMicros); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE quota_reservations
		SET status = $2, committed_requests = $3, committed_input_tokens = $4,
		    committed_output_tokens = $5, committed_spend_micros = $6,
		    committed_embedding_input_units = $7, committed_rerank_documents = $8, updated_at = now()
		WHERE id = $1`, record.id, targetStatus, actual.Requests, actual.InputTokens, actual.OutputTokens, actual.SpendMicros,
		actual.EmbeddingInputUnits, actual.RerankDocuments)
	return err
}

type counterScope struct {
	tenantID string
	apiKeyID string
}

func reserveScope(ctx context.Context, tx *sql.Tx, scope counterScope, limits core.QuotaLimits,
	requests, tokens, spend int64, currency string, minuteStart, dayStart, monthStart time.Time,
) error {
	checks := []struct {
		metric string
		start  time.Time
		end    time.Time
		amount int64
		limit  *int64
	}{
		{"requests", minuteStart, minuteStart.Add(time.Minute), requests, limits.RequestsPerMinute},
		{"tokens", minuteStart, minuteStart.Add(time.Minute), tokens, limits.TokensPerMinute},
		{financialMetric("spend_micros", currency), dayStart, dayStart.AddDate(0, 0, 1), spend, limits.DailySpendMicros},
		{financialMetric("spend_micros", currency), monthStart, monthStart.AddDate(0, 1, 0), spend, limits.MonthlySpendMicros},
	}
	for _, check := range checks {
		if check.limit == nil {
			continue
		}
		if err := reserveCounter(ctx, tx, scope, check.metric, check.start, check.end, check.amount, *check.limit); err != nil {
			return err
		}
	}
	return nil
}

func reserveRefreshScope(ctx context.Context, tx *sql.Tx, scope counterScope, limits core.QuotaLimits,
	spend int64, currency string, dayStart, monthStart time.Time,
) error {
	checks := []struct {
		start time.Time
		end   time.Time
		limit *int64
	}{
		{dayStart, dayStart.AddDate(0, 0, 1), limits.RefreshDailySpendMicros},
		{monthStart, monthStart.AddDate(0, 1, 0), limits.RefreshMonthlySpendMicros},
	}
	for _, check := range checks {
		if check.limit != nil {
			if err := reserveCounter(ctx, tx, scope, financialMetric("refresh_spend_micros", currency), check.start, check.end, spend, *check.limit); err != nil {
				return err
			}
		}
	}
	return nil
}

func financialMetric(name, currency string) string {
	return name + "/" + currency
}

func reserveCounter(ctx context.Context, tx *sql.Tx, scope counterScope, metric string, start, end time.Time, amount, limit int64) error {
	if amount > limit {
		return fmt.Errorf("%w: %s", ErrExceeded, metric)
	}
	var one int
	var err error
	if scope.apiKeyID == "" {
		err = tx.QueryRowContext(ctx, `
			INSERT INTO tenant_quota_counters (
				tenant_id, metric, window_start, window_end, reserved_amount
			) VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (tenant_id, metric, window_start) DO UPDATE
			SET reserved_amount = tenant_quota_counters.reserved_amount + EXCLUDED.reserved_amount,
			    revision = tenant_quota_counters.revision + 1, updated_at = now()
			WHERE tenant_quota_counters.committed_amount + tenant_quota_counters.reserved_amount + EXCLUDED.reserved_amount <= $6
			RETURNING 1`, scope.tenantID, metric, start, end, amount, limit).Scan(&one)
	} else {
		err = tx.QueryRowContext(ctx, `
			INSERT INTO api_key_quota_counters (
				tenant_id, api_key_id, metric, window_start, window_end, reserved_amount
			) VALUES ($1,$2,$3,$4,$5,$6)
			ON CONFLICT (tenant_id, api_key_id, metric, window_start) DO UPDATE
			SET reserved_amount = api_key_quota_counters.reserved_amount + EXCLUDED.reserved_amount,
			    revision = api_key_quota_counters.revision + 1, updated_at = now()
			WHERE api_key_quota_counters.committed_amount + api_key_quota_counters.reserved_amount + EXCLUDED.reserved_amount <= $7
			RETURNING 1`, scope.tenantID, scope.apiKeyID, metric, start, end, amount, limit).Scan(&one)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrExceeded, metric)
	}
	return err
}

func settleCounter(ctx context.Context, tx *sql.Tx, scope counterScope, metric string, start time.Time, reserved, committed int64) error {
	if scope.apiKeyID == "" {
		_, err := tx.ExecContext(ctx, `
			UPDATE tenant_quota_counters
			SET reserved_amount = GREATEST(0, reserved_amount - $4),
			    committed_amount = committed_amount + $5,
			    revision = revision + 1, updated_at = now()
			WHERE tenant_id = $1 AND metric = $2 AND window_start = $3`,
			scope.tenantID, metric, start, reserved, committed)
		return err
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE api_key_quota_counters
		SET reserved_amount = GREATEST(0, reserved_amount - $5),
		    committed_amount = committed_amount + $6,
		    revision = revision + 1, updated_at = now()
		WHERE tenant_id = $1 AND api_key_id = $2 AND metric = $3 AND window_start = $4`,
		scope.tenantID, scope.apiKeyID, metric, start, reserved, committed)
	return err
}

func lockQuotaScopes(ctx context.Context, tx *sql.Tx, tenantID, apiKeyID string) error {
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "tenant-quota\x1f"+tenantID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "api-key-quota\x1f"+tenantID+"\x1f"+apiKeyID)
	return err
}

func validateReservationRequest(request ReservationRequest) error {
	if request.TenantID == "" || request.APIKeyID == "" {
		return errors.New("quota reservation requires Tenant and API key identities")
	}
	responseIdentity := request.ResponseID != "" && request.ResponseAttemptID != "" && request.CapabilityOperationID == "" && request.Capability == ""
	capabilityIdentity := request.ResponseID == "" && request.ResponseAttemptID == "" && request.CapabilityOperationID != "" &&
		(request.Capability == core.CapabilityEmbeddings || request.Capability == core.CapabilityModeration || request.Capability == core.CapabilityRerank)
	if !responseIdentity && !capabilityIdentity {
		return errors.New("quota reservation requires exactly one Response or capability operation identity")
	}
	if capabilityIdentity && (request.HomeRegion == "" || request.ExecutionEpoch <= 0) {
		return errors.New("capability quota reservation requires Home Region and execution epoch")
	}
	if request.TenantPolicyRevision <= 0 || request.APIKeyPolicyRevision <= 0 {
		return errors.New("quota reservation requires positive policy revisions")
	}
	if request.Requests < 0 || request.ReservedInputTokens < 0 || request.ReservedOutputTokens < 0 || request.ReservedSpendMicros < 0 ||
		request.ReservedEmbeddingInputUnits < 0 || request.ReservedRerankDocuments < 0 {
		return errors.New("reserved quota amounts cannot be negative")
	}
	if request.ReservedInputTokens > int64(^uint64(0)>>1)-request.ReservedOutputTokens {
		return errors.New("reserved quota token total overflows int64")
	}
	if request.ReservedSpendMicros > 0 && request.Currency == "" {
		return errors.New("quota reservation currency is required")
	}
	if request.ExpiresAt.IsZero() {
		return errors.New("quota reservation expiry is required")
	}
	return nil
}

func exceeds(limit *int64, amount int64) bool { return limit != nil && amount > *limit }

func reservationID() (string, error) {
	var payload [16]byte
	if _, err := rand.Read(payload[:]); err != nil {
		return "", err
	}
	return "qres_" + hex.EncodeToString(payload[:]), nil
}
