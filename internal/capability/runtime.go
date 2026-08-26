package capability

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"time"

	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/provider"
	"github.com/toddzheng/llm-gateway/internal/quota"
	"github.com/toddzheng/llm-gateway/internal/store"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

var ErrRouteNotFound = errors.New("no compatible model route")
var ErrQuotaExceeded = quota.ErrExceeded

type Options struct {
	Now             func() time.Time
	QuotaController quota.Controller
	QuotaLeaseTTL   time.Duration
}

type Runtime struct {
	usageStore    store.CapabilityUsageStore
	router        provider.CapabilityRouter
	now           func() time.Time
	quota         quota.Controller
	quotaLeaseTTL time.Duration
	operations    metric.Int64Counter
	errors        metric.Int64Counter
	inputUnits    metric.Int64Counter
	documents     metric.Int64Counter
	spendMicros   metric.Int64Counter
	duration      metric.Float64Histogram
}

func New(usageStore store.CapabilityUsageStore, router provider.CapabilityRouter, options Options) *Runtime {
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.QuotaLeaseTTL <= 0 {
		options.QuotaLeaseTTL = 2 * time.Minute
	}
	meter := otel.Meter("github.com/toddzheng/llm-gateway/internal/capability")
	operations, _ := meter.Int64Counter("gateway.capability.operations")
	errorsCounter, _ := meter.Int64Counter("gateway.capability.errors")
	inputUnits, _ := meter.Int64Counter("gateway.capability.input_units")
	documents, _ := meter.Int64Counter("gateway.capability.documents")
	spendMicros, _ := meter.Int64Counter("gateway.capability.spend_micros")
	duration, _ := meter.Float64Histogram("gateway.capability.duration", metric.WithUnit("s"))
	return &Runtime{
		usageStore: usageStore, router: router, now: options.Now, quota: options.QuotaController,
		quotaLeaseTTL: options.QuotaLeaseTTL, operations: operations, errors: errorsCounter,
		inputUnits: inputUnits, documents: documents, spendMicros: spendMicros, duration: duration,
	}
}

