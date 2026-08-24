package cacheprotection

import (
	"math"
	"time"

	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/provider"
)

type Policy struct {
	Enabled                bool
	MaxSpendMicros         int64
	MaxRefreshes           int
	MaxProtectionWindow    time.Duration
	SafetyMarginMicros     int64
	AllowContentInspection bool
	ShadowMode             bool
}

type Lease struct {
	ID                 string
	Revision           int64
	Anchor             provider.CacheAnchor
	CreatedAt          time.Time
	EstimatedExpiresAt time.Time
	RefreshCount       int
	SpentMicros        int64
	FencingToken       int64
	CustomerRequestDue bool
	SideEffecting      bool
}

type Forecast struct {
	Probability float64
	ExpectedAt  time.Time
	CostMicros  int64
	Source      string
}

type Economics struct {
	PredictedColdCostMicros        int64
	PredictedHitCostMicros         int64
	RefreshCostMicros              int64
	RouteLockOpportunityCostMicros int64
}

type Candidate struct {
	Policy               Policy
	Lease                Lease
	Forecast             Forecast
	Economics            Economics
	RefreshPriceSnapshot core.PriceSnapshot
	HoldoutCohort        string
	ExperimentRevision   string
}

type Decision struct {
	Eligible                bool
	Reason                  string
	ExpectedNetSavingMicros int64
	ScheduledFor            time.Time
	LeaseRevision           int64
	FencingToken            int64
	Shadow                  bool
}

func Evaluate(now time.Time, candidate Candidate) Decision {
	decision := Decision{
		Reason: "ineligible", LeaseRevision: candidate.Lease.Revision,
		FencingToken: candidate.Lease.FencingToken, Shadow: candidate.Policy.ShadowMode,
	}
	policy := candidate.Policy
	lease := candidate.Lease
	if !policy.Enabled {
		decision.Reason = "policy_disabled"
		return decision
	}
	if policy.MaxSpendMicros <= 0 || policy.MaxRefreshes <= 0 || policy.MaxProtectionWindow <= 0 {
		decision.Reason = "policy_bounds_missing"
		return decision
	}
	if lease.Anchor.TenantID == "" || lease.Anchor.RouteID == "" || lease.Anchor.Provider == "" || lease.Anchor.Model == "" ||
		lease.Anchor.CredentialScope == "" || lease.Anchor.Region == "" || lease.Anchor.CacheKey == "" || lease.Anchor.PrefixHash == "" {
		decision.Reason = "cache_anchor_incomplete"
		return decision
	}
	if lease.Revision <= 0 || lease.FencingToken <= 0 {
		decision.Reason = "lease_fencing_invalid"
		return decision
	}
	if lease.CustomerRequestDue {
		decision.Reason = "customer_request_pending"
		return decision
	}
	if lease.SideEffecting {
		decision.Reason = "side_effecting_capability"
		return decision
	}
	if lease.RefreshCount >= policy.MaxRefreshes {
		decision.Reason = "max_refreshes_reached"
		return decision
	}
	if candidate.Economics.RefreshCostMicros < 0 || lease.SpentMicros+candidate.Economics.RefreshCostMicros > policy.MaxSpendMicros {
		decision.Reason = "max_spend_exceeded"
		return decision
	}
	windowEnds := lease.CreatedAt.Add(policy.MaxProtectionWindow)
	if !now.Before(windowEnds) || candidate.Forecast.ExpectedAt.After(windowEnds) {
		decision.Reason = "protection_window_exceeded"
		return decision
	}
	if !lease.EstimatedExpiresAt.After(now) {
		decision.Reason = "cache_lease_expired"
		return decision
	}
	if candidate.Forecast.Probability < 0 || candidate.Forecast.Probability > 1 {
		decision.Reason = "forecast_probability_invalid"
		return decision
	}
	avoidedColdCost := candidate.Economics.PredictedColdCostMicros - candidate.Economics.PredictedHitCostMicros
	expectedBenefit := int64(math.Round(candidate.Forecast.Probability * float64(avoidedColdCost)))
	decision.ExpectedNetSavingMicros = expectedBenefit - candidate.Economics.RefreshCostMicros - candidate.Forecast.CostMicros - candidate.Economics.RouteLockOpportunityCostMicros
	if decision.ExpectedNetSavingMicros <= policy.SafetyMarginMicros {
		decision.Reason = "roi_below_safety_margin"
		return decision
	}
	decision.Eligible = true
	decision.Reason = "positive_expected_value"
	decision.ScheduledFor = lease.EstimatedExpiresAt.Add(-10 * time.Second)
	if decision.ScheduledFor.Before(now) {
		decision.ScheduledFor = now
	}
	return decision
}
