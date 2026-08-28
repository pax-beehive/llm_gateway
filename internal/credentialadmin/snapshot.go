package credentialadmin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

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
	transaction, err := service.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return accessprojection.Snapshot{}, err
	}
	defer func() { _ = transaction.Rollback() }()
	var tenant accessprojection.TenantSnapshot
	var tenantPolicy []byte
	if err := transaction.QueryRowContext(ctx, `
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
	rows, err := transaction.QueryContext(ctx, `
		SELECT id, key_prefix, secret_digest, digest_version, status, revision,
		       policy_revision, policy, expires_at, revoked_at, last_used_at
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
			&key.Revision, &key.Policy.Revision, &policy, &key.ExpiresAt, &key.RevokedAt, &key.LastUsedAt); err != nil {
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
	if err := transaction.Commit(); err != nil {
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
		WHERE k.digest_version = $1 AND k.status = 'active' AND t.status <> 'closed'
		  AND (k.expires_at IS NULL OR k.expires_at > $2)`, version, service.now().UTC()).Scan(&count)
	return count, err
}

func (service *Service) ValidatePepperCoverage(ctx context.Context, actor tenantadmin.ActorEnvelope) error {
	if err := authorizePlatformRead(actor); err != nil {
		return err
	}
	rows, err := service.database.QueryContext(ctx, `
		SELECT DISTINCT k.digest_version
		FROM api_keys k JOIN tenants t ON t.id = k.tenant_id
		WHERE k.status = 'active' AND t.status <> 'closed'
		  AND (k.expires_at IS NULL OR k.expires_at > $1)
		ORDER BY k.digest_version`, service.now().UTC())
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var version int16
		if err := rows.Scan(&version); err != nil {
			return err
		}
		if len(service.peppers[version]) == 0 {
			return fmt.Errorf("active authoritative Gateway API Keys still require digest pepper version %d", version)
		}
	}
	return rows.Err()
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
