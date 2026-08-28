package credentialadmin

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/toddzheng/llm-gateway/internal/access"
	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

const (
	revokeOperation = "gateway_api_key.revoke"
	rotateOperation = "gateway_api_key.rotate"
)

func (service *Service) Revoke(
	ctx context.Context,
	actor tenantadmin.ActorEnvelope,
	idempotencyKey string,
	command RevokeCommand,
) (MutationResult, error) {
	if err := authorizeMutation(actor, command.TenantID); err != nil {
		return MutationResult{}, err
	}
	if idempotencyKey == "" || len(idempotencyKey) > 255 || command.CredentialID == "" || command.ExpectedRevision <= 0 {
		return MutationResult{}, fmt.Errorf("%w: IDs, idempotency, and expected revision are required", ErrInvalidArgument)
	}
	requestHash, err := hashRequest(command, actor.Reason)
	if err != nil {
		return MutationResult{}, err
	}
	transaction, replay, err := service.beginCommand(ctx, actor, revokeOperation, idempotencyKey, requestHash)
	if err != nil || replay != nil {
		if replay != nil {
			return MutationResult{Credential: *replay, Replay: true}, nil
		}
		return MutationResult{}, err
	}
	defer func() { _ = transaction.Rollback() }()
	current, err := getCredentialForUpdate(ctx, transaction, command.TenantID, command.CredentialID)
	if err != nil {
		return MutationResult{}, err
	}
	if current.Status == access.APIKeyRevoked {
		if err := recordCommandResult(ctx, transaction, actor, revokeOperation, idempotencyKey, requestHash, current); err != nil {
			return MutationResult{}, err
		}
		if err := transaction.Commit(); err != nil {
			return MutationResult{}, err
		}
		return MutationResult{Credential: current, Replay: true}, nil
	}
	if current.Revision != command.ExpectedRevision {
		return MutationResult{}, ErrRevisionConflict
	}
	revokedAt := service.now().UTC()
	result, err := transaction.ExecContext(ctx, `
		UPDATE api_keys SET status = 'revoked', revoked_at = $4, grace_expires_at = NULL,
		       revision = revision + 1, updated_at = now()
		WHERE tenant_id = $1 AND id = $2 AND revision = $3 AND status = 'active'`,
		command.TenantID, command.CredentialID, command.ExpectedRevision, revokedAt)
	if err != nil {
		return MutationResult{}, err
	}
	if err := requireOne(result); err != nil {
		return MutationResult{}, err
	}
	credential, err := getCredentialTx(ctx, transaction, command.TenantID, command.CredentialID)
	if err != nil {
		return MutationResult{}, err
	}
	if err := service.recordCredentialMutation(ctx, transaction, actor, credential, revokeOperation, "GatewayAPIKeyRevoked", nil); err != nil {
		return MutationResult{}, err
	}
	if err := recordCommandResult(ctx, transaction, actor, revokeOperation, idempotencyKey, requestHash, credential); err != nil {
		return MutationResult{}, err
	}
	if err := transaction.Commit(); err != nil {
		return MutationResult{}, err
	}
	return MutationResult{Credential: credential}, nil
}

