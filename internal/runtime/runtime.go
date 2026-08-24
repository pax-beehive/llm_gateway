package runtime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/toddzheng/llm-gateway/internal/cacheprotection"
	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/provider"
	"github.com/toddzheng/llm-gateway/internal/store"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

type Runtime struct {
	store               store.ResponseStore
	router              provider.Router
	now                 func() time.Time
	idleTimeout         time.Duration
	keepaliveInterval   time.Duration
	cacheCoordinator    *cacheprotection.Coordinator
	cacheError          func(error)
	coordinationError   func(error)
	tracer              trace.Tracer
	responses           metric.Int64Counter
	providerAttempts    metric.Int64Counter
	tokens              metric.Int64Counter
	costMicros          metric.Int64Counter
	cacheOutcomes       metric.Int64Counter
	providerDuration    metric.Float64Histogram
	cacheProtectionMode string
	cacheHoldoutPercent int
	tenantPolicies      map[string]core.TenantPolicy
	quotaLeaseTTL       time.Duration
	quotaRenewInterval  time.Duration

	mu       sync.Mutex
	cancels  map[string]context.CancelFunc
	inflight map[string]int
}

type Options struct {
	ProviderIdleTimeout time.Duration
	KeepaliveInterval   time.Duration
	CacheCoordinator    *cacheprotection.Coordinator
	OnCacheError        func(error)
	OnCoordinationError func(error)
	CacheProtectionMode string
	CacheHoldoutPercent int
	TenantPolicies      map[string]core.TenantPolicy
	QuotaLeaseTTL       time.Duration
	QuotaRenewInterval  time.Duration
}

const (
	CacheProtectionShadowMode          = "shadow"
	CacheProtectionAnthropicCanaryMode = "anthropic-one-refresh-canary"
)

var ErrProviderIdleTimeout = errors.New("provider stream idle timeout")

var ErrQuotaExceeded = store.ErrQuotaExceeded

func New(responseStore store.ResponseStore, router provider.Router) *Runtime {
	return NewWithOptions(responseStore, router, Options{})
}

func NewWithOptions(responseStore store.ResponseStore, router provider.Router, options Options) *Runtime {
	if options.ProviderIdleTimeout <= 0 {
		options.ProviderIdleTimeout = 90 * time.Second
	}
	if options.KeepaliveInterval <= 0 {
		options.KeepaliveInterval = 15 * time.Second
	}
	if options.CacheProtectionMode == "" {
		options.CacheProtectionMode = CacheProtectionShadowMode
	}
	if options.CacheHoldoutPercent < 0 || options.CacheHoldoutPercent > 100 {
		options.CacheHoldoutPercent = 100
	}
	if options.QuotaLeaseTTL <= 0 {
		options.QuotaLeaseTTL = 5 * time.Minute
	}
	if options.QuotaRenewInterval <= 0 || options.QuotaRenewInterval >= options.QuotaLeaseTTL {
		options.QuotaRenewInterval = min(options.QuotaLeaseTTL/3, time.Minute)
	}
	meter := otel.Meter("github.com/toddzheng/llm-gateway/runtime")
	responses, _ := meter.Int64Counter("gateway.responses")
	providerAttempts, _ := meter.Int64Counter("gateway.provider.attempts")
	tokens, _ := meter.Int64Counter("gateway.tokens")
	costMicros, _ := meter.Int64Counter("gateway.cost.micros")
	cacheOutcomes, _ := meter.Int64Counter("gateway.cache.outcomes")
	providerDuration, _ := meter.Float64Histogram("gateway.provider.duration", metric.WithUnit("s"))
	return &Runtime{
		store: responseStore, router: router, now: time.Now, cancels: make(map[string]context.CancelFunc), inflight: make(map[string]int),
		idleTimeout: options.ProviderIdleTimeout, keepaliveInterval: options.KeepaliveInterval,
		cacheCoordinator: options.CacheCoordinator, cacheError: options.OnCacheError,
		coordinationError: options.OnCoordinationError,
		tracer:            otel.Tracer("github.com/toddzheng/llm-gateway/runtime"), responses: responses,
		providerAttempts: providerAttempts, tokens: tokens, costMicros: costMicros,
		cacheOutcomes: cacheOutcomes, providerDuration: providerDuration,
		cacheProtectionMode: options.CacheProtectionMode, cacheHoldoutPercent: options.CacheHoldoutPercent,
		tenantPolicies: cloneTenantPolicies(options.TenantPolicies),
		quotaLeaseTTL:  options.QuotaLeaseTTL, quotaRenewInterval: options.QuotaRenewInterval,
	}
}

func (r *Runtime) Execute(ctx context.Context, request core.Request) (core.Response, error) {
	return r.execute(ctx, request, nil)
}

func (r *Runtime) ExecuteStreaming(ctx context.Context, request core.Request, emit func(core.Event) error) (core.Response, error) {
	if emit == nil {
		return core.Response{}, errors.New("stream event emitter is required")
	}
	return r.execute(ctx, request, emit)
}

