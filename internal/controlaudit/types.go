package controlaudit

import (
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrInvalidArgument = errors.New("Control Audit invalid argument")
	ErrPolicyDenied    = errors.New("Control Audit policy denied")
)

type Event struct {
	ID                string          `json:"id"`
	TenantID          string          `json:"tenant_id,omitempty"`
	ActorType         string          `json:"actor_type"`
	ActorID           string          `json:"actor_id"`
	ActingTenantID    string          `json:"acting_tenant_id,omitempty"`
	Scopes            []string        `json:"scopes"`
	RequestID         string          `json:"request_id"`
	Reason            string          `json:"reason"`
	Action            string          `json:"action"`
	AggregateType     string          `json:"aggregate_type,omitempty"`
	AggregateID       string          `json:"aggregate_id,omitempty"`
	AggregateRevision int64           `json:"aggregate_revision"`
	Payload           json.RawMessage `json:"payload"`
	OccurredAt        time.Time       `json:"occurred_at"`
}

type Filter struct {
	TenantID      string
	AggregateType string
	AggregateID   string
	ActorType     string
	ActorID       string
	Action        string
	From          time.Time
	Through       time.Time
	Cursor        string
	Limit         int
}

type Page struct {
	Data       []Event `json:"data"`
	NextCursor string  `json:"next_cursor,omitempty"`
}