func (r *Runtime) Embed(ctx context.Context, request core.EmbeddingRequest) (string, core.EmbeddingResult, error) {
	if request.TenantID == "" || request.Model == "" || len(request.Input) == 0 {
		return "", core.EmbeddingResult{}, errors.New("Tenant, model, and input are required")
	}
	operationID, err := newID("cap")
	if err != nil {
		return "", core.EmbeddingResult{}, err
	}
	routes, err := r.router.CapabilityCandidates(ctx, provider.CapabilityRouteQuery{
		TenantID: request.TenantID, Model: request.Model, HomeRegion: request.HomeRegion, Capability: core.CapabilityEmbeddings,
		CompatibilityMode: request.CompatibilityMode,
	})
	if err != nil {
		return operationID, core.EmbeddingResult{}, fmt.Errorf("%w: %v", ErrRouteNotFound, err)
	}
	limits, err := effectiveLimits(request.CapabilityPrincipal)
	if err != nil {
		return operationID, core.EmbeddingResult{}, err
	}
	estimatedInputUnits := estimateEmbeddingInputUnits(request.Input)
	if limits.EmbeddingInputUnits != nil && estimatedInputUnits > *limits.EmbeddingInputUnits {
		return operationID, core.EmbeddingResult{}, fmt.Errorf("%w: embedding input units", ErrQuotaExceeded)
	}
	if err := r.assertWriter(ctx, request.CapabilityPrincipal); err != nil {
		return operationID, core.EmbeddingResult{}, err
	}
	var lastErr error
	for attempt, route := range routes {
		if route.EmbeddingExecutor == nil {
			lastErr = errors.New("embedding executor is unavailable")
			continue
		}
		if err := r.assertWriter(ctx, request.CapabilityPrincipal); err != nil {
			return operationID, core.EmbeddingResult{}, err
		}
		reservedSpend := perMillionCost(estimatedInputUnits, route.PriceSnapshot.EmbeddingInputPerMillionMicros)
		if exceeds(limits.CapabilitySpendMicros, reservedSpend) || exceeds(limits.MaxCostMicros, reservedSpend) {
			return operationID, core.EmbeddingResult{}, fmt.Errorf("%w: capability spend", ErrQuotaExceeded)
		}
		reservation, reserveErr := r.reserve(ctx, request.CapabilityPrincipal, attemptOperationID(operationID, attempt), core.CapabilityEmbeddings,
			reservedSpend, route.PriceSnapshot.Currency, estimatedInputUnits, 0)
		if reserveErr != nil {
			if errors.Is(reserveErr, quota.ErrExceeded) {
				return operationID, core.EmbeddingResult{}, fmt.Errorf("%w: %v", ErrQuotaExceeded, reserveErr)
			}
			return operationID, core.EmbeddingResult{}, reserveErr
		}
		startedAt := r.now()
		result, executeErr := executeWithWriterFence(r, ctx, request.CapabilityPrincipal, func(executeCtx context.Context) (core.EmbeddingResult, error) {
			return route.EmbeddingExecutor.Embed(executeCtx, request)
		})
		if executeErr != nil {
			r.observe(ctx, core.CapabilityEmbeddings, route.Provider, startedAt, core.CapabilityUsageRecord{}, executeErr)
			if provider.SideEffectPossible(executeErr) {
				r.uncertain(context.WithoutCancel(ctx), reservation)
				return operationID, core.EmbeddingResult{}, executeErr
			}
			r.release(context.WithoutCancel(ctx), reservation)
			lastErr = executeErr
			continue
		}
		if err := r.assertWriter(context.WithoutCancel(ctx), request.CapabilityPrincipal); err != nil {
			r.uncertain(context.WithoutCancel(ctx), reservation)
			return operationID, core.EmbeddingResult{}, err
		}
		if err := validateEmbeddingResult(request, result); err != nil {
			r.uncertain(context.WithoutCancel(ctx), reservation)
			return operationID, core.EmbeddingResult{}, err
		}
		if !hasUsageMetric(result.ProviderUsage, result.InputUnits, "input_units", "input_tokens", "prompt_tokens") {
			r.uncertain(context.WithoutCancel(ctx), reservation)
			return operationID, core.EmbeddingResult{}, errors.New("Provider completed embeddings without matching input usage evidence")
		}
		amount := perMillionCost(result.InputUnits, route.PriceSnapshot.EmbeddingInputPerMillionMicros)
		record := core.CapabilityUsageRecord{
			ID: operationID + "_usage", TenantID: request.TenantID, APIKeyID: request.APIKeyID, OperationID: operationID,
			HomeRegion: request.HomeRegion, ExecutionEpoch: request.ExecutionEpoch,
			Capability: core.CapabilityEmbeddings, RouteID: route.ID, Provider: route.Provider, Model: result.Model,
			PriceSnapshot: route.PriceSnapshot, ProviderUsage: result.ProviderUsage, InputUnits: result.InputUnits,
			Dimensions: result.Dimensions, AmountMicros: amount, Currency: route.PriceSnapshot.Currency, CreatedAt: r.now().UTC(),
		}
		if reservation != nil {
			record.QuotaReservationID = reservation.ID
		}
		if err := r.usageStore.RecordCapabilityUsage(context.WithoutCancel(ctx), record); err != nil {
			r.uncertain(context.WithoutCancel(ctx), reservation)
			return operationID, core.EmbeddingResult{}, fmt.Errorf("record embedding usage: %w", err)
		}
		if reservation != nil {
			if err := r.quota.Commit(context.WithoutCancel(ctx), reservation.ID, quota.ActualUsage{
				Requests: 1, SpendMicros: amount, EmbeddingInputUnits: result.InputUnits,
			}); err != nil {
				return operationID, core.EmbeddingResult{}, fmt.Errorf("commit embedding quota: %w", err)
			}
		}
		r.observe(ctx, core.CapabilityEmbeddings, route.Provider, startedAt, record, nil)
		return operationID, result, nil
	}
	if lastErr == nil {
		lastErr = ErrRouteNotFound
	}
	return operationID, core.EmbeddingResult{}, lastErr
}

