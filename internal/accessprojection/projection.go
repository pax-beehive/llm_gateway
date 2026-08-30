package accessprojection

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/toddzheng/llm-gateway/internal/access"
	"github.com/toddzheng/llm-gateway/internal/controlevent"
	"github.com/toddzheng/llm-gateway/internal/core"
)

//go:embed migrations/000001_access_projection.sql
var migration string

var (
	ErrRevisionGap  = errors.New("Gateway Access Projection revision gap")
	ErrInvalidEvent = errors.New("invalid Gateway Access Projection event")
)

type PepperRing struct {
	CurrentVersion int16
	Peppers        map[int16][]byte
}

type ControlEvent struct {
	EventID           string
	DeliverySequence  int64
	SchemaVersion     int
	AggregateType     string
	AggregateID       string
	AggregateRevision int64
	TenantID          string
	EventType         string
	OccurredAt        time.Time
	Payload           json.RawMessage
}

type Disposition string

const (
	DispositionApplied   Disposition = "applied"
	DispositionDuplicate Disposition = "duplicate"
	DispositionStale     Disposition = "stale"
	DispositionGap       Disposition = "gap"
)

type ApplyResult struct {
	Disposition Disposition
	Lag         time.Duration
}

// Compatibility aliases keep existing projection callers source-compatible;
// the snapshot contract itself is owned by the authoritative access domain.
type Snapshot = access.Snapshot
type TenantSnapshot = access.TenantSnapshot
type KeySnapshot = access.KeySnapshot

type Status struct {
	GapCount                int
	OldestGapAt             *time.Time
	HeadCount               int
	MaxAggregateRevision    int64
	MaxDeliverySequence     int64
	PendingEventCount       int
	OldestPendingAt         *time.Time
	LastAppliedAt           *time.Time
	MaxApplyLag             time.Duration
	DeliveryLag             time.Duration
	LastRevocationAppliedAt *time.Time
	MaxRevocationApplyLag   time.Duration
}

type Store struct {
	database          *sql.DB
	peppers           map[int16][]byte
	current           int16
	now               func() time.Time
	usageMu           sync.Mutex
	recentUsage       map[string]time.Time
	usageObservations chan usageObservation
}

type usageObservation struct {
	apiKeyID string
	usedAt   time.Time
}

func Migrate(ctx context.Context, database *sql.DB) error {
	if database == nil {
		return errors.New("Gateway Access Projection migration requires PostgreSQL")
	}
	if _, err := database.ExecContext(ctx, migration); err != nil {
		return fmt.Errorf("migrate Gateway Access Projection: %w", err)
	}
	return nil
}

func New(database *sql.DB, ring PepperRing, now func() time.Time) (*Store, error) {
	if database == nil {
		return nil, errors.New("Gateway Access Projection requires PostgreSQL")
	}
	if ring.CurrentVersion <= 0 || len(ring.Peppers) == 0 || len(ring.Peppers) > 8 {
		return nil, errors.New("Gateway Access Projection requires one to eight digest peppers and a positive current version")
	}
	peppers := make(map[int16][]byte, len(ring.Peppers))
	for version, pepper := range ring.Peppers {
		if version <= 0 || len(pepper) < 16 {
			return nil, errors.New("Gateway Access Projection peppers require positive versions and at least 16 bytes")
		}
		peppers[version] = append([]byte(nil), pepper...)
	}
	if len(peppers[ring.CurrentVersion]) == 0 {
		return nil, errors.New("Gateway Access Projection current digest version has no pepper")
	}
	if now == nil {
		now = time.Now
	}
	return &Store{
		database: database, peppers: peppers, current: ring.CurrentVersion, now: now,
		recentUsage: make(map[string]time.Time), usageObservations: make(chan usageObservation, 4096),
	}, nil
}

