package providerconnection

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/toddzheng/llm-gateway/internal/secretcustody"
)

// GatewayResolver is the execution-plane port. Its PostgreSQL role only needs
// SELECT on gateway_provider_connection_resolutions and Secret Custody access
// through workload identity; it does not require management-table privileges.
type GatewayResolver struct {
	database *sql.DB
	custody  secretcustody.Store
}

func NewGatewayResolver(database *sql.DB, custody secretcustody.Store) (*GatewayResolver, error) {
	if database == nil || custody == nil {
		return nil, errors.New("Gateway Provider Connection resolution requires PostgreSQL and Secret Custody")
	}
	return &GatewayResolver{database: database, custody: custody}, nil
}

func (resolver *GatewayResolver) Resolve(ctx context.Context, connectionID string) (ResolvedConnection, error) {
	if !resourceIDPattern.MatchString(connectionID) {
		return ResolvedConnection{}, ErrInvalidArgument
	}
	var connection ProviderConnection
	var capability []byte
	var observedHealthy sql.NullBool
	err := resolver.database.QueryRowContext(ctx, `SELECT id,provider,base_url,region,credential_scope,
		capability_declaration,revision,credential_version,secret_ref,secret_external_version,observed_healthy
		FROM gateway_provider_connection_resolutions WHERE id=$1`, connectionID).Scan(
		&connection.ID, &connection.Provider, &connection.BaseURL, &connection.Region, &connection.CredentialScope,
		&capability, &connection.Revision, &connection.CredentialVersion, &connection.SecretRef, &connection.SecretExternalVersion, &observedHealthy)
	if errors.Is(err, sql.ErrNoRows) {
		return ResolvedConnection{}, ErrNotFound
	}
	if err != nil {
		return ResolvedConnection{}, err
	}
	if err := json.Unmarshal(capability, &connection.CapabilityDeclaration); err != nil {
		return ResolvedConnection{}, err
	}
	if err := validateBaseURL(connection.Provider, connection.BaseURL); err != nil {
		return ResolvedConnection{}, ErrPolicyDenied
	}
	if err := validateSecretReference(secretcustody.Reference{Name: connection.SecretRef, Version: connection.SecretExternalVersion}); err != nil {
		return ResolvedConnection{}, err
	}
	secret, err := resolver.custody.Access(ctx, secretcustody.Reference{Name: connection.SecretRef, Version: connection.SecretExternalVersion})
	if err != nil {
		return ResolvedConnection{}, err
	}
	connection.AdministrativeStatus = StatusEnabled
	resolved := ResolvedConnection{Connection: publicConnection(connection), Secret: secret}
	if observedHealthy.Valid {
		resolved.ObservedHealthy = &observedHealthy.Bool
	}
	return resolved, nil
}
