package tenantadmin

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/toddzheng/llm-gateway/internal/access"
)

const (
	updateTenantOperation     = "tenant.update"
	transitionTenantOperation = "tenant.transition"
)

func (s *Service) UpdateTenant(
	ctx context.Context,
	actor ActorEnvelope,
	idempotencyKey string,
	command UpdateTenantCommand,
) (MutationResult, error) {
	if err := authorizeTenantWrite(actor, command.TenantID); err != nil {
		return MutationResult{}, err
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return MutationResult{}, err
	}
	if command.ExpectedRevision <= 0 || command.DisplayName == nil && command.Metadata == nil {
		return MutationResult{}, fmt.Errorf("%w: expected revision and at least one profile field are required", ErrInvalidArgument)
	}
	if command.DisplayName != nil && strings.TrimSpace(*command.DisplayName) == "" {
		return MutationResult{}, fmt.Errorf("%w: display name cannot be empty", ErrInvalidArgument)
	}
	if command.Metadata != nil {
		if err := validateMetadata(*command.Metadata); err != nil {
			return MutationResult{}, err
		}
	}
	requestHash, err := commandHash(command, actor.Reason)
	if err != nil {
		return MutationResult{}, err
	}
	tx, replay, err := s.beginCommand(ctx, actor, updateTenantOperation, idempotencyKey, requestHash)
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
	if current.Status == access.TenantClosed || current.Revision != command.ExpectedRevision {
		return MutationResult{}, ErrRevisionConflict
	}
	displayName := current.DisplayName
	if command.DisplayName != nil {
		displayName = strings.TrimSpace(*command.DisplayName)
	}
	metadata := current.Metadata
	if command.Metadata != nil {
		metadata = *command.Metadata
	}
	encodedMetadata, err := metadataPayload(metadata)
	if err != nil {
		return MutationResult{}, err
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE tenants
		SET display_name = $3, metadata = $4, revision = revision + 1, updated_at = now()
		WHERE id = $1 AND revision = $2 AND status <> 'closed'`,
		command.TenantID, command.ExpectedRevision, displayName, encodedMetadata)
	if err != nil {
		return MutationResult{}, err
	}
	if err := requireOneMutation(result); err != nil {
		return MutationResult{}, err
	}
	changedFields := make([]string, 0, 2)
	extra := map[string]any{}
	if command.DisplayName != nil {
		changedFields = append(changedFields, "display_name")
		extra["display_name_before"] = current.DisplayName
		extra["display_name_after"] = displayName
	}
	if command.Metadata != nil {
		changedFields = append(changedFields, "metadata")
		extra["metadata_digest_before"] = metadataDigest(current.Metadata)
		extra["metadata_digest_after"] = metadataDigest(metadata)
	}
	extra["changed_fields"] = changedFields
	return s.completeMutation(ctx, tx, actor, updateTenantOperation, idempotencyKey, requestHash, command.TenantID, "TenantProfileChanged", extra)
}

func (s *Service) TransitionTenant(
	ctx context.Context,
	actor ActorEnvelope,
	idempotencyKey string,
	command TransitionTenantCommand,
) (MutationResult, error) {
	if err := authorizeTenantWrite(actor, command.TenantID); err != nil {
		return MutationResult{}, err
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return MutationResult{}, err
	}
	if command.ExpectedRevision <= 0 || !validTenantStatus(command.Target) {
		return MutationResult{}, fmt.Errorf("%w: expected revision and valid target status are required", ErrInvalidArgument)
	}
	requestHash, err := commandHash(command, actor.Reason)
	if err != nil {
		return MutationResult{}, err
	}
	tx, replay, err := s.beginCommand(ctx, actor, transitionTenantOperation, idempotencyKey, requestHash)
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
	if current.Revision != command.ExpectedRevision {
		return MutationResult{}, ErrRevisionConflict
	}
	if !allowedTransition(current.Status, command.Target) {
		return MutationResult{}, ErrInvalidTransition
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE tenants
		SET status = $3,
		    suspended_at = CASE
		        WHEN $3 = 'suspended' THEN now()
		        WHEN $3 = 'active' THEN NULL
		        ELSE suspended_at
		    END,
		    closed_at = CASE WHEN $3 = 'closed' THEN now() ELSE NULL END,
		    revision = revision + 1,
		    updated_at = now()
		WHERE id = $1 AND revision = $2`, command.TenantID, command.ExpectedRevision, command.Target)
	if err != nil {
		return MutationResult{}, err
	}
	if err := requireOneMutation(result); err != nil {
		return MutationResult{}, err
	}
	return s.completeMutation(ctx, tx, actor, transitionTenantOperation, idempotencyKey, requestHash, command.TenantID, "TenantStatusChanged", map[string]any{
		"status_before": current.Status,
		"status_after":  command.Target,
	})
}

func (s *Service) completeMutation(
	ctx context.Context,
	tx *sql.Tx,
	actor ActorEnvelope,
	operation, idempotencyKey string,
	requestHash []byte,
	tenantID, eventType string,
	extra map[string]any,
) (MutationResult, error) {
	tenant, err := getTenantTx(ctx, tx, tenantID)
	if err != nil {
		return MutationResult{}, err
	}
	payload := tenantEventPayload(tenant)
	for key, value := range extra {
		payload[key] = value
	}
	if err := s.recordMutation(ctx, tx, actor, tenant, eventType, operation, payload); err != nil {
		return MutationResult{}, err
	}
	if err := recordCommandResult(ctx, tx, actor, operation, idempotencyKey, requestHash, tenant); err != nil {
		return MutationResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return MutationResult{}, err
	}
	return MutationResult{Tenant: tenant}, nil
}

func getTenantForUpdate(ctx context.Context, tx *sql.Tx, tenantID string) (access.Tenant, error) {
	return scanTenant(tx.QueryRowContext(ctx, `
		SELECT id, slug, display_name, status, home_region, execution_epoch,
		       policy_revision, policy, metadata, revision
		FROM tenants WHERE id = $1 FOR UPDATE`, tenantID))
}

func requireOneMutation(result sql.Result) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrRevisionConflict
	}
	return nil
}

func validTenantStatus(status access.TenantStatus) bool {
	return status == access.TenantActive || status == access.TenantSuspended || status == access.TenantClosed
}

func allowedTransition(current, target access.TenantStatus) bool {
	switch current {
	case access.TenantActive:
		return target == access.TenantSuspended || target == access.TenantClosed
	case access.TenantSuspended:
		return target == access.TenantActive || target == access.TenantClosed
	default:
		return false
	}
}
