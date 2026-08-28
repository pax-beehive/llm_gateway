package providerconnection

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/toddzheng/llm-gateway/internal/provider"
	"github.com/toddzheng/llm-gateway/internal/secretcustody"
	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

const registerOperation = "provider_connection.register"

var resourceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~-]{0,127}$`)

type ProviderOperator interface {
	Probe(context.Context, ProviderConnection, []byte) (ProbeResult, error)
	Discover(context.Context, ProviderConnection, []byte) (DiscoveryResult, error)
}

type Service struct {
	database *sql.DB
	custody  secretcustody.Store
	operator ProviderOperator
	now      func() time.Time
	random   io.Reader
}

func NewService(database *sql.DB, custody secretcustody.Store, operator ProviderOperator, now func() time.Time, random io.Reader) (*Service, error) {
	if database == nil || custody == nil {
		return nil, errors.New("Provider Connection Registry requires PostgreSQL and Secret Custody")
	}
	if now == nil {
		now = time.Now
	}
	if random == nil {
		random = rand.Reader
	}
	return &Service{database: database, custody: custody, operator: operator, now: now, random: random}, nil
}

func (service *Service) Register(ctx context.Context, actor tenantadmin.ActorEnvelope, idempotencyKey string, command RegisterCommand) (MutationResult, error) {
	if err := authorizeMutation(actor); err != nil {
		return MutationResult{}, err
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return MutationResult{}, err
	}
	if err := validateRegister(command); err != nil {
		return MutationResult{}, err
	}
	requestHash, err := registerHash(command, actor.Reason)
	if err != nil {
		return MutationResult{}, err
	}
	secretKey := stableSecretKey(command.ID, actor, registerOperation, idempotencyKey)
	reference, err := service.custody.Put(ctx, secretKey, command.Secret)
	if errors.Is(err, secretcustody.ErrConflict) {
		return MutationResult{}, ErrIdempotencyConflict
	}
	if err != nil {
		return MutationResult{}, fmt.Errorf("store Provider credential: %w", err)
	}
	transaction, replay, err := service.beginCommand(ctx, actor, registerOperation, idempotencyKey, requestHash)
	if err != nil || replay != nil {
		if replay != nil {
			return MutationResult{Connection: *replay, Replay: true}, nil
		}
		return MutationResult{}, err
	}
	defer func() { _ = transaction.Rollback() }()
	capabilityPayload, err := json.Marshal(command.CapabilityDeclaration)
	if err != nil {
		return MutationResult{}, err
	}
	now := service.now().UTC()
	if _, err := transaction.ExecContext(ctx, `
		INSERT INTO provider_connections (
			id, provider, display_name, base_url, region, credential_scope,
			secret_ref, secret_external_version, capability_declaration,
			administrative_status, credential_version, revision, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'disabled',1,1,$10,$10)`,
		command.ID, command.Provider, strings.TrimSpace(command.DisplayName), command.BaseURL,
		command.Region, command.CredentialScope, reference.Name, reference.Version, capabilityPayload, now); err != nil {
		return MutationResult{}, mapDatabaseError(err)
	}
	if _, err := transaction.ExecContext(ctx, `
		INSERT INTO provider_connection_credential_versions (
			connection_id, version, secret_ref, secret_external_version, status, activated_at
		) VALUES ($1,1,$2,$3,'active',$4)`, command.ID, reference.Name, reference.Version, now); err != nil {
		return MutationResult{}, err
	}
	connection, err := getConnectionTx(ctx, transaction, command.ID)
	if err != nil {
		return MutationResult{}, err
	}
	if err := service.recordMutation(ctx, transaction, actor, connection, registerOperation, "ProviderConnectionRegistered", nil); err != nil {
		return MutationResult{}, err
	}
	if err := recordCommandResult(ctx, transaction, actor, registerOperation, idempotencyKey, requestHash, publicConnection(connection)); err != nil {
		return MutationResult{}, err
	}
	if err := transaction.Commit(); err != nil {
		return MutationResult{}, err
	}
	return MutationResult{Connection: publicConnection(connection)}, nil
}

func (service *Service) beginCommand(ctx context.Context, actor tenantadmin.ActorEnvelope, operation, idempotencyKey string, requestHash []byte) (*sql.Tx, *ProviderConnection, error) {
	transaction, err := service.database.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	identity := strings.Join([]string{actor.Type, actor.ID, operation, idempotencyKey}, "\x1f")
	if _, err := transaction.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, identity); err != nil {
		_ = transaction.Rollback()
		return nil, nil, err
	}
	var storedHash, result []byte
	err = transaction.QueryRowContext(ctx, `
		SELECT request_hash, result FROM control_command_idempotency
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
	var connection ProviderConnection
	if err := json.Unmarshal(result, &connection); err != nil {
		_ = transaction.Rollback()
		return nil, nil, err
	}
	if err := transaction.Commit(); err != nil {
		return nil, nil, err
	}
	return nil, &connection, nil
}

