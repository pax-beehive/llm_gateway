package providerconnection

import (
	"context"
	"crypto/hmac"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/toddzheng/llm-gateway/internal/secretcustody"
	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

const (
	probeRequestOperation     = "provider_connection.probe.request"
	discoveryRequestOperation = "provider_connection.model_discovery.request"
	rotationRequestOperation  = "provider_connection.credential_rotation.request"
)

var safeErrorCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
var rawResponseHashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func (service *Service) RequestProbe(ctx context.Context, actor tenantadmin.ActorEnvelope, idempotencyKey string, command OperationCommand) (OperationResult, error) {
	authorization, err := service.livePolicy.Authorize(ctx, actor, OperationProbe)
	if err != nil {
		return OperationResult{}, err
	}
	return service.enqueueOperation(ctx, actor, idempotencyKey, command, OperationProbe, probeRequestOperation, secretcustody.Reference{}, authorization)
}

func (service *Service) RequestDiscovery(ctx context.Context, actor tenantadmin.ActorEnvelope, idempotencyKey string, command OperationCommand) (OperationResult, error) {
	authorization, err := service.livePolicy.Authorize(ctx, actor, OperationModelDiscovery)
	if err != nil {
		return OperationResult{}, err
	}
	return service.enqueueOperation(ctx, actor, idempotencyKey, command, OperationModelDiscovery, discoveryRequestOperation, secretcustody.Reference{}, authorization)
}

func (service *Service) RequestRotation(ctx context.Context, actor tenantadmin.ActorEnvelope, idempotencyKey string, command RotationCommand) (OperationResult, error) {
	if err := authorizeMutation(actor); err != nil {
		return OperationResult{}, err
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil || !resourceIDPattern.MatchString(command.ConnectionID) ||
		command.ExpectedRevision <= 0 || len(command.Secret) == 0 || len(command.Secret) > 64<<10 {
		return OperationResult{}, ErrInvalidArgument
	}
	hash, err := hashCommand(struct {
		ConnectionID     string
		ExpectedRevision int64
	}{command.ConnectionID, command.ExpectedRevision}, actor.Reason)
	if err != nil {
		return OperationResult{}, err
	}
	secretKey := stableSecretKey(command.ConnectionID, actor, rotationRequestOperation, idempotencyKey)
	reference, err := service.custody.Put(ctx, secretKey, command.Secret)
	if errors.Is(err, secretcustody.ErrConflict) {
		return OperationResult{}, ErrIdempotencyConflict
	}
	if err != nil {
		return OperationResult{}, fmt.Errorf("store rotated Provider credential: %w", err)
	}
	if err := validateSecretReference(reference); err != nil {
		return OperationResult{}, err
	}
	return service.enqueueOperationWithHash(ctx, actor, idempotencyKey, OperationCommand{
		ConnectionID: command.ConnectionID, ExpectedRevision: command.ExpectedRevision,
	}, OperationCredentialRotation, rotationRequestOperation, reference, OperationAuthorization{
		Source: "secret-custody-idempotent-write", MaxProviderRequests: 0, MaxSpendMicros: 0,
	}, hash)
}

func (service *Service) enqueueOperation(ctx context.Context, actor tenantadmin.ActorEnvelope, idempotencyKey string, command OperationCommand, operationType OperationType, operationName string, reference secretcustody.Reference, authorization OperationAuthorization) (OperationResult, error) {
	if err := authorizeMutation(actor); err != nil {
		return OperationResult{}, err
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil || !resourceIDPattern.MatchString(command.ConnectionID) || command.ExpectedRevision <= 0 {
		return OperationResult{}, ErrInvalidArgument
	}
	hash, err := hashCommand(struct {
		OperationCommand
		Type OperationType
	}{command, operationType}, actor.Reason)
	if err != nil {
		return OperationResult{}, err
	}
	return service.enqueueOperationWithHash(ctx, actor, idempotencyKey, command, operationType, operationName, reference, authorization, hash)
}

func (service *Service) enqueueOperationWithHash(ctx context.Context, actor tenantadmin.ActorEnvelope, idempotencyKey string, command OperationCommand, operationType OperationType, operationName string, reference secretcustody.Reference, authorization OperationAuthorization, requestHash []byte) (OperationResult, error) {
	if strings.TrimSpace(authorization.Source) == "" || authorization.MaxProviderRequests < 0 || authorization.MaxProviderRequests > 100 || authorization.MaxSpendMicros != 0 {
		return OperationResult{}, ErrPolicyDenied
	}
	transaction, replay, err := service.beginOperationCommand(ctx, actor, operationName, idempotencyKey, requestHash)
	if err != nil || replay != nil {
		if replay != nil {
			current, getErr := service.GetOperation(ctx, actor, replay.ID)
			return OperationResult{Operation: current, Replay: true}, getErr
		}
		return OperationResult{}, err
	}
	defer func() { _ = transaction.Rollback() }()
	connection, err := getConnectionForUpdate(ctx, transaction, command.ConnectionID)
	if err != nil {
		return OperationResult{}, err
	}
	if connection.Revision != command.ExpectedRevision {
		return OperationResult{}, ErrRevisionConflict
	}
	operationID, err := randomID(service.random, "pop")
	if err != nil {
		return OperationResult{}, err
	}
	now := service.now().UTC()
	if _, err := transaction.ExecContext(ctx, `
		INSERT INTO provider_operations (
			id, operation_type, connection_id, expected_revision, status, authorization_source,
			max_provider_requests, max_spend_micros, retry_safe,
			pending_secret_ref, pending_secret_version, actor_type, actor_id, acting_tenant_id,
			scopes, request_id, reason, created_at
		) VALUES ($1,$2,$3,$4,'queued',$5,$6,$7,true,NULLIF($8,''),NULLIF($9,''),$10,$11,NULLIF($12,''),$13,$14,$15,$16)`,
		operationID, operationType, command.ConnectionID, command.ExpectedRevision, authorization.Source,
		authorization.MaxProviderRequests, authorization.MaxSpendMicros, reference.Name, reference.Version,
		actor.Type, actor.ID, actor.ActingTenantID, actor.Scopes, actor.RequestID, actor.Reason, now); err != nil {
		return OperationResult{}, err
	}
	operation, err := getOperationTx(ctx, transaction, operationID)
	if err != nil {
		return OperationResult{}, err
	}
	if err := service.recordOperationRequestAudit(ctx, transaction, operation, operationName); err != nil {
		return OperationResult{}, err
	}
	public := publicOperation(operation)
	if err := recordCommandResult(ctx, transaction, actor, operationName, idempotencyKey, requestHash, public); err != nil {
		return OperationResult{}, err
	}
	if err := transaction.Commit(); err != nil {
		return OperationResult{}, err
	}
	return OperationResult{Operation: public}, nil
}

func (service *Service) beginOperationCommand(ctx context.Context, actor tenantadmin.ActorEnvelope, operation, idempotencyKey string, requestHash []byte) (*sql.Tx, *Operation, error) {
	transaction, err := service.database.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	identity := strings.Join([]string{actor.Type, actor.ID, operation, idempotencyKey}, "\x1f")
	if _, err := transaction.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, identity); err != nil {
		_ = transaction.Rollback()
		return nil, nil, err
	}
	var storedHash, result []byte
	err = transaction.QueryRowContext(ctx, `SELECT request_hash, result FROM control_command_idempotency
		WHERE actor_type=$1 AND actor_id=$2 AND operation=$3 AND idempotency_key=$4`,
		actor.Type, actor.ID, operation, idempotencyKey).Scan(&storedHash, &result)
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
	var replay Operation
	if err := json.Unmarshal(result, &replay); err != nil {
		_ = transaction.Rollback()
		return nil, nil, err
	}
	if err := transaction.Commit(); err != nil {
		return nil, nil, err
	}
	return nil, &replay, nil
}

func (service *Service) GetOperation(ctx context.Context, actor tenantadmin.ActorEnvelope, operationID string) (Operation, error) {
	if err := authorizeRead(actor); err != nil {
		return Operation{}, err
	}
	if !resourceIDPattern.MatchString(operationID) {
		return Operation{}, ErrInvalidArgument
	}
	operation, err := scanOperation(service.database.QueryRowContext(ctx, operationSelect+` WHERE id=$1`, operationID))
	return publicOperation(operation), err
}

func (service *Service) RunNext(ctx context.Context) (bool, error) {
	transaction, err := service.database.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = transaction.Rollback() }()
	claimTime := service.now().UTC()
	operation, err := scanOperation(transaction.QueryRowContext(ctx, operationSelect+`
		WHERE status='queued' OR (status='running' AND lease_expires_at <= $1)
		ORDER BY created_at,id FOR UPDATE SKIP LOCKED LIMIT 1`, claimTime))
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if operation.Status == OperationRunning && !operation.RetrySafe {
		now := claimTime
		payload, _ := json.Marshal(map[string]any{})
		if _, err := transaction.ExecContext(ctx, `UPDATE provider_operations SET status='uncertain',result=$2,
			error_code='ambiguous_completion',error_message='Provider operation completion is uncertain',
			completed_at=$3,lease_expires_at=NULL WHERE id=$1 AND status='running'`, operation.ID, payload, now); err != nil {
			return false, err
		}
		return true, transaction.Commit()
	}
	now := claimTime
	if _, err := transaction.ExecContext(ctx, `UPDATE provider_operations SET status='running',
		started_at=COALESCE(started_at,$2), attempts=attempts+1, lease_expires_at=$3 WHERE id=$1`,
		operation.ID, now, now.Add(30*time.Second)); err != nil {
		return false, err
	}
	if err := transaction.Commit(); err != nil {
		return false, err
	}
	operation.Status = OperationRunning
	operation.StartedAt = &now
	operationCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	switch operation.Type {
	case OperationProbe:
		err = service.executeProbe(operationCtx, operation)
	case OperationModelDiscovery:
		err = service.executeDiscovery(operationCtx, operation)
	case OperationCredentialRotation:
		err = service.executeRotation(operationCtx, operation)
	default:
		err = service.failOperation(ctx, operation, "invalid_operation", false)
	}
	return true, err
}

func (service *Service) executeProbe(ctx context.Context, operation Operation) error {
	connection, secret, err := service.operationCredential(ctx, operation)
	if err != nil {
		return service.finishFailure(ctx, operation, classifyOperationError(err), true)
	}
	defer clear(secret)
	if service.operator == nil {
		return service.finishFailure(ctx, operation, "adapter_unavailable", true)
	}
	result, err := service.operator.Probe(ctx, connection, secret, operation.MaxProviderRequests)
	if err != nil {
		return service.finishFailure(ctx, operation, classifyOperationError(err), true)
	}
	if result.ObservedModelCount < 0 || result.ProviderRequests < 1 || result.ProviderRequests > operation.MaxProviderRequests || !rawResponseHashPattern.MatchString(result.RawResponseHash) {
		return service.finishFailure(ctx, operation, "invalid_provider_response", true)
	}
	latency := service.operationLatency(operation)
	finishCtx, cancel := completionContext(ctx)
	defer cancel()
	return service.completeProbe(finishCtx, operation, result, latency)
}

func (service *Service) executeDiscovery(ctx context.Context, operation Operation) error {
	connection, secret, err := service.operationCredential(ctx, operation)
	if err != nil {
		return service.finishFailure(ctx, operation, classifyOperationError(err), false)
	}
	defer clear(secret)
	if service.operator == nil {
		return service.finishFailure(ctx, operation, "adapter_unavailable", false)
	}
	result, err := service.operator.Discover(ctx, connection, secret, operation.MaxProviderRequests)
	if err != nil {
		return service.finishFailure(ctx, operation, classifyOperationError(err), false)
	}
	if len(result.Models) > 10_000 || result.ProviderRequests < 1 || result.ProviderRequests > operation.MaxProviderRequests || !rawResponseHashPattern.MatchString(result.RawResponseHash) {
		return service.finishFailure(ctx, operation, "invalid_provider_response", false)
	}
	seen := make(map[string]struct{}, len(result.Models))
	for _, model := range result.Models {
		if strings.TrimSpace(model.ID) == "" || len(model.ID) > 512 || len(model.OwnedBy) > 512 {
			return service.finishFailure(ctx, operation, "invalid_provider_response", false)
		}
		for name, support := range model.Capabilities {
			if strings.TrimSpace(name) == "" || support != "native" && support != "translated" && support != "unsupported" {
				return service.finishFailure(ctx, operation, "invalid_provider_response", false)
			}
		}
		if _, duplicate := seen[model.ID]; duplicate {
			return service.finishFailure(ctx, operation, "invalid_provider_response", false)
		}
		seen[model.ID] = struct{}{}
	}
	finishCtx, cancel := completionContext(ctx)
	defer cancel()
	return service.completeDiscovery(finishCtx, operation, connection, result)
}

func (service *Service) operationCredential(ctx context.Context, operation Operation) (ProviderConnection, []byte, error) {
	connection, err := scanConnection(service.database.QueryRowContext(ctx, connectionSelect+` WHERE id=$1`, operation.ConnectionID))
	if err != nil {
		return ProviderConnection{}, nil, err
	}
	if connection.Revision != operation.ExpectedRevision {
		return ProviderConnection{}, nil, ErrRevisionConflict
	}
	if err := validateBaseURL(connection.Provider, connection.BaseURL); err != nil {
		return ProviderConnection{}, nil, ErrPolicyDenied
	}
	secret, err := service.custody.Access(ctx, secretcustody.Reference{Name: connection.SecretRef, Version: connection.SecretExternalVersion})
	return publicConnection(connection), secret, err
}

func (service *Service) completeProbe(ctx context.Context, operation Operation, result ProbeResult, latency int64) error {
	transaction, err := service.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	now := service.now().UTC()
	if _, err := transaction.ExecContext(ctx, `
		INSERT INTO provider_connection_health (connection_id,observed_status,error_code,operation_id,latency_milliseconds,observed_at)
		VALUES ($1,'healthy',NULL,$2,$3,$4)
		ON CONFLICT (connection_id) DO UPDATE SET observed_status='healthy',error_code=NULL,
			operation_id=EXCLUDED.operation_id,latency_milliseconds=EXCLUDED.latency_milliseconds,observed_at=EXCLUDED.observed_at`,
		operation.ConnectionID, operation.ID, latency, now); err != nil {
		return err
	}
	if err := service.recordHealthObservation(ctx, transaction, operation, "healthy", "", now); err != nil {
		return err
	}
	return completeOperationTx(ctx, transaction, operation.ID, OperationSucceeded, map[string]any{
		"observed_status": "healthy", "observed_model_count": result.ObservedModelCount,
		"raw_response_hash": result.RawResponseHash, "latency_milliseconds": latency,
		"provider_requests": result.ProviderRequests,
	}, "", "", now)
}

func (service *Service) completeDiscovery(ctx context.Context, operation Operation, connection ProviderConnection, result DiscoveryResult) error {
	transaction, err := service.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	now := service.now().UTC()
	for _, model := range result.Models {
		capabilities, err := json.Marshal(model.Capabilities)
		if err != nil {
			return err
		}
		if _, err := transaction.ExecContext(ctx, `
			INSERT INTO provider_model_observations (
				operation_id,connection_id,provider_model_id,owned_by,capabilities,raw_response_hash,observed_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7)`, operation.ID, operation.ConnectionID,
			model.ID, model.OwnedBy, capabilities, result.RawResponseHash, now); err != nil {
			return err
		}
	}
	if err := service.recordDiscoveryEvidence(ctx, transaction, operation, connection, len(result.Models), result.RawResponseHash, now); err != nil {
		return err
	}
	return completeOperationTx(ctx, transaction, operation.ID, OperationSucceeded, map[string]any{
		"model_count": len(result.Models), "raw_response_hash": result.RawResponseHash,
		"provider_requests": result.ProviderRequests,
	}, "", "", now)
}

func (service *Service) executeRotation(ctx context.Context, operation Operation) error {
	if operation.PendingSecretRef == "" {
		return service.finishFailure(ctx, operation, "secret_reference_missing", false)
	}
	finishCtx, cancel := completionContext(ctx)
	defer cancel()
	ctx = finishCtx
	transaction, err := service.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	connection, err := getConnectionForUpdate(ctx, transaction, operation.ConnectionID)
	if err != nil {
		return err
	}
	if connection.Revision != operation.ExpectedRevision {
		if connection.Revision == operation.ExpectedRevision+1 && connection.SecretRef == operation.PendingSecretRef &&
			connection.SecretExternalVersion == operation.PendingSecretVersion {
			now := service.now().UTC()
			return completeOperationTx(ctx, transaction, operation.ID, OperationSucceeded, map[string]any{
				"connection_revision": connection.Revision, "credential_version": connection.CredentialVersion,
			}, "", "", now)
		}
		_ = transaction.Rollback()
		return service.failOperation(ctx, operation, "revision_conflict", false)
	}
	now := service.now().UTC()
	nextVersion := connection.CredentialVersion + 1
	if _, err := transaction.ExecContext(ctx, `UPDATE provider_connection_credential_versions
		SET status='retired',retired_at=$3 WHERE connection_id=$1 AND version=$2 AND status='active'`,
		connection.ID, connection.CredentialVersion, now); err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx, `INSERT INTO provider_connection_credential_versions (
		connection_id,version,secret_ref,secret_external_version,status,activated_at
	) VALUES ($1,$2,$3,$4,'active',$5)`, connection.ID, nextVersion, operation.PendingSecretRef, operation.PendingSecretVersion, now); err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx, `UPDATE provider_connections SET secret_ref=$2,secret_external_version=$3,
		credential_version=$4,revision=revision+1,updated_at=$5 WHERE id=$1 AND revision=$6`,
		connection.ID, operation.PendingSecretRef, operation.PendingSecretVersion, nextVersion, now, operation.ExpectedRevision); err != nil {
		return err
	}
	connection, err = getConnectionTx(ctx, transaction, connection.ID)
	if err != nil {
		return err
	}
	actor := operationActor(operation)
	if err := service.recordMutation(ctx, transaction, actor, connection, "provider_connection.credential_rotation", "ProviderCredentialRotated", map[string]any{
		"previous_credential_version": nextVersion - 1, "credential_version": nextVersion,
	}); err != nil {
		return err
	}
	if err := completeOperationTx(ctx, transaction, operation.ID, OperationSucceeded, map[string]any{
		"connection_revision": connection.Revision, "credential_version": nextVersion,
	}, "", "", now); err != nil {
		return err
	}
	return nil
}

