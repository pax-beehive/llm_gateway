package quota_test

import (
	"testing"

	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/quota"
)

func TestEffectiveLimitsUseTheMostRestrictiveTenantAndAPIKeyValues(t *testing.T) {
	tenant := core.QuotaLimits{
		RequestsPerMinute:  limit(100),
		TokensPerMinute:    limit(1_000_000),
		MonthlySpendMicros: limit(50_000_000),
		Currency:           "USD",
	}
	key := core.QuotaLimits{
		RequestsPerMinute:  limit(20),
		TokensPerMinute:    nil,
		MonthlySpendMicros: limit(5_000_000),
		Currency:           "USD",
	}

	effective, err := quota.EffectiveLimits(tenant, key)
	if err != nil {
		t.Fatal(err)
	}
	if value(effective.RequestsPerMinute) != 20 || value(effective.TokensPerMinute) != 1_000_000 ||
		value(effective.MonthlySpendMicros) != 5_000_000 {
		t.Fatalf("effective limits = %#v", effective)
	}
}

func TestExplicitZeroAPIKeyLimitDeniesRatherThanInherits(t *testing.T) {
	effective, err := quota.EffectiveLimits(
		core.QuotaLimits{RequestsPerMinute: limit(100), Currency: "USD"},
		core.QuotaLimits{RequestsPerMinute: limit(0), Currency: "USD"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if effective.RequestsPerMinute == nil || *effective.RequestsPerMinute != 0 {
		t.Fatalf("effective request limit = %#v, want explicit zero", effective.RequestsPerMinute)
	}
}

func TestSpendLimitsRejectDifferentCurrencies(t *testing.T) {
	_, err := quota.EffectiveLimits(
		core.QuotaLimits{MonthlySpendMicros: limit(100), Currency: "USD"},
		core.QuotaLimits{MonthlySpendMicros: limit(50), Currency: "CNY"},
	)
	if err == nil {
		t.Fatal("mixed-currency spend limits were accepted")
	}
}

func TestInheritedSpendLimitRejectsAnUnrelatedKeyCurrency(t *testing.T) {
	_, err := quota.EffectiveLimits(
		core.QuotaLimits{MonthlySpendMicros: limit(100), Currency: "USD"},
		core.QuotaLimits{RequestsPerMinute: limit(5), Currency: "CNY"},
	)
	if err == nil {
		t.Fatal("inherited spend limit accepted a conflicting API key currency")
	}
}

func limit(value int64) *int64 { return &value }

func value(limit *int64) int64 {
	if limit == nil {
		return -1
	}
	return *limit
}
