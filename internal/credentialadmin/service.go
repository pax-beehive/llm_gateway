package credentialadmin

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/toddzheng/llm-gateway/internal/access"
	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/quota"
	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

const issueOperation = "gateway_api_key.issue"

type Service struct {
	database *sql.DB
	peppers  map[int16][]byte
	current  int16
	now      func() time.Time
	random   io.Reader
}

func NewService(database *sql.DB, ring PepperRing, now func() time.Time, random io.Reader) (*Service, error) {
	if database == nil {
		return nil, errors.New("Gateway Credential Administration requires PostgreSQL")
	}
	if ring.CurrentVersion <= 0 {
		return nil, errors.New("current digest version must be positive")
	}
	peppers := make(map[int16][]byte, len(ring.Peppers))
	for version, pepper := range ring.Peppers {
		if version <= 0 || len(pepper) < 16 {
			return nil, errors.New("every digest pepper must have a positive version and at least 16 bytes")
		}
		peppers[version] = append([]byte(nil), pepper...)
	}
	if len(peppers[ring.CurrentVersion]) == 0 {
		return nil, errors.New("current digest version has no configured pepper")
	}
	if now == nil {
		now = time.Now
	}
	if random == nil {
		random = rand.Reader
	}
	return &Service{database: database, peppers: peppers, current: ring.CurrentVersion, now: now, random: random}, nil
}

func (service *Service) Issue(
	ctx context.Context,
	actor tenantadmin.ActorEnvelope,
	idempotencyKey string,
	command IssueCommand,
) (IssueResult, error) {
	if err := authorizeMutation(actor, command.TenantID); err != nil {
		return IssueResult{}, err
	}
	if err := validateIssue(idempotencyKey, command, service.now().UTC()); err != nil {
		return IssueResult{}, err
	}
	if command.Policy.Revision == 0 {
		command.Policy.Revision = 1
	}
	requestHash, err := hashRequest(command, actor.Reason)
	if err != nil {
		return IssueResult{}, err
	}
	transaction, replay, err := service.beginCommand(ctx, actor, issueOperation, idempotencyKey, requestHash)
	if err != nil || replay != nil {
		if replay != nil {
			return IssueResult{Credential: *replay, Replay: true}, nil
		}
		return IssueResult{}, err
	}
	defer func() { _ = transaction.Rollback() }()
	var tenantStatus access.TenantStatus
	if err := transaction.QueryRowContext(ctx, `SELECT status FROM tenants WHERE id = $1 FOR SHARE`, command.TenantID).Scan(&tenantStatus); errors.Is(err, sql.ErrNoRows) {
		return IssueResult{}, ErrNotFound
	} else if err != nil {
		return IssueResult{}, err
	}
	if tenantStatus != access.TenantActive {
		return IssueResult{}, ErrPolicyDenied
	}
	identifierBytes := make([]byte, 16)
	secretBytes := make([]byte, 32)
	if _, err := io.ReadFull(service.random, identifierBytes); err != nil {
		return IssueResult{}, fmt.Errorf("generate Gateway API Key identity: %w", err)
	}
	if _, err := io.ReadFull(service.random, secretBytes); err != nil {
		return IssueResult{}, fmt.Errorf("generate Gateway API Key secret: %w", err)
	}
	credentialID := "gak_" + hex.EncodeToString(identifierBytes)
	rawSecret := "gw_" + base64.RawURLEncoding.EncodeToString(secretBytes)
	prefix := rawSecret
	if len(prefix) > 12 {
		prefix = prefix[:12]
	}
	digest := digestSecret(service.peppers[service.current], rawSecret)
	policyPayload, err := policyJSON(command.Policy)
	if err != nil {
		return IssueResult{}, err
	}
	metadataPayload, err := metadataJSON(command.Metadata)
	if err != nil {
		return IssueResult{}, err
	}
	if _, err := transaction.ExecContext(ctx, `
		INSERT INTO api_keys (
			id, tenant_id, name, key_prefix, secret_digest, digest_version,
			status, revision, policy_revision, policy, metadata, expires_at
		) VALUES ($1,$2,$3,$4,$5,$6,'active',1,1,$7,$8,$9)`,
		credentialID, command.TenantID, strings.TrimSpace(command.Name), prefix, digest,
		service.current, policyPayload, metadataPayload, command.ExpiresAt); err != nil {
		return IssueResult{}, mapDatabaseError("issue Gateway API Key", err)
	}
	if _, err := transaction.ExecContext(ctx, `
		INSERT INTO api_key_policy_revisions (
			tenant_id, api_key_id, revision, policy, actor_type, actor_id, change_reason
		) VALUES ($1,$2,1,$3,$4,$5,$6)`,
		command.TenantID, credentialID, policyPayload, actor.Type, actor.ID, actor.Reason); err != nil {
		return IssueResult{}, fmt.Errorf("record initial Gateway API Key Policy: %w", err)
	}
	credential, err := getCredentialTx(ctx, transaction, command.TenantID, credentialID)
	if err != nil {
		return IssueResult{}, err
	}
	if err := service.recordCredentialMutation(ctx, transaction, actor, credential, issueOperation, "GatewayAPIKeyIssued", nil); err != nil {
		return IssueResult{}, err
	}
	if err := recordCommandResult(ctx, transaction, actor, issueOperation, idempotencyKey, requestHash, credential); err != nil {
		return IssueResult{}, err
	}
	if err := transaction.Commit(); err != nil {
		return IssueResult{}, err
	}
	return IssueResult{Credential: credential, RawSecret: rawSecret}, nil
}