func (store *Store) Apply(ctx context.Context, event ControlEvent) (ApplyResult, error) {
	if err := validateEvent(event); err != nil {
		return ApplyResult{}, err
	}
	transaction, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return ApplyResult{}, err
	}
	defer func() { _ = transaction.Rollback() }()
	if err := lockTenantProjection(ctx, transaction, event.TenantID); err != nil {
		return ApplyResult{}, err
	}
	var exists bool
	if err := transaction.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM gateway_access_inbox WHERE event_id = $1)`, event.EventID).Scan(&exists); err != nil {
		return ApplyResult{}, err
	}
	if exists {
		if err := transaction.Commit(); err != nil {
			return ApplyResult{}, err
		}
		return ApplyResult{Disposition: DispositionDuplicate}, nil
	}
	current, err := lockHead(ctx, transaction, event.AggregateType, event.AggregateID)
	if err != nil {
		return ApplyResult{}, err
	}
	if event.AggregateRevision <= current {
		if err := recordInbox(ctx, transaction, event, DispositionStale, store.now().UTC()); err != nil {
			return ApplyResult{}, err
		}
		if err := transaction.Commit(); err != nil {
			return ApplyResult{}, err
		}
		return ApplyResult{Disposition: DispositionStale}, nil
	}
	if event.AggregateRevision != current+1 {
		observedAt := store.now().UTC()
		if _, err := transaction.ExecContext(ctx, `
			INSERT INTO gateway_access_gaps (
				aggregate_type, aggregate_id, expected_revision, received_revision, detected_at, last_event_id,
				delivery_sequence, event_occurred_at
			) VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7::bigint,0),$8)
			ON CONFLICT (aggregate_type, aggregate_id) DO UPDATE SET
				expected_revision = EXCLUDED.expected_revision,
				received_revision = GREATEST(gateway_access_gaps.received_revision, EXCLUDED.received_revision),
				detected_at = LEAST(gateway_access_gaps.detected_at, EXCLUDED.detected_at),
				last_event_id = EXCLUDED.last_event_id,
				delivery_sequence = EXCLUDED.delivery_sequence,
				event_occurred_at = EXCLUDED.event_occurred_at`,
			event.AggregateType, event.AggregateID, current+1, event.AggregateRevision, observedAt, event.EventID,
			event.DeliverySequence, event.OccurredAt); err != nil {
			return ApplyResult{}, err
		}
		if err := recordRolloutReceipt(ctx, transaction, event, "rejected", "revision_gap", observedAt); err != nil {
			return ApplyResult{}, err
		}
		if err := transaction.Commit(); err != nil {
			return ApplyResult{}, err
		}
		return ApplyResult{Disposition: DispositionGap}, fmt.Errorf("%w: %s/%s expected %d received %d", ErrRevisionGap, event.AggregateType, event.AggregateID, current+1, event.AggregateRevision)
	}
	if err := applyEvent(ctx, transaction, event, store.now().UTC()); err != nil {
		return ApplyResult{}, err
	}
	if err := recordInbox(ctx, transaction, event, DispositionApplied, store.now().UTC()); err != nil {
		return ApplyResult{}, err
	}
	if _, err := transaction.ExecContext(ctx, `
		INSERT INTO gateway_access_heads (aggregate_type, aggregate_id, revision, updated_at)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (aggregate_type, aggregate_id) DO UPDATE SET revision = EXCLUDED.revision, updated_at = EXCLUDED.updated_at`,
		event.AggregateType, event.AggregateID, event.AggregateRevision, store.now().UTC()); err != nil {
		return ApplyResult{}, err
	}
	if _, err := transaction.ExecContext(ctx, `DELETE FROM gateway_access_gaps WHERE aggregate_type = $1 AND aggregate_id = $2 AND expected_revision <= $3`,
		event.AggregateType, event.AggregateID, event.AggregateRevision); err != nil {
		return ApplyResult{}, err
	}
	if err := transaction.Commit(); err != nil {
		return ApplyResult{}, err
	}
	lag := store.now().UTC().Sub(event.OccurredAt)
	if lag < 0 {
		lag = 0
	}
	return ApplyResult{Disposition: DispositionApplied, Lag: lag}, nil
}

// Consume applies the access subset of an externally relayed Control Event
// stream. Other aggregate types are handled by sibling Gateway projections.
func (store *Store) Consume(ctx context.Context, event controlevent.Event) error {
	if event.AggregateType != "Tenant" && event.AggregateType != "GatewayAPIKey" {
		return nil
	}
	_, err := store.Apply(ctx, ControlEvent{
		EventID: event.EventID, DeliverySequence: event.DeliverySequence, SchemaVersion: event.SchemaVersion,
		AggregateType: event.AggregateType, AggregateID: event.AggregateID, AggregateRevision: event.AggregateRevision,
		TenantID: event.TenantID, EventType: event.EventType, OccurredAt: event.OccurredAt, Payload: event.Payload,
	})
	return err
}

func validateEvent(event ControlEvent) error {
	if event.EventID == "" || event.SchemaVersion != 2 || event.AggregateID == "" || event.AggregateRevision <= 0 ||
		event.TenantID == "" || event.OccurredAt.IsZero() || len(event.Payload) == 0 || !json.Valid(event.Payload) ||
		(event.AggregateType != "GatewayAPIKey" && event.AggregateType != "Tenant") {
		return ErrInvalidEvent
	}
	return nil
}

func lockHead(ctx context.Context, transaction *sql.Tx, aggregateType, aggregateID string) (int64, error) {
	var revision int64
	err := transaction.QueryRowContext(ctx, `
		SELECT revision FROM gateway_access_heads WHERE aggregate_type = $1 AND aggregate_id = $2 FOR UPDATE`,
		aggregateType, aggregateID).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return revision, err
}

func lockTenantProjection(ctx context.Context, transaction *sql.Tx, tenantID string) error {
	_, err := transaction.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "gateway-access-projection\x1f"+tenantID)
	return err
}

func recordInbox(ctx context.Context, transaction *sql.Tx, event ControlEvent, disposition Disposition, receivedAt time.Time) error {
	lag := receivedAt.Sub(event.OccurredAt).Seconds()
	if lag < 0 {
		lag = 0
	}
	_, err := transaction.ExecContext(ctx, `
		INSERT INTO gateway_access_inbox (
			event_id, delivery_sequence, schema_version, aggregate_type, aggregate_id, aggregate_revision,
			event_type, event_occurred_at, apply_lag_seconds, disposition, received_at
		) VALUES ($1,NULLIF($2::bigint,0),$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		event.EventID, event.DeliverySequence, event.SchemaVersion, event.AggregateType, event.AggregateID, event.AggregateRevision,
		event.EventType, event.OccurredAt, lag, disposition, receivedAt)
	if err != nil {
		return err
	}
	return recordRolloutReceipt(ctx, transaction, event, "applied", "", receivedAt)
}

