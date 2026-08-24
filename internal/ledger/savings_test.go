package ledger_test

import (
	"testing"
	"time"

	"github.com/toddzheng/llm-gateway/internal/ledger"
)

func TestProtectionSavingRequiresAHitBeyondOriginalExpiry(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	price := ledger.PriceSnapshot{InputPerMillionMicros: 10_000_000, CachedInputPerMillionMicros: 1_000_000}
	discount, attribution := ledger.CalculateObservedDiscount(2_000_000, true, price)
	if discount != 18_000_000 || attribution != ledger.AttributionObserved {
		t.Fatalf("observed discount = %d/%s, want 18000000/observed", discount, attribution)
	}

	net, attribution := ledger.CalculateProtectedSaving(discount, ledger.CostComponents{
		RefreshMicros: 2_000_000, ForecastMicros: 100_000, RouteLockMicros: 200_000,
	}, ledger.ProtectedHitEvidence{
		CacheHitVerified: true, OriginalLeaseExpiresAt: now,
		RefreshSucceededAt: now.Add(-time.Minute), RefreshExpiresAt: now.Add(5 * time.Minute),
		CustomerRequestAt: now.Add(time.Minute),
	})
	if net != 15_700_000 || attribution != ledger.AttributionEstimated {
		t.Fatalf("protected saving = %d/%s, want 15700000/estimated", net, attribution)
	}

	beforeOriginalExpiry, attribution := ledger.CalculateProtectedSaving(discount, ledger.CostComponents{}, ledger.ProtectedHitEvidence{
		CacheHitVerified: true, OriginalLeaseExpiresAt: now,
		RefreshSucceededAt: now.Add(-time.Minute), RefreshExpiresAt: now.Add(5 * time.Minute),
		CustomerRequestAt: now.Add(-time.Second),
	})
	if beforeOriginalExpiry != 0 || attribution != ledger.AttributionUnavailable {
		t.Fatalf("ordinary cache hit was over-attributed: %d/%s", beforeOriginalExpiry, attribution)
	}
}

func TestExperimentSavingComparesTreatmentAgainstHoldoutPerResponse(t *testing.T) {
	t.Parallel()
	saving, attribution := ledger.CalculateExperimentSaving(
		ledger.ExperimentCohort{Responses: 100, CostMicros: 400_000},
		ledger.ExperimentCohort{Responses: 50, CostMicros: 300_000},
	)
	if saving != 200_000 || attribution != ledger.AttributionExperiment {
		t.Fatalf("experiment saving = %d/%s, want 200000/experiment", saving, attribution)
	}
}