func (r *Runtime) reserve(
	ctx context.Context,
	principal core.CapabilityPrincipal,
	operationID string,
	capability core.Capability,
	spendMicros int64,
	currency string,
	embeddingInputUnits int64,
	rerankDocuments int64,
) (*quota.Reservation, error) {
	if r.quota == nil {
		return nil, nil
	}
	if principal.APIKeyID == "" || principal.TenantPolicy == nil || principal.APIKeyPolicy == nil ||
		principal.TenantPolicy.Revision <= 0 || principal.APIKeyPolicy.Revision <= 0 {
		return nil, errors.New("capability quota requires current Tenant and Gateway API Key policies")
	}
	reservation, err := r.quota.Reserve(ctx, quota.ReservationRequest{
		TenantID: principal.TenantID, APIKeyID: principal.APIKeyID, CapabilityOperationID: operationID, Capability: capability,
		HomeRegion: principal.HomeRegion, ExecutionEpoch: principal.ExecutionEpoch,
		TenantPolicyRevision: principal.TenantPolicy.Revision, APIKeyPolicyRevision: principal.APIKeyPolicy.Revision,
		TenantLimits: principal.TenantPolicy.Limits, APIKeyLimits: principal.APIKeyPolicy.Limits,
		Requests: 1, ReservedSpendMicros: spendMicros, Currency: currency,
		ReservedEmbeddingInputUnits: embeddingInputUnits, ReservedRerankDocuments: rerankDocuments,
		ExpiresAt: r.now().UTC().Add(r.quotaLeaseTTL),
	})
	if err != nil {
		return nil, err
	}
	return &reservation, nil
}

func (r *Runtime) assertWriter(ctx context.Context, principal core.CapabilityPrincipal) error {
	admissionStore, ok := r.usageStore.(store.CapabilityAdmissionStore)
	if !ok {
		return errors.New("capability usage store does not support writer fencing")
	}
	if err := admissionStore.AssertCapabilityWriter(ctx, principal.TenantID, principal.HomeRegion, principal.ExecutionEpoch); err != nil {
		return fmt.Errorf("capability writer fencing: %w", err)
	}
	return nil
}

func attemptOperationID(operationID string, attempt int) string {
	return fmt.Sprintf("%s_attempt_%d", operationID, attempt+1)
}

func (r *Runtime) release(ctx context.Context, reservation *quota.Reservation) {
	if reservation != nil {
		_ = r.quota.Release(ctx, reservation.ID)
	}
}

func (r *Runtime) uncertain(ctx context.Context, reservation *quota.Reservation) {
	if reservation != nil {
		_ = r.quota.Uncertain(ctx, reservation.ID)
	}
}

func exceeds(limit *int64, amount int64) bool {
	return limit != nil && amount > *limit
}

func effectiveLimits(principal core.CapabilityPrincipal) (core.QuotaLimits, error) {
	tenantLimits := core.QuotaLimits{}
	apiKeyLimits := core.QuotaLimits{}
	if principal.TenantPolicy != nil {
		tenantLimits = principal.TenantPolicy.Limits
	}
	if principal.APIKeyPolicy != nil {
		apiKeyLimits = principal.APIKeyPolicy.Limits
	}
	return quota.EffectiveLimits(tenantLimits, apiKeyLimits)
}

func estimateEmbeddingInputUnits(input []core.EmbeddingInput) int64 {
	var units int64
	for _, item := range input {
		amount := len(item.Tokens)
		if item.Text != nil {
			amount = len([]byte(*item.Text))
		}
		if int64(amount) > math.MaxInt64-units {
			return math.MaxInt64
		}
		units += int64(amount)
	}
	return units
}

func estimateTextUnits(input []string) int64 {
	var units int64
	for _, value := range input {
		amount := int64(len([]byte(value)))
		if amount > math.MaxInt64-units {
			return math.MaxInt64
		}
		units += amount
	}
	return units
}