func recordRolloutReceipt(ctx context.Context, transaction *sql.Tx, event ControlEvent, status, errorCode string, observedAt time.Time) error {
	if event.DeliverySequence <= 0 {
		return nil
	}
	_, err := transaction.ExecContext(ctx, `INSERT INTO gateway_access_rollout_receipts (
		event_id,delivery_sequence,aggregate_type,aggregate_id,aggregate_revision,status,error_code,
		event_occurred_at,observed_at
	) VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8,$9)
	ON CONFLICT DO NOTHING`, event.EventID, event.DeliverySequence, event.AggregateType, event.AggregateID,
		event.AggregateRevision, status, errorCode, event.OccurredAt, observedAt)
	return err
}

type keyPayload struct {
	TenantID             string              `json:"tenant_id"`
	TenantStatus         access.TenantStatus `json:"tenant_status"`
	TenantRevision       int64               `json:"tenant_revision"`
	HomeRegion           string              `json:"home_region"`
	ExecutionEpoch       int64               `json:"execution_epoch"`
	TenantPolicyRevision int64               `json:"tenant_policy_revision"`
	TenantPolicy         core.TenantPolicy   `json:"tenant_policy"`
	APIKeyID             string              `json:"api_key_id"`
	Prefix               string              `json:"prefix"`
	SecretDigest         []byte              `json:"secret_digest"`
	DigestVersion        int16               `json:"digest_version"`
	Status               access.APIKeyStatus `json:"status"`
	KeyRevision          int64               `json:"key_revision"`
	PolicyRevision       int64               `json:"policy_revision"`
	Policy               core.APIKeyPolicy   `json:"policy"`
	ExpiresAt            *time.Time          `json:"expires_at"`
	RevokedAt            *time.Time          `json:"revoked_at"`
}

type tenantPayload struct {
	TenantID       string              `json:"tenant_id"`
	Status         access.TenantStatus `json:"status"`
	HomeRegion     string              `json:"home_region"`
	TenantRevision int64               `json:"tenant_revision"`
	PolicyRevision int64               `json:"policy_revision"`
	Policy         core.TenantPolicy   `json:"tenant_policy"`
	ExecutionEpoch int64               `json:"execution_epoch"`
}

