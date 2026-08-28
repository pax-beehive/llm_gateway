package credentialadmin

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/toddzheng/llm-gateway/internal/access"
	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

const updateOperation = "gateway_api_key.update"

func (service *Service) Update(
	ctx context.Context,
	actor tenantadmin.ActorEnvelope,
	idempotencyKey string,
	command UpdateCommand,
) (MutationResult, error) {
	if err := authorizeMutation(actor, command.TenantID); err != nil {
		return MutationResult{}, err
	}
	if idempotencyKey == "" || len(idempotencyKey) > 255 || command.CredentialID == "" || command.ExpectedRevision <= 0 ||
		command.Name == nil && command.Metadata == nil && command.ExpiresAt == nil && !command.ClearExpiresAt ||
		command.ExpiresAt != nil && command.ClearExpiresAt {
		return MutationResult{}, fmt.Errorf("%w: IDs, idempotency, revision, and one unambiguous update are required", ErrInvalidArgument)
	}
	if command.Name != nil && strings.TrimSpace(*command.Name) == "" {
		return MutationResult{}, fmt.Errorf("%w: name cannot be empty", ErrInvalidArgument)
	}
	if command.Metadata != nil {
		if _, err := metadataJSON(*command.Metadata); err != nil {
			return MutationResult{}, err
		}
	}
	if command.ExpiresAt != nil && !command.ExpiresAt.After(service.now().UTC()) {
		return MutationResult{}, fmt.Errorf("%w: expiry must be in the future", ErrInvalidArgument)
	}
	requestHash, err := hashRequest(command, actor.Reason)
	if err != nil {
		return MutationResult{}, err
	}
	transaction, replay, err := service.beginCommand(ctx, actor, updateOperation, idempotencyKey, requestHash)
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
	if current.Status != access.APIKeyActive || current.Revision != command.ExpectedRevision {
		return MutationResult{}, ErrRevisionConflict
	}
	name := current.Name
	if command.Name != nil {
		name = strings.TrimSpace(*command.Name)
	}
	metadata := current.Metadata
	if command.Metadata != nil {
		metadata = *command.Metadata
	}
	metadataPayload, err := metadataJSON(metadata)
	if err != nil {
		return MutationResult{}, err
	}
	expiresAt := current.ExpiresAt
	if command.ExpiresAt != nil {
		expiresAt = command.ExpiresAt
	} else if command.ClearExpiresAt {
		expiresAt = nil
	}
	result, err := transaction.ExecContext(ctx, `
		UPDATE api_keys SET name = $4, metadata = $5, expires_at = $6,
		       revision = revision + 1, updated_at = now()
		WHERE tenant_id = $1 AND id = $2 AND revision = $3 AND status = 'active'`,
		command.TenantID, command.CredentialID, command.ExpectedRevision, name, metadataPayload, expiresAt)
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
	audit := map[string]any{"changed_fields": changedFields(command)}
	if command.Name != nil {
		audit["name_before"], audit["name_after"] = current.Name, credential.Name
	}
	if command.Metadata != nil {
		audit["metadata_digest_before"], audit["metadata_digest_after"] = metadataDigest(current.Metadata), metadataDigest(credential.Metadata)
	}
	if err := service.recordCredentialMutation(ctx, transaction, actor, credential, updateOperation, "GatewayAPIKeyChanged", audit); err != nil {
		return MutationResult{}, err
	}
	if err := recordCommandResult(ctx, transaction, actor, updateOperation, idempotencyKey, requestHash, credential); err != nil {
		return MutationResult{}, err
	}
	if err := transaction.Commit(); err != nil {
		return MutationResult{}, err
	}
	return MutationResult{Credential: credential}, nil
}

