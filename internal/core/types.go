package core

import (
	"encoding/json"
	"fmt"
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
	Type        string          `json:"type"`
	Text        string          `json:"text,omitempty"`
	Annotations json.RawMessage `json:"annotations,omitempty"`
	ImageURL    string          `json:"image_url,omitempty"`
	Detail      string          `json:"detail,omitempty"`
	FileID      string          `json:"file_id,omitempty"`
	FileData    string          `json:"file_data,omitempty"`
	Filename    string          `json:"filename,omitempty"`
}

type Item struct {
	ID               string          `json:"id,omitempty"`
	Type             string          `json:"type"`
	Status           string          `json:"status,omitempty"`
	Role             string          `json:"role,omitempty"`
	Content          []Content       `json:"content,omitempty"`
	CallID           string          `json:"call_id,omitempty"`
	Name             string          `json:"name,omitempty"`
	Arguments        json.RawMessage `json:"arguments,omitempty"`
	Output           string          `json:"output,omitempty"`
	Summary          []Content       `json:"summary,omitempty"`
	EncryptedContent string          `json:"encrypted_content,omitempty"`
	ProviderMetadata json.RawMessage `json:"provider_metadata,omitempty"`
}

func (i Item) MarshalJSON() ([]byte, error) {
	type itemAlias Item
	payload := struct {
		itemAlias
		Arguments any `json:"arguments,omitempty"`
		Content   any `json:"content,omitempty"`
	}{itemAlias: itemAlias(i)}
	payload.itemAlias.Arguments = nil
	payload.itemAlias.Content = nil
	if len(i.Arguments) > 0 {
		payload.Arguments = string(i.Arguments)
	}
	if i.Type == "message" {
		if i.Content == nil {
			payload.Content = []Content{}
		} else {
			payload.Content = i.Content
		}
	}
	return json.Marshal(payload)
}

func (i *Item) UnmarshalJSON(data []byte) error {
	type itemAlias Item
	payload := struct {
		itemAlias
		Arguments json.RawMessage `json:"arguments"`
	}{}
	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}
	*i = Item(payload.itemAlias)
	if len(payload.Arguments) == 0 || string(payload.Arguments) == "null" {
		return nil
	}
	var encoded string
	if err := json.Unmarshal(payload.Arguments, &encoded); err == nil {
		if !json.Valid([]byte(encoded)) {
			return fmt.Errorf("function-call arguments are not valid JSON")
		}
		i.Arguments = json.RawMessage(encoded)
		return nil
	}
	if !json.Valid(payload.Arguments) {
		return fmt.Errorf("function-call arguments are not valid JSON")
	}
	i.Arguments = append(json.RawMessage(nil), payload.Arguments...)
	return nil
}