func applyEvent(ctx context.Context, transaction *sql.Tx, event ControlEvent, appliedAt time.Time) error {
	switch event.AggregateType {
	case "GatewayAPIKey":
		var payload keyPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return ErrInvalidEvent
		}
		if payload.TenantID != event.TenantID || payload.APIKeyID != event.AggregateID || payload.KeyRevision != event.AggregateRevision ||
			payload.Prefix == "" || len(payload.SecretDigest) != sha256.Size || payload.DigestVersion <= 0 ||
			payload.HomeRegion == "" || payload.ExecutionEpoch <= 0 || payload.TenantPolicyRevision <= 0 || payload.PolicyRevision <= 0 {
			return ErrInvalidEvent
		}
		payload.TenantPolicy.Revision = payload.TenantPolicyRevision
		payload.Policy.Revision = payload.PolicyRevision
		tenantPolicy, _ := json.Marshal(payload.TenantPolicy)
		keyPolicy, _ := json.Marshal(payload.Policy)
		_, err := transaction.ExecContext(ctx, `
			INSERT INTO gateway_access_projection (
				tenant_id, api_key_id, key_prefix, secret_digest, digest_version, key_status, key_revision,
				api_key_policy_revision, api_key_policy, expires_at, revoked_at, tenant_status, tenant_revision,
				home_region, execution_epoch, tenant_policy_revision, tenant_policy, event_occurred_at, applied_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
			ON CONFLICT (tenant_id, api_key_id) DO UPDATE SET
				key_prefix = EXCLUDED.key_prefix, secret_digest = EXCLUDED.secret_digest,
				digest_version = EXCLUDED.digest_version, key_status = EXCLUDED.key_status,
				key_revision = EXCLUDED.key_revision, api_key_policy_revision = EXCLUDED.api_key_policy_revision,
				api_key_policy = EXCLUDED.api_key_policy, expires_at = EXCLUDED.expires_at,
				revoked_at = EXCLUDED.revoked_at,
				tenant_status = CASE WHEN EXCLUDED.tenant_revision >= gateway_access_projection.tenant_revision
					THEN EXCLUDED.tenant_status ELSE gateway_access_projection.tenant_status END,
				tenant_revision = GREATEST(gateway_access_projection.tenant_revision, EXCLUDED.tenant_revision),
				home_region = CASE WHEN EXCLUDED.tenant_revision >= gateway_access_projection.tenant_revision
					THEN EXCLUDED.home_region ELSE gateway_access_projection.home_region END,
				execution_epoch = CASE WHEN EXCLUDED.tenant_revision >= gateway_access_projection.tenant_revision
					THEN EXCLUDED.execution_epoch ELSE gateway_access_projection.execution_epoch END,
				tenant_policy_revision = CASE WHEN EXCLUDED.tenant_revision >= gateway_access_projection.tenant_revision
					THEN EXCLUDED.tenant_policy_revision ELSE gateway_access_projection.tenant_policy_revision END,
				tenant_policy = CASE WHEN EXCLUDED.tenant_revision >= gateway_access_projection.tenant_revision
					THEN EXCLUDED.tenant_policy ELSE gateway_access_projection.tenant_policy END,
				event_occurred_at = EXCLUDED.event_occurred_at, applied_at = EXCLUDED.applied_at`,
			payload.TenantID, payload.APIKeyID, payload.Prefix, payload.SecretDigest, payload.DigestVersion,
			payload.Status, payload.KeyRevision, payload.PolicyRevision, keyPolicy, payload.ExpiresAt, payload.RevokedAt,
			payload.TenantStatus, max(payload.TenantRevision, int64(1)), payload.HomeRegion, payload.ExecutionEpoch,
			payload.TenantPolicyRevision, tenantPolicy, event.OccurredAt, appliedAt)
		return err
	case "Tenant":
		var payload tenantPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return ErrInvalidEvent
		}
		if payload.TenantID != event.TenantID || payload.TenantID != event.AggregateID || payload.TenantRevision != event.AggregateRevision ||
			payload.HomeRegion == "" || payload.ExecutionEpoch <= 0 || payload.PolicyRevision <= 0 {
			return ErrInvalidEvent
		}
		payload.Policy.Revision = payload.PolicyRevision
		policy, _ := json.Marshal(payload.Policy)
		_, err := transaction.ExecContext(ctx, `
			UPDATE gateway_access_projection SET
				tenant_status = $2, tenant_revision = $3, home_region = $4, execution_epoch = $5,
				tenant_policy_revision = $6, tenant_policy = $7, event_occurred_at = $8, applied_at = $9
			WHERE tenant_id = $1 AND tenant_revision <= $3`, payload.TenantID, payload.Status, payload.TenantRevision, payload.HomeRegion,
			payload.ExecutionEpoch, payload.PolicyRevision, policy, event.OccurredAt, appliedAt)
		return err
	default:
		return ErrInvalidEvent
	}
}

