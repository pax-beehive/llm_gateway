package quota

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/toddzheng/llm-gateway/internal/core"
)

type Balance struct {
	Limit     *int64 `json:"limit,omitempty"`
	Reserved  int64  `json:"reserved"`
	Committed int64  `json:"committed"`
	Uncertain int64  `json:"uncertain"`
	Remaining *int64 `json:"remaining,omitempty"`
}

type EnforcementSnapshot struct {
	TenantID             string             `json:"tenant_id"`
	APIKeyID             string             `json:"api_key_id,omitempty"`
	TenantPolicyRevision int64              `json:"tenant_policy_revision"`
	APIKeyPolicyRevision int64              `json:"api_key_policy_revision,omitempty"`
	Currency             string             `json:"currency,omitempty"`
	ObservedAt           time.Time          `json:"observed_at"`
	Balances             map[string]Balance `json:"balances"`
}

func (controller *PostgresController) EnforcementSnapshot(ctx context.Context, tenantID, apiKeyID string, at time.Time) (EnforcementSnapshot, error) {
	if tenantID == "" {
		return EnforcementSnapshot{}, errors.New("quota snapshot requires Tenant identity")
	}
	at = at.UTC()
	if at.IsZero() {
		at = controller.now().UTC()
	}
	var tenantRevision int64
	var tenantPayload []byte
	if err := controller.db.QueryRowContext(ctx, `SELECT policy_revision,policy FROM tenants WHERE id=$1`, tenantID).Scan(&tenantRevision, &tenantPayload); errors.Is(err, sql.ErrNoRows) {
		return EnforcementSnapshot{}, ErrReservationNotFound
	} else if err != nil {
		return EnforcementSnapshot{}, err
	}
	var tenantPolicy core.TenantPolicy
	if err := json.Unmarshal(tenantPayload, &tenantPolicy); err != nil {
		return EnforcementSnapshot{}, err
	}
	tenantPolicy.Revision = tenantRevision
	limits := tenantPolicy.Limits
	var keyRevision int64
	if apiKeyID != "" {
		var keyPayload []byte
		if err := controller.db.QueryRowContext(ctx, `SELECT policy_revision,policy FROM api_keys WHERE tenant_id=$1 AND id=$2`, tenantID, apiKeyID).Scan(&keyRevision, &keyPayload); errors.Is(err, sql.ErrNoRows) {
			return EnforcementSnapshot{}, ErrReservationNotFound
		} else if err != nil {
			return EnforcementSnapshot{}, err
		}
		var keyPolicy core.APIKeyPolicy
		if err := json.Unmarshal(keyPayload, &keyPolicy); err != nil {
			return EnforcementSnapshot{}, err
		}
		var err error
		limits, err = EffectiveLimits(tenantPolicy.Limits, keyPolicy.Limits)
		if err != nil {
			return EnforcementSnapshot{}, err
		}
	}
	result := EnforcementSnapshot{TenantID: tenantID, APIKeyID: apiKeyID, TenantPolicyRevision: tenantRevision, APIKeyPolicyRevision: keyRevision, Currency: limits.Currency, ObservedAt: at, Balances: map[string]Balance{}}
	minute := at.Truncate(time.Minute)
	day := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
	month := time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, time.UTC)
	metrics := []struct {
		name, metric string
		start        time.Time
		limit        *int64
		kind         string
		column       string
		windowColumn string
	}{
		{"requests_per_minute", "requests", minute, limits.RequestsPerMinute, "non_refresh", "committed_requests", "minute_window_start"},
		{"tokens_per_minute", "tokens", minute, limits.TokensPerMinute, "non_refresh", "committed_input_tokens + committed_output_tokens", "minute_window_start"},
		{"daily_spend_micros", financialMetric("spend_micros", limits.Currency), day, limits.DailySpendMicros, "non_refresh", "committed_spend_micros", "day_window_start"},
		{"monthly_spend_micros", financialMetric("spend_micros", limits.Currency), month, limits.MonthlySpendMicros, "non_refresh", "committed_spend_micros", "month_window_start"},
		{"refresh_daily_spend_micros", financialMetric("refresh_spend_micros", limits.Currency), day, limits.RefreshDailySpendMicros, "refresh", "committed_spend_micros", "day_window_start"},
		{"refresh_monthly_spend_micros", financialMetric("refresh_spend_micros", limits.Currency), month, limits.RefreshMonthlySpendMicros, "refresh", "committed_spend_micros", "month_window_start"},
	}
	for _, metric := range metrics {
		balance, err := controller.balance(ctx, tenantID, apiKeyID, metric.metric, metric.start, metric.limit, metric.kind, metric.column, metric.windowColumn)
		if err != nil {
			return EnforcementSnapshot{}, fmt.Errorf("quota snapshot %s: %w", metric.name, err)
		}
		result.Balances[metric.name] = balance
	}
	return result, nil
}

func (controller *PostgresController) balance(ctx context.Context, tenantID, apiKeyID, metric string, start time.Time, limit *int64, kind, column, windowColumn string) (Balance, error) {
	result := Balance{Limit: limit}
	table := "tenant_quota_counters"
	where := "tenant_id=$1 AND metric=$2 AND window_start=$3"
	args := []any{tenantID, metric, start}
	if apiKeyID != "" {
		table = "api_key_quota_counters"
		where = "tenant_id=$1 AND api_key_id=$2 AND metric=$3 AND window_start=$4"
		args = []any{tenantID, apiKeyID, metric, start}
	}
	err := controller.db.QueryRowContext(ctx, `SELECT reserved_amount,committed_amount FROM `+table+` WHERE `+where, args...).Scan(&result.Reserved, &result.Committed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Balance{}, err
	}
	kindPredicate := "kind <> 'cache_refresh'"
	if kind == "refresh" {
		kindPredicate = "kind = 'cache_refresh'"
	}
	keyPredicate := ""
	uncertainArgs := []any{tenantID, start}
	if apiKeyID != "" {
		keyPredicate = " AND api_key_id=$3"
		uncertainArgs = append(uncertainArgs, apiKeyID)
	}
	query := `SELECT COALESCE(sum(` + column + `),0)::bigint FROM quota_reservations WHERE tenant_id=$1 AND ` + windowColumn + `=$2 AND status='uncertain' AND ` + kindPredicate + keyPredicate
	if err := controller.db.QueryRowContext(ctx, query, uncertainArgs...).Scan(&result.Uncertain); err != nil {
		return Balance{}, err
	}
	if result.Committed >= result.Uncertain {
		result.Committed -= result.Uncertain
	}
	if limit != nil {
		remaining := *limit - result.Reserved - result.Committed - result.Uncertain
		if remaining < 0 {
			remaining = 0
		}
		result.Remaining = &remaining
	}
	return result, nil
}
