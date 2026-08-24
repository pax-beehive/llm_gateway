package ledger

import (
	"errors"
	"sync"
	"time"
)

type Measure string

const (
	ObservedCacheDiscount         Measure = "observed_discount"
	EstimatedProtectedSaving      Measure = "estimated_protected_saving"
	ExperimentallyValidatedSaving Measure = "experimentally_validated_saving"
)

type Attribution string

const (
	AttributionObserved    Attribution = "observed"
	AttributionEstimated   Attribution = "estimated"
	AttributionUnavailable Attribution = "unavailable"
	AttributionExperiment  Attribution = "experiment"
)

type PriceSnapshot struct {
	ID                          string
	Provider                    string
	Model                       string
	Region                      string
	Currency                    string
	InputPerMillionMicros       int64
	CachedInputPerMillionMicros int64
	OutputPerMillionMicros      int64
	EffectiveAt                 time.Time
}

type CostComponents struct {
	RefreshMicros   int64
	ForecastMicros  int64
	StorageMicros   int64
	RouteLockMicros int64
}

type Entry struct {
	ID                string
	TenantID          string
	ResponseID        string
	CacheLeaseID      string
	Measure           Measure
	Attribution       Attribution
	PriceSnapshotID   string
	ProviderUsage     []byte
	GrossSavingMicros int64
	Costs             CostComponents
	NetSavingMicros   int64
	Currency          string
	HoldoutCohort     string
	CreatedAt         time.Time
}

func CalculateObservedDiscount(cachedInputTokens int64, reliable bool, price PriceSnapshot) (gross int64, attribution Attribution) {
	if !reliable || cachedInputTokens < 0 || price.InputPerMillionMicros < price.CachedInputPerMillionMicros {
		return 0, AttributionUnavailable
	}
	difference := price.InputPerMillionMicros - price.CachedInputPerMillionMicros
	return multiplyPerMillion(cachedInputTokens, difference), AttributionObserved
}

type ProtectedHitEvidence struct {
	CacheHitVerified       bool
	OriginalLeaseExpiresAt time.Time
	RefreshSucceededAt     time.Time
	RefreshExpiresAt       time.Time
	CustomerRequestAt      time.Time
}

func CalculateProtectedSaving(observedDiscountMicros int64, costs CostComponents, evidence ProtectedHitEvidence) (int64, Attribution) {
	verified := evidence.CacheHitVerified &&
		!evidence.RefreshSucceededAt.IsZero() &&
		evidence.RefreshSucceededAt.Before(evidence.OriginalLeaseExpiresAt) &&
		evidence.CustomerRequestAt.After(evidence.OriginalLeaseExpiresAt) &&
		evidence.CustomerRequestAt.Before(evidence.RefreshExpiresAt)
	if !verified {
		return 0, AttributionUnavailable
	}
	net := observedDiscountMicros - costs.RefreshMicros - costs.ForecastMicros - costs.StorageMicros - costs.RouteLockMicros
	return net, AttributionEstimated
}

type ExperimentCohort struct {
	Responses  int64
	CostMicros int64
}

func CalculateExperimentSaving(treatment, holdout ExperimentCohort) (int64, Attribution) {
	if treatment.Responses <= 0 || holdout.Responses <= 0 {
		return 0, AttributionUnavailable
	}
	holdoutPerResponse := holdout.CostMicros / holdout.Responses
	treatmentPerResponse := treatment.CostMicros / treatment.Responses
	return (holdoutPerResponse - treatmentPerResponse) * treatment.Responses, AttributionExperiment
}

type SavingsStore interface {
	Append(Entry) error
}

type MemorySavingsStore struct {
	mu      sync.Mutex
	entries map[string]Entry
}

func NewMemorySavingsStore() *MemorySavingsStore {
	return &MemorySavingsStore{entries: make(map[string]Entry)}
}

func (s *MemorySavingsStore) Append(entry Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry.ID == "" || entry.TenantID == "" || entry.PriceSnapshotID == "" {
		return errors.New("savings entry identity is incomplete")
	}
	if _, exists := s.entries[entry.ID]; exists {
		return errors.New("savings entry is immutable")
	}
	s.entries[entry.ID] = entry
	return nil
}

func multiplyPerMillion(tokens, pricePerMillionMicros int64) int64 {
	whole := tokens / 1_000_000
	remainder := tokens % 1_000_000
	return whole*pricePerMillionMicros + remainder*pricePerMillionMicros/1_000_000
}
