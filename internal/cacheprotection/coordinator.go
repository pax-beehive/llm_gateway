package cacheprotection

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/toddzheng/llm-gateway/internal/provider"
)

type IntentStatus string

const (
	IntentPlanned   IntentStatus = "planned"
	IntentRunning   IntentStatus = "running"
	IntentSucceeded IntentStatus = "succeeded"
	IntentRejected  IntentStatus = "rejected"
	IntentUncertain IntentStatus = "uncertain"
	IntentCancelled IntentStatus = "cancelled"
	IntentShadow    IntentStatus = "shadow"
)

type Intent struct {
	ID                      string
	TenantID                string
	CacheLeaseID            string
	CacheLeaseRevision      int64
	FencingToken            int64
	Anchor                  provider.CacheAnchor
	Status                  IntentStatus
	ExpectedNetSavingMicros int64
	ScheduledFor            time.Time
	ProviderResult          provider.RefreshResult
	Error                   string
	CreatedAt               time.Time
	UpdatedAt               time.Time
	Candidate               Candidate
}

type IntentRepository interface {
	CurrentLease(context.Context, string, string) (Lease, bool, error)
	Reserve(context.Context, Intent) (Intent, bool, error)
	Update(context.Context, Intent, IntentStatus) (Intent, error)
	CustomerRequest(context.Context, provider.CacheAnchor, time.Time) (CustomerRequestResult, error)
	ClaimDue(context.Context, time.Time, int) ([]Intent, error)
}

type ProtectedHitCandidate struct {
	CacheLeaseID           string
	OriginalLeaseExpiresAt time.Time
	RefreshSucceededAt     time.Time
	RefreshExpiresAt       time.Time
	RefreshCostMicros      int64
	RefreshUsageID         string
	RefreshProviderUsage   json.RawMessage
	HoldoutCohort          string
	ForecastCostMicros     int64
	StorageCostMicros      int64
	RouteLockCostMicros    int64
}

type CustomerRequestResult struct {
	Cancelled             int
	ProtectedHitCandidate *ProtectedHitCandidate
}

type Coordinator struct {
	repository IntentRepository
	now        func() time.Time
}

func NewCoordinator(repository IntentRepository, now func() time.Time) *Coordinator {
	if now == nil {
		now = time.Now
	}
	return &Coordinator{repository: repository, now: now}
}

func (c *Coordinator) Run(ctx context.Context, candidate Candidate, protector provider.CacheProtector) (Intent, error) {
	now := c.now().UTC()
	current, exists, err := c.repository.CurrentLease(ctx, candidate.Lease.Anchor.TenantID, candidate.Lease.ID)
	if err != nil {
		return Intent{}, err
	}
	if exists {
		candidate.Lease.Revision = current.Revision
		candidate.Lease.CreatedAt = current.CreatedAt
		candidate.Lease.EstimatedExpiresAt = current.EstimatedExpiresAt
		candidate.Lease.RefreshCount = current.RefreshCount
		candidate.Lease.SpentMicros = current.SpentMicros
		candidate.Lease.FencingToken = current.FencingToken
	}
	decision := Evaluate(now, candidate)
	if !decision.Eligible {
		return Intent{
			TenantID: candidate.Lease.Anchor.TenantID, CacheLeaseID: candidate.Lease.ID,
			CacheLeaseRevision: candidate.Lease.Revision, FencingToken: candidate.Lease.FencingToken,
			Anchor: candidate.Lease.Anchor, Status: IntentRejected, Error: decision.Reason,
			ExpectedNetSavingMicros: decision.ExpectedNetSavingMicros, CreatedAt: now, UpdatedAt: now,
		}, nil
	}
	if protector == nil {
		return Intent{}, errors.New("cache protector is required")
	}
	capability := protector.Inspect(ctx, candidate.Lease.Anchor)
	if !capability.Supported {
		return Intent{
			TenantID: candidate.Lease.Anchor.TenantID, CacheLeaseID: candidate.Lease.ID,
			CacheLeaseRevision: candidate.Lease.Revision, FencingToken: candidate.Lease.FencingToken,
			Anchor: candidate.Lease.Anchor, Status: IntentRejected, Error: capability.Reason,
			ExpectedNetSavingMicros: decision.ExpectedNetSavingMicros, CreatedAt: now, UpdatedAt: now,
		}, nil
	}
	intent := Intent{
		ID: newIntentID(), TenantID: candidate.Lease.Anchor.TenantID, CacheLeaseID: candidate.Lease.ID,
		CacheLeaseRevision: candidate.Lease.Revision, FencingToken: candidate.Lease.FencingToken,
		Anchor: candidate.Lease.Anchor, Status: IntentPlanned,
		ExpectedNetSavingMicros: decision.ExpectedNetSavingMicros, ScheduledFor: decision.ScheduledFor,
		CreatedAt: now, UpdatedAt: now,
		Candidate: candidate,
	}
	reserved, created, err := c.repository.Reserve(ctx, intent)
	if err != nil {
		return reserved, err
	}
	if !created {
		if reserved.Status != IntentShadow || decision.Shadow {
			return reserved, nil
		}
		reserved.Candidate = candidate
		reserved.ExpectedNetSavingMicros = decision.ExpectedNetSavingMicros
		reserved.ScheduledFor = decision.ScheduledFor
		reserved.UpdatedAt = now
		reserved, err = c.repository.Update(ctx, reserved, IntentPlanned)
		if err != nil {
			return Intent{}, err
		}
	}
	if decision.Shadow {
		return c.repository.Update(ctx, reserved, IntentShadow)
	}
	if reserved.ScheduledFor.After(now) {
		return reserved, nil
	}
	return c.execute(ctx, reserved, protector)
}

