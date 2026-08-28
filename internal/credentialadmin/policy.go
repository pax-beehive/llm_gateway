package credentialadmin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"

	"github.com/toddzheng/llm-gateway/internal/access"
	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/quota"
	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

const publishPolicyOperation = "gateway_api_key.policy.publish"

var allowedOperations = map[string]struct{}{
	"responses": {}, "chat_completions": {}, "embeddings": {}, "moderation": {}, "rerank": {},
	"models": {}, "capabilities": {}, "conversations": {},
}

func (service *Service) PublishPolicy(
	ctx context.Context,
	actor tenantadmin.ActorEnvelope,
	idempotencyKey string,
	command PublishPolicyCommand,
) (MutationResult, error) {
	if err := authorizeMutation(actor, command.TenantID); err != nil {
		return MutationResult{}, err
	}
	if idempotencyKey == "" || len(idempotencyKey) > 255 || command.CredentialID == "" || command.ExpectedRevision <= 0 ||
		(command.Policy == nil) == (command.RestoreRevision == nil) {
		return MutationResult{}, fmt.Errorf("%w: IDs, idempotency, revision, and exactly one policy source are required", ErrInvalidArgument)
	}
	if command.RestoreRevision != nil && *command.RestoreRevision <= 0 {
		return MutationResult{}, fmt.Errorf("%w: restore revision must be positive", ErrInvalidArgument)
	}
	if command.Policy != nil {
		if command.Policy.Revision != command.ExpectedRevision+1 {
			return MutationResult{}, fmt.Errorf("%w: policy revision must advance by one", ErrInvalidArgument)
		}
		if err := validateAPIKeyPolicy(*command.Policy); err != nil {
			return MutationResult{}, err
		}
	}
	requestHash, err := hashRequest(command, actor.Reason)
	if err != nil {
		return MutationResult{}, err
	}
	transaction, replay, err := service.beginCommand(ctx, actor, publishPolicyOperation, idempotencyKey, requestHash)
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
	if current.Status != access.APIKeyActive || current.Policy.Revision != command.ExpectedRevision {
		return MutationResult{}, ErrRevisionConflict
	}
	policy, err := policyForPublication(ctx, transaction, command)
	if err != nil {
		return MutationResult{}, err
	}
	if err := validateAPIKeyPolicy(policy); err != nil {
		return MutationResult{}, err
	}
	var tenantPolicyPayload []byte
	if err := transaction.QueryRowContext(ctx, `SELECT policy FROM tenants WHERE id = $1 AND status <> 'closed'`, command.TenantID).Scan(&tenantPolicyPayload); errors.Is(err, sql.ErrNoRows) {
		return MutationResult{}, ErrNotFound
	} else if err != nil {
		return MutationResult{}, err
	}
	var tenantPolicy core.TenantPolicy
	if err := json.Unmarshal(tenantPolicyPayload, &tenantPolicy); err != nil {
		return MutationResult{}, err
	}
	if policy.MaxConcurrentResponses != nil && tenantPolicy.MaxConcurrentResponses > 0 && *policy.MaxConcurrentResponses > tenantPolicy.MaxConcurrentResponses {
		return MutationResult{}, fmt.Errorf("%w: key concurrency cannot exceed Tenant concurrency", ErrPolicyDenied)
	}
	payload, err := policyJSON(policy)
	if err != nil {
		return MutationResult{}, err
	}
	if _, err := transaction.ExecContext(ctx, `
		INSERT INTO api_key_policy_revisions (
			tenant_id, api_key_id, revision, policy, actor_type, actor_id, change_reason
		) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		command.TenantID, command.CredentialID, policy.Revision, payload, actor.Type, actor.ID, actor.Reason); err != nil {
		return MutationResult{}, mapDatabaseError("publish Gateway API Key Policy", err)
	}
	result, err := transaction.ExecContext(ctx, `
		UPDATE api_keys SET policy_revision = $4, policy = $5, revision = revision + 1, updated_at = now()
		WHERE tenant_id = $1 AND id = $2 AND policy_revision = $3 AND status = 'active'`,
		command.TenantID, command.CredentialID, command.ExpectedRevision, policy.Revision, payload)
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
	audit := map[string]any{"policy_revision_before": command.ExpectedRevision, "policy_revision_after": policy.Revision}
	if command.RestoreRevision != nil {
		audit["restored_from_revision"] = *command.RestoreRevision
	}
	if err := service.recordCredentialMutation(ctx, transaction, actor, credential, publishPolicyOperation, "GatewayAPIKeyPolicyPublished", audit); err != nil {
		return MutationResult{}, err
	}
	if err := recordCommandResult(ctx, transaction, actor, publishPolicyOperation, idempotencyKey, requestHash, credential); err != nil {
		return MutationResult{}, err
	}
	if err := transaction.Commit(); err != nil {
		return MutationResult{}, err
	}
	return MutationResult{Credential: credential}, nil
}

func policyForPublication(ctx context.Context, transaction *sql.Tx, command PublishPolicyCommand) (core.APIKeyPolicy, error) {
	nextRevision := command.ExpectedRevision + 1
	if command.Policy != nil {
		policy := *command.Policy
		policy.Revision = nextRevision
		return policy, nil
	}
	var payload []byte
	if err := transaction.QueryRowContext(ctx, `
		SELECT policy FROM api_key_policy_revisions
		WHERE tenant_id = $1 AND api_key_id = $2 AND revision = $3`,
		command.TenantID, command.CredentialID, *command.RestoreRevision).Scan(&payload); errors.Is(err, sql.ErrNoRows) {
		return core.APIKeyPolicy{}, ErrNotFound
	} else if err != nil {
		return core.APIKeyPolicy{}, err
	}
	var policy core.APIKeyPolicy
	if err := json.Unmarshal(payload, &policy); err != nil {
		return core.APIKeyPolicy{}, err
	}
	policy.Revision = nextRevision
	return policy, nil
}

func (service *Service) GetPolicy(ctx context.Context, actor tenantadmin.ActorEnvelope, tenantID, credentialID string) (core.APIKeyPolicy, error) {
	credential, err := service.Get(ctx, actor, tenantID, credentialID)
	return credential.Policy, err
}

func (service *Service) ListPolicyRevisions(
	ctx context.Context,
	actor tenantadmin.ActorEnvelope,
	tenantID, credentialID, cursor string,
	limit int,
) (PolicyRevisionPage, error) {
	if err := authorizeRead(actor, tenantID); err != nil {
		return PolicyRevisionPage{}, err
	}
	if _, err := service.Get(ctx, actor, tenantID, credentialID); err != nil {
		return PolicyRevisionPage{}, err
	}
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 100 {
		return PolicyRevisionPage{}, fmt.Errorf("%w: limit must be between 1 and 100", ErrInvalidArgument)
	}
	after, err := decodeRevisionCursor(cursor)
	if err != nil {
		return PolicyRevisionPage{}, err
	}
	rows, err := service.database.QueryContext(ctx, `
		SELECT tenant_id, api_key_id, revision, policy, actor_type, actor_id, COALESCE(change_reason,''), created_at
		FROM api_key_policy_revisions
		WHERE tenant_id = $1 AND api_key_id = $2 AND revision > $3
		ORDER BY revision LIMIT $4`, tenantID, credentialID, after, limit+1)
	if err != nil {
		return PolicyRevisionPage{}, err
	}
	defer rows.Close()
	page := PolicyRevisionPage{Data: make([]PolicyRevision, 0, limit)}
	for rows.Next() {
		var revision PolicyRevision
		var payload []byte
		if err := rows.Scan(&revision.TenantID, &revision.CredentialID, &revision.Revision, &payload,
			&revision.ActorType, &revision.ActorID, &revision.ChangeReason, &revision.CreatedAt); err != nil {
			return PolicyRevisionPage{}, err
		}
		if err := json.Unmarshal(payload, &revision.Policy); err != nil {
			return PolicyRevisionPage{}, err
		}
		revision.Policy.Revision = revision.Revision
		page.Data = append(page.Data, revision)
	}
	if err := rows.Err(); err != nil {
		return PolicyRevisionPage{}, err
	}
	if len(page.Data) > limit {
		page.Data = page.Data[:limit]
		page.NextCursor = encodeCursor(strconv.FormatInt(page.Data[len(page.Data)-1].Revision, 10))
	}
	return page, nil
}

func (service *Service) GetEffectivePolicy(ctx context.Context, actor tenantadmin.ActorEnvelope, tenantID, credentialID string) (EffectivePolicy, error) {
	credential, err := service.Get(ctx, actor, tenantID, credentialID)
	if err != nil {
		return EffectivePolicy{}, err
	}
	var tenantPolicy core.TenantPolicy
	var revision int64
	var payload []byte
	if err := service.database.QueryRowContext(ctx, `SELECT policy_revision, policy FROM tenants WHERE id = $1`, tenantID).Scan(&revision, &payload); errors.Is(err, sql.ErrNoRows) {
		return EffectivePolicy{}, ErrNotFound
	} else if err != nil {
		return EffectivePolicy{}, err
	}
	if err := json.Unmarshal(payload, &tenantPolicy); err != nil {
		return EffectivePolicy{}, err
	}
	tenantPolicy.Revision = revision
	limits, err := quota.EffectiveLimits(tenantPolicy.Limits, credential.Policy.Limits)
	if err != nil {
		return EffectivePolicy{}, err
	}
	return EffectivePolicy{
		TenantPolicy: tenantPolicy, APIKeyPolicy: credential.Policy, Limits: limits,
		MaxConcurrentResponses: effectiveConcurrency(tenantPolicy.MaxConcurrentResponses, credential.Policy.MaxConcurrentResponses),
	}, nil
}

func effectiveConcurrency(tenant int, key *int) int {
	if key == nil {
		return tenant
	}
	if tenant <= 0 || *key < tenant {
		return *key
	}
	return tenant
}

func validateAPIKeyPolicy(policy core.APIKeyPolicy) error {
	if policy.MaxConcurrentResponses != nil && *policy.MaxConcurrentResponses < 0 {
		return fmt.Errorf("%w: max concurrent Responses cannot be negative", ErrInvalidArgument)
	}
	if _, err := quota.EffectiveLimits(core.QuotaLimits{}, policy.Limits); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	if err := validateRestriction("allowed_public_models", policy.AllowedPublicModels, nil); err != nil {
		return err
	}
	if err := validateRestriction("allowed_operations", policy.AllowedOperations, allowedOperations); err != nil {
		return err
	}
	if err := validateRestriction("allowed_regions", policy.AllowedRegions, nil); err != nil {
		return err
	}
	if policy.AllowedCIDRs != nil {
		if len(*policy.AllowedCIDRs) > 256 {
			return fmt.Errorf("%w: allowed_cidrs exceeds 256 values", ErrInvalidArgument)
		}
		seen := make(map[string]struct{}, len(*policy.AllowedCIDRs))
		for _, value := range *policy.AllowedCIDRs {
			if _, network, err := net.ParseCIDR(value); err != nil || network.String() != value {
				return fmt.Errorf("%w: allowed_cidrs contains invalid canonical CIDR %q", ErrInvalidArgument, value)
			}
			if _, duplicate := seen[value]; duplicate {
				return fmt.Errorf("%w: allowed_cidrs contains duplicate %q", ErrInvalidArgument, value)
			}
			seen[value] = struct{}{}
		}
	}
	return nil
}

func validateRestriction(name string, values *[]string, allowed map[string]struct{}) error {
	if values == nil {
		return nil
	}
	if len(*values) > 256 {
		return fmt.Errorf("%w: %s exceeds 256 values", ErrInvalidArgument, name)
	}
	seen := make(map[string]struct{}, len(*values))
	for _, value := range *values {
		if value == "" || strings.TrimSpace(value) != value {
			return fmt.Errorf("%w: %s contains an invalid empty or padded value", ErrInvalidArgument, name)
		}
		if allowed != nil {
			if _, ok := allowed[value]; !ok {
				keys := make([]string, 0, len(allowed))
				for key := range allowed {
					keys = append(keys, key)
				}
				slices.Sort(keys)
				return fmt.Errorf("%w: %s value %q is unsupported; allowed: %s", ErrInvalidArgument, name, value, strings.Join(keys, ", "))
			}
		}
		if _, duplicate := seen[value]; duplicate {
			return fmt.Errorf("%w: %s contains duplicate %q", ErrInvalidArgument, name, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func decodeRevisionCursor(cursor string) (int64, error) {
	value, err := decodeCursor(cursor)
	if err != nil {
		return 0, err
	}
	if value == "" {
		return 0, nil
	}
	revision, err := strconv.ParseInt(value, 10, 64)
	if err != nil || revision <= 0 {
		return 0, fmt.Errorf("%w: invalid revision cursor", ErrInvalidArgument)
	}
	return revision, nil
}