func (service *Service) recordMutation(ctx context.Context, transaction *sql.Tx, actor tenantadmin.ActorEnvelope, connection ProviderConnection, action, eventType string, extra map[string]any) error {
	auditID, err := randomID(service.random, "caud")
	if err != nil {
		return err
	}
	eventID, err := randomID(service.random, "cevt")
	if err != nil {
		return err
	}
	payload := map[string]any{
		"connection_id": connection.ID, "provider": connection.Provider, "region": connection.Region,
		"administrative_status": connection.AdministrativeStatus, "credential_scope": connection.CredentialScope,
		"credential_version": connection.CredentialVersion, "revision": connection.Revision,
	}
	for key, value := range extra {
		payload[key] = value
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	now := service.now().UTC()
	if _, err := transaction.ExecContext(ctx, `
		INSERT INTO control_audit_events (
			event_id, tenant_id, actor_type, actor_id, acting_tenant_id, scopes,
			request_id, reason, action, aggregate_type, aggregate_id, aggregate_revision, payload, occurred_at
		) VALUES ($1,NULL,$2,$3,NULLIF($4,''),$5,$6,$7,$8,'ProviderConnection',$9,$10,$11,$12)`,
		auditID, actor.Type, actor.ID, actor.ActingTenantID, actor.Scopes, actor.RequestID, actor.Reason,
		action, connection.ID, connection.Revision, encoded, now); err != nil {
		return fmt.Errorf("append Provider Connection audit: %w", err)
	}
	if _, err := transaction.ExecContext(ctx, `
		INSERT INTO control_outbox (
			event_id, schema_version, aggregate_type, aggregate_id, aggregate_revision,
			tenant_id, event_type, occurred_at, payload
		) VALUES ($1,2,'ProviderConnection',$2,$3,NULL,$4,$5,$6)`,
		eventID, connection.ID, connection.Revision, eventType, now, encoded); err != nil {
		return fmt.Errorf("append Provider Connection outbox: %w", err)
	}
	return nil
}

func recordCommandResult(ctx context.Context, transaction *sql.Tx, actor tenantadmin.ActorEnvelope, operation, idempotencyKey string, requestHash []byte, result any) error {
	payload, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = transaction.ExecContext(ctx, `
		INSERT INTO control_command_idempotency (
			actor_type, actor_id, operation, idempotency_key, request_hash, result
		) VALUES ($1,$2,$3,$4,$5,$6)`, actor.Type, actor.ID, operation, idempotencyKey, requestHash, payload)
	return err
}

type scanner interface{ Scan(...any) error }

func getConnectionTx(ctx context.Context, transaction *sql.Tx, id string) (ProviderConnection, error) {
	return scanConnection(transaction.QueryRowContext(ctx, connectionSelect+` WHERE id=$1`, id))
}

const connectionSelect = `SELECT id, provider, display_name, base_url, region, credential_scope,
	secret_ref, secret_external_version, administrative_status, capability_declaration,
	credential_version, revision, created_at, updated_at FROM provider_connections`

func scanConnection(source scanner) (ProviderConnection, error) {
	var connection ProviderConnection
	var capability []byte
	err := source.Scan(&connection.ID, &connection.Provider, &connection.DisplayName, &connection.BaseURL,
		&connection.Region, &connection.CredentialScope, &connection.SecretRef, &connection.SecretExternalVersion,
		&connection.AdministrativeStatus, &capability, &connection.CredentialVersion, &connection.Revision,
		&connection.CreatedAt, &connection.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ProviderConnection{}, ErrNotFound
	}
	if err != nil {
		return ProviderConnection{}, err
	}
	if err := json.Unmarshal(capability, &connection.CapabilityDeclaration); err != nil {
		return ProviderConnection{}, err
	}
	return connection, nil
}

func publicConnection(connection ProviderConnection) ProviderConnection {
	connection.SecretRef = ""
	connection.SecretExternalVersion = ""
	return connection
}

func authorizeMutation(actor tenantadmin.ActorEnvelope) error {
	if actor.Type == "" || actor.ID == "" || actor.RequestID == "" || strings.TrimSpace(actor.Reason) == "" {
		return fmt.Errorf("%w: mutation Actor Envelope is incomplete", ErrInvalidArgument)
	}
	for _, scope := range actor.Scopes {
		if scope == tenantadmin.ScopePlatformWrite {
			return nil
		}
	}
	return ErrPolicyDenied
}

func validateIdempotencyKey(value string) error {
	if value == "" || len(value) > 255 {
		return fmt.Errorf("%w: Idempotency-Key must contain 1 to 255 characters", ErrInvalidArgument)
	}
	return nil
}

func validateRegister(command RegisterCommand) error {
	if !resourceIDPattern.MatchString(command.ID) || strings.TrimSpace(command.DisplayName) == "" || len(command.DisplayName) > 256 ||
		strings.TrimSpace(command.Region) == "" || len(command.Region) > 128 ||
		strings.TrimSpace(command.CredentialScope) == "" || len(command.CredentialScope) > 256 ||
		len(command.Secret) == 0 || len(command.Secret) > 64<<10 {
		return fmt.Errorf("%w: ID, display name, region, credential scope, and bounded secret are required", ErrInvalidArgument)
	}
	switch command.Provider {
	case "openai", "deepseek", "anthropic", "gemini":
	default:
		return fmt.Errorf("%w: unsupported Provider identity", ErrInvalidArgument)
	}
	if err := validateBaseURL(command.BaseURL); err != nil {
		return err
	}
	return validateCapabilityProfile(command.CapabilityDeclaration, 1)
}

func validateBaseURL(value string) error {
	baseURL, err := url.Parse(value)
	if err != nil || len(value) > 2048 || baseURL.Scheme != "https" || baseURL.Host == "" || baseURL.User != nil ||
		baseURL.Fragment != "" || baseURL.RawQuery != "" {
		return fmt.Errorf("%w: Base URL must be an absolute HTTPS URL without credentials, query, or fragments", ErrInvalidArgument)
	}
	return nil
}

func validateCapabilityProfile(profile provider.CapabilityProfile, expectedRevision int64) error {
	if profile.Revision != expectedRevision || len(profile.Features) == 0 {
		return fmt.Errorf("%w: Capability Profile revision must be %d", ErrInvalidArgument, expectedRevision)
	}
	if profile.Features["text"] != provider.CapabilityNative {
		return fmt.Errorf("%w: accepted Providers require declared native text capability", ErrInvalidArgument)
	}
	for name, support := range profile.Features {
		if strings.TrimSpace(name) == "" || support != provider.CapabilityNative && support != provider.CapabilityTranslated && support != provider.CapabilityUnsupported {
			return fmt.Errorf("%w: invalid capability declaration", ErrInvalidArgument)
		}
	}
	return nil
}

func validProvider(value string) bool {
	switch value {
	case "openai", "deepseek", "anthropic", "gemini":
		return true
	default:
		return false
	}
}

func validStatus(value AdministrativeStatus) bool {
	return value == StatusEnabled || value == StatusDisabled
}

func registerHash(command RegisterCommand, reason string) ([]byte, error) {
	secretDigest := sha256.Sum256(command.Secret)
	payload, err := json.Marshal(struct {
		ID, Provider, DisplayName, BaseURL, Region, CredentialScope, Reason string
		SecretDigest                                                        []byte
		Capability                                                          provider.CapabilityProfile
	}{command.ID, command.Provider, command.DisplayName, command.BaseURL, command.Region, command.CredentialScope, reason, secretDigest[:], command.CapabilityDeclaration})
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(payload)
	return digest[:], nil
}

func stableSecretKey(connectionID string, actor tenantadmin.ActorEnvelope, operation, idempotencyKey string) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{connectionID, actor.Type, actor.ID, operation, idempotencyKey}, "\x1f")))
	return "provider-connection-" + hex.EncodeToString(digest[:])
}

func randomID(random io.Reader, prefix string) (string, error) {
	value := make([]byte, 16)
	if _, err := io.ReadFull(random, value); err != nil {
		return "", err
	}
	return prefix + "_" + hex.EncodeToString(value), nil
}

func mapDatabaseError(err error) error {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) && postgresError.Code == "23505" {
		return ErrAlreadyExists
	}
	return err
}