func (store *Store) Authenticate(ctx context.Context, rawKey string) (access.Principal, error) {
	if rawKey == "" {
		return access.Principal{}, access.ErrInvalidAPIKey
	}
	versions := make([]int, 0, len(store.peppers)-1)
	for version := range store.peppers {
		if version != store.current {
			versions = append(versions, int(version))
		}
	}
	slices.Sort(versions)
	slices.Reverse(versions)
	versions = append([]int{int(store.current)}, versions...)
	for _, candidate := range versions {
		version := int16(candidate)
		principal, err := scanPrincipal(store.database.QueryRowContext(ctx, `
			SELECT tenant_id, api_key_id, home_region, execution_epoch,
			       tenant_policy_revision, tenant_policy, api_key_policy_revision, api_key_policy
			FROM gateway_access_projection
			WHERE secret_digest = $1 AND digest_version = $2 AND key_status = 'active' AND tenant_status = 'active'
			  AND (expires_at IS NULL OR expires_at > $3)`, digest(store.peppers[version], rawKey), version, store.now().UTC()))
		if err == nil {
			store.ObserveSuccessfulAuthentication(principal.APIKeyID)
			return principal, nil
		}
		if !errors.Is(err, access.ErrInvalidAPIKey) {
			return access.Principal{}, err
		}
	}
	return access.Principal{}, access.ErrInvalidAPIKey
}

func (store *Store) LookupPrincipal(ctx context.Context, tenantID, apiKeyID string) (access.Principal, error) {
	return scanPrincipal(store.database.QueryRowContext(ctx, `
		SELECT tenant_id, api_key_id, home_region, execution_epoch,
		       tenant_policy_revision, tenant_policy, api_key_policy_revision, api_key_policy
		FROM gateway_access_projection
		WHERE tenant_id = $1 AND api_key_id = $2 AND key_status = 'active' AND tenant_status = 'active'
		  AND (expires_at IS NULL OR expires_at > $3)`, tenantID, apiKeyID, store.now().UTC()))
}

type scanner interface{ Scan(...any) error }

func scanPrincipal(row scanner) (access.Principal, error) {
	var principal access.Principal
	var tenantPolicy, keyPolicy []byte
	if err := row.Scan(&principal.TenantID, &principal.APIKeyID, &principal.HomeRegion, &principal.ExecutionEpoch,
		&principal.TenantPolicy.Revision, &tenantPolicy, &principal.APIKeyPolicy.Revision, &keyPolicy); errors.Is(err, sql.ErrNoRows) {
		return access.Principal{}, access.ErrInvalidAPIKey
	} else if err != nil {
		return access.Principal{}, err
	}
	if err := json.Unmarshal(tenantPolicy, &principal.TenantPolicy); err != nil {
		return access.Principal{}, err
	}
	if err := json.Unmarshal(keyPolicy, &principal.APIKeyPolicy); err != nil {
		return access.Principal{}, err
	}
	return principal, nil
}

func digest(pepper []byte, rawKey string) []byte {
	mac := hmac.New(sha256.New, pepper)
	_, _ = mac.Write([]byte(rawKey))
	return mac.Sum(nil)
}