func (r *Runtime) execute(ctx context.Context, request core.Request, emit func(core.Event) error) (core.Response, error) {
	ctx, responseSpan := r.tracer.Start(ctx, "gateway.response.execute", trace.WithAttributes(
		attribute.String("gen_ai.request.model", request.Model),
		attribute.String("gateway.home_region", request.HomeRegion),
		attribute.Bool("gateway.background", request.Background),
	))
	defer responseSpan.End()
	if request.TenantID == "" || request.Model == "" {
		return core.Response{}, errors.New("tenant and model are required")
	}
	admittedCtx, release, err := r.admit(ctx, request)
	if err != nil {
		return core.Response{}, err
	}
	defer release()
	ctx = admittedCtx
	if request.CompatibilityMode == "" {
		request.CompatibilityMode = core.CompatibilityStrict
	}
	if request.CompatibilityMode != core.CompatibilityStrict && request.CompatibilityMode != core.CompatibilityBestEffort {
		return core.Response{}, errors.New("compatibility_mode must be strict or best_effort")
	}
	if request.HomeRegion == "" {
		request.HomeRegion = "local"
	}
	if request.ExecutionEpoch <= 0 {
		request.ExecutionEpoch = 1
	}
	if request.ConversationID != "" && request.PreviousResponseID != "" {
		return core.Response{}, errors.New("conversation and previous_response_id cannot both be set")
	}
	if !request.Store && request.ConversationID != "" {
		return core.Response{}, errors.New("store:false cannot mutate a Conversation")
	}
	responseInput := prepareItems(request.Input)
	request.Input = responseInput
	if request.ConversationID != "" {
		conversationStore, ok := r.store.(store.ConversationStore)
		if !ok {
			return core.Response{}, errors.New("configured Response Store does not support Conversations")
		}
		conversation, err := conversationStore.GetConversation(ctx, request.TenantID, request.ConversationID)
		if err != nil {
			return core.Response{}, fmt.Errorf("conversation: %w", err)
		}
		if conversation.HomeRegion != request.HomeRegion {
			return core.Response{}, errors.New("conversation belongs to a different home region")
		}
		if conversation.ExecutionEpoch != request.ExecutionEpoch {
			return core.Response{}, errors.New("conversation belongs to a different execution epoch")
		}
		request.Input = append(cloneItems(conversation.Items), responseInput...)
		request.ContextItemCount = len(conversation.Items)
		request.ExperimentIdentity = "conversation:" + request.ConversationID
	}
	if request.PreviousResponseID != "" {
		chain, previous, rootResponseID, err := r.responseChain(ctx, request.TenantID, request.PreviousResponseID, request.HomeRegion, request.ExecutionEpoch)
		if err != nil {
			return core.Response{}, fmt.Errorf("previous response: %w", err)
		}
		request.Input = append(chain, responseInput...)
		request.ContextItemCount = len(chain)
		request.ExperimentIdentity = "chain:" + rootResponseID
		if len(previous.Attempts) > 0 {
			request.PreferredRouteID = previous.Attempts[len(previous.Attempts)-1].RouteID
		}
	}
	now := r.now().UTC()
	response := core.Response{
		ID: newID("resp"), Object: "response", CreatedAt: now.Unix(), Status: core.ResponseStatusInProgress,
		Model: request.Model, PreviousResponseID: request.PreviousResponseID, ConversationID: request.ConversationID,
		Input: responseInput, Output: []core.Item{}, Metadata: request.Metadata,
		HomeRegion: request.HomeRegion, Revision: 1,
		ExecutionEpoch: request.ExecutionEpoch, RetainContent: request.Store,
	}
	if policy, exists := r.tenantPolicies[request.TenantID]; request.Store && exists && policy.RetentionSeconds > 0 {
		expiresAt := now.Add(time.Duration(policy.RetentionSeconds) * time.Second).Unix()
		response.ContentExpiresAt = &expiresAt
	}
	responseSpan.SetAttributes(attribute.String("gateway.response.id", response.ID))
	if request.IdempotencyKey != "" {
		idempotentStore, ok := r.store.(store.IdempotentResponseStore)
		if !ok {
			return core.Response{}, errors.New("configured Response Store does not support idempotency")
		}
		existing, created, err := idempotentStore.CreateIdempotent(
			ctx, request.TenantID, response, "responses.create", request.IdempotencyKey, request.RequestHash,
		)
		if err != nil {
			return core.Response{}, fmt.Errorf("create response idempotently: %w", err)
		}
		if !created {
			return existing, nil
		}
	} else if err := r.store.Create(ctx, request.TenantID, responseForPersistence(response)); err != nil {
		return core.Response{}, fmt.Errorf("create response: %w", err)
	}
	sequence := int64(0)
	emitEvent := func(event core.Event) error {
		if emit == nil {
			return nil
		}
		if event.Type == "gateway.keepalive" {
			return emit(event)
		}
		sequence++
		event.Sequence = sequence
		return emit(event)
	}
	createdSnapshot := response
	if err := emitEvent(core.Event{Type: "response.created", Response: &createdSnapshot}); err != nil {
		return r.fail(context.WithoutCancel(ctx), request.TenantID, response, "client_disconnected", err)
	}

	executionCtx, cancel := context.WithCancel(ctx)
	r.setCancel(response.ID, cancel)
	defer func() {
		cancel()
		r.clearCancel(response.ID)
	}()

	routes, err := r.router.Candidates(executionCtx, request)
	if err != nil {
		return r.fail(context.WithoutCancel(executionCtx), request.TenantID, response, "route_not_found", err)
	}

	var lastErr error
	for _, route := range routes {
		lastErr = nil
		attemptStart := r.now().UTC()
		attemptAttributes := []attribute.KeyValue{
			attribute.String("gen_ai.provider.name", route.Provider),
			attribute.String("gateway.route.id", route.ID),
			attribute.String("cloud.region", route.Region),
		}
		r.providerAttempts.Add(executionCtx, 1, metric.WithAttributes(attemptAttributes...))
		attemptRecorded := false
		recordAttempt := func(outcome string) {
			if attemptRecorded {
				return
			}
			attemptRecorded = true
			attributes := append(append([]attribute.KeyValue(nil), attemptAttributes...), attribute.String("gateway.outcome", outcome))
			r.providerDuration.Record(executionCtx, r.now().UTC().Sub(attemptStart).Seconds(), metric.WithAttributes(attributes...))
			responseSpan.AddEvent("provider.attempt", trace.WithAttributes(attributes...))
		}
		attempt := core.Attempt{
			ID: newID("attempt"), RouteID: route.ID, Provider: route.Provider, ProviderModel: route.PriceSnapshot.Model,
			Region: route.Region, StartedAt: attemptStart, PriceSnapshotID: route.PriceSnapshot.ID,
		}
		response.Attempts = append(response.Attempts, attempt)
		protectedHit := r.observeCustomerRequest(executionCtx, request, route)
		stream, executeErr := route.Executor.Execute(executionCtx, request)
		if executeErr != nil {
			recordAttempt("request_error")
			lastErr = executeErr
			response.Attempts[len(response.Attempts)-1].Error = gatewayError("provider_error", executeErr)
			continue
		}

		visible := false
		providerCompleted := false
		var outputText string
		var providerUsage []byte
		for {
			event, recvErr := r.recv(executionCtx, stream, emitEvent)
			if recvErr == io.EOF {
				if !providerCompleted {
					lastErr = io.ErrUnexpectedEOF
					response.Attempts[len(response.Attempts)-1].Error = gatewayError("provider_stream_error", lastErr)
					recordAttempt("incomplete_stream")
				}
				break
			}
			if recvErr != nil {
				lastErr = recvErr
				response.Attempts[len(response.Attempts)-1].Error = gatewayError("provider_stream_error", recvErr)
				_ = stream.Close()
				if executionCtx.Err() != nil {
					recordAttempt("cancelled")
					if outputText != "" {
						response.Output = append(response.Output, outputMessage(outputText))
					}
					return r.cancelled(context.WithoutCancel(executionCtx), request.TenantID, response)
				}
				if visible {
					recordAttempt("stream_error")
					if outputText != "" {
						response.Output = append(response.Output, outputMessage(outputText))
					}
					return r.fail(context.WithoutCancel(executionCtx), request.TenantID, response, "stream_interrupted", recvErr)
				}
				break
			}
			if event.Delta != "" || event.Item != nil {
				if !visible {
					firstVisible := r.now().UTC()
					response.Attempts[len(response.Attempts)-1].FirstVisibleAt = &firstVisible
				}
				visible = true
			}
			if event.Type != "response.completed" && event.Type != "response.failed" {
				if err := emitEvent(event); err != nil {
					_ = stream.Close()
					return r.fail(context.WithoutCancel(executionCtx), request.TenantID, response, "client_disconnected", err)
				}
			}
			outputText += event.Delta
			if event.Item != nil {
				if event.Item.ID == "" {
					event.Item.ID = newID("item")
				}
				response.Output = append(response.Output, *event.Item)
			}
			if event.Usage != nil {
				response.Usage = *event.Usage
			}
			if event.Type == "response.completed" {
				providerCompleted = true
			}
			if len(event.ProviderUsage) > 0 {
				providerUsage = append(providerUsage[:0], event.ProviderUsage...)
			}
		}
		_ = stream.Close()
		if lastErr != nil && !visible {
			recordAttempt("failed_before_output")
			continue
		}
		if lastErr != nil {
			recordAttempt("stream_error")
			if outputText != "" {
				response.Output = append(response.Output, outputMessage(outputText))
			}
			return r.fail(context.WithoutCancel(executionCtx), request.TenantID, response, "stream_interrupted", lastErr)
		}
		recordAttempt("completed")
		if outputText != "" {
			response.Output = append(response.Output, outputMessage(outputText))
		}
		completed := r.now().UTC()
		response.Status = core.ResponseStatusCompleted
		completedUnix := completed.Unix()
		response.CompletedAt = &completedUnix
		response.Attempts[len(response.Attempts)-1].CompletedAt = &completed
		if len(providerUsage) == 0 {
			providerUsage = []byte("{}")
		}
		usageRecord := core.UsageRecord{
			ID: newID("usage"), TenantID: request.TenantID, ResponseID: response.ID,
			AttemptID: response.Attempts[len(response.Attempts)-1].ID, PriceSnapshot: route.PriceSnapshot,
			ProviderUsage: providerUsage, Usage: response.Usage,
			AmountMicros: calculateUsageAmount(response.Usage, route.PriceSnapshot), Currency: route.PriceSnapshot.Currency,
			CacheUsageReliable: route.CacheUsageReliable, CreatedAt: completed,
			HoldoutCohort: r.experimentCohort(request, route), ExperimentRevision: r.experimentRevision(route),
		}
		if protectedHit != nil && route.CacheUsageReliable && response.Usage.CachedInputTokens > 0 {
			usageRecord.ProtectedHit = protectedHit
		}
		financialStore, supportsFinancialFinalization := r.store.(store.FinancialResponseFinalizer)
		var persistErr error
		if supportsFinancialFinalization {
			if err := validatePriceSnapshot(route, usageRecord.PriceSnapshot); err != nil {
				return core.Response{}, err
			}
			persistErr = financialStore.FinalizeWithUsage(
				context.WithoutCancel(executionCtx), request.TenantID, responseForPersistence(response), response.Revision, usageRecord,
			)
		} else {
			persistErr = r.store.Update(context.WithoutCancel(executionCtx), request.TenantID, responseForPersistence(response), response.Revision)
		}
		if persistErr != nil {
			return core.Response{}, fmt.Errorf("persist completed response and usage: %w", persistErr)
		}
		response.Revision++
		r.responses.Add(executionCtx, 1, metric.WithAttributes(attribute.String("gateway.response.status", "completed")))
		r.tokens.Add(executionCtx, response.Usage.InputTokens, metric.WithAttributes(attribute.String("gateway.token.type", "input"), attribute.String("gen_ai.provider.name", route.Provider)))
		r.tokens.Add(executionCtx, response.Usage.CachedInputTokens, metric.WithAttributes(attribute.String("gateway.token.type", "cached_input"), attribute.String("gen_ai.provider.name", route.Provider)))
		r.tokens.Add(executionCtx, response.Usage.OutputTokens, metric.WithAttributes(attribute.String("gateway.token.type", "output"), attribute.String("gen_ai.provider.name", route.Provider)))
		r.costMicros.Add(executionCtx, usageRecord.AmountMicros, metric.WithAttributes(attribute.String("currency", usageRecord.Currency), attribute.String("gen_ai.provider.name", route.Provider)))
		if usageRecord.ProtectedHit != nil {
			r.cacheOutcomes.Add(executionCtx, 1, metric.WithAttributes(attribute.String("gateway.cache.outcome", "protected_hit"), attribute.String("gen_ai.provider.name", route.Provider)))
		}
		responseSpan.SetStatus(codes.Ok, "completed")
		responseSpan.SetAttributes(attribute.String("gen_ai.provider.name", route.Provider), attribute.String("gateway.route.id", route.ID))
		r.planCacheProtection(context.WithoutCancel(executionCtx), request, response, route)
		if !request.Store {
			if err := r.store.Delete(context.WithoutCancel(executionCtx), request.TenantID, response.ID, response.Revision); err != nil {
				return core.Response{}, fmt.Errorf("apply store:false retention: %w", err)
			}
		}
		completedSnapshot := response
		if err := emitEvent(core.Event{Type: "response.completed", Response: &completedSnapshot, Usage: &completedSnapshot.Usage}); err != nil {
			return response, err
		}
		return response, nil
	}
	if lastErr == nil {
		lastErr = errors.New("all model routes failed")
	}
	if executionCtx.Err() != nil {
		return r.cancelled(context.WithoutCancel(executionCtx), request.TenantID, response)
	}
	return r.fail(context.WithoutCancel(executionCtx), request.TenantID, response, "provider_unavailable", lastErr)
}

