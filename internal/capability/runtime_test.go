package capability_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/toddzheng/llm-gateway/internal/capability"
	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/provider"
	"github.com/toddzheng/llm-gateway/internal/quota"
	"github.com/toddzheng/llm-gateway/internal/store"
)

func TestEmbeddingTypedLimitRejectsBeforeProviderSideEffect(t *testing.T) {
	t.Parallel()
	limit := int64(1)
	executor := &countingEmbeddingExecutor{delegate: provider.NewDeterministicCapabilityExecutor()}
	router := provider.NewRouter(provider.Route{
		ID: "embedding-route", Provider: "deterministic", Model: "embed-model", Region: "local", HomeRegion: "local", Healthy: true,
		EmbeddingExecutor: executor,
		Profile: provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{
			"embeddings": provider.CapabilityNative,
		}},
		PriceSnapshot: core.PriceSnapshot{
			ID: "embedding-price", Provider: "deterministic", Model: "embed-model", Region: "local",
			Currency: "USD", EffectiveAt: 1, Source: "test",
		},
	})
	controller := &quotaControllerStub{}
	runtime := capability.New(store.NewMemoryResponseStore(), router, capability.Options{QuotaController: controller})
	first, second := "a", "b"
	_, _, err := runtime.Embed(context.Background(), core.EmbeddingRequest{
		CapabilityPrincipal: core.CapabilityPrincipal{
			TenantID: "tenant-a", APIKeyID: "key-a", HomeRegion: "local", ExecutionEpoch: 1,
			TenantPolicy: &core.TenantPolicy{Revision: 1, Limits: core.QuotaLimits{EmbeddingInputUnits: &limit}},
			APIKeyPolicy: &core.APIKeyPolicy{Revision: 1},
		},
		Model: "embed-model", Input: []core.EmbeddingInput{{Text: &first}, {Text: &second}},
	})
	if !errors.Is(err, capability.ErrQuotaExceeded) {
		t.Fatalf("error = %v, want typed quota exceeded", err)
	}
	if executor.calls != 0 || controller.reserveCalls != 0 {
		t.Fatalf("Provider/reservation calls = %d/%d, want 0/0", executor.calls, controller.reserveCalls)
	}
}