func (store *Store) ReplaceSnapshot(ctx context.Context, snapshot access.Snapshot) error {
	if snapshot.Tenant.ID == "" || snapshot.Tenant.Revision <= 0 || snapshot.Tenant.HomeRegion == "" ||
		snapshot.Tenant.ExecutionEpoch <= 0 || snapshot.Tenant.Policy.Revision <= 0 || snapshot.CreatedAt.IsZero() ||
		(snapshot.Tenant.Status != access.TenantActive && snapshot.Tenant.Status != access.TenantSuspended && snapshot.Tenant.Status != access.TenantClosed) {
		return errors.New("invalid Gateway Access Projection snapshot")
	}
	transaction, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	if err := lockTenantProjection(ctx, transaction, snapshot.Tenant.ID); err != nil {
		return err
	}
	currentTenantRevision, err := lockHead(ctx, transaction, "Tenant", snapshot.Tenant.ID)
	if err != nil {
		return err
	}
	if currentTenantRevision > snapshot.Tenant.Revision {
		return fmt.Errorf("%w: Tenant snapshot revision %d is behind projection revision %d", ErrInvalidEvent, snapshot.Tenant.Revision, currentTenantRevision)
	}
	snapshotKeys := make(map[string]KeySnapshot, len(snapshot.Keys))
	for _, key := range snapshot.Keys {
		if _, duplicate := snapshotKeys[key.ID]; duplicate {
			return errors.New("invalid Gateway Access Projection snapshot: duplicate key")
		}
		snapshotKeys[key.ID] = key
		currentKeyRevision, err := lockHead(ctx, transaction, "GatewayAPIKey", key.ID)
		if err != nil {
			return err
		}
		if currentKeyRevision > key.Revision {
			return fmt.Errorf("%w: Gateway API Key %s snapshot revision %d is behind projection revision %d",
				ErrInvalidEvent, key.ID, key.Revision, currentKeyRevision)
		}
	}
	existingLastUsed := make(map[string]*time.Time)
	existingRows, err := transaction.QueryContext(ctx, `
		SELECT api_key_id, last_used_at FROM gateway_access_projection WHERE tenant_id = $1 FOR UPDATE`, snapshot.Tenant.ID)
	if err != nil {
		return err
	}
	for existingRows.Next() {
		var apiKeyID string
		var lastUsedAt *time.Time
		if err := existingRows.Scan(&apiKeyID, &lastUsedAt); err != nil {
			_ = existingRows.Close()
			return err
		}
		if _, present := snapshotKeys[apiKeyID]; !present {
			_ = existingRows.Close()
			return fmt.Errorf("%w: snapshot omits projected Gateway API Key %s", ErrInvalidEvent, apiKeyID)
		}
		existingLastUsed[apiKeyID] = lastUsedAt
	}
	if err := existingRows.Close(); err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx, `
		DELETE FROM gateway_access_gaps
		WHERE aggregate_id = $1 OR aggregate_id IN (SELECT api_key_id FROM gateway_access_projection WHERE tenant_id = $1)`, snapshot.Tenant.ID); err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx, `
		DELETE FROM gateway_access_heads
		WHERE aggregate_type = 'GatewayAPIKey'
		  AND aggregate_id IN (SELECT api_key_id FROM gateway_access_projection WHERE tenant_id = $1)`, snapshot.Tenant.ID); err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx, `DELETE FROM gateway_access_projection WHERE tenant_id = $1`, snapshot.Tenant.ID); err != nil {
		return err
	}
	tenantPolicy, _ := json.Marshal(snapshot.Tenant.Policy)
	for _, key := range snapshot.Keys {
		if key.ID == "" || key.Prefix == "" || key.Revision <= 0 || key.Policy.Revision <= 0 ||
			len(key.SecretDigest) != sha256.Size || key.DigestVersion <= 0 ||
			(key.Status != access.APIKeyActive && key.Status != access.APIKeyRevoked) {
			return errors.New("invalid Gateway Access Projection key snapshot")
		}
		keyPolicy, _ := json.Marshal(key.Policy)
		lastUsedAt := laterTime(key.LastUsedAt, existingLastUsed[key.ID])
		if _, err := transaction.ExecContext(ctx, `
			INSERT INTO gateway_access_projection (
				tenant_id, api_key_id, key_prefix, secret_digest, digest_version, key_status, key_revision,
				api_key_policy_revision, api_key_policy, expires_at, revoked_at, tenant_status, tenant_revision,
				home_region, execution_epoch, tenant_policy_revision, tenant_policy, event_occurred_at, applied_at, last_used_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)`,
			snapshot.Tenant.ID, key.ID, key.Prefix, key.SecretDigest, key.DigestVersion, key.Status, key.Revision,
			key.Policy.Revision, keyPolicy, key.ExpiresAt, key.RevokedAt, snapshot.Tenant.Status, snapshot.Tenant.Revision,
			snapshot.Tenant.HomeRegion, snapshot.Tenant.ExecutionEpoch, snapshot.Tenant.Policy.Revision, tenantPolicy,
			snapshot.CreatedAt, store.now().UTC(), lastUsedAt); err != nil {
			return err
		}
		if _, err := transaction.ExecContext(ctx, `
			INSERT INTO gateway_access_heads (aggregate_type, aggregate_id, revision, updated_at)
			VALUES ('GatewayAPIKey',$1,$2,$3)
			ON CONFLICT (aggregate_type, aggregate_id) DO UPDATE SET revision = EXCLUDED.revision, updated_at = EXCLUDED.updated_at`,
			key.ID, key.Revision, store.now().UTC()); err != nil {
			return err
		}
		if _, err := transaction.ExecContext(ctx, `
			DELETE FROM gateway_access_gaps WHERE aggregate_type = 'GatewayAPIKey' AND aggregate_id = $1`, key.ID); err != nil {
			return err
		}
	}
	if _, err := transaction.ExecContext(ctx, `
		INSERT INTO gateway_access_heads (aggregate_type, aggregate_id, revision, updated_at)
		VALUES ('Tenant',$1,$2,$3)
		ON CONFLICT (aggregate_type, aggregate_id) DO UPDATE SET revision = EXCLUDED.revision, updated_at = EXCLUDED.updated_at`,
		snapshot.Tenant.ID, snapshot.Tenant.Revision, store.now().UTC()); err != nil {
		return err
	}
	return transaction.Commit()
}

