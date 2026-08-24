package cacheprotection_test

import (
	"testing"
	"time"

	"github.com/toddzheng/llm-gateway/internal/cacheprotection"
	"github.com/toddzheng/llm-gateway/internal/provider"
)

func TestEvaluateSchedulesOnlyPositiveBoundedExactRefresh(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	candidate := cacheprotection.Candidate{
		Policy: cacheprotection.Policy{
			Enabled: true, MaxSpendMicros: 3_000_000, MaxRefreshes: 1,
			MaxProtectionWindow: time.Hour, SafetyMarginMicros: 500_000,
		},
		Lease: cacheprotection.Lease{
			ID: "lease-1", Revision: 7, CreatedAt: now.Add(-4 * time.Minute),
			EstimatedExpiresAt: now.Add(time.Minute), FencingToken: 12,
			Anchor: provider.CacheAnchor{
				TenantID: "tenant-a", RouteID: "anthropic-us", Provider: "anthropic", Model: "claude-opus-5",
				CredentialScope: "tenant-a-primary", Region: "us-west", CacheKey: "prefix-v1", PrefixHash: "sha256:abc",
			},
		},
		Forecast: cacheprotection.Forecast{Probability: 0.8, ExpectedAt: now.Add(4 * time.Minute), CostMicros: 100_000},
		Economics: cacheprotection.Economics{
			PredictedColdCostMicros: 10_000_000, PredictedHitCostMicros: 1_000_000,
			RefreshCostMicros: 2_000_000, RouteLockOpportunityCostMicros: 200_000,
		},
	}

	decision := cacheprotection.Evaluate(now, candidate)
	if !decision.Eligible {
		t.Fatalf("eligible = false, reason = %s", decision.Reason)
	}
	if decision.ExpectedNetSavingMicros != 4_900_000 {
		t.Fatalf("expected net saving = %d, want 4900000", decision.ExpectedNetSavingMicros)
	}
	if decision.LeaseRevision != 7 || decision.FencingToken != 12 {
		t.Fatalf("decision lost lease fencing identity: %#v", decision)
	}

	candidate.Lease.RefreshCount = 1
	limited := cacheprotection.Evaluate(now, candidate)
	if limited.Eligible || limited.Reason != "max_refreshes_reached" {
		t.Fatalf("refresh-limited decision = %#v", limited)
	}
}