func (service *Service) beginCommand(
	ctx context.Context,
	actor tenantadmin.ActorEnvelope,
	operation string,
	idempotencyKey string,
	requestHash []byte,
) (*sql.Tx, *Credential, error) {
	transaction, err := service.database.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	lockIdentity := strings.Join([]string{actor.Type, actor.ID, operation, idempotencyKey}, "\x1f")
	if _, err := transaction.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lockIdentity); err != nil {
		_ = transaction.Rollback()
		return nil, nil, err
	}
	var storedHash, resultPayload []byte
	err = transaction.QueryRowContext(ctx, `
		SELECT request_hash, result FROM control_command_idempotency
		WHERE actor_type = $1 AND actor_id = $2 AND operation = $3 AND idempotency_key = $4`,
		actor.Type, actor.ID, operation, idempotencyKey).Scan(&storedHash, &resultPayload)
	if errors.Is(err, sql.ErrNoRows) {
		return transaction, nil, nil
	}
	if err != nil {
		_ = transaction.Rollback()
		return nil, nil, err
	}
	if !hmac.Equal(storedHash, requestHash) {
		_ = transaction.Rollback()
		return nil, nil, ErrIdempotencyConflict
	}
	var credential Credential
	if err := json.Unmarshal(resultPayload, &credential); err != nil {
		_ = transaction.Rollback()
		return nil, nil, err
	}
	if err := transaction.Commit(); err != nil {
		return nil, nil, err
	}
	return nil, &credential, nil
}

func recordCommandResult(
	ctx context.Context,
	transaction *sql.Tx,
	actor tenantadmin.ActorEnvelope,
	operation string,
	idempotencyKey string,
	requestHash []byte,
	credential Credential,
) error {
	payload, err := json.Marshal(credential)
	if err != nil {
		return err
	}
	_, err = transaction.ExecContext(ctx, `
		INSERT INTO control_command_idempotency (
			actor_type, actor_id, operation, idempotency_key, request_hash, result
		) VALUES ($1,$2,$3,$4,$5,$6)`,
		actor.Type, actor.ID, operation, idempotencyKey, requestHash, payload)
	return err
}

func getCredentialTx(ctx context.Context, transaction *sql.Tx, tenantID, credentialID string) (Credential, error) {
	return scanCredential(transaction.QueryRowContext(ctx, `
		SELECT id, tenant_id, name, key_prefix, digest_version, status, revision,
		       policy_revision, policy, metadata, expires_at, revoked_at,
		       COALESCE(predecessor_id,''), COALESCE(replacement_id,''), grace_expires_at
		FROM api_keys WHERE tenant_id = $1 AND id = $2`, tenantID, credentialID))
}

type scanner interface{ Scan(...any) error }

