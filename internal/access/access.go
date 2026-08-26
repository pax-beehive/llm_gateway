package access

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/quota"
)

var (
	ErrInvalidAPIKey = errors.New("invalid API key")
	ErrConflict      = errors.New("access revision conflict")
	ErrNotFound      = errors.New("access record not found")
)

type TenantStatus string

const (
	TenantActive    TenantStatus = "active"
	TenantSuspended TenantStatus = "suspended"
	TenantClosed    TenantStatus = "closed"
)

type ChangeActor struct {
	Type string
	ID   string
}

type Tenant struct {
	ID             string
	Slug           string
	DisplayName    string
	Status         TenantStatus
	HomeRegion     string
	ExecutionEpoch int64
	Policy         core.TenantPolicy
	Metadata       map[string]any
	Revision       int64
}

type APIKeyStatus string

const (
	APIKeyActive  APIKeyStatus = "active"
	APIKeyRevoked APIKeyStatus = "revoked"
)

type APIKeySpec struct {
	TenantID  string
	Name      string
	RawKey    string
	Policy    core.APIKeyPolicy
	Metadata  map[string]any
	ExpiresAt *time.Time
}

type APIKey struct {
	ID        string
	TenantID  string
	Name      string
	Prefix    string
	Status    APIKeyStatus
	Revision  int64
	Policy    core.APIKeyPolicy
	Metadata  map[string]any
	ExpiresAt *time.Time
	RevokedAt *time.Time
}

type Principal struct {
	TenantID       string
	APIKeyID       string
	HomeRegion     string
	ExecutionEpoch int64
	TenantPolicy   core.TenantPolicy
	APIKeyPolicy   core.APIKeyPolicy
}

type PostgresService struct {
	db     *sql.DB
	pepper []byte
}

func NewPostgresService(db *sql.DB, pepper []byte) (*PostgresService, error) {
	if db == nil {
		return nil, errors.New("access service requires PostgreSQL")
	}
	if len(pepper) < 16 {
		return nil, errors.New("API key pepper must contain at least 16 bytes")
	}
	return &PostgresService{db: db, pepper: append([]byte(nil), pepper...)}, nil
}

func (s *PostgresService) CreateTenant(ctx context.Context, tenant Tenant, actor ChangeActor) error {
	if tenant.ID == "" || tenant.Slug == "" || tenant.DisplayName == "" || tenant.HomeRegion == "" {
		return errors.New("Tenant ID, slug, display name, and Home Region are required")
	}
	if tenant.Status == "" {
		tenant.Status = TenantActive
	}
	if tenant.Status != TenantActive {
		return errors.New("a new Tenant must be active")
	}
	if tenant.ExecutionEpoch <= 0 {
		tenant.ExecutionEpoch = 1
	}
	if tenant.Policy.Revision <= 0 {
		tenant.Policy.Revision = 1
	}
	if actor.Type == "" || actor.ID == "" {
		return errors.New("Tenant change actor is required")
	}
	if _, err := quota.EffectiveLimits(tenant.Policy.Limits, core.QuotaLimits{}); err != nil {
		return fmt.Errorf("validate Tenant policy: %w", err)
	}
	policyPayload, err := tenantPolicyJSON(tenant.Policy)
	if err != nil {
		return err
	}
	metadata, err := metadataJSON(tenant.Metadata)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT
		set_config('app.control_actor_type', $1, true),
		set_config('app.control_actor_id', $2, true)`, actor.Type, actor.ID); err != nil {
		return fmt.Errorf("set initial Tenant policy attribution: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tenants (
			id, slug, display_name, status, home_region, execution_epoch,
			policy_revision, policy, metadata
		) VALUES ($1,$2,$3,'active',$4,$5,$6,$7,$8)
		ON CONFLICT (id) DO NOTHING`,
		tenant.ID, tenant.Slug, tenant.DisplayName, tenant.HomeRegion, tenant.ExecutionEpoch,
		tenant.Policy.Revision, policyPayload, metadata,
	); err != nil {
		return fmt.Errorf("create Tenant: %w", err)
	}
	var matches bool
	if err := tx.QueryRowContext(ctx, `
		SELECT slug = $2 AND display_name = $3 AND status = 'active' AND home_region = $4
		       AND execution_epoch = $5 AND policy_revision = $6
		       AND policy = $7::jsonb AND metadata = $8::jsonb
		FROM tenants WHERE id = $1`, tenant.ID, tenant.Slug, tenant.DisplayName, tenant.HomeRegion,
		tenant.ExecutionEpoch, tenant.Policy.Revision, policyPayload, metadata).Scan(&matches); err != nil {
		return err
	}
	if !matches {
		return errors.New("persisted Tenant does not match bootstrap configuration")
	}
	return tx.Commit()
}

