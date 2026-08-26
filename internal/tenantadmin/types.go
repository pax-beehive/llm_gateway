package tenantadmin

import (
	"errors"
	"time"

	"github.com/toddzheng/llm-gateway/internal/access"
	"github.com/toddzheng/llm-gateway/internal/core"
)

var (
	ErrNotFound            = errors.New("tenant administration record not found")
	ErrRevisionConflict    = errors.New("tenant administration revision conflict")
	ErrIdempotencyConflict = errors.New("tenant administration idempotency conflict")
	ErrPolicyDenied        = errors.New("tenant administration policy denied")
	ErrInvalidArgument     = errors.New("tenant administration invalid argument")
)

const (
	ScopePlatformRead  = "platform:tenants:read"
	ScopePlatformWrite = "platform:tenants:write"
	ScopeTenantRead    = "tenant:read"
	ScopeTenantWrite   = "tenant:write"
)

type ActorEnvelope struct {
	Type           string
	ID             string
	ActingTenantID string
	Scopes         []string
	RequestID      string
	Reason         string
}

type CreateTenantCommand struct {
	ID            string
	Slug          string
	DisplayName   string
	HomeRegion    string
	Metadata      map[string]any
	InitialPolicy core.TenantPolicy
}

type UpdateTenantCommand struct {
	TenantID         string
	ExpectedRevision int64
	DisplayName      *string
	Metadata         *map[string]any
}

type TransitionTenantCommand struct {
	TenantID         string
	ExpectedRevision int64
	Target           access.TenantStatus
}

type PublishPolicyCommand struct {
	TenantID         string
	ExpectedRevision int64
	Policy           *core.TenantPolicy
	RestoreRevision  *int64
}

type MutationResult struct {
	Tenant access.Tenant
	Replay bool
}

type TenantFilter struct {
	ID            string
	Slug          string
	Status        access.TenantStatus
	HomeRegion    string
	IncludeClosed bool
	Cursor        string
	Limit         int
}

type TenantPage struct {
	Data       []access.Tenant
	NextCursor string
}

type PolicyRevision struct {
	TenantID     string            `json:"tenant_id"`
	Revision     int64             `json:"revision"`
	Policy       core.TenantPolicy `json:"policy"`
	ActorType    string            `json:"actor_type"`
	ActorID      string            `json:"actor_id"`
	ChangeReason string            `json:"change_reason"`
	CreatedAt    time.Time         `json:"created_at"`
}

type PolicyRevisionPage struct {
	Data       []PolicyRevision
	NextCursor string
}