func (service *Service) Rotate(
	ctx context.Context,
	actor tenantadmin.ActorEnvelope,
	idempotencyKey string,
	command RotateCommand,
) (RotationResult, error) {
	if err := authorizeMutation(actor, command.TenantID); err != nil {
		return RotationResult{}, err
	}
	if idempotencyKey == "" || len(idempotencyKey) > 255 || command.CredentialID == "" || command.ExpectedRevision <= 0 ||
		command.RevokeImmediately == (command.GraceExpiresAt != nil) {
		return RotationResult{}, fmt.Errorf("%w: IDs, idempotency, revision, and exactly one rotation mode are required", ErrInvalidArgument)
	}
	if command.GraceExpiresAt != nil && !command.GraceExpiresAt.After(service.now().UTC()) {
		return RotationResult{}, fmt.Errorf("%w: grace deadline must be in the future", ErrInvalidArgument)
	}
	requestHash, err := hashRequest(command, actor.Reason)
	if err != nil {
		return RotationResult{}, err
	}
	transaction, replay, err := service.beginCommand(ctx, actor, rotateOperation, idempotencyKey, requestHash)
	if err != nil || replay != nil {
		if replay != nil {
			predecessor, getErr := service.Get(ctx, actor, replay.TenantID, replay.PredecessorID)
			if getErr != nil {
				return RotationResult{}, getErr
			}
			return RotationResult{Predecessor: predecessor, Replacement: *replay, Replay: true}, nil
		}
		return RotationResult{}, err
	}
	defer func() { _ = transaction.Rollback() }()
	predecessor, err := getCredentialForUpdate(ctx, transaction, command.TenantID, command.CredentialID)
	if err != nil {
		return RotationResult{}, err
	}
	if predecessor.Status != access.APIKeyActive || predecessor.Revision != command.ExpectedRevision || predecessor.ReplacementID != "" {
		return RotationResult{}, ErrRevisionConflict
	}
	replacementPolicy := predecessor.Policy
	replacementPolicy.Revision = 1
	replacement, rawSecret, err := service.insertReplacement(ctx, transaction, predecessor, replacementPolicy)
	if err != nil {
		return RotationResult{}, err
	}
	status := access.APIKeyActive
	var revokedAt *time.Time
	if command.RevokeImmediately {
		status = access.APIKeyRevoked
		value := service.now().UTC()
		revokedAt = &value
	}
	result, err := transaction.ExecContext(ctx, `
		UPDATE api_keys SET replacement_id = $4, grace_expires_at = $5, status = $6, revoked_at = $7,
		       revision = revision + 1, updated_at = now()
		WHERE tenant_id = $1 AND id = $2 AND revision = $3 AND status = 'active'`,
		command.TenantID, command.CredentialID, command.ExpectedRevision, replacement.ID,
		command.GraceExpiresAt, status, revokedAt)
	if err != nil {
		return RotationResult{}, err
	}
	if err := requireOne(result); err != nil {
		return RotationResult{}, err
	}
	predecessor, err = getCredentialTx(ctx, transaction, command.TenantID, command.CredentialID)
	if err != nil {
		return RotationResult{}, err
	}
	if err := service.recordCredentialMutation(ctx, transaction, actor, replacement, issueOperation, "GatewayAPIKeyIssued", map[string]any{"rotation_predecessor_id": predecessor.ID}); err != nil {
		return RotationResult{}, err
	}
	eventType := "GatewayAPIKeyChanged"
	if predecessor.Status == access.APIKeyRevoked {
		eventType = "GatewayAPIKeyRevoked"
	}
	if err := service.recordCredentialMutation(ctx, transaction, actor, predecessor, rotateOperation, eventType, map[string]any{"replacement_id": replacement.ID}); err != nil {
		return RotationResult{}, err
	}
	if err := recordCommandResult(ctx, transaction, actor, rotateOperation, idempotencyKey, requestHash, replacement); err != nil {
		return RotationResult{}, err
	}
	if err := transaction.Commit(); err != nil {
		return RotationResult{}, err
	}
	return RotationResult{Predecessor: predecessor, Replacement: replacement, RawSecret: rawSecret}, nil
}