func scanCredential(source scanner) (Credential, error) {
	var credential Credential
	var policyPayload, metadataPayload []byte
	err := source.Scan(
		&credential.ID, &credential.TenantID, &credential.Name, &credential.Prefix, &credential.DigestVersion,
		&credential.Status, &credential.Revision, &credential.Policy.Revision, &policyPayload, &metadataPayload,
		&credential.ExpiresAt, &credential.RevokedAt, &credential.PredecessorID, &credential.ReplacementID, &credential.GraceExpiresAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Credential{}, ErrNotFound
	}
	if err != nil {
		return Credential{}, err
	}
	if err := json.Unmarshal(policyPayload, &credential.Policy); err != nil {
		return Credential{}, err
	}
	if err := json.Unmarshal(metadataPayload, &credential.Metadata); err != nil {
		return Credential{}, err
	}
	return credential, nil
}

func validateIssue(idempotencyKey string, command IssueCommand, now time.Time) error {
	if idempotencyKey == "" || len(idempotencyKey) > 255 || command.TenantID == "" || strings.TrimSpace(command.Name) == "" {
		return fmt.Errorf("%w: Tenant, name, and Idempotency-Key are required", ErrInvalidArgument)
	}
	if command.Policy.Revision != 0 && command.Policy.Revision != 1 {
		return fmt.Errorf("%w: initial Gateway API Key Policy revision must be 1", ErrInvalidArgument)
	}
	if command.ExpiresAt != nil && !command.ExpiresAt.After(now) {
		return fmt.Errorf("%w: expiry must be in the future", ErrInvalidArgument)
	}
	if _, err := quota.EffectiveLimits(core.QuotaLimits{}, command.Policy.Limits); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	_, err := metadataJSON(command.Metadata)
	return err
}

func authorizeMutation(actor tenantadmin.ActorEnvelope, tenantID string) error {
	if actor.Type == "" || actor.ID == "" || actor.RequestID == "" || strings.TrimSpace(actor.Reason) == "" {
		return fmt.Errorf("%w: mutation Actor Envelope is incomplete", ErrInvalidArgument)
	}
	for _, scope := range actor.Scopes {
		if scope == tenantadmin.ScopePlatformWrite || scope == tenantadmin.ScopeTenantWrite && actor.ActingTenantID == tenantID {
			return nil
		}
	}
	return ErrPolicyDenied
}

func metadataJSON(metadata map[string]any) ([]byte, error) {
	if metadata == nil {
		return []byte(`{}`), nil
	}
	for key := range metadata {
		switch strings.ToLower(key) {
		case "status", "policy", "permissions", "limits", "digest", "secret", "tenant_id", "expires_at":
			return nil, fmt.Errorf("%w: %q is typed behavior and cannot be metadata", ErrInvalidArgument, key)
		}
	}
	payload, err := json.Marshal(metadata)
	if err != nil || len(payload) > 64<<10 {
		return nil, fmt.Errorf("%w: invalid Gateway API Key metadata", ErrInvalidArgument)
	}
	return payload, nil
}

func policyJSON(policy core.APIKeyPolicy) ([]byte, error) {
	policy.Revision = 0
	return json.Marshal(policy)
}

func hashRequest(command any, reason string) ([]byte, error) {
	payload, err := json.Marshal(struct {
		Command any    `json:"command"`
		Reason  string `json:"reason"`
	}{Command: command, Reason: reason})
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(payload)
	return digest[:], nil
}

func digestSecret(pepper []byte, rawSecret string) []byte {
	digest := hmac.New(sha256.New, pepper)
	_, _ = digest.Write([]byte(rawSecret))
	return digest.Sum(nil)
}

func randomID(source io.Reader, prefix string, size int) (string, error) {
	payload := make([]byte, size)
	if _, err := io.ReadFull(source, payload); err != nil {
		return "", err
	}
	return prefix + "_" + hex.EncodeToString(payload), nil
}

func mapDatabaseError(operation string, err error) error {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) && postgresError.Code == "23505" {
		return fmt.Errorf("%w: %s", ErrAlreadyExists, operation)
	}
	return fmt.Errorf("%s: %w", operation, err)
}
