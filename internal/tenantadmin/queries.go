package tenantadmin

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/toddzheng/llm-gateway/internal/access"
)

func (s *Service) GetTenant(ctx context.Context, actor ActorEnvelope, tenantID string) (access.Tenant, error) {
	if tenantID == "" {
		return access.Tenant{}, fmt.Errorf("%w: Tenant ID is required", ErrInvalidArgument)
	}
	if err := authorizeTenantRead(actor, tenantID); err != nil {
		return access.Tenant{}, err
	}
	return scanTenant(s.db.QueryRowContext(ctx, `
		SELECT id, slug, display_name, status, home_region, execution_epoch,
		       policy_revision, policy, metadata, revision
		FROM tenants WHERE id = $1`, tenantID))
}

func (s *Service) ListTenants(ctx context.Context, actor ActorEnvelope, filter TenantFilter) (TenantPage, error) {
	if err := validateQueryActor(actor); err != nil {
		return TenantPage{}, err
	}
	platform := hasScope(actor, ScopePlatformRead) || hasScope(actor, ScopePlatformWrite)
	if !platform {
		if (!hasScope(actor, ScopeTenantRead) && !hasScope(actor, ScopeTenantWrite)) || actor.ActingTenantID == "" {
			return TenantPage{}, ErrPolicyDenied
		}
		if filter.ID != "" && filter.ID != actor.ActingTenantID {
			return TenantPage{}, ErrPolicyDenied
		}
		filter.ID = actor.ActingTenantID
	}
	if filter.Status != "" && filter.Status != access.TenantActive && filter.Status != access.TenantSuspended && filter.Status != access.TenantClosed {
		return TenantPage{}, fmt.Errorf("%w: invalid Tenant status filter", ErrInvalidArgument)
	}
	if filter.Limit == 0 {
		filter.Limit = 50
	}
	if filter.Limit < 1 || filter.Limit > 100 {
		return TenantPage{}, fmt.Errorf("%w: limit must be between 1 and 100", ErrInvalidArgument)
	}
	cursor, err := decodeStringCursor(filter.Cursor)
	if err != nil {
		return TenantPage{}, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, slug, display_name, status, home_region, execution_epoch,
		       policy_revision, policy, metadata, revision
		FROM tenants
		WHERE id > $1
		  AND ($2::text = '' OR id = $2)
		  AND ($3::text = '' OR slug = $3)
		  AND ($4::text = '' OR status = $4)
		  AND ($5::text = '' OR home_region = $5)
		  AND ($6::boolean OR status <> 'closed')
		ORDER BY id
		LIMIT $7`, cursor, filter.ID, filter.Slug, filter.Status, filter.HomeRegion,
		filter.IncludeClosed || filter.Status == access.TenantClosed, filter.Limit+1)
	if err != nil {
		return TenantPage{}, err
	}
	defer rows.Close()
	page := TenantPage{Data: make([]access.Tenant, 0, filter.Limit)}
	for rows.Next() {
		tenant, err := scanTenant(rows)
		if err != nil {
			return TenantPage{}, err
		}
		page.Data = append(page.Data, tenant)
	}
	if err := rows.Err(); err != nil {
		return TenantPage{}, err
	}
	if len(page.Data) > filter.Limit {
		page.Data = page.Data[:filter.Limit]
		page.NextCursor = encodeStringCursor(page.Data[len(page.Data)-1].ID)
	}
	return page, nil
}

func (s *Service) GetTenantPolicy(ctx context.Context, actor ActorEnvelope, tenantID string) (access.Tenant, error) {
	return s.GetTenant(ctx, actor, tenantID)
}

func (s *Service) ListTenantPolicyRevisions(
	ctx context.Context,
	actor ActorEnvelope,
	tenantID, cursor string,
	limit int,
) (PolicyRevisionPage, error) {
	if err := authorizeTenantRead(actor, tenantID); err != nil {
		return PolicyRevisionPage{}, err
	}
	if _, err := s.GetTenant(ctx, actor, tenantID); err != nil {
		return PolicyRevisionPage{}, err
	}
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 100 {
		return PolicyRevisionPage{}, fmt.Errorf("%w: limit must be between 1 and 100", ErrInvalidArgument)
	}
	afterRevision, err := decodeRevisionCursor(cursor)
	if err != nil {
		return PolicyRevisionPage{}, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT tenant_id, revision, policy, actor_type, actor_id, COALESCE(change_reason,''), created_at
		FROM tenant_policy_revisions
		WHERE tenant_id = $1 AND revision > $2
		ORDER BY revision
		LIMIT $3`, tenantID, afterRevision, limit+1)
	if err != nil {
		return PolicyRevisionPage{}, err
	}
	defer rows.Close()
	page := PolicyRevisionPage{Data: make([]PolicyRevision, 0, limit)}
	for rows.Next() {
		var revision PolicyRevision
		var policyPayload []byte
		if err := rows.Scan(
			&revision.TenantID, &revision.Revision, &policyPayload, &revision.ActorType,
			&revision.ActorID, &revision.ChangeReason, &revision.CreatedAt,
		); err != nil {
			return PolicyRevisionPage{}, err
		}
		if err := json.Unmarshal(policyPayload, &revision.Policy); err != nil {
			return PolicyRevisionPage{}, err
		}
		revision.Policy.Revision = revision.Revision
		page.Data = append(page.Data, revision)
	}
	if err := rows.Err(); err != nil {
		return PolicyRevisionPage{}, err
	}
	if len(page.Data) > limit {
		page.Data = page.Data[:limit]
		page.NextCursor = encodeRevisionCursor(page.Data[len(page.Data)-1].Revision)
	}
	return page, nil
}

