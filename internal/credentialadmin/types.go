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
