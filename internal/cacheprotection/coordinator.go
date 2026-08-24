package cacheprotection

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	Reserve(context.Context, Intent) (Intent, bool, error)
	Update(context.Context, Intent, IntentStatus) (Intent, error)
	CancelPending(context.Context, provider.CacheAnchor) (int, error)
	ClaimDue(context.Context, time.Time, int) ([]Intent, error)
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
	if err != nil || !created {
		return reserved, err
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
	running, err := c.repository.Update(ctx, intent, IntentRunning)
	if err != nil {
		return Intent{}, err
	}
	return c.executeClaimed(ctx, running, protector)
}

func (c *Coordinator) executeClaimed(ctx context.Context, running Intent, protector provider.CacheProtector) (Intent, error) {
	result, refreshErr := protector.Refresh(ctx, running.Anchor)
	running.ProviderResult = result
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

func (c *Coordinator) CustomerRequest(ctx context.Context, anchor provider.CacheAnchor) (int, error) {
	return c.repository.CancelPending(ctx, anchor)
}

type MemoryIntentRepository struct {
	mu      sync.Mutex
	intents map[string]Intent
	unique  map[string]string
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
	intent.UpdatedAt = time.Now().UTC()
	r.intents[intent.ID] = intent
	return intent, nil
}

func (r *MemoryIntentRepository) CancelPending(_ context.Context, anchor provider.CacheAnchor) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cancelled := 0
	for id, intent := range r.intents {
		if intent.TenantID != anchor.TenantID || intent.Anchor.RouteID != anchor.RouteID || intent.Anchor.CacheKey != anchor.CacheKey || intent.Anchor.PrefixHash != anchor.PrefixHash {
			continue
		}
		if intent.Status == IntentPlanned {
			intent.Status = IntentCancelled
			intent.UpdatedAt = time.Now().UTC()
			r.intents[id] = intent
			cancelled++
		}
	}
	return cancelled, nil
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