type scanner interface {
	Scan(...any) error
}

func scanTenant(source scanner) (access.Tenant, error) {
	var tenant access.Tenant
	var policyPayload, metadataPayload []byte
	err := source.Scan(
		&tenant.ID, &tenant.Slug, &tenant.DisplayName, &tenant.Status, &tenant.HomeRegion,
		&tenant.ExecutionEpoch, &tenant.Policy.Revision, &policyPayload, &metadataPayload, &tenant.Revision,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return access.Tenant{}, ErrNotFound
	}
	if err != nil {
		return access.Tenant{}, err
	}
	if err := json.Unmarshal(policyPayload, &tenant.Policy); err != nil {
		return access.Tenant{}, err
	}
	if err := json.Unmarshal(metadataPayload, &tenant.Metadata); err != nil {
		return access.Tenant{}, err
	}
	return tenant, nil
}

func authorizeTenantRead(actor ActorEnvelope, tenantID string) error {
	if err := validateQueryActor(actor); err != nil {
		return err
	}
	if hasScope(actor, ScopePlatformRead) || hasScope(actor, ScopePlatformWrite) {
		return nil
	}
	if (hasScope(actor, ScopeTenantRead) || hasScope(actor, ScopeTenantWrite)) && actor.ActingTenantID == tenantID {
		return nil
	}
	return ErrPolicyDenied
}

func authorizeTenantWrite(actor ActorEnvelope, tenantID string) error {
	if err := validateMutationActor(actor); err != nil {
		return err
	}
	if hasScope(actor, ScopePlatformWrite) {
		return nil
	}
	if hasScope(actor, ScopeTenantWrite) && actor.ActingTenantID == tenantID {
		return nil
	}
	return ErrPolicyDenied
}

func validateQueryActor(actor ActorEnvelope) error {
	if actor.Type == "" || actor.ID == "" {
		return fmt.Errorf("%w: query Actor Envelope is incomplete", ErrInvalidArgument)
	}
	return nil
}

func encodeStringCursor(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func decodeStringCursor(cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	value, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(value) == 0 || len(value) > 512 || strings.ContainsRune(string(value), '\x00') {
		return "", fmt.Errorf("%w: invalid cursor", ErrInvalidArgument)
	}
	return string(value), nil
}

func encodeRevisionCursor(revision int64) string {
	return encodeStringCursor(strconv.FormatInt(revision, 10))
}

func decodeRevisionCursor(cursor string) (int64, error) {
	value, err := decodeStringCursor(cursor)
	if err != nil || value == "" && cursor != "" {
		return 0, fmt.Errorf("%w: invalid revision cursor", ErrInvalidArgument)
	}
	if value == "" {
		return 0, nil
	}
	revision, parseErr := strconv.ParseInt(value, 10, 64)
	if parseErr != nil || revision <= 0 {
		return 0, fmt.Errorf("%w: invalid revision cursor", ErrInvalidArgument)
	}
	return revision, nil
}