func (r *Runtime) admit(ctx context.Context, request core.Request) (context.Context, func(), error) {
	policy, configured := r.tenantPolicies[request.TenantID]
	if configured {
		if policy.MaxInputItems > 0 && len(request.Input) > policy.MaxInputItems {
			return nil, nil, fmt.Errorf("tenant input-item quota exceeded: maximum is %d", policy.MaxInputItems)
		}
		if request.Store && policy.AllowStoredResponses != nil && !*policy.AllowStoredResponses {
			return nil, nil, errors.New("tenant policy forbids stored Responses")
		}
		if request.CacheProtection != nil && request.CacheProtection.Enabled {
			if policy.AllowCacheProtection != nil && !*policy.AllowCacheProtection {
				return nil, nil, errors.New("tenant policy forbids Cache Protection")
			}
			if request.CacheProtection.AllowContentInspection &&
				(policy.AllowContentInspection == nil || !*policy.AllowContentInspection) {
				return nil, nil, errors.New("tenant policy forbids Cache Protection content inspection")
			}
		}
	}
	if !configured || policy.MaxConcurrentResponses <= 0 {
		return ctx, func() {}, nil
	}
	if global, ok := r.store.(store.GlobalQuotaStore); ok {
		leaseID := newID("quota")
		if err := global.AcquireResponseSlot(ctx, request.TenantID, leaseID, policy.MaxConcurrentResponses, r.now().UTC().Add(r.quotaLeaseTTL)); err != nil {
			return nil, nil, err
		}
		leaseCtx, cancelLease := context.WithCancel(ctx)
		go func() {
			ticker := time.NewTicker(r.quotaRenewInterval)
			defer ticker.Stop()
			for {
				select {
				case <-leaseCtx.Done():
					return
				case <-ticker.C:
					renewalCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					renewErr := global.RenewResponseSlot(renewalCtx, request.TenantID, leaseID, r.now().UTC().Add(r.quotaLeaseTTL))
					cancel()
					if renewErr != nil {
						if r.coordinationError != nil {
							r.coordinationError(fmt.Errorf("renew global Response quota lease: %w", renewErr))
						}
						cancelLease()
						return
					}
				}
			}
		}()
		return leaseCtx, func() {
			cancelLease()
			releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = global.ReleaseResponseSlot(releaseCtx, request.TenantID, leaseID)
		}, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inflight[request.TenantID] >= policy.MaxConcurrentResponses {
		return nil, nil, ErrQuotaExceeded
	}
	r.inflight[request.TenantID]++
	return ctx, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.inflight[request.TenantID] <= 1 {
			delete(r.inflight, request.TenantID)
			return
		}
		r.inflight[request.TenantID]--
	}, nil
}

