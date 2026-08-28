package tenantadmin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/toddzheng/llm-gateway/internal/access"
	"github.com/toddzheng/llm-gateway/internal/core"
)

const createTenantOperation = "tenant.create"

var resourceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~-]{0,127}$`)

type Service struct {
	db  *sql.DB
	now func() time.Time
}

func NewService(db *sql.DB, now func() time.Time) (*Service, error) {
	if db == nil {
		return nil, errors.New("Tenant Administration requires PostgreSQL")
	}
	if now == nil {
		now = time.Now
	}
	return &Service{db: db, now: now}, nil
}

func (s *Service) CreateTenant(
	ctx context.Context,
	actor ActorEnvelope,
	idempotencyKey string,
	command CreateTenantCommand,
) (MutationResult, error) {
	if err := validateMutationActor(actor); err != nil {
		return MutationResult{}, err
	}
	if !hasScope(actor, ScopePlatformWrite) {
		return MutationResult{}, ErrPolicyDenied
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return MutationResult{}, err
	}
	if err := validateCreateCommand(command); err != nil {
		return MutationResult{}, err
	}
	if command.InitialPolicy.Revision == 0 {
		command.InitialPolicy.Revision = 1
	}
	requestHash, err := commandHash(command, actor.Reason)
	if err != nil {
		return MutationResult{}, err
	}
	tx, replay, err := s.beginCommand(ctx, actor, createTenantOperation, idempotencyKey, requestHash)
	if err != nil || replay != nil {
		if replay != nil {
			return MutationResult{Tenant: *replay, Replay: true}, nil
		}
		return MutationResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	policyPayload, err := tenantPolicyPayload(command.InitialPolicy)
	if err != nil {
		return MutationResult{}, err
	}
	metadataPayload, err := metadataPayload(command.Metadata)
	if err != nil {
		return MutationResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `SELECT
		set_config('app.control_actor_type', $1, true),
		set_config('app.control_actor_id', $2, true),
		set_config('app.control_change_reason', $3, true)`, actor.Type, actor.ID, actor.Reason); err != nil {
		return MutationResult{}, fmt.Errorf("set initial Tenant Policy attribution: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tenants (
			id, slug, display_name, status, home_region, execution_epoch,
			policy_revision, policy, metadata, revision
		) VALUES ($1,$2,$3,'active',$4,1,1,$5,$6,1)`,
		command.ID, command.Slug, command.DisplayName, command.HomeRegion, policyPayload, metadataPayload,
	); err != nil {
		return MutationResult{}, mapDatabaseError("create Tenant", err, ErrAlreadyExists)
	}
	tenant, err := getTenantTx(ctx, tx, command.ID)
	if err != nil {
		return MutationResult{}, err
	}
	eventPayload := tenantEventPayload(tenant)
	if err := s.recordMutation(ctx, tx, actor, tenant, "TenantProvisioned", createTenantOperation, eventPayload); err != nil {
		return MutationResult{}, err
	}
	if err := recordCommandResult(ctx, tx, actor, createTenantOperation, idempotencyKey, requestHash, tenant); err != nil {
		return MutationResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return MutationResult{}, err
	}
	return MutationResult{Tenant: tenant}, nil
}

func (s *Service) beginCommand(
	ctx context.Context,
	actor ActorEnvelope,
	operation, idempotencyKey string,
	requestHash []byte,
) (*sql.Tx, *access.Tenant, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	lockIdentity := strings.Join([]string{actor.Type, actor.ID, operation, idempotencyKey}, "\x1f")
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lockIdentity); err != nil {
		_ = tx.Rollback()
		return nil, nil, err
	}
	var storedHash, resultPayload []byte
	err = tx.QueryRowContext(ctx, `
		SELECT request_hash, result FROM control_command_idempotency
		WHERE actor_type = $1 AND actor_id = $2 AND operation = $3 AND idempotency_key = $4`,
		actor.Type, actor.ID, operation, idempotencyKey,
	).Scan(&storedHash, &resultPayload)
	if errors.Is(err, sql.ErrNoRows) {
		return tx, nil, nil
	}
	if err != nil {
		_ = tx.Rollback()
		return nil, nil, err
	}
	if !equalBytes(storedHash, requestHash) {
		_ = tx.Rollback()
		return nil, nil, ErrIdempotencyConflict
	}
	var tenant access.Tenant
	if err := json.Unmarshal(resultPayload, &tenant); err != nil {
		_ = tx.Rollback()
		return nil, nil, fmt.Errorf("decode idempotent Tenant result: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return nil, &tenant, nil
}

func (s *Service) recordMutation(
	ctx context.Context,
	tx *sql.Tx,
	actor ActorEnvelope,
	tenant access.Tenant,
	eventType, action string,
	payload map[string]any,
) error {
	now := s.now().UTC()
	auditID, err := newID("caud")
	if err != nil {
		return err
	}
	eventID, err := newID("cevt")
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO control_audit_events (
			event_id, tenant_id, actor_type, actor_id, acting_tenant_id, scopes,
			request_id, reason, action, aggregate_revision, payload, occurred_at
		) VALUES ($1,$2,$3,$4,NULLIF($5,''),$6,$7,$8,$9,$10,$11,$12)`,
		auditID, tenant.ID, actor.Type, actor.ID, actor.ActingTenantID, actor.Scopes,
		actor.RequestID, actor.Reason, action, tenant.Revision, encoded, now,
	); err != nil {
		return fmt.Errorf("append control audit: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO control_outbox (
			event_id, schema_version, aggregate_type, aggregate_id, aggregate_revision,
			tenant_id, event_type, occurred_at, payload
		) VALUES ($1,2,'Tenant',$2,$3,$2,$4,$5,$6)`,
		eventID, tenant.ID, tenant.Revision, eventType, now, encoded,
	); err != nil {
		return fmt.Errorf("append control outbox: %w", err)
	}
	return nil
}

