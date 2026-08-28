package credentialadmin

import (
	"errors"
	"time"

	"github.com/toddzheng/llm-gateway/internal/access"
	"github.com/toddzheng/llm-gateway/internal/core"
)

var (
	ErrNotFound            = errors.New("gateway credential administration record not found")
	ErrAlreadyExists       = errors.New("gateway credential administration record already exists")
	ErrRevisionConflict    = errors.New("gateway credential administration revision conflict")
	ErrIdempotencyConflict = errors.New("gateway credential administration idempotency conflict")
	ErrPolicyDenied        = errors.New("gateway credential administration policy denied")
	ErrInvalidArgument     = errors.New("gateway credential administration invalid argument")
)

type PepperRing struct {
	CurrentVersion int16
	Peppers        map[int16][]byte
}

type IssueCommand struct {
	TenantID  string
	Name      string
	Metadata  map[string]any
	ExpiresAt *time.Time
	Policy    core.APIKeyPolicy
}

type UpdateCommand struct {
	TenantID         string
	CredentialID     string
	ExpectedRevision int64
	Name             *string
	Metadata         *map[string]any
	ExpiresAt        *time.Time
	ClearExpiresAt   bool
}

type RevokeCommand struct {
	TenantID         string
	CredentialID     string
	ExpectedRevision int64
}

type RotateCommand struct {
	TenantID          string
	CredentialID      string
	ExpectedRevision  int64
	RevokeImmediately bool
	GraceExpiresAt    *time.Time
}

type PublishPolicyCommand struct {
	TenantID         string
	CredentialID     string
	ExpectedRevision int64
	Policy           *core.APIKeyPolicy
	RestoreRevision  *int64
}

type CredentialFilter struct {
	TenantID string
	Status   access.APIKeyStatus
	Cursor   string
	Limit    int
}

type Credential struct {
	ID             string
	TenantID       string
	Name           string
	Prefix         string
	DigestVersion  int16
	Status         access.APIKeyStatus
	Revision       int64
	Policy         core.APIKeyPolicy
	Metadata       map[string]any
	ExpiresAt      *time.Time
	RevokedAt      *time.Time
	PredecessorID  string
	ReplacementID  string
	GraceExpiresAt *time.Time
}

type IssueResult struct {
	Credential Credential
	RawSecret  string
	Replay     bool
}

type MutationResult struct {
	Credential Credential
	Replay     bool
}

type RotationResult struct {
	Predecessor Credential
	Replacement Credential
	RawSecret   string
	Replay      bool
}

type CredentialPage struct {
	Data       []Credential
	NextCursor string
}

type PolicyRevision struct {
	TenantID     string            `json:"tenant_id"`
	CredentialID string            `json:"api_key_id"`
	Revision     int64             `json:"revision"`
	Policy       core.APIKeyPolicy `json:"policy"`
	ActorType    string            `json:"actor_type"`
	ActorID      string            `json:"actor_id"`
	ChangeReason string            `json:"change_reason"`
	CreatedAt    time.Time         `json:"created_at"`
}

type PolicyRevisionPage struct {
	Data       []PolicyRevision
	NextCursor string
}

type EffectivePolicy struct {
	TenantPolicy           core.TenantPolicy `json:"tenant_policy"`
	APIKeyPolicy           core.APIKeyPolicy `json:"api_key_policy"`
	Limits                 core.QuotaLimits  `json:"limits"`
	MaxConcurrentResponses int               `json:"max_concurrent_responses"`
}