func cloneTenantPolicies(policies map[string]core.TenantPolicy) map[string]core.TenantPolicy {
	cloned := make(map[string]core.TenantPolicy, len(policies))
	for tenantID, policy := range policies {
		cloned[tenantID] = policy
	}
	return cloned
}

func validatePriceSnapshot(route provider.Route, snapshot core.PriceSnapshot) error {
	if snapshot.ID == "" || snapshot.Provider != route.Provider || snapshot.Model == "" || snapshot.Region != route.Region ||
		snapshot.Currency == "" || snapshot.Source == "" || snapshot.InputPerMillionMicros < 0 ||
		snapshot.CachedInputPerMillionMicros < 0 || snapshot.CacheWritePerMillionMicros < 0 || snapshot.OutputPerMillionMicros < 0 {
		return fmt.Errorf("model route %q has an invalid immutable price snapshot", route.ID)
	}
	return nil
}

func calculateUsageAmount(usage core.Usage, snapshot core.PriceSnapshot) int64 {
	cached := min(max(usage.CachedInputTokens, 0), max(usage.InputTokens, 0))
	cacheWrite := min(max(usage.CacheWriteInputTokens, 0), max(usage.InputTokens-cached, 0))
	uncached := max(usage.InputTokens-cached-cacheWrite, 0)
	return perMillionCost(uncached, snapshot.InputPerMillionMicros) +
		perMillionCost(cached, snapshot.CachedInputPerMillionMicros) +
		perMillionCost(cacheWrite, snapshot.CacheWritePerMillionMicros) +
		perMillionCost(max(usage.OutputTokens, 0), snapshot.OutputPerMillionMicros)
}