func TestEmbeddingReservesBeforeExecutionAndSettlesTypedUsage(t *testing.T) {
	t.Parallel()
	spendLimit := int64(10)
	executor := &countingEmbeddingExecutor{delegate: provider.NewDeterministicCapabilityExecutor()}
	router := provider.NewRouter(provider.Route{
		ID: "embedding-route", Provider: "deterministic", Model: "embed-model", Region: "local", HomeRegion: "local", Healthy: true,
		EmbeddingExecutor: executor,
		Profile: provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{
			"embeddings": provider.CapabilityNative,
		}},
		PriceSnapshot: core.PriceSnapshot{
			ID: "embedding-price", Provider: "deterministic", Model: "embed-model", Region: "local",
			Currency: "USD", EffectiveAt: 1, Source: "test", EmbeddingInputPerMillionMicros: 100_000,
		},
	})
	controller := &quotaControllerStub{}
	usageStore := store.NewMemoryResponseStore()
	runtime := capability.New(usageStore, router, capability.Options{QuotaController: controller})
	input := "a"
	operationID, _, err := runtime.Embed(context.Background(), core.EmbeddingRequest{
		CapabilityPrincipal: core.CapabilityPrincipal{
			TenantID: "tenant-a", APIKeyID: "key-a", HomeRegion: "local", ExecutionEpoch: 1,
			TenantPolicy: &core.TenantPolicy{Revision: 3, Limits: core.QuotaLimits{CapabilitySpendMicros: &spendLimit, Currency: "USD"}},
			APIKeyPolicy: &core.APIKeyPolicy{Revision: 4, Limits: core.QuotaLimits{Currency: "USD"}},
		},
		Model: "embed-model", Input: []core.EmbeddingInput{{Text: &input}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if executor.calls != 1 || controller.reserveCalls != 1 || controller.request.CapabilityOperationID != operationID+"_attempt_1" ||
		controller.request.Capability != core.CapabilityEmbeddings || controller.request.Requests != 1 ||
		controller.request.ReservedInputTokens != 0 || controller.request.ReservedSpendMicros != 1 ||
		controller.request.ReservedEmbeddingInputUnits != 1 {
		t.Fatalf("reservation/execution = %d / %#v", executor.calls, controller.request)
	}
	if controller.committed.Requests != 1 || controller.committed.InputTokens != 0 || controller.committed.SpendMicros != 1 ||
		controller.committed.EmbeddingInputUnits != 1 {
		t.Fatalf("settlement = %#v", controller.committed)
	}
	records := usageStore.CapabilityUsageRecords("tenant-a")
	if len(records) != 1 || records[0].QuotaReservationID != "reservation-1" || records[0].PublicModel != "embed-model" {
		t.Fatalf("usage records = %#v", records)
	}
}

func TestRerankDocumentLimitRejectsBeforeProviderSideEffect(t *testing.T) {
	t.Parallel()
	limit := int64(1)
	executor := &countingRerankExecutor{delegate: provider.NewDeterministicCapabilityExecutor()}
	router := provider.NewRouter(provider.Route{
		ID: "rerank-route", Provider: "deterministic", Model: "rerank-model", Region: "local", HomeRegion: "local", Healthy: true,
		RerankExecutor: executor,
		Profile:        provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{"rerank": provider.CapabilityNative}},
		PriceSnapshot: core.PriceSnapshot{
			ID: "rerank-price", Provider: "deterministic", Model: "rerank-model", Region: "local", Currency: "USD", EffectiveAt: 1, Source: "test",
		},
	})
	runtime := capability.New(store.NewMemoryResponseStore(), router, capability.Options{})
	_, _, err := runtime.Rerank(context.Background(), core.RerankRequest{
		CapabilityPrincipal: core.CapabilityPrincipal{
			TenantID: "tenant-a", HomeRegion: "local", ExecutionEpoch: 1,
			TenantPolicy: &core.TenantPolicy{Revision: 1, Limits: core.QuotaLimits{RerankDocuments: &limit}},
		},
		Model: "rerank-model", Query: "query", Documents: []core.RerankDocument{{Text: "one"}, {Text: "two"}},
	})
	if !errors.Is(err, capability.ErrQuotaExceeded) || executor.calls != 0 {
		t.Fatalf("error/calls = %v/%d, want quota rejection before Provider", err, executor.calls)
	}
}

func TestAmbiguousProviderFailureKeepsCapabilityReservationUncertain(t *testing.T) {
	t.Parallel()
	executor := &countingEmbeddingExecutor{
		delegate: provider.NewDeterministicCapabilityExecutor(),
		err:      provider.NewExecutionError(errors.New("connection reset after write"), true),
	}
	router := provider.NewRouter(provider.Route{
		ID: "embedding-route", Provider: "provider", Model: "embed-model", Region: "local", HomeRegion: "local", Healthy: true,
		EmbeddingExecutor: executor,
		Profile:           provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{"embeddings": provider.CapabilityNative}},
		PriceSnapshot: core.PriceSnapshot{
			ID: "embedding-price", Provider: "provider", Model: "embed-model", Region: "local", Currency: "USD", EffectiveAt: 1, Source: "test",
		},
	})
	controller := &quotaControllerStub{}
	runtime := capability.New(store.NewMemoryResponseStore(), router, capability.Options{QuotaController: controller})
	input := "a"
	_, _, err := runtime.Embed(context.Background(), core.EmbeddingRequest{
		CapabilityPrincipal: core.CapabilityPrincipal{
			TenantID: "tenant-a", APIKeyID: "key-a", HomeRegion: "local", ExecutionEpoch: 1,
			TenantPolicy: &core.TenantPolicy{Revision: 1}, APIKeyPolicy: &core.APIKeyPolicy{Revision: 1},
		},
		Model: "embed-model", Input: []core.EmbeddingInput{{Text: &input}},
	})
	if err == nil || !controller.uncertain || controller.released {
		t.Fatalf("error/uncertain/released = %v/%v/%v", err, controller.uncertain, controller.released)
	}
}