func (service *Service) failOperation(ctx context.Context, operation Operation, code string, updateHealth bool) error {
	if !safeErrorCodePattern.MatchString(code) {
		code = "provider_operation_failed"
	}
	transaction, err := service.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	now := service.now().UTC()
	if updateHealth {
		if _, err := transaction.ExecContext(ctx, `
			INSERT INTO provider_connection_health (connection_id,observed_status,error_code,operation_id,latency_milliseconds,observed_at)
			VALUES ($1,'unhealthy',$2,$3,$4,$5)
			ON CONFLICT (connection_id) DO UPDATE SET observed_status='unhealthy',error_code=EXCLUDED.error_code,
				operation_id=EXCLUDED.operation_id,latency_milliseconds=EXCLUDED.latency_milliseconds,observed_at=EXCLUDED.observed_at`,
			operation.ConnectionID, code, operation.ID, service.operationLatency(operation), now); err != nil {
			return err
		}
		if err := service.recordHealthObservation(ctx, transaction, operation, "unhealthy", code, now); err != nil {
			return err
		}
	}
	return completeOperationTx(ctx, transaction, operation.ID, OperationFailed, nil, code, "Provider operation failed", now)
}

func (service *Service) recordHealthObservation(ctx context.Context, transaction *sql.Tx, operation Operation, status, errorCode string, observedAt time.Time) error {
	eventID, err := randomID(service.random, "cevt")
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{
		"operation_id": operation.ID, "connection_id": operation.ConnectionID,
		"connection_revision": operation.ExpectedRevision, "observed_status": status,
		"error_code": errorCode, "observed_at": observedAt,
	})
	_, err = transaction.ExecContext(ctx, `INSERT INTO control_outbox (
		event_id,schema_version,aggregate_type,aggregate_id,aggregate_revision,tenant_id,event_type,occurred_at,payload
	) VALUES ($1,2,'ProviderOperation',$2,1,NULL,'ProviderConnectionHealthObserved',$3,$4)`,
		eventID, operation.ID, observedAt, payload)
	return err
}

