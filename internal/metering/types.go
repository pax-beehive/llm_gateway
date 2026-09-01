package metering

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/toddzheng/llm-gateway/internal/quota"
)

const (
	EventUsageRecorded           = "UsageRecorded"
	EventCapabilityUsageRecorded = "CapabilityUsageRecorded"
	EventCacheRefreshRecorded    = "CacheRefreshUsageRecorded"
	EventUsageCorrected          = "UsageCorrected"
	CurrentEventSchemaVersion    = 1

	ScopePlatformRead  = "platform:metering:read"
	ScopePlatformWrite = "platform:metering:write"
	ScopeTenantRead    = "tenant:metering:read"
)

var (
	ErrInvalidArgument = errors.New("metering invalid argument")
	ErrNotFound        = errors.New("metering record not found")
	ErrPolicyDenied    = errors.New("metering policy denied")
)

// UsageEvent is deliberately content-free. Provider response bodies, prompts,
// outputs, metadata, and credentials have no field in this contract.
type UsageEvent struct {
	EventID                  string          `json:"event_id"`
	SchemaVersion            int             `json:"schema_version"`
	Type                     string          `json:"type"`
	UsageID                  string          `json:"usage_id"`
	TenantID                 string          `json:"tenant_id"`
	APIKeyID                 string          `json:"api_key_id,omitempty"`
	ResponseID               string          `json:"response_id,omitempty"`
	AttemptID                string          `json:"attempt_id,omitempty"`
	OperationID              string          `json:"operation_id,omitempty"`
	Capability               string          `json:"capability,omitempty"`
	RouteID                  string          `json:"route_id,omitempty"`
	Provider                 string          `json:"provider"`
	PublicModel              string          `json:"public_model"`
	ProviderModel            string          `json:"provider_model"`
	Region                   string          `json:"region"`
	PriceSnapshotID          string          `json:"price_snapshot_id"`
	InputTokens              int64           `json:"input_tokens"`
	CachedInputTokens        int64           `json:"cached_input_tokens"`
	CacheWriteInputTokens    int64           `json:"cache_write_input_tokens"`
	OutputTokens             int64           `json:"output_tokens"`
	InputUnits               int64           `json:"input_units"`
	Documents                int64           `json:"documents"`
	AmountMicros             int64           `json:"amount_micros"`
	Currency                 string          `json:"currency"`
	Outcome                  string          `json:"outcome"`
	CorrectsEventID          string          `json:"corrects_event_id,omitempty"`
	CorrectionActorID        string          `json:"correction_actor_id,omitempty"`
	Reason                   string          `json:"reason,omitempty"`
	OccurredAt               time.Time       `json:"occurred_at"`
	ProviderUsageFingerprint json.RawMessage `json:"-"`
}

func (event UsageEvent) Validate() error {
	if event.SchemaVersion != CurrentEventSchemaVersion || event.EventID == "" || event.UsageID == "" ||
		event.TenantID == "" || event.Provider == "" || event.PublicModel == "" ||
		event.ProviderModel == "" || event.Region == "" || event.PriceSnapshotID == "" ||
		event.Currency == "" || event.Outcome == "" || event.OccurredAt.IsZero() {
		return ErrInvalidArgument
	}
	switch event.Type {
	case EventUsageRecorded:
		if event.ResponseID == "" || event.AttemptID == "" {
			return ErrInvalidArgument
		}
	case EventCapabilityUsageRecorded:
		if event.OperationID == "" || event.Capability == "" {
			return ErrInvalidArgument
		}
	case EventCacheRefreshRecorded:
		if event.OperationID == "" {
			return ErrInvalidArgument
		}
	case EventUsageCorrected:
		if event.CorrectsEventID == "" || event.CorrectionActorID == "" || event.Reason == "" {
			return ErrInvalidArgument
		}
	default:
		return ErrInvalidArgument
	}
	if event.Type != EventUsageCorrected && (event.InputTokens < 0 || event.CachedInputTokens < 0 ||
		event.CacheWriteInputTokens < 0 || event.OutputTokens < 0 || event.InputUnits < 0 ||
		event.Documents < 0 || event.AmountMicros < 0) {
		return ErrInvalidArgument
	}
	return nil
}

type Filter struct {
	TenantID      string
	APIKeyID      string
	ResponseID    string
	Provider      string
	PublicModel   string
	ProviderModel string
	RouteID       string
	Outcome       string
	Currency      string
	From          time.Time
	Through       time.Time
	// AllTenants is set only after platform-scope authorization. It is never
	// accepted from request JSON and cannot be used for Tenant-scoped exports.
	AllTenants bool `json:"-"`
}

type Totals struct {
	Currency              string `json:"currency"`
	OperationCount        int64  `json:"operation_count"`
	InputTokens           int64  `json:"input_tokens"`
	CachedInputTokens     int64  `json:"cached_input_tokens"`
	CacheWriteInputTokens int64  `json:"cache_write_input_tokens"`
	OutputTokens          int64  `json:"output_tokens"`
	InputUnits            int64  `json:"input_units"`
	Documents             int64  `json:"documents"`
	AmountMicros          int64  `json:"amount_micros"`
}

type Summary struct {
	DataCutoff time.Time `json:"data_cutoff"`
	Totals     []Totals  `json:"totals"`
}

type TimePoint struct {
	Start  time.Time `json:"start"`
	Totals Totals    `json:"totals"`
}

type EventPage struct {
	Data       []UsageEvent `json:"data"`
	NextCursor string       `json:"next_cursor,omitempty"`
	DataCutoff time.Time    `json:"data_cutoff"`
}

type QuotaDenialFilter struct {
	Filter
	Scope     string
	Dimension string
	Cursor    string
	Limit     int
}

type QuotaDenialPage struct {
	Data       []quota.DenialEvent `json:"data"`
	NextCursor string              `json:"next_cursor,omitempty"`
	DataCutoff time.Time           `json:"data_cutoff"`
}

type Export struct {
	ID          string          `json:"id"`
	TenantID    string          `json:"tenant_id"`
	Status      string          `json:"status"`
	Filter      json.RawMessage `json:"filter"`
	Cutoff      time.Time       `json:"cutoff"`
	ObjectKey   string          `json:"object_key,omitempty"`
	SHA256      string          `json:"sha256,omitempty"`
	RowCount    int64           `json:"row_count"`
	ErrorCode   string          `json:"error_code,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	CompletedAt *time.Time      `json:"completed_at,omitempty"`
}

type Status struct {
	ProjectionGeneration int64      `json:"projection_generation"`
	ProjectionCutoff     time.Time  `json:"projection_cutoff"`
	PendingEvents        int64      `json:"pending_events"`
	OldestPendingAt      *time.Time `json:"oldest_pending_at,omitempty"`
	PoisonEvents         int64      `json:"poison_events"`
	QueuedExports        int64      `json:"queued_exports"`
}