func TestSafeFailureReleasesAttemptReservationBeforeFallback(t *testing.T) {
	t.Parallel()
	failing := &countingEmbeddingExecutor{
		delegate: provider.NewDeterministicCapabilityExecutor(),
		err:      provider.NewExecutionError(errors.New("rejected before processing"), false),
	}
	succeeding := &countingEmbeddingExecutor{delegate: provider.NewDeterministicCapabilityExecutor()}
	router := provider.NewRouter(
		embeddingRoute("first", failing, 0),
		embeddingRoute("second", succeeding, 1),
	)
	controller := &quotaControllerStub{}
	runtime := capability.New(store.NewMemoryResponseStore(), router, capability.Options{QuotaController: controller})
	input := "a"
	operationID, _, err := runtime.Embed(context.Background(), embeddingRequest(input))
	if err != nil {
		t.Fatal(err)
	}
	if failing.calls != 1 || succeeding.calls != 1 || len(controller.requests) != 2 {
		t.Fatalf("attempts = %d/%d reservations=%d", failing.calls, succeeding.calls, len(controller.requests))
	}
	if controller.requests[0].CapabilityOperationID != operationID+"_attempt_1" ||
		controller.requests[1].CapabilityOperationID != operationID+"_attempt_2" ||
		controller.requests[0].CapabilityOperationID == controller.requests[1].CapabilityOperationID {
		t.Fatalf("reservation operation IDs = %q/%q", controller.requests[0].CapabilityOperationID, controller.requests[1].CapabilityOperationID)
	}
	if len(controller.releasedIDs) != 1 || controller.releasedIDs[0] != "reservation-1" || controller.committedReservationID != "reservation-2" {
		t.Fatalf("released/committed = %#v/%q", controller.releasedIDs, controller.committedReservationID)
	}
}

func TestWriterFenceRejectsBeforeReservationAndProvider(t *testing.T) {
	t.Parallel()
	executor := &countingEmbeddingExecutor{delegate: provider.NewDeterministicCapabilityExecutor()}
	controller := &quotaControllerStub{}
	runtime := capability.New(rejectingCapabilityStore{}, provider.NewRouter(embeddingRoute("route", executor, 0)), capability.Options{QuotaController: controller})
	input := "a"
	_, _, err := runtime.Embed(context.Background(), embeddingRequest(input))
	if !errors.Is(err, store.ErrConflict) || executor.calls != 0 || controller.reserveCalls != 0 {
		t.Fatalf("error/provider/reservations = %v/%d/%d", err, executor.calls, controller.reserveCalls)
	}
}

func TestPaidEmbeddingWithoutUsageEvidenceBecomesUncertain(t *testing.T) {
	t.Parallel()
	input := "a"
	executor := &countingEmbeddingExecutor{delegate: fixedEmbeddingExecutor{result: core.EmbeddingResult{
		Model: "embed-model", Data: []core.EmbeddingData{{Index: 0, Embedding: []float64{0.5}}}, InputUnits: 0, Dimensions: 1,
		ProviderUsage: []byte(`{}`),
	}}}
	route := embeddingRoute("route", executor, 0)
	route.PriceSnapshot.EmbeddingInputPerMillionMicros = 1
	controller := &quotaControllerStub{}
	runtime := capability.New(store.NewMemoryResponseStore(), provider.NewRouter(route), capability.Options{QuotaController: controller})
	_, _, err := runtime.Embed(context.Background(), embeddingRequest(input))
	if err == nil || !controller.uncertain || controller.committedReservationID != "" {
		t.Fatalf("error/uncertain/committed = %v/%v/%q", err, controller.uncertain, controller.committedReservationID)
	}
}