func (service *Service) finishFailure(ctx context.Context, operation Operation, code string, updateHealth bool) error {
	finishCtx, cancel := completionContext(ctx)
	defer cancel()
	return service.failOperation(finishCtx, operation, code, updateHealth)
}

func completionContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}

func completeOperationTx(ctx context.Context, transaction *sql.Tx, operationID string, status OperationStatus, result map[string]any, errorCode, errorMessage string, completedAt time.Time) error {
	if result == nil {
		result = map[string]any{}
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx, `UPDATE provider_operations SET status=$2,result=$3,
		error_code=NULLIF($4,''),error_message=NULLIF($5,''),completed_at=$6,lease_expires_at=NULL WHERE id=$1 AND status='running'`,
		operationID, status, payload, errorCode, errorMessage, completedAt); err != nil {
		return err
	}
	return transaction.Commit()
}

func (service *Service) recordOperationRequestAudit(ctx context.Context, transaction *sql.Tx, operation Operation, action string) error {
	auditID, err := randomID(service.random, "caud")
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{
		"operation_id": operation.ID, "connection_id": operation.ConnectionID, "operation_type": operation.Type,
		"authorization_source": operation.AuthorizationSource, "max_provider_requests": operation.MaxProviderRequests,
		"max_spend_micros": operation.MaxSpendMicros, "retry_safe": operation.RetrySafe,
	})
	_, err = transaction.ExecContext(ctx, `INSERT INTO control_audit_events (
		event_id,tenant_id,actor_type,actor_id,acting_tenant_id,scopes,request_id,reason,action,
		aggregate_type,aggregate_id,aggregate_revision,payload,occurred_at
	) VALUES ($1,NULL,$2,$3,NULLIF($4,''),$5,$6,$7,$8,'ProviderOperation',$9,1,$10,$11)`,
		auditID, operation.ActorType, operation.ActorID, operation.ActingTenantID, operation.Scopes,
		operation.RequestID, operation.Reason, action, operation.ID, payload, operation.CreatedAt)
	return err
}