func (s *PostgresService) ImportAPIKey(ctx context.Context, spec APIKeySpec) (APIKey, error) {
	if spec.TenantID == "" || strings.TrimSpace(spec.Name) == "" {
		return APIKey{}, errors.New("API key Tenant and name are required")
	}
	if len(spec.RawKey) < 24 {
		return APIKey{}, errors.New("API key must contain at least 24 characters")
	}
	if spec.Policy.Revision <= 0 {
		spec.Policy.Revision = 1
	}
	if _, err := quota.EffectiveLimits(core.QuotaLimits{}, spec.Policy.Limits); err != nil {
		return APIKey{}, fmt.Errorf("validate API key policy: %w", err)
	}
	digest := s.digest(spec.RawKey)
	id := "key_" + hex.EncodeToString(digest[:12])
	prefix := spec.RawKey
	if len(prefix) > 12 {
		prefix = prefix[:12]
	}
	policyPayload, err := apiKeyPolicyJSON(spec.Policy)
	if err != nil {
		return APIKey{}, err
	}
	metadata, err := metadataJSON(spec.Metadata)
	if err != nil {
		return APIKey{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return APIKey{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO api_keys (
			id, tenant_id, name, key_prefix, secret_digest, policy_revision,
			policy, metadata, expires_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (secret_digest) DO NOTHING`,
		id, spec.TenantID, strings.TrimSpace(spec.Name), prefix, digest,
		spec.Policy.Revision, policyPayload, metadata, spec.ExpiresAt,
	); err != nil {
		return APIKey{}, fmt.Errorf("import API key: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO api_key_policy_revisions (
			tenant_id, api_key_id, revision, policy, actor_type, actor_id, change_reason
		) VALUES ($1,$2,$3,$4,'bootstrap','api-key-import','initial API key policy')
		ON CONFLICT (tenant_id, api_key_id, revision) DO NOTHING`,
		spec.TenantID, id, spec.Policy.Revision, policyPayload,
	); err != nil {
		return APIKey{}, fmt.Errorf("record API key policy: %w", err)
	}
	var matches bool
	if err := tx.QueryRowContext(ctx, `
		SELECT id = $1 AND tenant_id = $2 AND name = $3 AND key_prefix = $4
		       AND status = 'active' AND policy_revision = $5
		       AND policy = $6::jsonb AND metadata = $7::jsonb
		       AND expires_at IS NOT DISTINCT FROM $8::timestamptz
		FROM api_keys WHERE secret_digest = $9`, id, spec.TenantID, strings.TrimSpace(spec.Name), prefix,
		spec.Policy.Revision, policyPayload, metadata, spec.ExpiresAt, digest).Scan(&matches); err != nil {
		return APIKey{}, err
	}
	if !matches {
		return APIKey{}, errors.New("persisted API key does not match bootstrap configuration")
	}
	if err := tx.Commit(); err != nil {
		return APIKey{}, err
	}
	return s.GetAPIKey(ctx, spec.TenantID, id)
}

func (s *PostgresService) GetTenant(ctx context.Context, tenantID string) (Tenant, error) {
	var tenant Tenant
	var policyPayload, metadataPayload []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT id, slug, display_name, status, home_region, execution_epoch,
		       policy_revision, policy, metadata, revision
		FROM tenants WHERE id = $1`, tenantID).Scan(
		&tenant.ID, &tenant.Slug, &tenant.DisplayName, &tenant.Status, &tenant.HomeRegion,
		&tenant.ExecutionEpoch, &tenant.Policy.Revision, &policyPayload, &metadataPayload, &tenant.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return Tenant{}, ErrNotFound
	}
	if err != nil {
		return Tenant{}, err
	}
	if err := json.Unmarshal(policyPayload, &tenant.Policy); err != nil {
		return Tenant{}, err
	}
	if err := json.Unmarshal(metadataPayload, &tenant.Metadata); err != nil {
		return Tenant{}, err
	}
	return tenant, nil
}

func (s *PostgresService) GetAPIKey(ctx context.Context, tenantID, apiKeyID string) (APIKey, error) {
	var key APIKey
	var policyPayload, metadataPayload []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT id, tenant_id, name, key_prefix, status, revision, policy_revision,
		       policy, metadata, expires_at, revoked_at
		FROM api_keys WHERE tenant_id = $1 AND id = $2`, tenantID, apiKeyID).Scan(
		&key.ID, &key.TenantID, &key.Name, &key.Prefix, &key.Status, &key.Revision,
		&key.Policy.Revision, &policyPayload, &metadataPayload, &key.ExpiresAt, &key.RevokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return APIKey{}, ErrNotFound
	}
	if err != nil {
		return APIKey{}, err
	}
	if err := json.Unmarshal(policyPayload, &key.Policy); err != nil {
		return APIKey{}, err
	}
	if err := json.Unmarshal(metadataPayload, &key.Metadata); err != nil {
		return APIKey{}, err
	}
	return key, nil
}

func (s *PostgresService) UpdateAPIKeyMetadata(ctx context.Context, tenantID, apiKeyID string, expectedRevision int64, metadata map[string]any) (APIKey, error) {
	if expectedRevision <= 0 {
		return APIKey{}, errors.New("API key metadata update requires a positive expected revision")
	}
	payload, err := metadataJSON(metadata)
	if err != nil {
		return APIKey{}, err
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE api_keys SET metadata = $4, revision = revision + 1, updated_at = now()
		WHERE tenant_id = $1 AND id = $2 AND revision = $3 AND status = 'active'`,
		tenantID, apiKeyID, expectedRevision, payload)
	if err != nil {
		return APIKey{}, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return APIKey{}, err
	}
	if rows != 1 {
		return APIKey{}, ErrConflict
	}
	return s.GetAPIKey(ctx, tenantID, apiKeyID)
}

func (s *PostgresService) TransitionTenant(ctx context.Context, tenantID string, expectedRevision int64, target TenantStatus, at time.Time) (Tenant, error) {
	if expectedRevision <= 0 {
		return Tenant{}, errors.New("Tenant transition requires a positive expected revision")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	var result sql.Result
	var err error
	switch target {
	case TenantSuspended:
		result, err = s.db.ExecContext(ctx, `
			UPDATE tenants SET status = 'suspended', suspended_at = $3, revision = revision + 1, updated_at = now()
			WHERE id = $1 AND revision = $2 AND status = 'active'`, tenantID, expectedRevision, at)
	case TenantActive:
		result, err = s.db.ExecContext(ctx, `
			UPDATE tenants SET status = 'active', suspended_at = NULL, closed_at = NULL,
			       revision = revision + 1, updated_at = now()
			WHERE id = $1 AND revision = $2 AND status = 'suspended'`, tenantID, expectedRevision)
	case TenantClosed:
		result, err = s.db.ExecContext(ctx, `
			UPDATE tenants SET status = 'closed', closed_at = $3, revision = revision + 1, updated_at = now()
			WHERE id = $1 AND revision = $2 AND status IN ('active','suspended')`, tenantID, expectedRevision, at)
	default:
		return Tenant{}, errors.New("invalid Tenant lifecycle target")
	}
	if err != nil {
		return Tenant{}, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return Tenant{}, err
	}
	if rows != 1 {
		return Tenant{}, ErrConflict
	}
	return s.GetTenant(ctx, tenantID)
}

func (s *PostgresService) Authenticate(ctx context.Context, rawKey string) (Principal, error) {
	if rawKey == "" {
		return Principal{}, ErrInvalidAPIKey
	}
	return scanPrincipal(s.db.QueryRowContext(ctx, `
		SELECT t.id, k.id, t.home_region, t.execution_epoch,
		       t.policy_revision, t.policy, k.policy_revision, k.policy
		FROM api_keys k
		JOIN tenants t ON t.id = k.tenant_id
		WHERE k.secret_digest = $1
		  AND k.digest_version = 1
		  AND k.status = 'active'
		  AND t.status = 'active'
		  AND (k.expires_at IS NULL OR k.expires_at > now())`, s.digest(rawKey)))
}

// LookupPrincipal revalidates a previously authenticated principal for delayed
// work such as active cache refresh. It deliberately accepts no raw secret.
func (s *PostgresService) LookupPrincipal(ctx context.Context, tenantID, apiKeyID string) (Principal, error) {
	if tenantID == "" || apiKeyID == "" {
		return Principal{}, ErrInvalidAPIKey
	}
	return scanPrincipal(s.db.QueryRowContext(ctx, `
		SELECT t.id, k.id, t.home_region, t.execution_epoch,
		       t.policy_revision, t.policy, k.policy_revision, k.policy
		FROM api_keys k
		JOIN tenants t ON t.id = k.tenant_id
		WHERE t.id = $1 AND k.id = $2
		  AND k.status = 'active'
		  AND t.status = 'active'
		  AND (k.expires_at IS NULL OR k.expires_at > now())`, tenantID, apiKeyID))
}

func scanPrincipal(row *sql.Row) (Principal, error) {
	var principal Principal
	var tenantPolicyPayload, keyPolicyPayload []byte
	err := row.Scan(
		&principal.TenantID, &principal.APIKeyID, &principal.HomeRegion, &principal.ExecutionEpoch,
		&principal.TenantPolicy.Revision, &tenantPolicyPayload,
		&principal.APIKeyPolicy.Revision, &keyPolicyPayload,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, ErrInvalidAPIKey
	}
	if err != nil {
		return Principal{}, fmt.Errorf("authenticate API key: %w", err)
	}
	if err := json.Unmarshal(tenantPolicyPayload, &principal.TenantPolicy); err != nil {
		return Principal{}, fmt.Errorf("decode Tenant policy: %w", err)
	}
	if err := json.Unmarshal(keyPolicyPayload, &principal.APIKeyPolicy); err != nil {
		return Principal{}, fmt.Errorf("decode API key policy: %w", err)
	}
	return principal, nil
}

func (s *PostgresService) RevokeAPIKey(ctx context.Context, tenantID, apiKeyID string, revokedAt time.Time) error {
	if revokedAt.IsZero() {
		revokedAt = time.Now().UTC()
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE api_keys
		SET status = 'revoked', revoked_at = $3, revision = revision + 1, updated_at = now()
		WHERE tenant_id = $1 AND id = $2 AND status = 'active'`, tenantID, apiKeyID, revokedAt)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrInvalidAPIKey
	}
	return nil
}

func (s *PostgresService) PublishAPIKeyPolicy(
	ctx context.Context,
	tenantID, apiKeyID string,
	expectedRevision int64,
	policy core.APIKeyPolicy,
	actor ChangeActor,
) error {
	if tenantID == "" || apiKeyID == "" || actor.Type == "" || actor.ID == "" {
		return errors.New("API key policy publication requires identities and an actor")
	}
	if expectedRevision <= 0 || policy.Revision != expectedRevision+1 {
		return errors.New("API key policy revision must advance expected revision by one")
	}
	if _, err := quota.EffectiveLimits(core.QuotaLimits{}, policy.Limits); err != nil {
		return fmt.Errorf("validate API key policy: %w", err)
	}
	payload, err := apiKeyPolicyJSON(policy)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `
		UPDATE api_keys
		SET policy_revision = $4, policy = $5, revision = revision + 1, updated_at = now()
		WHERE tenant_id = $1 AND id = $2 AND policy_revision = $3 AND status = 'active'`,
		tenantID, apiKeyID, expectedRevision, policy.Revision, payload)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO api_key_policy_revisions (
			tenant_id, api_key_id, revision, policy, actor_type, actor_id
		) VALUES ($1,$2,$3,$4,$5,$6)`, tenantID, apiKeyID, policy.Revision, payload, actor.Type, actor.ID); err != nil {
		return fmt.Errorf("record API key policy revision: %w", err)
	}
	return tx.Commit()
}

func (s *PostgresService) PublishTenantPolicy(
	ctx context.Context,
	tenantID string,
	expectedRevision int64,
	policy core.TenantPolicy,
	actor ChangeActor,
) error {
	if tenantID == "" || actor.Type == "" || actor.ID == "" {
		return errors.New("Tenant policy publication requires identities and an actor")
	}
	if expectedRevision <= 0 || policy.Revision != expectedRevision+1 {
		return errors.New("Tenant policy revision must advance expected revision by one")
	}
	if _, err := quota.EffectiveLimits(policy.Limits, core.QuotaLimits{}); err != nil {
		return fmt.Errorf("validate Tenant policy: %w", err)
	}
	payload, err := tenantPolicyJSON(policy)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `
		UPDATE tenants
		SET policy_revision = $3, policy = $4, revision = revision + 1, updated_at = now()
		WHERE id = $1 AND policy_revision = $2 AND status <> 'closed'`,
		tenantID, expectedRevision, policy.Revision, payload)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tenant_policy_revisions (
			tenant_id, revision, policy, actor_type, actor_id
		) VALUES ($1,$2,$3,$4,$5)`, tenantID, policy.Revision, payload, actor.Type, actor.ID); err != nil {
		return fmt.Errorf("record Tenant policy revision: %w", err)
	}
	return tx.Commit()
}

func (s *PostgresService) digest(rawKey string) []byte {
	mac := hmac.New(sha256.New, s.pepper)
	_, _ = mac.Write([]byte(rawKey))
	return mac.Sum(nil)
}

func tenantPolicyJSON(policy core.TenantPolicy) ([]byte, error) {
	policy.Revision = 0
	return policyJSON(policy)
}

func apiKeyPolicyJSON(policy core.APIKeyPolicy) ([]byte, error) {
	policy.Revision = 0
	return policyJSON(policy)
}

func policyJSON(policy any) ([]byte, error) {
	payload, err := json.Marshal(policy)
	if err != nil {
		return nil, fmt.Errorf("encode policy: %w", err)
	}
	return payload, nil
}

func metadataJSON(metadata map[string]any) ([]byte, error) {
	if metadata == nil {
		return []byte(`{}`), nil
	}
	payload, err := json.Marshal(metadata)
	if err != nil {
		return nil, fmt.Errorf("encode metadata: %w", err)
	}
	return payload, nil
}
