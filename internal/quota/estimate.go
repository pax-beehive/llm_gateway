package quota

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/bits"

	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/provider"
)

type Estimate struct {
	InputTokens  int64
	OutputTokens int64
	SpendMicros  int64
	Currency     string
}

func ApplyRequestLimits(request *core.Request, limits core.QuotaLimits) error {
	if request == nil {
		return errors.New("quota request is required")
	}
	if request.MaxOutputTokens == nil && limits.MaxOutputTokens != nil {
		if *limits.MaxOutputTokens > int64(math.MaxInt) {
			return errors.New("max output token limit exceeds platform integer size")
		}
		value := int(*limits.MaxOutputTokens)
		request.MaxOutputTokens = &value
	}
	if request.MaxOutputTokens != nil {
		if *request.MaxOutputTokens < 0 {
			return errors.New("max output tokens cannot be negative")
		}
		if limits.MaxOutputTokens != nil && int64(*request.MaxOutputTokens) > *limits.MaxOutputTokens {
			return fmt.Errorf("%w: max output tokens", ErrExceeded)
		}
	}
	inputTokens, err := conservativeInputTokens(*request)
	if err != nil {
		return err
	}
	if limits.MaxInputTokens != nil && inputTokens > *limits.MaxInputTokens {
		return fmt.Errorf("%w: max input tokens", ErrExceeded)
	}
	return nil
}

func EstimateRequest(request core.Request, routes []provider.Route, limits core.QuotaLimits) (Estimate, error) {
	inputTokens, err := conservativeInputTokens(request)
	if err != nil {
		return Estimate{}, err
	}
	var outputTokens int64
	if request.MaxOutputTokens != nil {
		outputTokens = int64(*request.MaxOutputTokens)
	}
	estimate := Estimate{InputTokens: inputTokens, OutputTokens: outputTokens, Currency: limits.Currency}
	if !hasSpendLimit(limits) {
		return estimate, nil
	}
	if outputTokens == 0 && limits.MaxOutputTokens == nil {
		return Estimate{}, errors.New("spend limits require a finite max_output_tokens limit")
	}
	if len(routes) == 0 {
		return Estimate{}, errors.New("cannot estimate quota without a Model Route")
	}
	for _, route := range routes {
		price := route.PriceSnapshot
		if price.Currency == "" || price.Currency != limits.Currency {
			return Estimate{}, errors.New("all eligible Model Routes must use the quota currency")
		}
		cost := saturatedAdd(tokenCost(inputTokens, price.InputPerMillionMicros), tokenCost(outputTokens, price.OutputPerMillionMicros))
		if cost > estimate.SpendMicros {
			estimate.SpendMicros = cost
		}
	}
	return estimate, nil
}

func HasLimits(limits core.QuotaLimits) bool {
	return limits.MaxInputTokens != nil || limits.MaxOutputTokens != nil || limits.MaxCostMicros != nil ||
		limits.RequestsPerMinute != nil || limits.TokensPerMinute != nil || limits.DailySpendMicros != nil ||
		limits.MonthlySpendMicros != nil || limits.RefreshDailySpendMicros != nil || limits.RefreshMonthlySpendMicros != nil
}

func HasRefreshLimits(limits core.QuotaLimits) bool {
	return limits.RefreshDailySpendMicros != nil || limits.RefreshMonthlySpendMicros != nil
}

func conservativeInputTokens(request core.Request) (int64, error) {
	payload, err := json.Marshal(struct {
		Input          []core.Item       `json:"input"`
		Tools          []json.RawMessage `json:"tools,omitempty"`
		ToolChoice     json.RawMessage   `json:"tool_choice,omitempty"`
		Reasoning      json.RawMessage   `json:"reasoning,omitempty"`
		Text           json.RawMessage   `json:"text,omitempty"`
		ClientMetadata json.RawMessage   `json:"client_metadata,omitempty"`
	}{request.Input, request.Tools, request.ToolChoice, request.Reasoning, request.Text, request.ClientMetadata})
	if err != nil {
		return 0, fmt.Errorf("estimate input tokens: %w", err)
	}
	// Provider tokenizers cannot emit more byte-backed tokens than this serialized
	// request upper bound. It intentionally over-reserves rather than risking a
	// financial hard-limit overrun.
	return int64(len(payload)), nil
}

func tokenCost(tokens, perMillionMicros int64) int64 {
	if tokens <= 0 || perMillionMicros <= 0 {
		return 0
	}
	hi, lo := bits.Mul64(uint64(tokens), uint64(perMillionMicros))
	if hi >= 1_000_000 {
		return math.MaxInt64
	}
	quotient, remainder := bits.Div64(hi, lo, 1_000_000)
	if remainder > 0 {
		quotient++
	}
	if quotient > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(quotient)
}

func saturatedAdd(left, right int64) int64 {
	if left > math.MaxInt64-right {
		return math.MaxInt64
	}
	return left + right
}