func (service *Service) recordDiscoveryEvidence(ctx context.Context, transaction *sql.Tx, operation Operation, connection ProviderConnection, modelCount int, rawHash string, now time.Time) error {
	auditID, err := randomID(service.random, "caud")
	if err != nil {
		return err
	}
	eventID, err := randomID(service.random, "cevt")
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{
		"operation_id": operation.ID, "connection_id": connection.ID, "provider": connection.Provider,
		"model_count": modelCount, "raw_response_hash": rawHash, "observed_at": now,
	})
	actor := operationActor(operation)
	if _, err := transaction.ExecContext(ctx, `INSERT INTO control_audit_events (
		event_id,tenant_id,actor_type,actor_id,acting_tenant_id,scopes,request_id,reason,action,
		aggregate_type,aggregate_id,aggregate_revision,payload,occurred_at
	) VALUES ($1,NULL,$2,$3,NULLIF($4,''),$5,$6,$7,'provider_connection.models_observed',
		'ProviderOperation',$8,1,$9,$10)`, auditID, actor.Type, actor.ID, actor.ActingTenantID, actor.Scopes,
		actor.RequestID, actor.Reason, operation.ID, payload, now); err != nil {
		return err
	}
	_, err = transaction.ExecContext(ctx, `INSERT INTO control_outbox (
		event_id,schema_version,aggregate_type,aggregate_id,aggregate_revision,tenant_id,event_type,occurred_at,payload
	) VALUES ($1,2,'ProviderOperation',$2,1,NULL,'ProviderModelsObserved',$3,$4)`, eventID, operation.ID, now, payload)
	return err
}

