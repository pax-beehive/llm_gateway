package credentialadmin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/toddzheng/llm-gateway/internal/accessprojection"
	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

func (service *Service) BuildAccessSnapshot(
	ctx context.Context,
	actor tenantadmin.ActorEnvelope,
	tenantID string,
) (accessprojection.Snapshot, error) {
	if err := authorizeRead(actor, tenantID); err != nil {
		return accessprojection.Snapshot{}, err
	}
	var tenant accessprojection.TenantSnapshot
	var tenantPolicy []byte
	if err := service.database.QueryRowContext(ctx, `
		SELECT id, status, revision, home_region, execution_epoch, policy_revision, policy
		FROM tenants WHERE id = $1`, tenantID).Scan(
		&tenant.ID, &tenant.Status, &tenant.Revision, &tenant.HomeRegion, &tenant.ExecutionEpoch,
		&tenant.Policy.Revision, &tenantPolicy); errors.Is(err, sql.ErrNoRows) {
		return accessprojection.Snapshot{}, ErrNotFound
	} else if err != nil {
		return accessprojection.Snapshot{}, err
	}
	if err := json.Unmarshal(tenantPolicy, &tenant.Policy); err != nil {
		return accessprojection.Snapshot{}, err
	}
	rows, err := service.database.QueryContext(ctx, `
		SELECT id, key_prefix, secret_digest, digest_version, status, revision,
		       policy_revision, policy, expires_at, revoked_at
		FROM api_keys WHERE tenant_id = $1 ORDER BY id`, tenantID)
	if err != nil {
		return accessprojection.Snapshot{}, err
	}
	defer rows.Close()
	keys := make([]accessprojection.KeySnapshot, 0)
	for rows.Next() {
		var key accessprojection.KeySnapshot
		var policy []byte
		if err := rows.Scan(&key.ID, &key.Prefix, &key.SecretDigest, &key.DigestVersion, &key.Status,
			&key.Revision, &key.Policy.Revision, &policy, &key.ExpiresAt, &key.RevokedAt); err != nil {
			return accessprojection.Snapshot{}, err
		}
		if err := json.Unmarshal(policy, &key.Policy); err != nil {
			return accessprojection.Snapshot{}, err
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return accessprojection.Snapshot{}, err
	}
	return accessprojection.Snapshot{Tenant: tenant, Keys: keys, CreatedAt: service.now().UTC()}, nil
}

func (service *Service) ActiveDigestVersionCount(ctx context.Context, actor tenantadmin.ActorEnvelope, version int16) (int64, error) {
	if err := authorizePlatformRead(actor); err != nil {
		return 0, err
	}
	if version <= 0 {
		return 0, ErrInvalidArgument
	}
	var count int64
	err := service.database.QueryRowContext(ctx, `
		SELECT count(*) FROM api_keys k JOIN tenants t ON t.id = k.tenant_id
		WHERE k.digest_version = $1 AND k.status = 'active' AND t.status = 'active'
		  AND (k.expires_at IS NULL OR k.expires_at > $2)`, version, service.now().UTC()).Scan(&count)
	return count, err
}

func authorizePlatformRead(actor tenantadmin.ActorEnvelope) error {
	if actor.Type == "" || actor.ID == "" {
		return ErrInvalidArgument
	}
	for _, scope := range actor.Scopes {
		if scope == tenantadmin.ScopePlatformRead || scope == tenantadmin.ScopePlatformWrite {
			return nil
		}
	}
	return ErrPolicyDenied
}
