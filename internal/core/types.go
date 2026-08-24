package core

import (
	"encoding/json"
	"time"
)

type ResponseStatus string

const (
	ResponseStatusQueued     ResponseStatus = "queued"
	ResponseStatusInProgress ResponseStatus = "in_progress"
	ResponseStatusCompleted  ResponseStatus = "completed"
	ResponseStatusFailed     ResponseStatus = "failed"
	ResponseStatusCancelled  ResponseStatus = "cancelled"
	ResponseStatusDeleted    ResponseStatus = "deleted"
)

type CompatibilityMode string

const (
	CompatibilityStrict     CompatibilityMode = "strict"
	CompatibilityBestEffort CompatibilityMode = "best_effort"
)

type Content struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type Item struct {
	ID        string          `json:"id,omitempty"`
	Type      string          `json:"type"`
	Role      string          `json:"role,omitempty"`
	Content   []Content       `json:"content,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Output    string          `json:"output,omitempty"`
}

type Usage struct {
	InputTokens       int64 `json:"input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
	TotalTokens       int64 `json:"total_tokens"`
	CachedInputTokens int64 `json:"cached_input_tokens,omitempty"`
}

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
	Param   string `json:"param,omitempty"`
}

type Request struct {
	TenantID           string
	Model              string
	Input              []Item
	Stream             bool
	Background         bool
	Store              bool
	PreviousResponseID string
	ConversationID     string
	HomeRegion         string
	CompatibilityMode  CompatibilityMode
	RequestedFeatures  []string
	Metadata           map[string]string
	Tools              []json.RawMessage
	ToolChoice         json.RawMessage
	Temperature        *float64
	TopP               *float64
	MaxOutputTokens    *int
	Stop               json.RawMessage
	EndUserID          string
	PreferredRouteID   string
	IdempotencyKey     string
	RequestHash        []byte
	ContextItemCount   int
	CacheProtection    *CacheProtectionPolicy
}

type CacheProtectionPolicy struct {
	Enabled                bool  `json:"enabled"`
	MaxSpendMicros         int64 `json:"max_spend_micros"`
	MaxRefreshes           int   `json:"max_refreshes"`
	MaxProtectionWindowSec int64 `json:"max_protection_window_seconds"`
	SafetyMarginMicros     int64 `json:"safety_margin_micros"`
	AllowContentInspection bool  `json:"allow_content_inspection,omitempty"`
	ShadowMode             bool  `json:"shadow_mode,omitempty"`
}

type Attempt struct {
	ID              string     `json:"id"`
	RouteID         string     `json:"route_id"`
	Provider        string     `json:"provider"`
	ProviderModel   string     `json:"provider_model"`
	Region          string     `json:"region"`
	StartedAt       time.Time  `json:"started_at"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
	FirstVisibleAt  *time.Time `json:"first_visible_at,omitempty"`
	Error           *Error     `json:"error,omitempty"`
	PriceSnapshotID string     `json:"price_snapshot_id,omitempty"`
}

type Response struct {
	ID                 string            `json:"id"`
	Object             string            `json:"object"`
	CreatedAt          int64             `json:"created_at"`
	CompletedAt        *int64            `json:"completed_at,omitempty"`
	Status             ResponseStatus    `json:"status"`
	Model              string            `json:"model"`
	PreviousResponseID string            `json:"previous_response_id,omitempty"`
	ConversationID     string            `json:"conversation,omitempty"`
	Input              []Item            `json:"input,omitempty"`
	Output             []Item            `json:"output"`
	Usage              Usage             `json:"usage"`
	Error              *Error            `json:"error,omitempty"`
	Metadata           map[string]string `json:"metadata,omitempty"`
	Attempts           []Attempt         `json:"attempts,omitempty"`
	HomeRegion         string            `json:"home_region,omitempty"`
	Revision           int64             `json:"revision"`
}

type Conversation struct {
	ID               string            `json:"id"`
	Object           string            `json:"object"`
	CreatedAt        int64             `json:"created_at"`
	HomeRegion       string            `json:"home_region"`
	Items            []Item            `json:"items"`
	Metadata         map[string]string `json:"metadata,omitempty"`
	Revision         int64             `json:"revision"`
	ActiveResponseID string            `json:"active_response_id,omitempty"`
}

func (r Response) OutputText() string {
	var text string
	for _, item := range r.Output {
		if item.Type != "message" {
			continue
		}
		for _, content := range item.Content {
			if content.Type == "output_text" {
				text += content.Text
			}
		}
	}
	return text
}

type Event struct {
	Sequence      int64           `json:"sequence_number"`
	Type          string          `json:"type"`
	Delta         string          `json:"delta,omitempty"`
	Item          *Item           `json:"item,omitempty"`
	Usage         *Usage          `json:"usage,omitempty"`
	Error         *Error          `json:"error,omitempty"`
	Response      *Response       `json:"response,omitempty"`
	ProviderUsage json.RawMessage `json:"-"`
}

type PriceSnapshot struct {
	ID                          string `json:"id"`
	Provider                    string `json:"provider"`
	Model                       string `json:"model"`
	Region                      string `json:"region"`
	Currency                    string `json:"currency"`
	InputPerMillionMicros       int64  `json:"input_per_million_micros"`
	CachedInputPerMillionMicros int64  `json:"cached_input_per_million_micros"`
	OutputPerMillionMicros      int64  `json:"output_per_million_micros"`
	EffectiveAt                 int64  `json:"effective_at"`
	Source                      string `json:"source"`
}

type UsageRecord struct {
	ID                 string
	TenantID           string
	ResponseID         string
	AttemptID          string
	PriceSnapshot      PriceSnapshot
	ProviderUsage      json.RawMessage
	Usage              Usage
	AmountMicros       int64
	Currency           string
	CacheUsageReliable bool
	CreatedAt          time.Time
}