func (service *Service) operationLatency(operation Operation) int64 {
	started := operation.CreatedAt
	if operation.StartedAt != nil {
		started = *operation.StartedAt
	}
	latency := service.now().UTC().Sub(started).Milliseconds()
	if latency < 0 {
		return 0
	}
	return latency
}

func classifyOperationError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, ErrRevisionConflict) {
		return "revision_conflict"
	}
	var operationError *OperationError
	if errors.As(err, &operationError) && safeErrorCodePattern.MatchString(operationError.Code) {
		return operationError.Code
	}
	return "provider_operation_failed"
}

func operationActor(operation Operation) tenantadmin.ActorEnvelope {
	return tenantadmin.ActorEnvelope{
		Type: operation.ActorType, ID: operation.ActorID, ActingTenantID: operation.ActingTenantID,
		Scopes: append([]string(nil), operation.Scopes...), RequestID: operation.RequestID, Reason: operation.Reason,
	}
}

const operationSelect = `SELECT id,operation_type,connection_id,expected_revision,status,authorization_source,
	max_provider_requests,max_spend_micros,retry_safe,
	COALESCE(pending_secret_ref,''),COALESCE(pending_secret_version,''),actor_type,actor_id,
	COALESCE(acting_tenant_id,''),to_json(scopes),request_id,reason,result,COALESCE(error_code,''),
	COALESCE(error_message,''),created_at,started_at,completed_at FROM provider_operations`