func perMillionCost(tokens, rate int64) int64 {
	return (tokens/1_000_000)*rate + (tokens%1_000_000)*rate/1_000_000
}

func (r *Runtime) observeCustomerRequest(ctx context.Context, request core.Request, route provider.Route) *core.ProtectedHitEvidence {
	if r.cacheCoordinator == nil || route.CacheAnchorBuilder == nil {
		return nil
	}
	anchor, exists, err := route.CacheAnchorBuilder.CurrentCacheAnchor(ctx, request)
	if err == nil && exists {
		var result cacheprotection.CustomerRequestResult
		result, err = r.cacheCoordinator.CustomerRequest(ctx, anchor)
		if err == nil && result.ProtectedHitCandidate != nil {
			candidate := result.ProtectedHitCandidate
			return &core.ProtectedHitEvidence{
				CacheLeaseID: candidate.CacheLeaseID, OriginalLeaseExpiresAt: candidate.OriginalLeaseExpiresAt,
				RefreshSucceededAt: candidate.RefreshSucceededAt, RefreshExpiresAt: candidate.RefreshExpiresAt,
				CustomerRequestAt: r.now().UTC(), RefreshCostMicros: candidate.RefreshCostMicros,
				RefreshUsageID:       candidate.RefreshUsageID,
				RefreshProviderUsage: append(json.RawMessage(nil), candidate.RefreshProviderUsage...),
				HoldoutCohort:        candidate.HoldoutCohort,
				ForecastCostMicros:   candidate.ForecastCostMicros, StorageCostMicros: candidate.StorageCostMicros,
				RouteLockCostMicros: candidate.RouteLockCostMicros,
			}
		}
	}
	if err != nil && r.cacheError != nil {
		r.cacheError(err)
	}
	return nil
}

