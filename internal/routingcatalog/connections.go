package routingcatalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

type PostgresConnectionLookup struct {
	database *sql.DB
}

func NewPostgresConnectionLookup(database *sql.DB) (*PostgresConnectionLookup, error) {
	if database == nil {
		return nil, errors.New("Routing Catalog Provider Connection lookup requires PostgreSQL")
	}
	return &PostgresConnectionLookup{database: database}, nil
}

func (lookup *PostgresConnectionLookup) LookupConnection(ctx context.Context, id string) (ConnectionDescriptor, error) {
	var descriptor ConnectionDescriptor
	var capability []byte
	var administrativeStatus string
	var observedStatus sql.NullString
	err := lookup.database.QueryRowContext(ctx, `SELECT c.id,c.provider,c.region,c.credential_scope,
		c.administrative_status,c.revision,c.credential_version,c.capability_declaration,h.observed_status
		FROM provider_connections c LEFT JOIN provider_connection_health h ON h.connection_id=c.id
		WHERE c.id=$1`, id).Scan(&descriptor.ID, &descriptor.Provider, &descriptor.Region, &descriptor.CredentialScope,
		&administrativeStatus, &descriptor.Revision, &descriptor.CredentialVersion, &capability, &observedStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return ConnectionDescriptor{}, ErrConnectionNotFound
	}
	if err != nil {
		return ConnectionDescriptor{}, err
	}
	if err := json.Unmarshal(capability, &descriptor.CapabilityProfile); err != nil {
		return ConnectionDescriptor{}, err
	}
	descriptor.Enabled = administrativeStatus == "enabled"
	if observedStatus.Valid {
		healthy := observedStatus.String == "healthy"
		descriptor.ObservedHealthy = &healthy
	}
	return descriptor, nil
}

func (lookup *PostgresConnectionLookup) TenantPolicyRevisionExists(ctx context.Context, tenantID string, revision int64) (bool, error) {
	var exists bool
	err := lookup.database.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM tenant_policy_revisions WHERE tenant_id=$1 AND revision=$2
	)`, tenantID, revision).Scan(&exists)
	return exists, err
}