func getOperationTx(ctx context.Context, transaction *sql.Tx, operationID string) (Operation, error) {
	return scanOperation(transaction.QueryRowContext(ctx, operationSelect+` WHERE id=$1`, operationID))
}

func scanOperation(source scanner) (Operation, error) {
	var operation Operation
	var result, scopes []byte
	err := source.Scan(&operation.ID, &operation.Type, &operation.ConnectionID, &operation.ExpectedRevision,
		&operation.Status, &operation.AuthorizationSource, &operation.MaxProviderRequests, &operation.MaxSpendMicros,
		&operation.RetrySafe, &operation.PendingSecretRef, &operation.PendingSecretVersion,
		&operation.ActorType, &operation.ActorID, &operation.ActingTenantID, &scopes,
		&operation.RequestID, &operation.Reason, &result, &operation.ErrorCode, &operation.ErrorMessage,
		&operation.CreatedAt, &operation.StartedAt, &operation.CompletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Operation{}, ErrNotFound
	}
	if err != nil {
		return Operation{}, err
	}
	if err := json.Unmarshal(result, &operation.Result); err != nil {
		return Operation{}, err
	}
	if err := json.Unmarshal(scopes, &operation.Scopes); err != nil {
		return Operation{}, err
	}
	return operation, nil
}

func publicOperation(operation Operation) Operation {
	operation.PendingSecretRef = ""
	operation.PendingSecretVersion = ""
	operation.ActorType = ""
	operation.ActorID = ""
	operation.ActingTenantID = ""
	operation.Scopes = nil
	operation.RequestID = ""
	operation.Reason = ""
	operation.AuthorizationSource = ""
	operation.MaxProviderRequests = 0
	operation.MaxSpendMicros = 0
	operation.RetrySafe = false
	return operation
}