func (r *Runtime) planCacheProtection(ctx context.Context, request core.Request, response core.Response, route provider.Route) {
	if r.cacheCoordinator == nil || request.CacheProtection == nil || !request.CacheProtection.Enabled ||
		route.CacheAnchorBuilder == nil || route.CacheProtector == nil {
		return
	}
	observation, err := route.CacheAnchorBuilder.BuildCacheAnchor(ctx, request, response)
	if err != nil {
		if r.cacheError != nil {
			r.cacheError(err)
		}
		return
	}
	now := r.now().UTC()
	ttl := observation.EstimatedExpiresAt.Sub(now)
	if ttl <= 0 {
		return
	}
	probability := 0.25
	source := "stateless_recent_request"
	if request.Background {
		probability, source = 0.90, "open_background_response"
	} else if hasToolWork(response.Output) {
		probability, source = 0.85, "active_tool_work"
	} else if request.ConversationID != "" {
		probability, source = 0.65, "conversation_continuity"
	} else if request.PreviousResponseID != "" {
		probability, source = 0.60, "response_chain_continuity"
	}
	expectedDelay := ttl / 2
	if expectedDelay > 2*time.Minute {
		expectedDelay = 2 * time.Minute
	}
	coldCost := perMillionCost(max(observation.PrefixTokens, 0), route.PriceSnapshot.InputPerMillionMicros)
	hitCost := perMillionCost(max(observation.PrefixTokens, 0), route.PriceSnapshot.CachedInputPerMillionMicros)
	refreshCost := observation.RefreshCostMicros
	if refreshCost <= 0 {
		refreshCost = coldCost
	}
	policy := request.CacheProtection
	shadowMode := true
	holdoutCohort := "shadow"
	maxRefreshes := policy.MaxRefreshes
	leaseID := cacheLeaseID(observation.Anchor)
	if r.cacheProtectionMode == CacheProtectionAnthropicCanaryMode {
		maxRefreshes = min(maxRefreshes, 1)
		if r.experimentCohort(request, route) == "holdout" {
			holdoutCohort = "holdout"
		} else {
			holdoutCohort = "treatment"
			shadowMode = policy.ShadowMode
		}
	}
	candidate := cacheprotection.Candidate{
		Policy: cacheprotection.Policy{
			Enabled: policy.Enabled, MaxSpendMicros: policy.MaxSpendMicros, MaxRefreshes: maxRefreshes,
			MaxProtectionWindow: time.Duration(policy.MaxProtectionWindowSec) * time.Second,
			SafetyMarginMicros:  policy.SafetyMarginMicros, AllowContentInspection: policy.AllowContentInspection,
			ShadowMode: shadowMode,
		},
		Lease: cacheprotection.Lease{
			ID: leaseID, Revision: 1, Anchor: observation.Anchor,
			CreatedAt: now, EstimatedExpiresAt: observation.EstimatedExpiresAt, FencingToken: 1,
			SideEffecting: observation.SideEffecting,
		},
		Forecast: cacheprotection.Forecast{
			Probability: probability, ExpectedAt: now.Add(expectedDelay), Source: source,
		},
		Economics: cacheprotection.Economics{
			PredictedColdCostMicros: coldCost, PredictedHitCostMicros: hitCost,
			RefreshCostMicros: refreshCost,
		},
		RefreshPriceSnapshot: route.PriceSnapshot,
		HoldoutCohort:        holdoutCohort,
		ExperimentRevision:   r.experimentRevision(route),
	}
	intent, err := r.cacheCoordinator.Run(ctx, candidate, route.CacheProtector)
	status := string(intent.Status)
	if err != nil {
		status = "error"
	}
	r.cacheOutcomes.Add(ctx, 1, metric.WithAttributes(
		attribute.String("gateway.cache.outcome", "protection_"+status),
		attribute.String("gen_ai.provider.name", route.Provider),
	))
	if err != nil && r.cacheError != nil {
		r.cacheError(err)
	}
}

func (r *Runtime) experimentCohort(request core.Request, route provider.Route) string {
	if r.cacheProtectionMode != CacheProtectionAnthropicCanaryMode || request.CacheProtection == nil ||
		!request.CacheProtection.Enabled || route.CacheAnchorBuilder == nil || route.CacheProtector == nil {
		return ""
	}
	identity := request.TenantID + "\x1f" + route.ID + "\x1f"
	switch {
	case request.ExperimentIdentity != "":
		identity += request.ExperimentIdentity
	default:
		encoded, _ := json.Marshal(request.Input)
		digest := sha256.Sum256(encoded)
		identity += "stateless:" + hex.EncodeToString(digest[:])
	}
	if stableCohortBucket(identity) < r.cacheHoldoutPercent {
		return "holdout"
	}
	return "treatment"
}

func (r *Runtime) experimentRevision(route provider.Route) string {
	if r.cacheProtectionMode != CacheProtectionAnthropicCanaryMode {
		return ""
	}
	return fmt.Sprintf("%s:v1:route-%s:holdout-%d:capability-%d", r.cacheProtectionMode, route.ID, r.cacheHoldoutPercent, route.Profile.Revision)
}

func stableCohortBucket(identity string) int {
	digest := sha256.Sum256([]byte(identity))
	return int(digest[0]) * 100 / 256
}

func hasToolWork(items []core.Item) bool {
	for _, item := range items {
		if item.Type == "function_call" {
			return true
		}
	}
	return false
}

func cacheLeaseID(anchor provider.CacheAnchor) string {
	digest := sha256.Sum256([]byte(
		anchor.TenantID + "\x1f" + anchor.RouteID + "\x1f" + anchor.Provider + "\x1f" + anchor.Model + "\x1f" +
			anchor.CredentialScope + "\x1f" + anchor.Region + "\x1f" + anchor.CacheKey + "\x1f" + anchor.PrefixHash,
	))
	return "lease_" + hex.EncodeToString(digest[:16])
}

