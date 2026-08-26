package tenantadmin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/toddzheng/llm-gateway/internal/access"
	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/quota"
)

const publishPolicyOperation = "tenant.policy.publish"

func (s *Service) PublishTenantPolicy(
	ctx context.Context,
	actor ActorEnvelope,
	idempotencyKey string,
	command PublishPolicyCommand,
) (MutationResult, error) {
	if err := authorizeTenantWrite(actor, command.TenantID); err != nil {
		return MutationResult{}, err
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return MutationResult{}, err
	}
	if command.ExpectedRevision <= 0 || (command.Policy == nil) == (command.RestoreRevision == nil) {
		return MutationResult{}, fmt.Errorf("%w: expected revision and exactly one policy source are required", ErrInvalidArgument)
	}
	if command.RestoreRevision != nil && *command.RestoreRevision <= 0 {
		return MutationResult{}, fmt.Errorf("%w: restore revision must be positive", ErrInvalidArgument)
	}
	if command.Policy != nil {
		if command.Policy.Revision != command.ExpectedRevision+1 {
			return MutationResult{}, fmt.Errorf("%w: policy revision must advance by one", ErrInvalidArgument)
		}
		if err := validatePolicy(*command.Policy); err != nil {
			return MutationResult{}, err
		}
	}
	requestHash, err := commandHash(command, actor.Reason)
	if err != nil {
		return MutationResult{}, err
	}
	tx, replay, err := s.beginCommand(ctx, actor, publishPolicyOperation, idempotencyKey, requestHash)
	if err != nil || replay != nil {
		if replay != nil {
			return MutationResult{Tenant: *replay, Replay: true}, nil
		}
		return MutationResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := getTenantForUpdate(ctx, tx, command.TenantID)
	if err != nil {
		return MutationResult{}, err
	}
	if current.Status == access.TenantClosed || current.Policy.Revision != command.ExpectedRevision {
		return MutationResult{}, ErrRevisionConflict
	}
	nextRevision := command.ExpectedRevision + 1
	policy, err := policyForPublication(ctx, tx, command, nextRevision)
	if err != nil {
		return MutationResult{}, err
	}
	if err := validatePolicy(policy); err != nil {
		return MutationResult{}, err
	}
	payload, err := tenantPolicyPayload(policy)
	if err != nil {
		return MutationResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tenant_policy_revisions (
			tenant_id, revision, policy, actor_type, actor_id, change_reason
		) VALUES ($1,$2,$3,$4,$5,$6)`,
		command.TenantID, nextRevision, payload, actor.Type, actor.ID, actor.Reason); err != nil {
		return MutationResult{}, mapDatabaseError("publish Tenant Policy", err, ErrRevisionConflict)
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE tenants
		SET policy_revision = $3, policy = $4, revision = revision + 1, updated_at = now()
		WHERE id = $1 AND policy_revision = $2 AND status <> 'closed'`,
		command.TenantID, command.ExpectedRevision, nextRevision, payload)
	if err != nil {
		return MutationResult{}, err
	}
	if err := requireOneMutation(result); err != nil {
		return MutationResult{}, err
	}
	extra := map[string]any{
		"policy_revision_before": command.ExpectedRevision,
		"policy_revision_after":  nextRevision,
	}
	if command.RestoreRevision != nil {
		extra["restored_from_revision"] = *command.RestoreRevision
	}
	return s.completeMutation(ctx, tx, actor, publishPolicyOperation, idempotencyKey, requestHash, command.TenantID, "TenantPolicyPublished", extra)
}

func policyForPublication(
	ctx context.Context,
	tx *sql.Tx,
	command PublishPolicyCommand,
	nextRevision int64,
) (core.TenantPolicy, error) {
	if command.Policy != nil {
		policy := *command.Policy
		policy.Revision = nextRevision
		return policy, nil
	}
	var payload []byte
	err := tx.QueryRowContext(ctx, `
		SELECT policy FROM tenant_policy_revisions
		WHERE tenant_id = $1 AND revision = $2`, command.TenantID, *command.RestoreRevision).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return core.TenantPolicy{}, ErrNotFound
	}
	if err != nil {
		return core.TenantPolicy{}, err
	}
	var policy core.TenantPolicy
	if err := json.Unmarshal(payload, &policy); err != nil {
		return core.TenantPolicy{}, err
	}
	policy.Revision = nextRevision
	return policy, nil
}

func validatePolicy(policy core.TenantPolicy) error {
	if policy.MaxConcurrentResponses < 0 || policy.MaxInputItems < 0 || policy.RetentionSeconds < 0 {
		return fmt.Errorf("%w: policy limits cannot be negative", ErrInvalidArgument)
	}
	if _, err := quota.EffectiveLimits(policy.Limits, core.QuotaLimits{}); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	return nil
}