func (r *Runtime) Moderate(ctx context.Context, request core.ModerationRequest) (string, core.ModerationResult, error) {
	if request.TenantID == "" || request.Model == "" || len(request.Input) == 0 {
		return "", core.ModerationResult{}, errors.New("Tenant, model, and input are required")
	}
	operationID, err := newID("cap")
	if err != nil {
		return "", core.ModerationResult{}, err
	}
	routes, err := r.router.CapabilityCandidates(ctx, provider.CapabilityRouteQuery{
		TenantID: request.TenantID, Model: request.Model, HomeRegion: request.HomeRegion, Capability: core.CapabilityModeration,
		CompatibilityMode: request.CompatibilityMode,
	})
	if err != nil {
		return operationID, core.ModerationResult{}, fmt.Errorf("%w: %v", ErrRouteNotFound, err)
	}
	limits, err := effectiveLimits(request.CapabilityPrincipal)
	if err != nil {
		return operationID, core.ModerationResult{}, err
	}
	estimatedInputUnits := estimateTextUnits(request.Input)
	if err := r.assertWriter(ctx, request.CapabilityPrincipal); err != nil {
		return operationID, core.ModerationResult{}, err
	}
	var lastErr error
	for attempt, route := range routes {
		if route.ModerationExecutor == nil {
			lastErr = errors.New("moderation executor is unavailable")
			continue
		}
		if err := r.assertWriter(ctx, request.CapabilityPrincipal); err != nil {
			return operationID, core.ModerationResult{}, err
		}
		reservedSpend := perMillionCost(estimatedInputUnits, route.PriceSnapshot.ModerationInputPerMillionMicros)
		if exceeds(limits.CapabilitySpendMicros, reservedSpend) || exceeds(limits.MaxCostMicros, reservedSpend) {
			return operationID, core.ModerationResult{}, fmt.Errorf("%w: capability spend", ErrQuotaExceeded)
		}
		reservation, reserveErr := r.reserve(ctx, request.CapabilityPrincipal, attemptOperationID(operationID, attempt), core.CapabilityModeration,
			reservedSpend, route.PriceSnapshot.Currency, 0, 0)
		if reserveErr != nil {
			if errors.Is(reserveErr, quota.ErrExceeded) {
				return operationID, core.ModerationResult{}, fmt.Errorf("%w: %v", ErrQuotaExceeded, reserveErr)
			}
			return operationID, core.ModerationResult{}, reserveErr
		}
		startedAt := r.now()
		result, executeErr := executeWithWriterFence(r, ctx, request.CapabilityPrincipal, func(executeCtx context.Context) (core.ModerationResult, error) {
			return route.ModerationExecutor.Moderate(executeCtx, request)
		})
		if executeErr != nil {
			r.observe(ctx, core.CapabilityModeration, route.Provider, startedAt, core.CapabilityUsageRecord{}, executeErr)
			if provider.SideEffectPossible(executeErr) {
				r.uncertain(context.WithoutCancel(ctx), reservation)
				return operationID, core.ModerationResult{}, executeErr
			}
			r.release(context.WithoutCancel(ctx), reservation)
			lastErr = executeErr
			continue
		}
		if err := r.assertWriter(context.WithoutCancel(ctx), request.CapabilityPrincipal); err != nil {
			r.uncertain(context.WithoutCancel(ctx), reservation)
			return operationID, core.ModerationResult{}, err
		}
		if result.Model == "" || len(result.Results) != len(request.Input) || result.InputUnits < 0 {
			r.uncertain(context.WithoutCancel(ctx), reservation)
			return operationID, core.ModerationResult{}, errors.New("Provider returned invalid moderation result")
		}
		if result.ID == "" {
			result.ID = operationID
		}
		hasEvidence := hasUsageMetric(result.ProviderUsage, result.InputUnits, "input_units", "input_tokens", "prompt_tokens", "total_tokens")
		if !hasEvidence && route.PriceSnapshot.ModerationInputPerMillionMicros > 0 {
			r.uncertain(context.WithoutCancel(ctx), reservation)
			return operationID, core.ModerationResult{}, errors.New("Provider completed moderation without matching billable usage evidence")
		}
		amount := perMillionCost(result.InputUnits, route.PriceSnapshot.ModerationInputPerMillionMicros)
		if !hasEvidence && amount == 0 {
			if reservation != nil {
				if err := r.quota.Commit(context.WithoutCancel(ctx), reservation.ID, quota.ActualUsage{Requests: 1}); err != nil {
					return operationID, core.ModerationResult{}, fmt.Errorf("commit moderation quota: %w", err)
				}
			}
			r.observe(ctx, core.CapabilityModeration, route.Provider, startedAt, core.CapabilityUsageRecord{}, nil)
			return operationID, result, nil
		}
		record := core.CapabilityUsageRecord{
			ID: operationID + "_usage", TenantID: request.TenantID, APIKeyID: request.APIKeyID, OperationID: operationID,
			HomeRegion: request.HomeRegion, ExecutionEpoch: request.ExecutionEpoch,
			Capability: core.CapabilityModeration, RouteID: route.ID, Provider: route.Provider, Model: result.Model,
			PriceSnapshot: route.PriceSnapshot, ProviderUsage: result.ProviderUsage, InputUnits: result.InputUnits,
			AmountMicros: amount, Currency: route.PriceSnapshot.Currency, CreatedAt: r.now().UTC(),
		}
		if reservation != nil {
			record.QuotaReservationID = reservation.ID
		}
		if err := r.usageStore.RecordCapabilityUsage(context.WithoutCancel(ctx), record); err != nil {
			r.uncertain(context.WithoutCancel(ctx), reservation)
			return operationID, core.ModerationResult{}, fmt.Errorf("record moderation usage: %w", err)
		}
		if reservation != nil {
			if err := r.quota.Commit(context.WithoutCancel(ctx), reservation.ID, quota.ActualUsage{Requests: 1, SpendMicros: amount}); err != nil {
				return operationID, core.ModerationResult{}, fmt.Errorf("commit moderation quota: %w", err)
			}
		}
		r.observe(ctx, core.CapabilityModeration, route.Provider, startedAt, record, nil)
		return operationID, result, nil
	}
	if lastErr == nil {
		lastErr = ErrRouteNotFound
	}
	return operationID, core.ModerationResult{}, lastErr
}

