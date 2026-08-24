package cacheprotection_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/toddzheng/llm-gateway/internal/cacheprotection"
	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/provider"
)

func TestAmbiguousRefreshIsRecordedUncertainAndNeverRetried(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	repository := cacheprotection.NewMemoryIntentRepository()
	coordinator := cacheprotection.NewCoordinator(repository, func() time.Time { return now })
	var refreshCalls atomic.Int64
	protector := cacheProtectorStub{
		inspect: provider.CacheCapability{Supported: true},
		refresh: func(context.Context, provider.CacheAnchor) (provider.RefreshResult, error) {
			refreshCalls.Add(1)
			return provider.RefreshResult{Status: "uncertain"}, errors.New("connection reset after request write")
		},
	}
	candidate := eligibleCandidate(now)

	intent, err := coordinator.Run(context.Background(), candidate, protector)
	if err == nil {
		t.Fatal("run error = nil, want ambiguous provider error")
	}
	if intent.Status != cacheprotection.IntentUncertain {
		t.Fatalf("intent status = %q, want uncertain", intent.Status)
	}

	repeated, repeatedErr := coordinator.Run(context.Background(), candidate, protector)
	if repeatedErr != nil {
		t.Fatalf("repeat returned error: %v", repeatedErr)
	}
	if repeated.ID != intent.ID || repeated.Status != cacheprotection.IntentUncertain {
		t.Fatalf("repeat intent = %#v, want original uncertain intent", repeated)
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("refresh calls = %d, want exactly one", refreshCalls.Load())
	}
}

func TestSuccessfulRefreshHydratesNextLeaseRevisionAndPolicyTotals(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	repository := cacheprotection.NewMemoryIntentRepository()
	protector := cacheProtectorStub{
		inspect: provider.CacheCapability{Supported: true},
		refresh: func(context.Context, provider.CacheAnchor) (provider.RefreshResult, error) {
			return provider.RefreshResult{Status: "succeeded", ExpiresAt: clock.Add(5 * time.Minute)}, nil
		},
	}
	candidate := eligibleCandidate(clock)
	candidate.Policy.MaxRefreshes = 2
	candidate.Policy.MaxSpendMicros = 5_000_000
	coordinator := cacheprotection.NewCoordinator(repository, func() time.Time { return clock })
	first, err := coordinator.Run(context.Background(), candidate, protector)
	if err != nil || first.Status != cacheprotection.IntentSucceeded {
		t.Fatalf("first refresh = %#v, err = %v", first, err)
	}
	clock = clock.Add(4*time.Minute + 55*time.Second)
	second, err := coordinator.Run(context.Background(), candidate, protector)
	if err != nil || second.Status != cacheprotection.IntentSucceeded {
		t.Fatalf("second refresh = %#v, err = %v", second, err)
	}
	if second.CacheLeaseRevision != candidate.Lease.Revision+1 || second.FencingToken != candidate.Lease.FencingToken+1 {
		t.Fatalf("second lease revision/fence = %d/%d", second.CacheLeaseRevision, second.FencingToken)
	}
	limited, err := coordinator.Run(context.Background(), candidate, protector)
	if err != nil || limited.Status != cacheprotection.IntentRejected || limited.Error != "max_refreshes_reached" {
		t.Fatalf("policy-limited third refresh = %#v, err = %v", limited, err)
	}
}

func TestShadowIntentCanBePromotedToTreatmentAtSameLeaseRevision(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	repository := cacheprotection.NewMemoryIntentRepository()
	coordinator := cacheprotection.NewCoordinator(repository, func() time.Time { return now })
	var refreshCalls atomic.Int64
	protector := cacheProtectorStub{
		inspect: provider.CacheCapability{Supported: true},
		refresh: func(context.Context, provider.CacheAnchor) (provider.RefreshResult, error) {
			refreshCalls.Add(1)
			return provider.RefreshResult{Status: "succeeded", ExpiresAt: now.Add(5 * time.Minute)}, nil
		},
	}
	candidate := eligibleCandidate(now)
	candidate.Policy.ShadowMode = true
	shadow, err := coordinator.Run(context.Background(), candidate, protector)
	if err != nil || shadow.Status != cacheprotection.IntentShadow {
		t.Fatalf("shadow intent/error = %#v / %v", shadow, err)
	}
	candidate.Policy.ShadowMode = false
	treatment, err := coordinator.Run(context.Background(), candidate, protector)
	if err != nil || treatment.ID != shadow.ID || treatment.Status != cacheprotection.IntentSucceeded || refreshCalls.Load() != 1 {
		t.Fatalf("promoted treatment/error/calls = %#v / %v / %d", treatment, err, refreshCalls.Load())
	}
}

func eligibleCandidate(now time.Time) cacheprotection.Candidate {
	return cacheprotection.Candidate{
		Policy: cacheprotection.Policy{
			Enabled: true, MaxSpendMicros: 3_000_000, MaxRefreshes: 1,
			MaxProtectionWindow: time.Hour, SafetyMarginMicros: 100_000,
		},
		Lease: cacheprotection.Lease{
			ID: "lease-1", Revision: 7, CreatedAt: now.Add(-4 * time.Minute),
			EstimatedExpiresAt: now.Add(5 * time.Second), FencingToken: 12,
			Anchor: provider.CacheAnchor{
				TenantID: "tenant-a", RouteID: "anthropic-us", Provider: "anthropic", Model: "claude-opus-5",
				CredentialScope: "tenant-a-primary", Region: "us-west", CacheKey: "prefix-v1", PrefixHash: "sha256:abc",
			},
		},
		Forecast: cacheprotection.Forecast{Probability: 0.9, ExpectedAt: now.Add(10 * time.Minute)},
		Economics: cacheprotection.Economics{
			PredictedColdCostMicros: 10_000_000, PredictedHitCostMicros: 1_000_000, RefreshCostMicros: 2_000_000,
		},
		RefreshPriceSnapshot: core.PriceSnapshot{
			ID: "anthropic-price-v1", Provider: "anthropic", Model: "claude-opus-5", Region: "us-west",
			Currency: "USD", InputPerMillionMicros: 10_000_000, CachedInputPerMillionMicros: 1_000_000,
			CacheWritePerMillionMicros: 12_500_000, OutputPerMillionMicros: 20_000_000,
			EffectiveAt: 1, Source: "test",
		},
	}
}

type cacheProtectorStub struct {
	inspect provider.CacheCapability
	refresh func(context.Context, provider.CacheAnchor) (provider.RefreshResult, error)
}

func (s cacheProtectorStub) Inspect(context.Context, provider.CacheAnchor) provider.CacheCapability {
	return s.inspect
}

func (s cacheProtectorStub) Refresh(ctx context.Context, anchor provider.CacheAnchor) (provider.RefreshResult, error) {
	return s.refresh(ctx, anchor)
}