type Usage struct {
	InputTokens           int64 `json:"input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	TotalTokens           int64 `json:"total_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens,omitempty"`
	CacheWriteInputTokens int64 `json:"cache_write_input_tokens,omitempty"`
}

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
	Param   string `json:"param,omitempty"`
}

type Request struct {
	TenantID           string
	APIKeyID           string
	TenantPolicy       *TenantPolicy
	APIKeyPolicy       *APIKeyPolicy
	Model              string
	Input              []Item
	Stream             bool
	Background         bool
	Store              bool
	PreviousResponseID string
	ConversationID     string
	HomeRegion         string
	ExecutionEpoch     int64
	CompatibilityMode  CompatibilityMode
	RequestedFeatures  []string
	Metadata           map[string]string
	Tools              []json.RawMessage
	ToolChoice         json.RawMessage
	ParallelToolCalls  *bool
	Reasoning          json.RawMessage
	Include            []string
	PromptCacheKey     string
	ClientMetadata     json.RawMessage
	Text               json.RawMessage
	ServiceTier        string
	Truncation         string
	MaxToolCalls       *int
	SafetyIdentifier   string
	Temperature        *float64
	TopP               *float64
	MaxOutputTokens    *int
	Stop               json.RawMessage
	EndUserID          string
	PreferredRouteID   string
	IdempotencyKey     string
	RequestHash        []byte
	ContextItemCount   int
	ExperimentIdentity string
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

type TenantPolicy struct {
	Revision               int64       `json:"revision,omitempty"`
	MaxConcurrentResponses int         `json:"max_concurrent_responses,omitempty"`
	MaxInputItems          int         `json:"max_input_items,omitempty"`
	AllowStoredResponses   *bool       `json:"allow_stored_responses,omitempty"`
	AllowCacheProtection   *bool       `json:"allow_cache_protection,omitempty"`
	AllowContentInspection *bool       `json:"allow_content_inspection,omitempty"`
	RetentionSeconds       int64       `json:"retention_seconds,omitempty"`
	Limits                 QuotaLimits `json:"limits,omitempty"`
}

type QuotaLimits struct {
	MaxInputTokens            *int64 `json:"max_input_tokens,omitempty"`
	MaxOutputTokens           *int64 `json:"max_output_tokens,omitempty"`
	MaxCostMicros             *int64 `json:"max_cost_micros,omitempty"`
	RequestsPerMinute         *int64 `json:"requests_per_minute,omitempty"`
	TokensPerMinute           *int64 `json:"tokens_per_minute,omitempty"`
	DailySpendMicros          *int64 `json:"daily_spend_micros,omitempty"`
	MonthlySpendMicros        *int64 `json:"monthly_spend_micros,omitempty"`
	RefreshDailySpendMicros   *int64 `json:"refresh_daily_spend_micros,omitempty"`
	RefreshMonthlySpendMicros *int64 `json:"refresh_monthly_spend_micros,omitempty"`
	EmbeddingInputUnits       *int64 `json:"embedding_input_units,omitempty"`
	RerankDocuments           *int64 `json:"rerank_documents,omitempty"`
	CapabilitySpendMicros     *int64 `json:"capability_spend_micros,omitempty"`
	Currency                  string `json:"currency,omitempty"`
}

type APIKeyPolicy struct {
	Revision               int64       `json:"revision,omitempty"`
	AllowCacheProtection   bool        `json:"allow_cache_protection,omitempty"`
	AllowContentInspection bool        `json:"allow_content_inspection,omitempty"`
	AllowedPublicModels    *[]string   `json:"allowed_public_models"`
	AllowedOperations      *[]string   `json:"allowed_operations"`
	AllowedCIDRs           *[]string   `json:"allowed_cidrs"`
	AllowedRegions         *[]string   `json:"allowed_regions"`
	MaxConcurrentResponses *int        `json:"max_concurrent_responses"`
	Limits                 QuotaLimits `json:"limits,omitempty"`
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
	ExecutionEpoch     int64             `json:"execution_epoch,omitempty"`
	Revision           int64             `json:"revision"`
	RetainContent      bool              `json:"-"`
	ContentExpiresAt   *int64            `json:"content_expires_at,omitempty"`
	ContentExpiredAt   *int64            `json:"content_expired_at,omitempty"`
}

type Conversation struct {
	ID               string            `json:"id"`
	Object           string            `json:"object"`
	CreatedAt        int64             `json:"created_at"`
	HomeRegion       string            `json:"home_region"`
	ExecutionEpoch   int64             `json:"execution_epoch"`
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
	ItemID        string          `json:"item_id,omitempty"`
	OutputIndex   *int            `json:"output_index,omitempty"`
	ContentIndex  *int            `json:"content_index,omitempty"`
	Logprobs      json.RawMessage `json:"logprobs,omitempty"`
	Part          *Content        `json:"part,omitempty"`
	Item          *Item           `json:"item,omitempty"`
	Usage         *Usage          `json:"usage,omitempty"`
	Error         *Error          `json:"error,omitempty"`
	Response      *Response       `json:"response,omitempty"`
	ProviderUsage json.RawMessage `json:"-"`
}

type PriceSnapshot struct {
	ID                              string `json:"id"`
	Provider                        string `json:"provider"`
	Model                           string `json:"model"`
	Region                          string `json:"region"`
	Currency                        string `json:"currency"`
	InputPerMillionMicros           int64  `json:"input_per_million_micros"`
	CachedInputPerMillionMicros     int64  `json:"cached_input_per_million_micros"`
	CacheWritePerMillionMicros      int64  `json:"cache_write_per_million_micros"`
	OutputPerMillionMicros          int64  `json:"output_per_million_micros"`
	EmbeddingInputPerMillionMicros  int64  `json:"embedding_input_per_million_micros,omitempty"`
	ModerationInputPerMillionMicros int64  `json:"moderation_input_per_million_micros,omitempty"`
	RerankDocumentPerThousandMicros int64  `json:"rerank_document_per_thousand_micros,omitempty"`
	EffectiveAt                     int64  `json:"effective_at"`
	Source                          string `json:"source"`
}

type UsageRecord struct {
	ID                 string
	TenantID           string
	APIKeyID           string
	QuotaReservationID string
	ResponseID         string
	AttemptID          string
	PriceSnapshot      PriceSnapshot
	ProviderUsage      json.RawMessage
	Usage              Usage
	AmountMicros       int64
	Currency           string
	CacheUsageReliable bool
	HoldoutCohort      string
	ExperimentRevision string
	ProtectedHit       *ProtectedHitEvidence
	CreatedAt          time.Time
}

type ProtectedHitEvidence struct {
	CacheLeaseID           string
	OriginalLeaseExpiresAt time.Time
	RefreshSucceededAt     time.Time
	RefreshExpiresAt       time.Time
	CustomerRequestAt      time.Time
	RefreshCostMicros      int64
	RefreshUsageID         string
	RefreshProviderUsage   json.RawMessage
	HoldoutCohort          string
	ForecastCostMicros     int64
	StorageCostMicros      int64
	RouteLockCostMicros    int64
}