type ProtectorResolver func(provider.CacheAnchor) provider.CacheProtector

func (c *Coordinator) RunDue(ctx context.Context, limit int, resolve ProtectorResolver) ([]Intent, error) {
	if limit <= 0 || resolve == nil {
		return nil, errors.New("positive worker limit and protector resolver are required")
	}
	claimed, err := c.repository.ClaimDue(ctx, c.now().UTC(), limit)
	if err != nil {
		return nil, err
	}
	results := make([]Intent, 0, len(claimed))
	for _, intent := range claimed {
		decision := Evaluate(c.now().UTC(), intent.Candidate)
		if !decision.Eligible {
			intent.Error = "worker revalidation: " + decision.Reason
			updated, updateErr := c.repository.Update(ctx, intent, IntentRejected)
			if updateErr != nil {
				return results, updateErr
			}
			results = append(results, updated)
			continue
		}
		protector := resolve(intent.Anchor)
		if protector == nil {
			intent.Error = "cache protector disabled by current route configuration"
			updated, updateErr := c.repository.Update(ctx, intent, IntentRejected)
			if updateErr != nil {
				return results, updateErr
			}
			results = append(results, updated)
			continue
		}
		updated, executeErr := c.executeClaimed(ctx, intent, protector)
		results = append(results, updated)
		if executeErr != nil {
			// The terminal uncertain/rejected intent is durable. Continue other independent work.
			continue
		}
	}
	return results, nil
}

func (c *Coordinator) execute(ctx context.Context, intent Intent, protector provider.CacheProtector) (Intent, error) {
	intent.UpdatedAt = c.now().UTC()
	running, err := c.repository.Update(ctx, intent, IntentRunning)
	if err != nil {
		return Intent{}, err
	}
	return c.executeClaimed(ctx, running, protector)
}

func (c *Coordinator) executeClaimed(ctx context.Context, running Intent, protector provider.CacheProtector) (Intent, error) {
	result, refreshErr := protector.Refresh(ctx, running.Anchor)
	running.ProviderResult = result
	running.UpdatedAt = c.now().UTC()
	if refreshErr != nil {
		running.Error = refreshErr.Error()
		status := IntentUncertain
		if result.Status == "rejected" {
			status = IntentRejected
		}
		updated, updateErr := c.repository.Update(context.WithoutCancel(ctx), running, status)
		return updated, errors.Join(refreshErr, updateErr)
	}
	return c.repository.Update(ctx, running, IntentSucceeded)
}

func (c *Coordinator) CustomerRequest(ctx context.Context, anchor provider.CacheAnchor) (CustomerRequestResult, error) {
	return c.repository.CustomerRequest(ctx, anchor, c.now().UTC())
}

type MemoryIntentRepository struct {
	mu      sync.Mutex
	intents map[string]Intent
	unique  map[string]string
}

func (r *MemoryIntentRepository) CurrentLease(_ context.Context, tenantID, leaseID string) (Lease, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var current Lease
	found := false
	for _, intent := range r.intents {
		if intent.TenantID != tenantID || intent.CacheLeaseID != leaseID {
			continue
		}
		lease := intent.Candidate.Lease
		if intent.Status == IntentSucceeded {
			lease.Revision++
			lease.FencingToken++
			lease.RefreshCount++
			lease.SpentMicros += actualRefreshCost(intent)
			lease.EstimatedExpiresAt = intent.ProviderResult.ExpiresAt
		}
		if !found || lease.Revision > current.Revision ||
			(lease.Revision == current.Revision && lease.EstimatedExpiresAt.After(current.EstimatedExpiresAt)) {
			current, found = lease, true
		}
	}
	return current, found, nil
}

func NewMemoryIntentRepository() *MemoryIntentRepository {
	return &MemoryIntentRepository{intents: make(map[string]Intent), unique: make(map[string]string)}
}

