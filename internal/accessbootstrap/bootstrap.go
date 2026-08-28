// Package accessbootstrap owns the one-time import of development access
// configuration into the authoritative access store. It must be invoked by a
// dedicated bootstrap process, never by the Gateway data plane.
package accessbootstrap

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/toddzheng/llm-gateway/internal/access"
	"github.com/toddzheng/llm-gateway/internal/core"
)

type Input struct {
	APIKeys         map[string]string
	HomeRegions     map[string]string
	ExecutionEpochs map[string]int64
	TenantPolicies  map[string]core.TenantPolicy
	APIKeyPolicies  map[string]core.APIKeyPolicy
	APIKeyMetadata  map[string]map[string]any
}

func Bootstrap(ctx context.Context, service *access.PostgresService, input Input) ([]string, error) {
	if service == nil || len(input.APIKeys) == 0 {
		return nil, errors.New("access bootstrap requires an authoritative store and at least one API key")
	}
	for rawKey := range input.APIKeyPolicies {
		if _, exists := input.APIKeys[rawKey]; !exists {
			return nil, errors.New("API key policies contain a key absent from API keys")
		}
	}
	for rawKey := range input.APIKeyMetadata {
		if _, exists := input.APIKeys[rawKey]; !exists {
			return nil, errors.New("API key metadata contains a key absent from API keys")
		}
	}
	tenantIDs := make([]string, 0, len(input.APIKeys))
	seen := make(map[string]struct{})
	for _, tenantID := range input.APIKeys {
		if tenantID == "" {
			return nil, errors.New("bootstrap API key has an empty Tenant ID")
		}
		if _, exists := seen[tenantID]; !exists {
			seen[tenantID] = struct{}{}
			tenantIDs = append(tenantIDs, tenantID)
		}
	}
	slices.Sort(tenantIDs)
	for _, tenantID := range tenantIDs {
		homeRegion := input.HomeRegions[tenantID]
		if homeRegion == "" {
			return nil, fmt.Errorf("Tenant %q has no configured Home Region", tenantID)
		}
		epoch := input.ExecutionEpochs[tenantID]
		if epoch == 0 {
			epoch = 1
		}
		policy := input.TenantPolicies[tenantID]
		if policy.Revision == 0 {
			policy.Revision = 1
		}
		if err := service.CreateTenant(ctx, access.Tenant{ID: tenantID, Slug: tenantID, DisplayName: tenantID, Status: access.TenantActive, HomeRegion: homeRegion, ExecutionEpoch: epoch, Policy: policy}, access.ChangeActor{Type: "bootstrap", ID: "access-bootstrap"}); err != nil {
			return nil, fmt.Errorf("bootstrap Tenant %q: %w", tenantID, err)
		}
	}
	rawKeys := make([]string, 0, len(input.APIKeys))
	for rawKey := range input.APIKeys {
		rawKeys = append(rawKeys, rawKey)
	}
	slices.Sort(rawKeys)
	for _, rawKey := range rawKeys {
		policy := input.APIKeyPolicies[rawKey]
		if policy.Revision == 0 {
			policy.Revision = 1
		}
		if _, err := service.ImportAPIKey(ctx, access.APIKeySpec{TenantID: input.APIKeys[rawKey], Name: "gateway bootstrap key", RawKey: rawKey, Policy: policy, Metadata: input.APIKeyMetadata[rawKey]}); err != nil {
			return nil, fmt.Errorf("bootstrap API key for Tenant %q: %w", input.APIKeys[rawKey], err)
		}
	}
	return tenantIDs, nil
}
