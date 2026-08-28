package providerconnection

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

const (
	updateOperation  = "provider_connection.update"
	enableOperation  = "provider_connection.enable"
	disableOperation = "provider_connection.disable"
)

func (service *Service) Update(ctx context.Context, actor tenantadmin.ActorEnvelope, idempotencyKey string, command UpdateCommand) (MutationResult, error) {
	if err := authorizeMutation(actor); err != nil {
		return MutationResult{}, err
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return MutationResult{}, err
	}
	if !resourceIDPattern.MatchString(command.ConnectionID) || command.ExpectedRevision <= 0 ||
		command.DisplayName == nil && command.BaseURL == nil && command.Region == nil && command.CredentialScope == nil && command.CapabilityDeclaration == nil {
		return MutationResult{}, fmt.Errorf("%w: connection ID, expected revision, and at least one update are required", ErrInvalidArgument)
	}
	if command.DisplayName != nil && (strings.TrimSpace(*command.DisplayName) == "" || len(*command.DisplayName) > 256) ||
		command.Region != nil && (strings.TrimSpace(*command.Region) == "" || len(*command.Region) > 128) ||
		command.CredentialScope != nil && (strings.TrimSpace(*command.CredentialScope) == "" || len(*command.CredentialScope) > 256) {
		return MutationResult{}, ErrInvalidArgument
	}
	if command.BaseURL != nil {
		if err := validateBaseURL(*command.BaseURL); err != nil {
			return MutationResult{}, err
		}
	}
	requestHash, err := hashCommand(command, actor.Reason)
	if err != nil {
		return MutationResult{}, err
	}
	transaction, replay, err := service.beginCommand(ctx, actor, updateOperation, idempotencyKey, requestHash)
	if err != nil || replay != nil {
		if replay != nil {
			return MutationResult{Connection: *replay, Replay: true}, nil
		}
		return MutationResult{}, err
	}
	defer func() { _ = transaction.Rollback() }()
	current, err := getConnectionForUpdate(ctx, transaction, command.ConnectionID)
	if err != nil {
		return MutationResult{}, err
	}
	if current.Revision != command.ExpectedRevision {
		return MutationResult{}, ErrRevisionConflict
	}
	displayName, baseURL, region, credentialScope := current.DisplayName, current.BaseURL, current.Region, current.CredentialScope
	capability := current.CapabilityDeclaration
	changedFields := make([]string, 0, 5)
	if command.DisplayName != nil {
		displayName = strings.TrimSpace(*command.DisplayName)
		changedFields = append(changedFields, "display_name")
	}
	if command.BaseURL != nil {
		baseURL = *command.BaseURL
		changedFields = append(changedFields, "base_url")
	}
	if command.Region != nil {
		region = strings.TrimSpace(*command.Region)
		changedFields = append(changedFields, "region")
	}
	if command.CredentialScope != nil {
		credentialScope = strings.TrimSpace(*command.CredentialScope)
		changedFields = append(changedFields, "credential_scope")
	}
	if command.CapabilityDeclaration != nil {
		if err := validateCapabilityProfile(*command.CapabilityDeclaration, current.CapabilityDeclaration.Revision+1); err != nil {
			return MutationResult{}, err
		}
		capability = *command.CapabilityDeclaration
		changedFields = append(changedFields, "capability_declaration")
	}
	capabilityPayload, err := json.Marshal(capability)
	if err != nil {
		return MutationResult{}, err
	}
	result, err := transaction.ExecContext(ctx, `
		UPDATE provider_connections SET display_name=$3, base_url=$4, region=$5, credential_scope=$6,
			capability_declaration=$7, revision=revision+1, updated_at=$8
		WHERE id=$1 AND revision=$2`, command.ConnectionID, command.ExpectedRevision,
		displayName, baseURL, region, credentialScope, capabilityPayload, service.now().UTC())
	if err != nil {
		return MutationResult{}, err
	}
	if err := requireOne(result); err != nil {
		return MutationResult{}, err
	}
	return service.completeConnectionMutation(ctx, transaction, actor, updateOperation, idempotencyKey, requestHash,
		command.ConnectionID, "ProviderConnectionChanged", map[string]any{"changed_fields": changedFields})
}

