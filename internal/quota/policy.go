package quota

import (
	"errors"
	"fmt"

	"github.com/toddzheng/llm-gateway/internal/core"
)

var ErrExceeded = errors.New("quota exceeded")

func EffectiveLimits(tenant, apiKey core.QuotaLimits) (core.QuotaLimits, error) {
	if err := validateLimits("Tenant", tenant); err != nil {
		return core.QuotaLimits{}, err
	}
	if err := validateLimits("API key", apiKey); err != nil {
		return core.QuotaLimits{}, err
	}
	if (hasSpendLimit(tenant) || hasSpendLimit(apiKey)) && tenant.Currency != "" && apiKey.Currency != "" && tenant.Currency != apiKey.Currency {
		return core.QuotaLimits{}, errors.New("Tenant and API key spend limits must use the same currency")
	}
	currency := tenant.Currency
	if apiKey.Currency != "" {
		currency = apiKey.Currency
	}
	return core.QuotaLimits{
		MaxInputTokens:            restrictive(tenant.MaxInputTokens, apiKey.MaxInputTokens),
		MaxOutputTokens:           restrictive(tenant.MaxOutputTokens, apiKey.MaxOutputTokens),
		MaxCostMicros:             restrictive(tenant.MaxCostMicros, apiKey.MaxCostMicros),
		RequestsPerMinute:         restrictive(tenant.RequestsPerMinute, apiKey.RequestsPerMinute),
		TokensPerMinute:           restrictive(tenant.TokensPerMinute, apiKey.TokensPerMinute),
		DailySpendMicros:          restrictive(tenant.DailySpendMicros, apiKey.DailySpendMicros),
		MonthlySpendMicros:        restrictive(tenant.MonthlySpendMicros, apiKey.MonthlySpendMicros),
		RefreshDailySpendMicros:   restrictive(tenant.RefreshDailySpendMicros, apiKey.RefreshDailySpendMicros),
		RefreshMonthlySpendMicros: restrictive(tenant.RefreshMonthlySpendMicros, apiKey.RefreshMonthlySpendMicros),
		EmbeddingInputUnits:       restrictive(tenant.EmbeddingInputUnits, apiKey.EmbeddingInputUnits),
		RerankDocuments:           restrictive(tenant.RerankDocuments, apiKey.RerankDocuments),
		CapabilitySpendMicros:     restrictive(tenant.CapabilitySpendMicros, apiKey.CapabilitySpendMicros),
		Currency:                  currency,
	}, nil
}

func validateLimits(scope string, limits core.QuotaLimits) error {
	values := []struct {
		name  string
		value *int64
	}{
		{"max_input_tokens", limits.MaxInputTokens},
		{"max_output_tokens", limits.MaxOutputTokens},
		{"max_cost_micros", limits.MaxCostMicros},
		{"requests_per_minute", limits.RequestsPerMinute},
		{"tokens_per_minute", limits.TokensPerMinute},
		{"daily_spend_micros", limits.DailySpendMicros},
		{"monthly_spend_micros", limits.MonthlySpendMicros},
		{"refresh_daily_spend_micros", limits.RefreshDailySpendMicros},
		{"refresh_monthly_spend_micros", limits.RefreshMonthlySpendMicros},
		{"embedding_input_units", limits.EmbeddingInputUnits},
		{"rerank_documents", limits.RerankDocuments},
		{"capability_spend_micros", limits.CapabilitySpendMicros},
	}
	for _, candidate := range values {
		if candidate.value != nil && *candidate.value < 0 {
			return fmt.Errorf("%s %s cannot be negative", scope, candidate.name)
		}
	}
	if hasSpendLimit(limits) && limits.Currency == "" {
		return fmt.Errorf("%s spend limits require a currency", scope)
	}
	return nil
}

func restrictive(parent, child *int64) *int64 {
	if parent == nil && child == nil {
		return nil
	}
	if parent == nil {
		return cloneLimit(child)
	}
	if child == nil {
		return cloneLimit(parent)
	}
	value := min(*parent, *child)
	return &value
}

func cloneLimit(limit *int64) *int64 {
	if limit == nil {
		return nil
	}
	value := *limit
	return &value
}

func hasSpendLimit(limits core.QuotaLimits) bool {
	return limits.MaxCostMicros != nil || limits.DailySpendMicros != nil || limits.MonthlySpendMicros != nil ||
		limits.RefreshDailySpendMicros != nil || limits.RefreshMonthlySpendMicros != nil || limits.CapabilitySpendMicros != nil
}