func (service *Service) insertReplacement(
	ctx context.Context,
	transaction *sql.Tx,
	predecessor Credential,
	policy core.APIKeyPolicy,
) (Credential, string, error) {
	identifierBytes := make([]byte, 16)
	secretBytes := make([]byte, 32)
	if _, err := io.ReadFull(service.random, identifierBytes); err != nil {
		return Credential{}, "", fmt.Errorf("generate replacement identity: %w", err)
	}
	if _, err := io.ReadFull(service.random, secretBytes); err != nil {
		return Credential{}, "", fmt.Errorf("generate replacement secret: %w", err)
	}
	id := "gak_" + hex.EncodeToString(identifierBytes)
	rawSecret := "gw_" + base64.RawURLEncoding.EncodeToString(secretBytes)
	prefix := rawSecret
	if len(prefix) > 12 {
		prefix = prefix[:12]
	}
	policyPayload, err := policyJSON(policy)
	if err != nil {
		return Credential{}, "", err
	}
	metadataPayload, err := metadataJSON(predecessor.Metadata)
	if err != nil {
		return Credential{}, "", err
	}
	if _, err := transaction.ExecContext(ctx, `
		INSERT INTO api_keys (
			id, tenant_id, name, key_prefix, secret_digest, digest_version, status, revision,
			policy_revision, policy, metadata, expires_at, predecessor_id
		) VALUES ($1,$2,$3,$4,$5,$6,'active',1,1,$7,$8,$9,$10)`,
		id, predecessor.TenantID, predecessor.Name, prefix, digestSecret(service.peppers[service.current], rawSecret),
		service.current, policyPayload, metadataPayload, predecessor.ExpiresAt, predecessor.ID); err != nil {
		return Credential{}, "", mapDatabaseError("rotate Gateway API Key", err)
	}
	if _, err := transaction.ExecContext(ctx, `
		INSERT INTO api_key_policy_revisions (
			tenant_id, api_key_id, revision, policy, actor_type, actor_id, change_reason
		) VALUES ($1,$2,1,$3,'system','gateway-api-key-rotation','copied predecessor policy')`,
		predecessor.TenantID, id, policyPayload); err != nil {
		return Credential{}, "", err
	}
	credential, err := getCredentialTx(ctx, transaction, predecessor.TenantID, id)
	return credential, rawSecret, err
}

func (service *Service) ReconcileExpiredGrace(ctx context.Context, limit int) (int, error) {
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 1000 {
		return 0, fmt.Errorf("%w: reconciliation limit must be between 1 and 1000", ErrInvalidArgument)
	}
	transaction, err := service.database.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = transaction.Rollback() }()
	rows, err := transaction.QueryContext(ctx, `
		SELECT tenant_id, id FROM api_keys
		WHERE status = 'active' AND grace_expires_at IS NOT NULL AND grace_expires_at <= $1
		ORDER BY grace_expires_at, tenant_id, id
		FOR UPDATE SKIP LOCKED LIMIT $2`, service.now().UTC(), limit)
	if err != nil {
		return 0, err
	}
	type identity struct{ tenantID, credentialID string }
	identities := make([]identity, 0, limit)
	for rows.Next() {
		var value identity
		if err := rows.Scan(&value.tenantID, &value.credentialID); err != nil {
			_ = rows.Close()
			return 0, err
		}
		identities = append(identities, value)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	actor := tenantadmin.ActorEnvelope{
		Type: "system", ID: "gateway-api-key-grace-reconciler", Scopes: []string{tenantadmin.ScopePlatformWrite},
		RequestID: "grace-reconcile-" + strings.ReplaceAll(service.now().UTC().Format(time.RFC3339Nano), ":", "-"),
		Reason:    "rotation grace deadline expired",
	}
	for _, identity := range identities {
		if _, err := transaction.ExecContext(ctx, `
			UPDATE api_keys SET status = 'revoked', revoked_at = $3, grace_expires_at = NULL,
			       revision = revision + 1, updated_at = now()
			WHERE tenant_id = $1 AND id = $2 AND status = 'active'`,
			identity.tenantID, identity.credentialID, service.now().UTC()); err != nil {
			return 0, err
		}
		credential, err := getCredentialTx(ctx, transaction, identity.tenantID, identity.credentialID)
		if err != nil {
			return 0, err
		}
		if err := service.recordCredentialMutation(ctx, transaction, actor, credential, revokeOperation, "GatewayAPIKeyRevoked", map[string]any{"reason": "rotation_grace_expired"}); err != nil {
			return 0, err
		}
	}
	if err := transaction.Commit(); err != nil {
		return 0, err
	}
	return len(identities), nil
}