// ReplaceSnapshots installs the complete set of authoritative Tenant snapshots
// for one region. Each Tenant replacement is atomic; the relay cursor is moved
// only after every replacement and the other bootstrap projections succeed, so
// an interrupted bootstrap safely retries the same revisioned snapshots.
func (store *Store) ReplaceSnapshots(ctx context.Context, region string, snapshots []access.Snapshot) error {
	if strings.TrimSpace(region) == "" {
		return errors.New("Gateway Access Projection bootstrap requires a region")
	}
	seen := make(map[string]struct{}, len(snapshots))
	for _, snapshot := range snapshots {
		if snapshot.Tenant.HomeRegion != region {
			return errors.New("Gateway Access Projection bootstrap contains another region")
		}
		if _, duplicate := seen[snapshot.Tenant.ID]; duplicate {
			return errors.New("Gateway Access Projection bootstrap contains a duplicate Tenant")
		}
		seen[snapshot.Tenant.ID] = struct{}{}
		if err := store.ReplaceSnapshot(ctx, snapshot); err != nil {
			return err
		}
	}
	transaction, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	if _, err := transaction.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "gateway-access-bootstrap\x1f"+region); err != nil {
		return err
	}
	tenantIDs := make([]string, 0, len(seen))
	for tenantID := range seen {
		tenantIDs = append(tenantIDs, tenantID)
	}
	if _, err := transaction.ExecContext(ctx, `DELETE FROM gateway_access_gaps WHERE (aggregate_type='GatewayAPIKey' AND aggregate_id IN (
		SELECT api_key_id FROM gateway_access_projection WHERE home_region=$1 AND NOT (tenant_id=ANY($2))
	)) OR (aggregate_type='Tenant' AND aggregate_id IN (
		SELECT tenant_id FROM gateway_access_projection WHERE home_region=$1 AND NOT (tenant_id=ANY($2))
	))`, region, tenantIDs); err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx, `DELETE FROM gateway_access_heads WHERE (aggregate_type='GatewayAPIKey' AND aggregate_id IN (
		SELECT api_key_id FROM gateway_access_projection WHERE home_region=$1 AND NOT (tenant_id=ANY($2))
	)) OR (aggregate_type='Tenant' AND aggregate_id IN (
		SELECT tenant_id FROM gateway_access_projection WHERE home_region=$1 AND NOT (tenant_id=ANY($2))
	))`, region, tenantIDs); err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx, `DELETE FROM gateway_access_projection
		WHERE home_region=$1 AND NOT (tenant_id=ANY($2))`, region, tenantIDs); err != nil {
		return err
	}
	return transaction.Commit()
}

func laterTime(first, second *time.Time) *time.Time {
	if first == nil {
		return second
	}
	if second == nil || first.After(*second) {
		return first
	}
	return second
}

func (store *Store) ObserveSuccessfulAuthentication(apiKeyID string) {
	if apiKeyID == "" {
		return
	}
	now := store.now().UTC()
	store.usageMu.Lock()
	defer store.usageMu.Unlock()
	if observedAt, ok := store.recentUsage[apiKeyID]; ok && now.Sub(observedAt) < time.Minute {
		return
	}
	select {
	case store.usageObservations <- usageObservation{apiKeyID: apiKeyID, usedAt: now}:
		store.recentUsage[apiKeyID] = now
	default:
	}
}

func (store *Store) FlushLastUsed(ctx context.Context, limit int) (int, error) {
	if limit == 0 {
		limit = 1000
	}
	if limit < 1 || limit > 4096 {
		return 0, errors.New("last-used flush limit must be between 1 and 4096")
	}
	observations := make(map[string]time.Time, limit)
	for len(observations) < limit {
		select {
		case observation := <-store.usageObservations:
			if current, ok := observations[observation.apiKeyID]; !ok || observation.usedAt.After(current) {
				observations[observation.apiKeyID] = observation.usedAt
			}
		default:
			limit = 0
		}
		if limit == 0 {
			break
		}
	}
	if len(observations) == 0 {
		store.pruneRecentUsage()
		return 0, nil
	}
	transaction, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		store.retryUsageObservations(observations)
		return 0, err
	}
	defer func() { _ = transaction.Rollback() }()
	for apiKeyID, usedAt := range observations {
		if _, err := transaction.ExecContext(ctx, `
			UPDATE gateway_access_projection
			SET last_used_at = GREATEST(COALESCE(last_used_at, $2), $2)
			WHERE api_key_id = $1`, apiKeyID, usedAt); err != nil {
			store.retryUsageObservations(observations)
			return 0, err
		}
	}
	if err := transaction.Commit(); err != nil {
		store.retryUsageObservations(observations)
		return 0, err
	}
	store.pruneRecentUsage()
	return len(observations), nil
}

