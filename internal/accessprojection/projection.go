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
	"time"

	"github.com/toddzheng/llm-gateway/internal/access"
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

type TenantSnapshot struct {
	ID             string
	Status         access.TenantStatus
	Revision       int64
	HomeRegion     string
	ExecutionEpoch int64
	Policy         core.TenantPolicy
}

type KeySnapshot struct {
	ID            string
	Prefix        string
	SecretDigest  []byte
	DigestVersion int16
	Status        access.APIKeyStatus
	Revision      int64
	Policy        core.APIKeyPolicy
	ExpiresAt     *time.Time
	RevokedAt     *time.Time
}

type Snapshot struct {
	Tenant    TenantSnapshot
	Keys      []KeySnapshot
	CreatedAt time.Time
}

type Status struct {
	GapCount             int
	OldestGapAt          *time.Time
	HeadCount            int
	MaxAggregateRevision int64
	PendingEventCount    int
	OldestPendingAt      *time.Time
	LastAppliedAt        *time.Time
	MaxApplyLag          time.Duration
	DeliveryLag          time.Duration
}

type Store struct {
	database *sql.DB
	peppers  map[int16][]byte
	current  int16
	now      func() time.Time
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
	return &Store{database: database, peppers: peppers, current: ring.CurrentVersion, now: now}, nil
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
		if _, err := transaction.ExecContext(ctx, `
			INSERT INTO gateway_access_gaps (
				aggregate_type, aggregate_id, expected_revision, received_revision, detected_at, last_event_id
			) VALUES ($1,$2,$3,$4,$5,$6)
			ON CONFLICT (aggregate_type, aggregate_id) DO UPDATE SET
				expected_revision = EXCLUDED.expected_revision,
				received_revision = GREATEST(gateway_access_gaps.received_revision, EXCLUDED.received_revision),
				detected_at = LEAST(gateway_access_gaps.detected_at, EXCLUDED.detected_at),
				last_event_id = EXCLUDED.last_event_id`,
			event.AggregateType, event.AggregateID, current+1, event.AggregateRevision, store.now().UTC(), event.EventID); err != nil {
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

func recordInbox(ctx context.Context, transaction *sql.Tx, event ControlEvent, disposition Disposition, receivedAt time.Time) error {
	_, err := transaction.ExecContext(ctx, `
		INSERT INTO gateway_access_inbox (
			event_id, schema_version, aggregate_type, aggregate_id, aggregate_revision, disposition, received_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		event.EventID, event.SchemaVersion, event.AggregateType, event.AggregateID, event.AggregateRevision, disposition, receivedAt)
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
			WHERE tenant_id = $1`, payload.TenantID, payload.Status, payload.TenantRevision, payload.HomeRegion,
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

func (store *Store) ReplaceSnapshot(ctx context.Context, snapshot Snapshot) error {
	if snapshot.Tenant.ID == "" || snapshot.Tenant.Revision <= 0 || snapshot.Tenant.HomeRegion == "" ||
		snapshot.Tenant.ExecutionEpoch <= 0 || snapshot.Tenant.Policy.Revision <= 0 || snapshot.CreatedAt.IsZero() {
		return errors.New("invalid Gateway Access Projection snapshot")
	}
	transaction, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	if _, err := transaction.ExecContext(ctx, `
		DELETE FROM gateway_access_response_slots
		WHERE api_key_id IN (SELECT api_key_id FROM gateway_access_projection WHERE tenant_id = $1)`, snapshot.Tenant.ID); err != nil {
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
		if key.ID == "" || key.Revision <= 0 || key.Policy.Revision <= 0 || len(key.SecretDigest) != sha256.Size || key.DigestVersion <= 0 {
			return errors.New("invalid Gateway Access Projection key snapshot")
		}
		keyPolicy, _ := json.Marshal(key.Policy)
		if _, err := transaction.ExecContext(ctx, `
			INSERT INTO gateway_access_projection (
				tenant_id, api_key_id, key_prefix, secret_digest, digest_version, key_status, key_revision,
				api_key_policy_revision, api_key_policy, expires_at, revoked_at, tenant_status, tenant_revision,
				home_region, execution_epoch, tenant_policy_revision, tenant_policy, event_occurred_at, applied_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`,
			snapshot.Tenant.ID, key.ID, key.Prefix, key.SecretDigest, key.DigestVersion, key.Status, key.Revision,
			key.Policy.Revision, keyPolicy, key.ExpiresAt, key.RevokedAt, snapshot.Tenant.Status, snapshot.Tenant.Revision,
			snapshot.Tenant.HomeRegion, snapshot.Tenant.ExecutionEpoch, snapshot.Tenant.Policy.Revision, tenantPolicy,
			snapshot.CreatedAt, store.now().UTC()); err != nil {
			return err
		}
		if _, err := transaction.ExecContext(ctx, `
			INSERT INTO gateway_access_heads (aggregate_type, aggregate_id, revision, updated_at)
			VALUES ('GatewayAPIKey',$1,$2,$3)
			ON CONFLICT (aggregate_type, aggregate_id) DO UPDATE SET revision = EXCLUDED.revision, updated_at = EXCLUDED.updated_at`,
			key.ID, key.Revision, store.now().UTC()); err != nil {
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
	if err := store.database.QueryRowContext(ctx, `
		SELECT count(*), min(occurred_at)
		FROM control_outbox
		WHERE aggregate_type IN ('Tenant','GatewayAPIKey') AND schema_version = 2
		  AND NOT EXISTS (SELECT 1 FROM gateway_access_inbox i WHERE i.event_id = control_outbox.event_id)`).Scan(
		&status.PendingEventCount, &status.OldestPendingAt); err != nil {
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
	return status, nil
}

func (store *Store) ActiveDigestVersionCount(ctx context.Context, version int16) (int64, error) {
	var count int64
	err := store.database.QueryRowContext(ctx, `
		SELECT count(*) FROM gateway_access_projection
		WHERE digest_version = $1 AND key_status = 'active' AND tenant_status = 'active'
		  AND (expires_at IS NULL OR expires_at > $2)`, version, store.now().UTC()).Scan(&count)
	return count, err
}