func TestEmbeddingRejectsIgnoredDimensions(t *testing.T) {
	t.Parallel()
	input := "a"
	dimensions := 2
	executor := &countingEmbeddingExecutor{delegate: fixedEmbeddingExecutor{result: core.EmbeddingResult{
		Model: "embed-model", Data: []core.EmbeddingData{{Index: 0, Embedding: []float64{0.5}}}, InputUnits: 1, Dimensions: 1,
		ProviderUsage: []byte(`{"input_units":1}`),
	}}}
	controller := &quotaControllerStub{}
	runtime := capability.New(store.NewMemoryResponseStore(), provider.NewRouter(embeddingRoute("route", executor, 0)), capability.Options{QuotaController: controller})
	request := embeddingRequest(input)
	request.Dimensions = &dimensions
	_, _, err := runtime.Embed(context.Background(), request)
	if err == nil || !controller.uncertain {
		t.Fatalf("error/uncertain = %v/%v", err, controller.uncertain)
	}
}

func TestEmbeddingRejectsInvalidBase64VectorLength(t *testing.T) {
	t.Parallel()
	input := "a"
	dimensions := 2
	executor := &countingEmbeddingExecutor{delegate: fixedEmbeddingExecutor{result: core.EmbeddingResult{
		Model: "embed-model", Data: []core.EmbeddingData{{Index: 0, Base64: "YQ=="}}, InputUnits: 1, Dimensions: 2,
		ProviderUsage: []byte(`{"input_units":1}`),
	}}}
	controller := &quotaControllerStub{}
	runtime := capability.New(store.NewMemoryResponseStore(), provider.NewRouter(embeddingRoute("route", executor, 0)), capability.Options{QuotaController: controller})
	request := embeddingRequest(input)
	request.Dimensions = &dimensions
	request.EncodingFormat = "base64"
	_, _, err := runtime.Embed(context.Background(), request)
	if err == nil || !controller.uncertain {
		t.Fatalf("error/uncertain = %v/%v", err, controller.uncertain)
	}
}

func TestRerankRequiresUsageEvidenceAndRequestedDocuments(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		result core.RerankResult
	}{
		{"empty usage object", core.RerankResult{
			Model: "rerank-model", Documents: 1,
			ProviderUsage: []byte(`{}`),
			Results:       []core.RerankResultItem{{Index: 0, RelevanceScore: 1, Document: &core.RerankDocument{Text: "document"}}},
		}},
		{"missing returned document", core.RerankResult{
			Model: "rerank-model", Documents: 1, ProviderUsage: []byte(`{"documents":1}`),
			Results: []core.RerankResultItem{{Index: 0, RelevanceScore: 1}},
		}},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			controller := &quotaControllerStub{}
			route := provider.Route{
				ID: "route", Provider: "provider", Model: "rerank-model", Region: "local", HomeRegion: "local", Healthy: true,
				RerankExecutor: fixedRerankExecutor{result: test.result},
				Profile:        provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{"rerank": provider.CapabilityNative}},
				PriceSnapshot:  core.PriceSnapshot{ID: "price", Provider: "provider", Model: "rerank-model", Region: "local", Currency: "USD", EffectiveAt: 1, Source: "test"},
			}
			runtime := capability.New(store.NewMemoryResponseStore(), provider.NewRouter(route), capability.Options{QuotaController: controller})
			_, _, err := runtime.Rerank(context.Background(), core.RerankRequest{
				CapabilityPrincipal: core.CapabilityPrincipal{
					TenantID: "tenant-a", APIKeyID: "key-a", HomeRegion: "local", ExecutionEpoch: 1,
					TenantPolicy: &core.TenantPolicy{Revision: 1}, APIKeyPolicy: &core.APIKeyPolicy{Revision: 1},
				},
				Model: "rerank-model", Query: "query", Documents: []core.RerankDocument{{Text: "document"}}, ReturnDocuments: true,
			})
			if err == nil || !controller.uncertain {
				t.Fatalf("error/uncertain = %v/%v", err, controller.uncertain)
			}
		})
	}
}

