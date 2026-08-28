package access

import (
	"time"

	"github.com/toddzheng/llm-gateway/internal/core"
)

// Snapshot is an authoritative, point-in-time access contract that can be
// consumed by any Gateway Access Projection implementation.
type Snapshot struct {
	Tenant    TenantSnapshot
	Keys      []KeySnapshot
	CreatedAt time.Time
}

type TenantSnapshot struct {
	ID             string
	Status         TenantStatus
	Revision       int64
	HomeRegion     string
	ExecutionEpoch int64
	Policy         core.TenantPolicy
}

type KeySnapshot struct {
	ID            string
	Prefix        string
	SecretDigest  []byte
	DigestVersion int16
	Status        APIKeyStatus
	Revision      int64
	Policy        core.APIKeyPolicy
	ExpiresAt     *time.Time
	RevokedAt     *time.Time
	LastUsedAt    *time.Time
}