func (r *Runtime) Rerank(ctx context.Context, request core.RerankRequest) (string, core.RerankResult, error) {
	if request.TenantID == "" || request.Model == "" || request.Query == "" || len(request.Documents) == 0 {
		return "", core.RerankResult{}, errors.New("Tenant, model, query, and documents are required")
	}
	operationID, err := newID("cap")
	if err != nil {
		return "", core.RerankResult{}, err
	}
	routes, err := r.router.CapabilityCandidates(ctx, provider.CapabilityRouteQuery{
		TenantID: request.TenantID, Model: request.Model, HomeRegion: request.HomeRegion, Capability: core.CapabilityRerank,
		CompatibilityMode: request.CompatibilityMode,
	})
	if err != nil {
		return operationID, core.RerankResult{}, fmt.Errorf("%w: %v", ErrRouteNotFound, err)
	}
	limits, err := effectiveLimits(request.CapabilityPrincipal)
	if err != nil {
		return operationID, core.RerankResult{}, err
	}
	documents := int64(len(request.Documents))
	if limits.RerankDocuments != nil && documents > *limits.RerankDocuments {
		return operationID, core.RerankResult{}, fmt.Errorf("%w: rerank documents", ErrQuotaExceeded)
	}
	if err := r.assertWriter(ctx, request.CapabilityPrincipal); err != nil {
		return operationID, core.RerankResult{}, err
	}
	var lastErr error
	for attempt, route := range routes {
		if route.RerankExecutor == nil {
			lastErr = errors.New("rerank executor is unavailable")
			continue
		}
		if err := r.assertWriter(ctx, request.CapabilityPrincipal); err != nil {
			return operationID, core.RerankResult{}, err
		}
		reservedSpend := perThousandCost(documents, route.PriceSnapshot.RerankDocumentPerThousandMicros)
		if exceeds(limits.CapabilitySpendMicros, reservedSpend) || exceeds(limits.MaxCostMicros, reservedSpend) {
			return operationID, core.RerankResult{}, fmt.Errorf("%w: capability spend", ErrQuotaExceeded)
		}
		reservation, reserveErr := r.reserve(ctx, request.CapabilityPrincipal, attemptOperationID(operationID, attempt), core.CapabilityRerank,
			reservedSpend, route.PriceSnapshot.Currency, 0, documents)
		if reserveErr != nil {
			if errors.Is(reserveErr, quota.ErrExceeded) {
				return operationID, core.RerankResult{}, fmt.Errorf("%w: %v", ErrQuotaExceeded, reserveErr)
			}
			return operationID, core.RerankResult{}, reserveErr
		}
		startedAt := r.now()
		result, executeErr := executeWithWriterFence(r, ctx, request.CapabilityPrincipal, func(executeCtx context.Context) (core.RerankResult, error) {
			return route.RerankExecutor.Rerank(executeCtx, request)
		})
		if executeErr != nil {
			r.observe(ctx, core.CapabilityRerank, route.Provider, startedAt, core.CapabilityUsageRecord{}, executeErr)
			if provider.SideEffectPossible(executeErr) {
				r.uncertain(context.WithoutCancel(ctx), reservation)
				return operationID, core.RerankResult{}, executeErr
			}
			r.release(context.WithoutCancel(ctx), reservation)
			lastErr = executeErr
			continue
		}
		if err := r.assertWriter(context.WithoutCancel(ctx), request.CapabilityPrincipal); err != nil {
			r.uncertain(context.WithoutCancel(ctx), reservation)
			return operationID, core.RerankResult{}, err
		}
		if err := validateRerankResult(request, result); err != nil {
			r.uncertain(context.WithoutCancel(ctx), reservation)
			return operationID, core.RerankResult{}, err
		}
		if !hasUsageMetric(result.ProviderUsage, result.ProviderTokens, "provider_tokens", "input_tokens", "total_tokens") {
			r.uncertain(context.WithoutCancel(ctx), reservation)
			return operationID, core.RerankResult{}, errors.New("Provider completed rerank without matching token usage evidence")
		}
		if !request.ReturnDocuments {
			for index := range result.Results {
				result.Results[index].Document = nil
			}
		}
		if result.ID == "" {
			result.ID = operationID
		}
		amount := perThousandCost(result.Documents, route.PriceSnapshot.RerankDocumentPerThousandMicros)
		record := core.CapabilityUsageRecord{
			ID: operationID + "_usage", TenantID: request.TenantID, APIKeyID: request.APIKeyID, OperationID: operationID,
			HomeRegion: request.HomeRegion, ExecutionEpoch: request.ExecutionEpoch,
			Capability: core.CapabilityRerank, RouteID: route.ID, Provider: route.Provider, Model: result.Model,
			PriceSnapshot: route.PriceSnapshot, ProviderUsage: result.ProviderUsage, InputUnits: result.ProviderTokens,
			Documents: result.Documents, AmountMicros: amount, Currency: route.PriceSnapshot.Currency, CreatedAt: r.now().UTC(),
		}
		if reservation != nil {
			record.QuotaReservationID = reservation.ID
		}
		if err := r.usageStore.RecordCapabilityUsage(context.WithoutCancel(ctx), record); err != nil {
			r.uncertain(context.WithoutCancel(ctx), reservation)
			return operationID, core.RerankResult{}, fmt.Errorf("record rerank usage: %w", err)
		}
		if reservation != nil {
			if err := r.quota.Commit(context.WithoutCancel(ctx), reservation.ID, quota.ActualUsage{
				Requests: 1, SpendMicros: amount, RerankDocuments: result.Documents,
			}); err != nil {
				return operationID, core.RerankResult{}, fmt.Errorf("commit rerank quota: %w", err)
			}
		}
		r.observe(ctx, core.CapabilityRerank, route.Provider, startedAt, record, nil)
		return operationID, result, nil
	}
	if lastErr == nil {
		lastErr = ErrRouteNotFound
	}
	return operationID, core.RerankResult{}, lastErr
}

