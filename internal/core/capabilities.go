package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type Capability string

const (
	CapabilityEmbeddings Capability = "embeddings"
	CapabilityModeration Capability = "moderation"
	CapabilityRerank     Capability = "rerank"
)

type CapabilityPrincipal struct {
	TenantID          string
	APIKeyID          string
	HomeRegion        string
	ExecutionEpoch    int64
	CompatibilityMode CompatibilityMode
	TenantPolicy      *TenantPolicy
	APIKeyPolicy      *APIKeyPolicy
}

type EmbeddingInput struct {
	Text   *string
	Tokens []int64
}

type EmbeddingRequest struct {
	CapabilityPrincipal
	Model          string
	Input          []EmbeddingInput
	EncodingFormat string
	Dimensions     *int
	EndUserID      string
}

type EmbeddingData struct {
	Index     int
	Embedding []float64
	Base64    string
}

type EmbeddingResult struct {
	Model         string
	Data          []EmbeddingData
	InputUnits    int64
	Dimensions    int64
	ProviderUsage json.RawMessage
}

type ModerationRequest struct {
	CapabilityPrincipal
	Model     string
	Input     []string
	EndUserID string
}

type ModerationResultItem struct {
	Flagged                   bool
	Categories                map[string]bool
	CategoryScores            map[string]float64
	CategoryAppliedInputTypes map[string][]string
}

type ModerationResult struct {
	ID            string
	Model         string
	Results       []ModerationResultItem
	InputUnits    int64
	ProviderUsage json.RawMessage
}

type RerankDocument struct {
	Text string
}

type RerankRequest struct {
	CapabilityPrincipal
	Model           string
	Query           string
	Documents       []RerankDocument
	TopN            *int
	ReturnDocuments bool
}

type RerankResultItem struct {
	Index          int
	RelevanceScore float64
	Document       *RerankDocument
}

type RerankResult struct {
	ID             string
	Model          string
	Results        []RerankResultItem
	Documents      int64
	ProviderTokens int64
	ProviderUsage  json.RawMessage
}

type CapabilityUsageRecord struct {
	ID                 string
	TenantID           string
	APIKeyID           string
	OperationID        string
	HomeRegion         string
	ExecutionEpoch     int64
	QuotaReservationID string
	Capability         Capability
	RouteID            string
	Provider           string
	Model              string
	PublicModel        string
	PriceSnapshot      PriceSnapshot
	ProviderUsage      json.RawMessage
	InputUnits         int64
	Dimensions         int64
	Documents          int64
	AmountMicros       int64
	Currency           string
	CreatedAt          time.Time
}

func ValidateCapabilityProviderUsage(raw json.RawMessage) error {
	if len(raw) == 0 || len(raw) > 64<<10 || !json.Valid(raw) {
		return errors.New("capability Provider usage must be a valid bounded JSON object")
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return errors.New("capability Provider usage must be a JSON object")
	}
	return validateUsageValue(object, 0)
}

func validateUsageValue(value any, depth int) error {
	if depth > 8 {
		return errors.New("capability Provider usage nesting is too deep")
	}
	switch typed := value.(type) {
	case nil, bool, float64:
		return nil
	case string:
		return errors.New("capability Provider usage cannot contain string content")
	case []any:
		for _, item := range typed {
			if err := validateUsageValue(item, depth+1); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		for key, item := range typed {
			if !validUsageKey(key) {
				return fmt.Errorf("capability Provider usage key %q is invalid", key)
			}
			if err := validateUsageValue(item, depth+1); err != nil {
				return err
			}
		}
		return nil
	default:
		return errors.New("capability Provider usage contains an unsupported value")
	}
}

func validUsageKey(key string) bool {
	if key == "" || len(key) > 128 {
		return false
	}
	for _, character := range key {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '_' || character == '-' || character == '.' {
			continue
		}
		return false
	}
	return true
}