func (service *Service) Enable(ctx context.Context, actor tenantadmin.ActorEnvelope, key string, command StatusCommand) (MutationResult, error) {
	return service.setStatus(ctx, actor, key, command, StatusEnabled, enableOperation, "ProviderConnectionEnabled")
}

func (service *Service) Disable(ctx context.Context, actor tenantadmin.ActorEnvelope, key string, command StatusCommand) (MutationResult, error) {
	return service.setStatus(ctx, actor, key, command, StatusDisabled, disableOperation, "ProviderConnectionDisabled")
}

func (service *Service) setStatus(ctx context.Context, actor tenantadmin.ActorEnvelope, idempotencyKey string, command StatusCommand, target AdministrativeStatus, operation, eventType string) (MutationResult, error) {
	if err := authorizeMutation(actor); err != nil {
		return MutationResult{}, err
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil || !resourceIDPattern.MatchString(command.ConnectionID) || command.ExpectedRevision <= 0 {
		return MutationResult{}, ErrInvalidArgument
	}
	requestHash, err := hashCommand(struct {
		StatusCommand
		Target AdministrativeStatus
	}{command, target}, actor.Reason)
	if err != nil {
		return MutationResult{}, err
	}
	transaction, replay, err := service.beginCommand(ctx, actor, operation, idempotencyKey, requestHash)
	if err != nil || replay != nil {
		if replay != nil {
			return MutationResult{Connection: *replay, Replay: true}, nil
		}
		return MutationResult{}, err
	}
	defer func() { _ = transaction.Rollback() }()
	current, err := getConnectionForUpdate(ctx, transaction, command.ConnectionID)
	if err != nil {
		return MutationResult{}, err
	}
	if current.Revision != command.ExpectedRevision || current.AdministrativeStatus == target {
		return MutationResult{}, ErrRevisionConflict
	}
	result, err := transaction.ExecContext(ctx, `
		UPDATE provider_connections SET administrative_status=$3, revision=revision+1, updated_at=$4
		WHERE id=$1 AND revision=$2`, command.ConnectionID, command.ExpectedRevision, target, service.now().UTC())
	if err != nil {
		return MutationResult{}, err
	}
	if err := requireOne(result); err != nil {
		return MutationResult{}, err
	}
	return service.completeConnectionMutation(ctx, transaction, actor, operation, idempotencyKey, requestHash, command.ConnectionID, eventType, nil)
}

func (service *Service) completeConnectionMutation(ctx context.Context, transaction *sql.Tx, actor tenantadmin.ActorEnvelope, operation, idempotencyKey string, requestHash []byte, connectionID, eventType string, audit map[string]any) (MutationResult, error) {
	connection, err := getConnectionTx(ctx, transaction, connectionID)
	if err != nil {
		return MutationResult{}, err
	}
	if err := service.recordMutation(ctx, transaction, actor, connection, operation, eventType, audit); err != nil {
		return MutationResult{}, err
	}
	public := publicConnection(connection)
	if err := recordCommandResult(ctx, transaction, actor, operation, idempotencyKey, requestHash, public); err != nil {
		return MutationResult{}, err
	}
	if err := transaction.Commit(); err != nil {
		return MutationResult{}, err
	}
	return MutationResult{Connection: public}, nil
}

func getConnectionForUpdate(ctx context.Context, transaction *sql.Tx, id string) (ProviderConnection, error) {
	return scanConnection(transaction.QueryRowContext(ctx, connectionSelect+` WHERE id=$1 FOR UPDATE`, id))
}

func requireOne(result sql.Result) error {
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrRevisionConflict
	}
	return nil
}

func hashCommand(command any, reason string) ([]byte, error) {
	payload, err := json.Marshal(struct {
		Command any
		Reason  string
	}{command, reason})
	if err != nil {
		return nil, err
	}
	return hashBytes(payload), nil
}

func hashBytes(payload []byte) []byte {
	digest := sha256.Sum256(payload)
	return digest[:]
}