func recordCommandResult(
	ctx context.Context,
	tx *sql.Tx,
	actor ActorEnvelope,
	operation, idempotencyKey string,
	requestHash []byte,
	tenant access.Tenant,
) error {
	result, err := json.Marshal(tenant)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO control_command_idempotency (
			actor_type, actor_id, operation, idempotency_key, request_hash, result
		) VALUES ($1,$2,$3,$4,$5,$6)`,
		actor.Type, actor.ID, operation, idempotencyKey, requestHash, result,
	)
	if err != nil {
		return fmt.Errorf("record control command idempotency: %w", err)
	}
	return nil
}

func getTenantTx(ctx context.Context, tx *sql.Tx, tenantID string) (access.Tenant, error) {
	return scanTenant(tx.QueryRowContext(ctx, `
		SELECT id, slug, display_name, status, home_region, execution_epoch,
		       policy_revision, policy, metadata, revision
		FROM tenants WHERE id = $1`, tenantID))
}

func validateCreateCommand(command CreateTenantCommand) error {
	if !resourceIDPattern.MatchString(command.ID) || !resourceIDPattern.MatchString(command.Slug) {
		return fmt.Errorf("%w: Tenant ID and slug must be URL-safe resource identifiers", ErrInvalidArgument)
	}
	if strings.TrimSpace(command.DisplayName) == "" || command.HomeRegion == "" {
		return fmt.Errorf("%w: Tenant ID, slug, display name, and Home Region are required", ErrInvalidArgument)
	}
	if command.InitialPolicy.Revision != 0 && command.InitialPolicy.Revision != 1 {
		return fmt.Errorf("%w: initial Tenant Policy revision must be 1", ErrInvalidArgument)
	}
	if err := validatePolicy(command.InitialPolicy); err != nil {
		return err
	}
	if err := validateMetadata(command.Metadata); err != nil {
		return err
	}
	return nil
}

func validateMutationActor(actor ActorEnvelope) error {
	if actor.Type == "" || actor.ID == "" || actor.RequestID == "" || strings.TrimSpace(actor.Reason) == "" {
		return fmt.Errorf("%w: mutation Actor Envelope is incomplete", ErrInvalidArgument)
	}
	return nil
}

func validateIdempotencyKey(value string) error {
	if value == "" || len(value) > 255 {
		return fmt.Errorf("%w: Idempotency-Key must contain 1 to 255 characters", ErrInvalidArgument)
	}
	return nil
}

func validateMetadata(metadata map[string]any) error {
	reserved := map[string]struct{}{
		"status": {}, "lifecycle": {}, "home_region": {}, "execution_epoch": {}, "policy": {},
		"permissions": {}, "limits": {},
	}
	for key := range metadata {
		if _, denied := reserved[strings.ToLower(key)]; denied {
			return fmt.Errorf("%w: %q is typed behavior and cannot be metadata", ErrInvalidArgument, key)
		}
	}
	_, err := metadataPayload(metadata)
	return err
}

func metadataPayload(metadata map[string]any) ([]byte, error) {
	if metadata == nil {
		return []byte(`{}`), nil
	}
	payload, err := json.Marshal(metadata)
	if err != nil {
		return nil, fmt.Errorf("%w: encode metadata: %v", ErrInvalidArgument, err)
	}
	if len(payload) > 64<<10 {
		return nil, fmt.Errorf("%w: metadata exceeds 64 KiB", ErrInvalidArgument)
	}
	return payload, nil
}

func tenantPolicyPayload(policy core.TenantPolicy) ([]byte, error) {
	policy.Revision = 0
	return json.Marshal(policy)
}

func tenantEventPayload(tenant access.Tenant) map[string]any {
	policyPayload, _ := tenantPolicyPayload(tenant.Policy)
	digest := sha256.Sum256(policyPayload)
	return map[string]any{
		"tenant_id": tenant.ID, "slug": tenant.Slug, "status": tenant.Status,
		"home_region": tenant.HomeRegion, "tenant_revision": tenant.Revision,
		"execution_epoch": tenant.ExecutionEpoch, "policy_revision": tenant.Policy.Revision,
		"tenant_policy": tenant.Policy, "policy_digest": hex.EncodeToString(digest[:]),
	}
}

func metadataDigest(metadata map[string]any) string {
	payload, _ := metadataPayload(metadata)
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func commandHash(command any, reason string) ([]byte, error) {
	payload, err := json.Marshal(struct {
		Command any    `json:"command"`
		Reason  string `json:"reason"`
	}{Command: command, Reason: reason})
	if err != nil {
		return nil, fmt.Errorf("%w: encode command: %v", ErrInvalidArgument, err)
	}
	digest := sha256.Sum256(payload)
	return digest[:], nil
}

func hasScope(actor ActorEnvelope, wanted string) bool {
	for _, scope := range actor.Scopes {
		if scope == wanted {
			return true
		}
	}
	return false
}

func mapDatabaseError(operation string, err error, conflict error) error {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) && postgresError.Code == "23505" {
		return fmt.Errorf("%w: %s", conflict, operation)
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func newID(prefix string) (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return prefix + "_" + hex.EncodeToString(value[:]), nil
}

func equalBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var difference byte
	for index := range left {
		difference |= left[index] ^ right[index]
	}
	return difference == 0
}