func (r *Runtime) responseChain(ctx context.Context, tenantID, responseID, homeRegion string, executionEpoch int64) ([]core.Item, core.Response, string, error) {
	seen := make(map[string]struct{})
	var reversed []core.Response
	currentID := responseID
	for currentID != "" {
		if len(reversed) >= 256 {
			return nil, core.Response{}, "", errors.New("response chain exceeds 256 links")
		}
		if _, exists := seen[currentID]; exists {
			return nil, core.Response{}, "", errors.New("response chain contains a cycle")
		}
		seen[currentID] = struct{}{}
		response, err := r.store.Get(ctx, tenantID, currentID)
		if err != nil {
			return nil, core.Response{}, "", err
		}
		if response.HomeRegion != "" && response.HomeRegion != homeRegion {
			return nil, core.Response{}, "", errors.New("response chain crosses home regions")
		}
		if response.ExecutionEpoch != executionEpoch {
			return nil, core.Response{}, "", errors.New("response chain crosses execution epochs")
		}
		if response.Status != core.ResponseStatusCompleted {
			return nil, core.Response{}, "", errors.New("previous response is not completed")
		}
		reversed = append(reversed, response)
		currentID = response.PreviousResponseID
	}
	items := make([]core.Item, 0)
	for index := len(reversed) - 1; index >= 0; index-- {
		items = append(items, cloneItems(reversed[index].Input)...)
		items = append(items, cloneItems(reversed[index].Output)...)
	}
	return items, reversed[0], reversed[len(reversed)-1].ID, nil
}

func responseForPersistence(response core.Response) core.Response {
	if response.RetainContent {
		return response
	}
	redacted := response
	redacted.Input = nil
	redacted.Output = nil
	redacted.Metadata = nil
	if redacted.Error != nil {
		redacted.Error = &core.Error{Code: redacted.Error.Code, Type: redacted.Error.Type, Param: redacted.Error.Param, Message: "redacted by store:false"}
	}
	redacted.Attempts = append([]core.Attempt(nil), response.Attempts...)
	for index := range redacted.Attempts {
		if redacted.Attempts[index].Error != nil {
			errorValue := redacted.Attempts[index].Error
			redacted.Attempts[index].Error = &core.Error{Code: errorValue.Code, Type: errorValue.Type, Param: errorValue.Param, Message: "redacted by store:false"}
		}
	}
	return redacted
}

func prepareItems(items []core.Item) []core.Item {
	prepared := cloneItems(items)
	for index := range prepared {
		if prepared[index].ID == "" {
			prepared[index].ID = newID("item")
		}
	}
	return prepared
}

func cloneItems(items []core.Item) []core.Item {
	cloned := make([]core.Item, len(items))
	for index, item := range items {
		cloned[index] = item
		cloned[index].Content = append([]core.Content(nil), item.Content...)
		cloned[index].Arguments = append([]byte(nil), item.Arguments...)
	}
	return cloned
}

type receiveResult struct {
	event core.Event
	err   error
}

func (r *Runtime) recv(ctx context.Context, stream provider.EventStream, emit func(core.Event) error) (core.Event, error) {
	result := make(chan receiveResult, 1)
	go func() {
		event, err := stream.Recv()
		result <- receiveResult{event: event, err: err}
	}()
	idleTimer := time.NewTimer(r.idleTimeout)
	defer idleTimer.Stop()
	keepalive := time.NewTicker(r.keepaliveInterval)
	defer keepalive.Stop()
	for {
		select {
		case received := <-result:
			return received.event, received.err
		case <-keepalive.C:
			if err := emit(core.Event{Type: "gateway.keepalive"}); err != nil {
				_ = stream.Close()
				return core.Event{}, err
			}
		case <-idleTimer.C:
			_ = stream.Close()
			return core.Event{}, ErrProviderIdleTimeout
		case <-ctx.Done():
			_ = stream.Close()
			return core.Event{}, ctx.Err()
		}
	}
}

func outputMessage(text string) core.Item {
	return core.Item{
		ID: newID("msg"), Type: "message", Role: "assistant",
		Content: []core.Content{{Type: "output_text", Text: text}},
	}
}

func (r *Runtime) Get(ctx context.Context, tenantID, responseID string) (core.Response, error) {
	return r.store.Get(ctx, tenantID, responseID)
}

func (r *Runtime) InputItems(ctx context.Context, tenantID, responseID string) ([]core.Item, error) {
	return r.store.ListInputItems(ctx, tenantID, responseID)
}

func (r *Runtime) CreateConversation(ctx context.Context, tenantID, homeRegion string, executionEpoch int64, items []core.Item, metadata map[string]string) (core.Conversation, error) {
	conversationStore, ok := r.store.(store.ConversationStore)
	if !ok {
		return core.Conversation{}, errors.New("configured Response Store does not support Conversations")
	}
	conversation := core.Conversation{
		ID: newID("conv"), Object: "conversation", CreatedAt: r.now().UTC().Unix(), HomeRegion: homeRegion,
		ExecutionEpoch: executionEpoch, Items: prepareItems(items), Metadata: metadata, Revision: 1,
	}
	if err := conversationStore.CreateConversation(ctx, tenantID, conversation); err != nil {
		return core.Conversation{}, err
	}
	return conversation, nil
}

func (r *Runtime) GetConversation(ctx context.Context, tenantID, conversationID string) (core.Conversation, error) {
	conversationStore, ok := r.store.(store.ConversationStore)
	if !ok {
		return core.Conversation{}, errors.New("configured Response Store does not support Conversations")
	}
	return conversationStore.GetConversation(ctx, tenantID, conversationID)
}