func (r *Runtime) observe(
	ctx context.Context,
	capability core.Capability,
	providerName string,
	startedAt time.Time,
	usage core.CapabilityUsageRecord,
	err error,
) {
	attributes := metric.WithAttributes(
		attribute.String("gateway.capability", string(capability)),
		attribute.String("gen_ai.provider.name", providerName),
	)
	r.operations.Add(ctx, 1, attributes)
	if err != nil {
		r.errors.Add(ctx, 1, attributes)
	}
	r.inputUnits.Add(ctx, usage.InputUnits, attributes)
	r.documents.Add(ctx, usage.Documents, attributes)
	r.spendMicros.Add(ctx, usage.AmountMicros, attributes)
	r.duration.Record(ctx, r.now().Sub(startedAt).Seconds(), attributes)
}

func executeWithWriterFence[T any](
	runtime *Runtime,
	ctx context.Context,
	principal core.CapabilityPrincipal,
	execute func(context.Context) (T, error),
) (T, error) {
	var result T
	var executionErr error
	started := false
	fenceStore, ok := runtime.usageStore.(store.CapabilityAdmissionStore)
	if !ok {
		return result, errors.New("capability usage store does not support writer fencing")
	}
	err := fenceStore.ExecuteWithCapabilityWriterFence(ctx, principal.TenantID, principal.HomeRegion, principal.ExecutionEpoch, func(executeCtx context.Context) error {
		started = true
		result, executionErr = execute(executeCtx)
		return executionErr
	})
	if executionErr != nil {
		return result, executionErr
	}
	if err != nil {
		if !started {
			return result, err
		}
		return result, provider.NewExecutionError(err, true)
	}
	return result, nil
}

