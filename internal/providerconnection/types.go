package providerconnection

import (
	"errors"
	"time"

	"github.com/toddzheng/llm-gateway/internal/provider"
)

var (
	ErrNotFound            = errors.New("Provider Connection not found")
	ErrAlreadyExists       = errors.New("Provider Connection already exists")
	ErrRevisionConflict    = errors.New("Provider Connection revision conflict")
	ErrIdempotencyConflict = errors.New("Provider Connection idempotency conflict")
	ErrPolicyDenied        = errors.New("Provider Connection policy denied")
	ErrInvalidArgument     = errors.New("invalid Provider Connection argument")
)

type AdministrativeStatus string

const (
	StatusEnabled  AdministrativeStatus = "enabled"
	StatusDisabled AdministrativeStatus = "disabled"
)

type ProviderConnection struct {
	ID                    string                     `json:"id"`
	Provider              string                     `json:"provider"`
	DisplayName           string                     `json:"display_name"`
	BaseURL               string                     `json:"base_url"`
	Region                string                     `json:"region"`
	CredentialScope       string                     `json:"credential_scope"`
	AdministrativeStatus  AdministrativeStatus       `json:"administrative_status"`
	CapabilityDeclaration provider.CapabilityProfile `json:"capability_declaration"`
	CredentialVersion     int64                      `json:"credential_version"`
	Revision              int64                      `json:"revision"`
	CreatedAt             time.Time                  `json:"created_at"`
	UpdatedAt             time.Time                  `json:"updated_at"`
	SecretRef             string                     `json:"-"`
	SecretExternalVersion string                     `json:"-"`
}

type RegisterCommand struct {
	ID                    string
	Provider              string
	DisplayName           string
	BaseURL               string
	Region                string
	CredentialScope       string
	Secret                []byte
	CapabilityDeclaration provider.CapabilityProfile
}

type MutationResult struct {
	Connection ProviderConnection `json:"connection"`
	Replay     bool               `json:"replay,omitempty"`
}

type ConnectionFilter struct {
	Provider string
	Region   string
	Status   AdministrativeStatus
	Cursor   string
	Limit    int
}

type ConnectionPage struct {
	Data       []ProviderConnection `json:"data"`
	NextCursor string               `json:"next_cursor,omitempty"`
}

type UpdateCommand struct {
	ConnectionID          string
	ExpectedRevision      int64
	DisplayName           *string
	BaseURL               *string
	Region                *string
	CredentialScope       *string
	CapabilityDeclaration *provider.CapabilityProfile
}

type StatusCommand struct {
	ConnectionID     string
	ExpectedRevision int64
}

type OperationType string

const (
	OperationProbe              OperationType = "probe"
	OperationModelDiscovery     OperationType = "model_discovery"
	OperationCredentialRotation OperationType = "credential_rotation"
)

type OperationStatus string

const (
	OperationQueued    OperationStatus = "queued"
	OperationRunning   OperationStatus = "running"
	OperationSucceeded OperationStatus = "succeeded"
	OperationFailed    OperationStatus = "failed"
	OperationUncertain OperationStatus = "uncertain"
)

type OperationAuthorization struct {
	Source              string
	MaxProviderRequests int
	MaxSpendMicros      int64
}

type Operation struct {
	ID                   string          `json:"id"`
	Type                 OperationType   `json:"type"`
	ConnectionID         string          `json:"connection_id"`
	ExpectedRevision     int64           `json:"expected_revision"`
	Status               OperationStatus `json:"status"`
	Result               map[string]any  `json:"result,omitempty"`
	ErrorCode            string          `json:"error_code,omitempty"`
	ErrorMessage         string          `json:"error_message,omitempty"`
	CreatedAt            time.Time       `json:"created_at"`
	StartedAt            *time.Time      `json:"started_at,omitempty"`
	CompletedAt          *time.Time      `json:"completed_at,omitempty"`
	PendingSecretRef     string          `json:"-"`
	PendingSecretVersion string          `json:"-"`
	ActorType            string          `json:"-"`
	ActorID              string          `json:"-"`
	ActingTenantID       string          `json:"-"`
	Scopes               []string        `json:"-"`
	RequestID            string          `json:"-"`
	Reason               string          `json:"-"`
	AuthorizationSource  string          `json:"-"`
	MaxProviderRequests  int             `json:"-"`
	MaxSpendMicros       int64           `json:"-"`
	RetrySafe            bool            `json:"-"`
}

type OperationResult struct {
	Operation Operation `json:"operation"`
	Replay    bool      `json:"replay,omitempty"`
}

type OperationCommand struct {
	ConnectionID     string
	ExpectedRevision int64
}

type RotationCommand struct {
	ConnectionID     string
	ExpectedRevision int64
	Secret           []byte
}

type ProbeResult struct {
	ObservedModelCount int
	RawResponseHash    string
	ProviderRequests   int
}

type ObservedModel struct {
	ID           string
	OwnedBy      string
	Capabilities map[string]provider.CapabilitySupport
}

type DiscoveryResult struct {
	Models           []ObservedModel
	RawResponseHash  string
	ProviderRequests int
}

type OperationError struct {
	Code string
}

func (err *OperationError) Error() string { return err.Code }

type ResolvedConnection struct {
	Connection      ProviderConnection
	Secret          []byte
	ObservedHealthy *bool
}