func (r *Runtime) AppendConversationItems(ctx context.Context, tenantID, conversationID, homeRegion string, executionEpoch int64, items []core.Item, expectedRevision int64) (core.Conversation, error) {
	conversationStore, ok := r.store.(store.ConversationStore)
	if !ok {
		return core.Conversation{}, errors.New("configured Response Store does not support Conversations")
	}
	conversation, err := conversationStore.GetConversation(ctx, tenantID, conversationID)
	if err != nil {
		return core.Conversation{}, err
	}
	if conversation.HomeRegion != homeRegion || conversation.ExecutionEpoch != executionEpoch {
		return core.Conversation{}, errors.New("Conversation belongs to a different Home Region or execution epoch")
	}
	return conversationStore.AppendConversationItems(ctx, tenantID, conversationID, prepareItems(items), expectedRevision)
}

func (r *Runtime) DeleteConversation(ctx context.Context, tenantID, conversationID, homeRegion string, executionEpoch, expectedRevision int64) error {
	conversationStore, ok := r.store.(store.ConversationStore)
	if !ok {
		return errors.New("configured Response Store does not support Conversations")
	}
	conversation, err := conversationStore.GetConversation(ctx, tenantID, conversationID)
	if err != nil {
		return err
	}
	if conversation.HomeRegion != homeRegion || conversation.ExecutionEpoch != executionEpoch {
		return errors.New("Conversation belongs to a different Home Region or execution epoch")
	}
	return conversationStore.DeleteConversation(ctx, tenantID, conversationID, expectedRevision)
}

func (r *Runtime) Delete(ctx context.Context, tenantID, responseID, homeRegion string, executionEpoch int64) error {
	response, err := r.store.Get(ctx, tenantID, responseID)
	if err != nil {
		return err
	}
	if response.HomeRegion != homeRegion || response.ExecutionEpoch != executionEpoch {
		return errors.New("Response belongs to a different Home Region or execution epoch")
	}
	return r.store.Delete(ctx, tenantID, responseID, response.Revision)
}

func (r *Runtime) Cancel(ctx context.Context, tenantID, responseID, homeRegion string, executionEpoch int64) (core.Response, error) {
	response, err := r.store.Get(ctx, tenantID, responseID)
	if err != nil {
		return core.Response{}, err
	}
	if response.HomeRegion != homeRegion || response.ExecutionEpoch != executionEpoch {
		return core.Response{}, errors.New("Response belongs to a different Home Region or execution epoch")
	}
	if response.Status != core.ResponseStatusInProgress && response.Status != core.ResponseStatusQueued {
		return core.Response{}, errors.New("response is not cancellable")
	}
	r.mu.Lock()
	cancel := r.cancels[responseID]
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	response.Status = core.ResponseStatusCancelled
	if err := r.store.Update(ctx, tenantID, responseForPersistence(response), response.Revision); err != nil {
		return core.Response{}, err
	}
	response.Revision++
	if !response.RetainContent {
		if err := r.store.Delete(ctx, tenantID, response.ID, response.Revision); err != nil {
			return core.Response{}, err
		}
	}
	return response, nil
}

func (r *Runtime) fail(ctx context.Context, tenantID string, response core.Response, code string, cause error) (core.Response, error) {
	span := trace.SpanFromContext(ctx)
	span.RecordError(cause)
	span.SetStatus(codes.Error, code)
	r.responses.Add(ctx, 1, metric.WithAttributes(attribute.String("gateway.response.status", "failed"), attribute.String("gateway.error.code", code)))
	response.Status = core.ResponseStatusFailed
	response.Error = gatewayError(code, cause)
	if err := r.store.Update(ctx, tenantID, responseForPersistence(response), response.Revision); err != nil {
		return core.Response{}, errors.Join(cause, err)
	}
	response.Revision++
	if !response.RetainContent {
		if err := r.store.Delete(ctx, tenantID, response.ID, response.Revision); err != nil {
			return core.Response{}, errors.Join(cause, err)
		}
	}
	return response, cause
}

func (r *Runtime) cancelled(ctx context.Context, tenantID string, response core.Response) (core.Response, error) {
	trace.SpanFromContext(ctx).SetStatus(codes.Error, "cancelled")
	r.responses.Add(ctx, 1, metric.WithAttributes(attribute.String("gateway.response.status", "cancelled")))
	response.Status = core.ResponseStatusCancelled
	response.Error = nil
	err := r.store.Update(ctx, tenantID, responseForPersistence(response), response.Revision)
	if errors.Is(err, store.ErrConflict) {
		current, getErr := r.store.Get(ctx, tenantID, response.ID)
		if getErr == nil && current.Status == core.ResponseStatusCancelled {
			return current, context.Canceled
		}
	}
	if err != nil {
		return core.Response{}, errors.Join(context.Canceled, err)
	}
	response.Revision++
	if !response.RetainContent {
		if deleteErr := r.store.Delete(ctx, tenantID, response.ID, response.Revision); deleteErr != nil {
			return core.Response{}, errors.Join(context.Canceled, deleteErr)
		}
	}
	return response, context.Canceled
}

func gatewayError(code string, cause error) *core.Error {
	return &core.Error{Code: code, Message: cause.Error(), Type: "gateway_error"}
}

func (r *Runtime) setCancel(responseID string, cancel context.CancelFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancels[responseID] = cancel
}

func (r *Runtime) clearCancel(responseID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cancels, responseID)
}

func newID(prefix string) string {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return prefix + "_" + hex.EncodeToString(random[:])
}
