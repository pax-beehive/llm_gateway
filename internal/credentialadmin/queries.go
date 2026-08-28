package credentialadmin

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/toddzheng/llm-gateway/internal/access"
	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

func (service *Service) Get(ctx context.Context, actor tenantadmin.ActorEnvelope, tenantID, credentialID string) (Credential, error) {
	if tenantID == "" || credentialID == "" {
		return Credential{}, fmt.Errorf("%w: Tenant and Gateway API Key IDs are required", ErrInvalidArgument)
	}
	if err := authorizeRead(actor, tenantID); err != nil {
		return Credential{}, err
	}
	return scanCredential(service.database.QueryRowContext(ctx, `
		SELECT id, tenant_id, name, key_prefix, digest_version, status, revision,
		       policy_revision, policy, metadata, expires_at, revoked_at,
		       COALESCE(predecessor_id,''), COALESCE(replacement_id,''), grace_expires_at
		FROM api_keys WHERE tenant_id = $1 AND id = $2`, tenantID, credentialID))
}

func (service *Service) List(ctx context.Context, actor tenantadmin.ActorEnvelope, filter CredentialFilter) (CredentialPage, error) {
	if filter.TenantID == "" {
		return CredentialPage{}, fmt.Errorf("%w: Tenant ID is required", ErrInvalidArgument)
	}
	if err := authorizeRead(actor, filter.TenantID); err != nil {
		return CredentialPage{}, err
	}
	if filter.Status != "" && filter.Status != access.APIKeyActive && filter.Status != access.APIKeyRevoked {
		return CredentialPage{}, fmt.Errorf("%w: invalid Gateway API Key status", ErrInvalidArgument)
	}
	if filter.Limit == 0 {
		filter.Limit = 50
	}
	if filter.Limit < 1 || filter.Limit > 100 {
		return CredentialPage{}, fmt.Errorf("%w: limit must be between 1 and 100", ErrInvalidArgument)
	}
	cursor, err := decodeCursor(filter.Cursor)
	if err != nil {
		return CredentialPage{}, err
	}
	rows, err := service.database.QueryContext(ctx, `
		SELECT id, tenant_id, name, key_prefix, digest_version, status, revision,
		       policy_revision, policy, metadata, expires_at, revoked_at,
		       COALESCE(predecessor_id,''), COALESCE(replacement_id,''), grace_expires_at
		FROM api_keys
		WHERE tenant_id = $1 AND id > $2 AND ($3::text = '' OR status = $3)
		ORDER BY id LIMIT $4`, filter.TenantID, cursor, filter.Status, filter.Limit+1)
	if err != nil {
		return CredentialPage{}, err
	}
	defer rows.Close()
	page := CredentialPage{Data: make([]Credential, 0, filter.Limit)}
	for rows.Next() {
		credential, err := scanCredential(rows)
		if err != nil {
			return CredentialPage{}, err
		}
		page.Data = append(page.Data, credential)
	}
	if err := rows.Err(); err != nil {
		return CredentialPage{}, err
	}
	if len(page.Data) > filter.Limit {
		page.Data = page.Data[:filter.Limit]
		page.NextCursor = encodeCursor(page.Data[len(page.Data)-1].ID)
	}
	return page, nil
}

func authorizeRead(actor tenantadmin.ActorEnvelope, tenantID string) error {
	if actor.Type == "" || actor.ID == "" {
		return fmt.Errorf("%w: query Actor Envelope is incomplete", ErrInvalidArgument)
	}
	for _, scope := range actor.Scopes {
		if scope == tenantadmin.ScopePlatformRead || scope == tenantadmin.ScopePlatformWrite ||
			(scope == tenantadmin.ScopeTenantRead || scope == tenantadmin.ScopeTenantWrite) && actor.ActingTenantID == tenantID {
			return nil
		}
	}
	return ErrPolicyDenied
}

func encodeCursor(value string) string { return base64.RawURLEncoding.EncodeToString([]byte(value)) }

func decodeCursor(cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	value, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(value) == 0 || len(value) > 512 || strings.ContainsRune(string(value), '\x00') {
		return "", fmt.Errorf("%w: invalid cursor", ErrInvalidArgument)
	}
	return string(value), nil
}

func scanCredentialRow(row *sql.Row) (Credential, error) {
	credential, err := scanCredential(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Credential{}, ErrNotFound
	}
	return credential, err
}
