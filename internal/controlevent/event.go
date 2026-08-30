package controlevent

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrExecutionSecretNotFound    = errors.New("Provider Connection execution secret not found")
	ErrExecutionSecretUnavailable = errors.New("Provider Connection execution secret unavailable")
)

// Event is the transport-neutral Control Event envelope consumed by Gateway
// projections. Payloads are aggregate-specific and must never contain secret
// material or Secret Custody references.
type Event struct {
	EventID           string          `json:"event_id"`
	DeliverySequence  int64           `json:"delivery_sequence"`
	SchemaVersion     int             `json:"schema_version"`
	AggregateType     string          `json:"aggregate_type"`
	AggregateID       string          `json:"aggregate_id"`
	AggregateRevision int64           `json:"aggregate_revision"`
	TenantID          string          `json:"tenant_id,omitempty"`
	EventType         string          `json:"event_type"`
	OccurredAt        time.Time       `json:"occurred_at"`
	Payload           json.RawMessage `json:"payload"`
}

type Batch struct {
	Events     []Event `json:"events"`
	NextCursor int64   `json:"next_cursor"`
	SourceHead int64   `json:"source_head"`
}

type Audience struct {
	GatewayID string
	Region    string
}

// Publisher is the control-plane port for a region-scoped, ordered event
// stream. A returned cursor may advance across events outside the audience.
type Publisher interface {
	Publish(context.Context, Audience, int64, int) (Batch, error)
}

// Consumer is the Gateway-side port. Implementations must be idempotent.
type Consumer interface {
	Consume(context.Context, Event) error
}

type ConsumerFunc func(context.Context, Event) error

func (function ConsumerFunc) Consume(ctx context.Context, event Event) error {
	return function(ctx, event)
}