func (store *Store) pruneRecentUsage() {
	threshold := store.now().UTC().Add(-time.Minute)
	store.usageMu.Lock()
	defer store.usageMu.Unlock()
	for apiKeyID, observedAt := range store.recentUsage {
		if !observedAt.After(threshold) {
			delete(store.recentUsage, apiKeyID)
		}
	}
}

func (store *Store) retryUsageObservations(observations map[string]time.Time) {
	store.usageMu.Lock()
	defer store.usageMu.Unlock()
	for apiKeyID, usedAt := range observations {
		delete(store.recentUsage, apiKeyID)
		select {
		case store.usageObservations <- usageObservation{apiKeyID: apiKeyID, usedAt: usedAt}:
			store.recentUsage[apiKeyID] = usedAt
		default:
		}
	}
}

func (store *Store) Status(ctx context.Context) (Status, error) {
	var status Status
	if err := store.database.QueryRowContext(ctx, `SELECT count(*), min(detected_at) FROM gateway_access_gaps`).Scan(&status.GapCount, &status.OldestGapAt); err != nil {
		return Status{}, err
	}
	if err := store.database.QueryRowContext(ctx, `
		SELECT count(*), COALESCE(max(revision), 0) FROM gateway_access_heads`).Scan(
		&status.HeadCount, &status.MaxAggregateRevision); err != nil {
		return Status{}, err
	}
	if err := store.database.QueryRowContext(ctx, `SELECT COALESCE(max(delivery_sequence),0) FROM gateway_access_inbox`).Scan(&status.MaxDeliverySequence); err != nil {
		return Status{}, err
	}
	var maxLagSeconds sql.NullFloat64
	if err := store.database.QueryRowContext(ctx, `
		SELECT max(applied_at), max(EXTRACT(EPOCH FROM (applied_at - event_occurred_at)))
		FROM gateway_access_projection`).Scan(&status.LastAppliedAt, &maxLagSeconds); err != nil {
		return Status{}, err
	}
	if maxLagSeconds.Valid && maxLagSeconds.Float64 > 0 {
		status.MaxApplyLag = time.Duration(maxLagSeconds.Float64 * float64(time.Second))
	}
	if status.OldestPendingAt != nil {
		status.DeliveryLag = store.now().UTC().Sub(*status.OldestPendingAt)
		if status.DeliveryLag < 0 {
			status.DeliveryLag = 0
		}
	}
	var revocationLagSeconds sql.NullFloat64
	if err := store.database.QueryRowContext(ctx, `
		SELECT max(received_at), max(apply_lag_seconds)
		FROM gateway_access_inbox
		WHERE event_type = 'GatewayAPIKeyRevoked' AND disposition = 'applied'`).Scan(
		&status.LastRevocationAppliedAt, &revocationLagSeconds); err != nil {
		return Status{}, err
	}
	if revocationLagSeconds.Valid && revocationLagSeconds.Float64 > 0 {
		status.MaxRevocationApplyLag = time.Duration(revocationLagSeconds.Float64 * float64(time.Second))
	}
	return status, nil
}

func (store *Store) ActiveDigestVersionCount(ctx context.Context, version int16) (int64, error) {
	var count int64
	err := store.database.QueryRowContext(ctx, `
		SELECT count(*) FROM gateway_access_projection
		WHERE digest_version = $1 AND key_status = 'active' AND tenant_status <> 'closed'`, version).Scan(&count)
	return count, err
}

func (store *Store) ValidatePepperCoverage(ctx context.Context) error {
	rows, err := store.database.QueryContext(ctx, `
		SELECT DISTINCT digest_version FROM gateway_access_projection
		WHERE key_status = 'active' AND tenant_status <> 'closed'
		ORDER BY digest_version`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var version int16
		if err := rows.Scan(&version); err != nil {
			return err
		}
		if len(store.peppers[version]) == 0 {
			return fmt.Errorf("active Gateway Access Projection keys still require digest pepper version %d", version)
		}
	}
	return rows.Err()
}