func getCredentialForUpdate(ctx context.Context, transaction *sql.Tx, tenantID, credentialID string) (Credential, error) {
	return scanCredential(transaction.QueryRowContext(ctx, `
		SELECT id, tenant_id, name, key_prefix, digest_version, status, revision,
		       policy_revision, policy, metadata, expires_at, revoked_at,
		       COALESCE(predecessor_id,''), COALESCE(replacement_id,''), grace_expires_at
		FROM api_keys WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, tenantID, credentialID))
}

func (service *Service) recordCredentialMutation(
	ctx context.Context,
	transaction *sql.Tx,
	actor tenantadmin.ActorEnvelope,
	credential Credential,
	action, eventType string,
	auditExtra map[string]any,
) error {
	auditID, err := randomID(service.random, "caud", 16)
	if err != nil {
		return err
	}
	eventID, err := randomID(service.random, "cevt", 16)
	if err != nil {
		return err
	}
	now := service.now().UTC()
	audit := map[string]any{
		"tenant_id": credential.TenantID, "api_key_id": credential.ID, "prefix": credential.Prefix,
		"digest_version": credential.DigestVersion, "revision": credential.Revision,
	}
	for key, value := range auditExtra {
		audit[key] = value
	}
	auditPayload, err := json.Marshal(audit)
	if err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx, `
		INSERT INTO control_audit_events (
			event_id, tenant_id, actor_type, actor_id, acting_tenant_id, scopes,
			request_id, reason, action, aggregate_revision, payload, occurred_at
		) VALUES ($1,$2,$3,$4,NULLIF($5,''),$6,$7,$8,$9,$10,$11,$12)`,
		auditID, credential.TenantID, actor.Type, actor.ID, actor.ActingTenantID, actor.Scopes,
		actor.RequestID, actor.Reason, action, credential.Revision, auditPayload, now); err != nil {
		return fmt.Errorf("append credential audit: %w", err)
	}
	eventPayload, err := service.projectionEventPayload(ctx, transaction, credential)
	if err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx, `
		INSERT INTO control_outbox (
			event_id, schema_version, aggregate_type, aggregate_id, aggregate_revision,
			tenant_id, event_type, occurred_at, payload
		) VALUES ($1,2,'GatewayAPIKey',$2,$3,$4,$5,$6,$7)`,
		eventID, credential.ID, credential.Revision, credential.TenantID, eventType, now, eventPayload); err != nil {
		return fmt.Errorf("append credential outbox: %w", err)
	}
	return nil
}

func (service *Service) projectionEventPayload(ctx context.Context, transaction *sql.Tx, credential Credential) ([]byte, error) {
	var digest []byte
	var tenantStatus access.TenantStatus
	var homeRegion string
	var executionEpoch, tenantRevision, tenantPolicyRevision int64
	var tenantPolicy []byte
	if err := transaction.QueryRowContext(ctx, `
		SELECT k.secret_digest, t.status, t.home_region, t.execution_epoch, t.revision, t.policy_revision, t.policy
		FROM api_keys k JOIN tenants t ON t.id = k.tenant_id
		WHERE k.tenant_id = $1 AND k.id = $2`, credential.TenantID, credential.ID).Scan(
		&digest, &tenantStatus, &homeRegion, &executionEpoch, &tenantRevision, &tenantPolicyRevision, &tenantPolicy); err != nil {
		return nil, err
	}
	var decodedTenantPolicy any
	if err := json.Unmarshal(tenantPolicy, &decodedTenantPolicy); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"tenant_id": credential.TenantID, "tenant_status": tenantStatus, "home_region": homeRegion,
		"execution_epoch": executionEpoch, "tenant_revision": tenantRevision,
		"tenant_policy_revision": tenantPolicyRevision, "tenant_policy": decodedTenantPolicy,
		"api_key_id": credential.ID, "prefix": credential.Prefix, "secret_digest": digest,
		"digest_version": credential.DigestVersion, "status": credential.Status, "key_revision": credential.Revision,
		"policy_revision": credential.Policy.Revision, "policy": credential.Policy, "expires_at": credential.ExpiresAt,
		"revoked_at": credential.RevokedAt, "occurred_at": service.now().UTC(),
	})
}

func changedFields(command UpdateCommand) []string {
	fields := make([]string, 0, 3)
	if command.Name != nil {
		fields = append(fields, "name")
	}
	if command.Metadata != nil {
		fields = append(fields, "metadata")
	}
	if command.ExpiresAt != nil || command.ClearExpiresAt {
		fields = append(fields, "expires_at")
	}
	return fields
}

func metadataDigest(metadata map[string]any) string {
	payload, _ := metadataJSON(metadata)
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func requireOne(result sql.Result) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrRevisionConflict
	}
	return nil
}