func TestRerankStripsUnrequestedProviderDocuments(t *testing.T) {
	t.Parallel()
	controller := &quotaControllerStub{}
	route := provider.Route{
		ID: "route", Provider: "provider", Model: "rerank-model", Region: "local", HomeRegion: "local", Healthy: true,
		RerankExecutor: fixedRerankExecutor{result: core.RerankResult{
			Model: "rerank-model", Documents: 1, ProviderTokens: 2, ProviderUsage: []byte(`{"provider_tokens":2}`),
			Results: []core.RerankResultItem{{Index: 0, RelevanceScore: 1, Document: &core.RerankDocument{Text: "document"}}},
		}},
		Profile:       provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{"rerank": provider.CapabilityNative}},
		PriceSnapshot: core.PriceSnapshot{ID: "price", Provider: "provider", Model: "rerank-model", Region: "local", Currency: "USD", EffectiveAt: 1, Source: "test"},
	}
	runtime := capability.New(store.NewMemoryResponseStore(), provider.NewRouter(route), capability.Options{QuotaController: controller})
	_, result, err := runtime.Rerank(context.Background(), core.RerankRequest{
		CapabilityPrincipal: core.CapabilityPrincipal{
			TenantID: "tenant-a", APIKeyID: "key-a", HomeRegion: "local", ExecutionEpoch: 1,
			TenantPolicy: &core.TenantPolicy{Revision: 1}, APIKeyPolicy: &core.APIKeyPolicy{Revision: 1},
		},
		Model: "rerank-model", Query: "query", Documents: []core.RerankDocument{{Text: "document"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 1 || result.Results[0].Document != nil {
		t.Fatalf("unrequested document leaked: %#v", result.Results)
	}
}

func TestPaidModerationRejectsEmptyUsageObject(t *testing.T) {
	t.Parallel()
	controller := &quotaControllerStub{}
	route := provider.Route{
		ID: "route", Provider: "provider", Model: "moderation-model", Region: "local", HomeRegion: "local", Healthy: true,
		ModerationExecutor: fixedModerationExecutor{result: core.ModerationResult{
			Model: "moderation-model", Results: []core.ModerationResultItem{{}}, ProviderUsage: []byte(`{}`),
		}},
		Profile: provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{"moderation": provider.CapabilityNative}},
		PriceSnapshot: core.PriceSnapshot{
			ID: "price", Provider: "provider", Model: "moderation-model", Region: "local", Currency: "USD", EffectiveAt: 1,
			Source: "test", ModerationInputPerMillionMicros: 1,
		},
	}
	runtime := capability.New(store.NewMemoryResponseStore(), provider.NewRouter(route), capability.Options{QuotaController: controller})
	_, _, err := runtime.Moderate(context.Background(), core.ModerationRequest{
		CapabilityPrincipal: core.CapabilityPrincipal{
			TenantID: "tenant-a", APIKeyID: "key-a", HomeRegion: "local", ExecutionEpoch: 1,
			TenantPolicy: &core.TenantPolicy{Revision: 1}, APIKeyPolicy: &core.APIKeyPolicy{Revision: 1},
		},
		Model: "moderation-model", Input: []string{"text"},
	})
	if err == nil || !controller.uncertain {
		t.Fatalf("error/uncertain = %v/%v", err, controller.uncertain)
	}
}

func embeddingRoute(id string, executor provider.EmbeddingExecutor, cost float64) provider.Route {
	return provider.Route{
		ID: id, Provider: "provider", Model: "embed-model", Region: "local", HomeRegion: "local", Healthy: true,
		InputCost: cost, EmbeddingExecutor: executor,
		Profile:       provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{"embeddings": provider.CapabilityNative}},
		PriceSnapshot: core.PriceSnapshot{ID: "price-" + id, Provider: "provider", Model: "embed-model", Region: "local", Currency: "USD", EffectiveAt: 1, Source: "test"},
	}
}

func embeddingRequest(input string) core.EmbeddingRequest {
	return core.EmbeddingRequest{
		CapabilityPrincipal: core.CapabilityPrincipal{
			TenantID: "tenant-a", APIKeyID: "key-a", HomeRegion: "local", ExecutionEpoch: 1,
			TenantPolicy: &core.TenantPolicy{Revision: 1}, APIKeyPolicy: &core.APIKeyPolicy{Revision: 1},
		},
		Model: "embed-model", Input: []core.EmbeddingInput{{Text: &input}},
	}
}

type fixedEmbeddingExecutor struct{ result core.EmbeddingResult }

func (e fixedEmbeddingExecutor) Embed(context.Context, core.EmbeddingRequest) (core.EmbeddingResult, error) {
	return e.result, nil
}

type fixedRerankExecutor struct{ result core.RerankResult }

func (e fixedRerankExecutor) Rerank(context.Context, core.RerankRequest) (core.RerankResult, error) {
	return e.result, nil
}

type fixedModerationExecutor struct{ result core.ModerationResult }

func (e fixedModerationExecutor) Moderate(context.Context, core.ModerationRequest) (core.ModerationResult, error) {
	return e.result, nil
}

type rejectingCapabilityStore struct{}

func (rejectingCapabilityStore) AssertCapabilityWriter(context.Context, string, string, int64) error {
	return store.ErrConflict
}

func (rejectingCapabilityStore) ExecuteWithCapabilityWriterFence(
	context.Context, string, string, int64, func(context.Context) error,
) error {
	return store.ErrConflict
}

func (rejectingCapabilityStore) RecordCapabilityUsage(context.Context, core.CapabilityUsageRecord) error {
	return nil
}

type countingEmbeddingExecutor struct {
	calls    int
	delegate provider.EmbeddingExecutor
	err      error
}

type countingRerankExecutor struct {
	calls    int
	delegate provider.RerankExecutor
}

func (e *countingRerankExecutor) Rerank(ctx context.Context, request core.RerankRequest) (core.RerankResult, error) {
	e.calls++
	return e.delegate.Rerank(ctx, request)
}

func (e *countingEmbeddingExecutor) Embed(ctx context.Context, request core.EmbeddingRequest) (core.EmbeddingResult, error) {
	e.calls++
	if e.err != nil {
		return core.EmbeddingResult{}, e.err
	}
	return e.delegate.Embed(ctx, request)
}

type quotaControllerStub struct {
	reserveCalls           int
	request                quota.ReservationRequest
	committed              quota.ActualUsage
	released               bool
	uncertain              bool
	requests               []quota.ReservationRequest
	releasedIDs            []string
	committedReservationID string
}

func (s *quotaControllerStub) Reserve(_ context.Context, request quota.ReservationRequest) (quota.Reservation, error) {
	s.reserveCalls++
	s.request = request
	s.requests = append(s.requests, request)
	return quota.Reservation{ID: fmt.Sprintf("reservation-%d", s.reserveCalls), TenantID: request.TenantID, APIKeyID: request.APIKeyID}, nil
}

func (s *quotaControllerStub) Commit(_ context.Context, reservationID string, actual quota.ActualUsage) error {
	s.committed = actual
	s.committedReservationID = reservationID
	return nil
}

func (s *quotaControllerStub) Release(_ context.Context, reservationID string) error {
	s.released = true
	s.releasedIDs = append(s.releasedIDs, reservationID)
	return nil
}
func (s *quotaControllerStub) Uncertain(context.Context, string) error {
	s.uncertain = true
	return nil
}
func (s *quotaControllerStub) ReserveRefresh(context.Context, quota.RefreshReservationRequest) (quota.Reservation, error) {
	return quota.Reservation{}, nil
}
func (s *quotaControllerStub) Reconcile(context.Context, int) (int, error) { return 0, nil }
