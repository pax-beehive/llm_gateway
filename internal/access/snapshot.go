package access

import (
	"time"

	"github.com/toddzheng/llm-gateway/internal/core"
)

// Snapshot is an authoritative, point-in-time access contract that can be
// consumed by any Gateway Access Projection implementation.
type Snapshot struct {
	Tenant    TenantSnapshot `json:"tenant"`
	Keys      []KeySnapshot  `json:"keys"`
	CreatedAt time.Time      `json:"created_at"`
}

type TenantSnapshot struct {
	ID             string            `json:"id"`
	Status         TenantStatus      `json:"status"`
	Revision       int64             `json:"revision"`
	HomeRegion     string            `json:"home_region"`
	ExecutionEpoch int64             `json:"execution_epoch"`
	Policy         core.TenantPolicy `json:"policy"`
}

type KeySnapshot struct {
	ID            string            `json:"id"`
	Prefix        string            `json:"prefix"`
	SecretDigest  []byte            `json:"secret_digest"`
	DigestVersion int16             `json:"digest_version"`
	Status        APIKeyStatus      `json:"status"`
	Revision      int64             `json:"revision"`
	Policy        core.APIKeyPolicy `json:"policy"`
	ExpiresAt     *time.Time        `json:"expires_at,omitempty"`
	RevokedAt     *time.Time        `json:"revoked_at,omitempty"`
	LastUsedAt    *time.Time        `json:"last_used_at,omitempty"`
}