func validateRerankResult(request core.RerankRequest, result core.RerankResult) error {
	if result.Model == "" || result.Documents != int64(len(request.Documents)) || result.ProviderTokens < 0 {
		return errors.New("Provider returned invalid rerank result")
	}
	if request.TopN != nil && len(result.Results) > *request.TopN {
		return errors.New("Provider returned too many rerank results")
	}
	seen := make(map[int]struct{}, len(result.Results))
	for _, item := range result.Results {
		if item.Index < 0 || item.Index >= len(request.Documents) || math.IsNaN(item.RelevanceScore) || math.IsInf(item.RelevanceScore, 0) {
			return errors.New("Provider returned invalid rerank item")
		}
		if _, exists := seen[item.Index]; exists {
			return errors.New("Provider returned duplicate rerank index")
		}
		seen[item.Index] = struct{}{}
		if request.ReturnDocuments && (item.Document == nil || item.Document.Text != request.Documents[item.Index].Text) {
			return errors.New("Provider did not honor return_documents")
		}
	}
	return nil
}

func validateEmbeddingResult(request core.EmbeddingRequest, result core.EmbeddingResult) error {
	if result.Model == "" || len(result.Data) != len(request.Input) || result.InputUnits < 0 || result.Dimensions <= 0 {
		return errors.New("Provider returned invalid embedding result")
	}
	if request.Dimensions != nil && result.Dimensions != int64(*request.Dimensions) {
		return errors.New("Provider did not honor requested embedding dimensions")
	}
	for index, item := range result.Data {
		if item.Index != index || (len(item.Embedding) == 0 && item.Base64 == "") ||
			(len(item.Embedding) > 0 && int64(len(item.Embedding)) != result.Dimensions) {
			return errors.New("Provider returned invalid embedding vector")
		}
		if request.EncodingFormat == "base64" && (item.Base64 == "" || len(item.Embedding) != 0) {
			return errors.New("Provider did not honor base64 encoding_format")
		}
		if request.EncodingFormat == "base64" {
			decoded, err := base64.StdEncoding.DecodeString(item.Base64)
			if err != nil || int64(len(decoded)) != result.Dimensions*4 {
				return errors.New("Provider returned invalid base64 embedding dimensions")
			}
		}
		if request.EncodingFormat != "base64" && (len(item.Embedding) == 0 || item.Base64 != "") {
			return errors.New("Provider did not honor float encoding_format")
		}
	}
	return nil
}

func hasUsageMetric(raw json.RawMessage, expected int64, keys ...string) bool {
	if expected <= 0 || len(raw) == 0 {
		return false
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return false
	}
	for _, key := range keys {
		var value int64
		if encoded, ok := object[key]; ok && json.Unmarshal(encoded, &value) == nil && value == expected {
			return true
		}
	}
	return false
}

func newID(prefix string) (string, error) {
	var payload [16]byte
	if _, err := rand.Read(payload[:]); err != nil {
		return "", err
	}
	return prefix + "_" + hex.EncodeToString(payload[:]), nil
}

func perMillionCost(units, rate int64) int64 {
	if units <= 0 || rate <= 0 {
		return 0
	}
	hi, lo := bits.Mul64(uint64(units), uint64(rate))
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

func perThousandCost(units, rate int64) int64 {
	if units <= 0 || rate <= 0 {
		return 0
	}
	hi, lo := bits.Mul64(uint64(units), uint64(rate))
	if hi >= 1_000 {
		return math.MaxInt64
	}
	quotient, remainder := bits.Div64(hi, lo, 1_000)
	if remainder > 0 {
		quotient++
	}
	if quotient > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(quotient)
}