func (r *MemoryIntentRepository) Reserve(_ context.Context, intent Intent) (Intent, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := intent.TenantID + "\x00" + intent.CacheLeaseID + "\x00" + stringInt64(intent.CacheLeaseRevision)
	if existingID := r.unique[key]; existingID != "" {
		return r.intents[existingID], false, nil
	}
	r.unique[key] = intent.ID
	r.intents[intent.ID] = intent
	return intent, true, nil
}

func (r *MemoryIntentRepository) Update(_ context.Context, intent Intent, status IntentStatus) (Intent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	current, exists := r.intents[intent.ID]
	if !exists {
		return Intent{}, errors.New("cache refresh intent not found")
	}
	if current.FencingToken != intent.FencingToken || current.CacheLeaseRevision != intent.CacheLeaseRevision {
		return Intent{}, errors.New("cache refresh intent fencing conflict")
	}
	intent.Status = status
	if intent.UpdatedAt.IsZero() {
		intent.UpdatedAt = time.Now().UTC()
	}
	r.intents[intent.ID] = intent
	return intent, nil
}

func (r *MemoryIntentRepository) CustomerRequest(_ context.Context, anchor provider.CacheAnchor, requestedAt time.Time) (CustomerRequestResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := CustomerRequestResult{}
	for id, intent := range r.intents {
		if intent.TenantID != anchor.TenantID || intent.Anchor.RouteID != anchor.RouteID || intent.Anchor.CacheKey != anchor.CacheKey || intent.Anchor.PrefixHash != anchor.PrefixHash {
			continue
		}
		if intent.Status == IntentPlanned {
			intent.Status = IntentCancelled
			intent.UpdatedAt = requestedAt
			r.intents[id] = intent
			result.Cancelled++
		}
		if intent.Status == IntentSucceeded &&
			intent.UpdatedAt.Before(intent.Candidate.Lease.EstimatedExpiresAt) &&
			requestedAt.After(intent.Candidate.Lease.EstimatedExpiresAt) &&
			requestedAt.Before(intent.ProviderResult.ExpiresAt) {
			result.ProtectedHitCandidate = &ProtectedHitCandidate{
				CacheLeaseID: intent.CacheLeaseID, OriginalLeaseExpiresAt: intent.Candidate.Lease.EstimatedExpiresAt,
				RefreshSucceededAt: intent.UpdatedAt, RefreshExpiresAt: intent.ProviderResult.ExpiresAt,
				RefreshCostMicros:    actualRefreshCost(intent),
				RefreshUsageID:       intent.ID + "_usage",
				RefreshProviderUsage: append(json.RawMessage(nil), intent.ProviderResult.ProviderUsage...),
				HoldoutCohort:        intent.Candidate.HoldoutCohort,
				ForecastCostMicros:   intent.Candidate.Forecast.CostMicros,
				RouteLockCostMicros:  intent.Candidate.Economics.RouteLockOpportunityCostMicros,
			}
		}
	}
	return result, nil
}

func actualRefreshCost(intent Intent) int64 {
	usage := intent.ProviderResult.Usage
	if usage.InputTokens == 0 && usage.OutputTokens == 0 && usage.CachedInputTokens == 0 && usage.CacheWriteInputTokens == 0 {
		return intent.Candidate.Economics.RefreshCostMicros
	}
	snapshot := intent.Candidate.RefreshPriceSnapshot
	cached := min(max(usage.CachedInputTokens, 0), max(usage.InputTokens, 0))
	cacheWrite := min(max(usage.CacheWriteInputTokens, 0), max(usage.InputTokens-cached, 0))
	uncached := max(usage.InputTokens-cached-cacheWrite, 0)
	return refreshPerMillion(uncached, snapshot.InputPerMillionMicros) +
		refreshPerMillion(cached, snapshot.CachedInputPerMillionMicros) +
		refreshPerMillion(cacheWrite, snapshot.CacheWritePerMillionMicros) +
		refreshPerMillion(max(usage.OutputTokens, 0), snapshot.OutputPerMillionMicros)
}

func refreshPerMillion(tokens, rate int64) int64 {
	return (tokens/1_000_000)*rate + (tokens%1_000_000)*rate/1_000_000
}

func (r *MemoryIntentRepository) ClaimDue(_ context.Context, now time.Time, limit int) ([]Intent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	claimed := make([]Intent, 0, limit)
	for id, intent := range r.intents {
		if len(claimed) >= limit {
			break
		}
		if intent.Status != IntentPlanned || intent.ScheduledFor.After(now) {
			continue
		}
		intent.Status = IntentRunning
		intent.UpdatedAt = now
		r.intents[id] = intent
		claimed = append(claimed, intent)
	}
	return claimed, nil
}

func newIntentID() string {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic(err)
	}
	return "refresh_" + hex.EncodeToString(value[:])
}

func stringInt64(value int64) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var buffer [20]byte
	index := len(buffer)
	for value > 0 {
		index--
		buffer[index] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		index--
		buffer[index] = '-'
	}
	return string(buffer[index:])
}
