package runtime_test

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/toddzheng/llm-gateway/internal/cacheprotection"
	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/provider"
	gatewayruntime "github.com/toddzheng/llm-gateway/internal/runtime"
	"github.com/toddzheng/llm-gateway/internal/store"
)

func TestVisibleProviderFailureIsPersistedWithoutFallback(t *testing.T) {
	t.Parallel()

	var fallbackCalls atomic.Int64
	first := executorFunc(func(context.Context, core.Request) (provider.EventStream, error) {
		return &scriptedStream{steps: []streamStep{
			{event: core.Event{Type: "response.output_text.delta", Delta: "partial"}},
			{err: errors.New("provider disconnected")},
		}}, nil
	})
	second := executorFunc(func(context.Context, core.Request) (provider.EventStream, error) {
		fallbackCalls.Add(1)
		return &scriptedStream{steps: []streamStep{{event: core.Event{Type: "response.output_text.delta", Delta: "wrong fallback"}}}}, nil
	})
	router := provider.NewRouter(
		testRoute("first", first),
		testRoute("second", second),
	)
	responseStore := store.NewMemoryResponseStore()
	engine := gatewayruntime.New(responseStore, router)

	response, err := engine.Execute(context.Background(), core.Request{
		TenantID: "tenant-a", Model: "gateway-model", Store: true,
		Input:             []core.Item{{Type: "message", Role: "user", Content: []core.Content{{Type: "input_text", Text: "hello"}}}},
		RequestedFeatures: []string{"text"},
	})
	if err == nil {
		t.Fatal("execute error = nil, want provider stream error")
	}
	if fallbackCalls.Load() != 0 {
		t.Fatalf("fallback calls = %d, want 0 after visible output", fallbackCalls.Load())
	}
	if response.Status != core.ResponseStatusFailed {
		t.Fatalf("status = %q, want failed", response.Status)
	}
	if response.OutputText() != "partial" {
		t.Fatalf("recorded partial output = %q, want partial", response.OutputText())
	}

	persisted, getErr := engine.Get(context.Background(), "tenant-a", response.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if persisted.Status != core.ResponseStatusFailed || persisted.OutputText() != "partial" {
		t.Fatalf("persisted response = %#v, want failed with partial output", persisted)
	}
}

func TestProviderIdleTimeoutEmitsKeepaliveAndPersistsFailure(t *testing.T) {
	t.Parallel()

	stream := newBlockingStream()
	executor := executorFunc(func(context.Context, core.Request) (provider.EventStream, error) {
		return stream, nil
	})
	responseStore := store.NewMemoryResponseStore()
	engine := gatewayruntime.NewWithOptions(responseStore, provider.NewRouter(testRoute("idle", executor)), gatewayruntime.Options{
		ProviderIdleTimeout: 25 * time.Millisecond,
		KeepaliveInterval:   5 * time.Millisecond,
	})
	var keepalives atomic.Int64
	response, err := engine.ExecuteStreaming(context.Background(), core.Request{
		TenantID: "tenant-a", Model: "gateway-model", Store: true, RequestedFeatures: []string{"text"},
	}, func(event core.Event) error {
		if event.Type == "gateway.keepalive" {
			keepalives.Add(1)
		}
		return nil
	})
	if !errors.Is(err, gatewayruntime.ErrProviderIdleTimeout) {
		t.Fatalf("execute error = %v, want provider idle timeout", err)
	}
	if response.Status != core.ResponseStatusFailed {
		t.Fatalf("status = %q, want failed", response.Status)
	}
	if keepalives.Load() == 0 {
		t.Fatal("keepalive count = 0, want at least one before idle timeout")
	}
}

func TestCompletionRecordsNormalizedUsageAgainstImmutablePriceSnapshot(t *testing.T) {
	t.Parallel()

	usage := core.Usage{InputTokens: 1_000_000, CachedInputTokens: 800_000, OutputTokens: 100_000, TotalTokens: 1_100_000}
	executor := executorFunc(func(context.Context, core.Request) (provider.EventStream, error) {
		return &scriptedStream{steps: []streamStep{
			{event: core.Event{Type: "response.output_text.delta", Delta: "done"}},
			{event: core.Event{Type: "response.completed", Usage: &usage, ProviderUsage: []byte(`{"cache":"reported"}`)}},
		}}, nil
	})
	route := testRoute("priced", executor)
	route.PriceSnapshot = core.PriceSnapshot{
		ID: "price-v1", Provider: "priced", Model: "gateway-model", Region: "local", Currency: "USD",
		InputPerMillionMicros: 10_000_000, CachedInputPerMillionMicros: 1_000_000,
		OutputPerMillionMicros: 20_000_000, EffectiveAt: 1, Source: "test-contract",
	}
	route.CacheUsageReliable = true
	responseStore := store.NewMemoryResponseStore()
	engine := gatewayruntime.New(responseStore, provider.NewRouter(route))
	response, err := engine.Execute(context.Background(), core.Request{
		TenantID: "tenant-a", Model: "gateway-model", Store: true, RequestedFeatures: []string{"text"},
	})
	if err != nil {
		t.Fatal(err)
	}
	records := responseStore.UsageRecords("tenant-a", response.ID)
	if len(records) != 1 {
		t.Fatalf("usage records = %#v, want one", records)
	}
	record := records[0]
	if record.AmountMicros != 4_800_000 || record.PriceSnapshot.ID != "price-v1" {
		t.Fatalf("usage amount/snapshot = %d/%q, want 4800000/price-v1", record.AmountMicros, record.PriceSnapshot.ID)
	}
	if string(record.ProviderUsage) != `{"cache":"reported"}` {
		t.Fatalf("provider usage = %s", record.ProviderUsage)
	}
}

func TestOptInResponsePlansProviderGeneratedCacheProtection(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	usage := core.Usage{InputTokens: 1_000_000, OutputTokens: 10, TotalTokens: 1_000_010}
	executor := executorFunc(func(context.Context, core.Request) (provider.EventStream, error) {
		return &scriptedStream{steps: []streamStep{
			{event: core.Event{Type: "response.output_text.delta", Delta: "done"}},
			{event: core.Event{Type: "response.completed", Usage: &usage}},
		}}, nil
	})
	anchor := provider.CacheAnchor{
		TenantID: "tenant-a", RouteID: "cache-route", Provider: "cache-provider", Model: "gateway-model",
		CredentialScope: "tenant-primary", Region: "local", CacheKey: "cache-key", PrefixHash: "sha256:prefix",
	}
	adapter := &cacheAdapterStub{
		observation: provider.CacheObservation{
			Anchor: anchor, EstimatedExpiresAt: now.Add(5 * time.Minute), PrefixTokens: 1_000_000, RefreshCostMicros: 100_000,
		},
		refreshResult: provider.RefreshResult{Status: "succeeded", ExpiresAt: now.Add(10 * time.Minute)},
	}
	route := testRoute("cache-route", executor)
	route.Provider = "cache-provider"
	route.CacheProtector = adapter
	route.CacheAnchorBuilder = adapter
	route.PriceSnapshot = core.PriceSnapshot{
		ID: "cache-price-v1", Provider: "cache-provider", Model: "gateway-model", Region: "local", Currency: "USD",
		InputPerMillionMicros: 10_000_000, CachedInputPerMillionMicros: 1_000_000,
		OutputPerMillionMicros: 1_000_000, EffectiveAt: 1, Source: "test",
	}
	repository := cacheprotection.NewMemoryIntentRepository()
	coordinator := cacheprotection.NewCoordinator(repository, time.Now)
	responseStore := store.NewMemoryResponseStore()
	engine := gatewayruntime.NewWithOptions(responseStore, provider.NewRouter(route), gatewayruntime.Options{CacheCoordinator: coordinator})
	_, err := engine.Execute(context.Background(), core.Request{
		TenantID: "tenant-a", Model: "gateway-model", Store: true, RequestedFeatures: []string{"text"},
		CacheProtection: &core.CacheProtectionPolicy{
			Enabled: true, MaxSpendMicros: 1_000_000, MaxRefreshes: 1,
			MaxProtectionWindowSec: 3600, SafetyMarginMicros: 100_000,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	worker := cacheprotection.NewCoordinator(repository, func() time.Time { return now.Add(4*time.Minute + 55*time.Second) })
	completed, err := worker.RunDue(context.Background(), 10, func(provider.CacheAnchor) provider.CacheProtector { return adapter })
	if err != nil {
		t.Fatal(err)
	}
	if len(completed) != 1 || completed[0].Status != cacheprotection.IntentSucceeded || adapter.refreshCalls.Load() != 1 {
		t.Fatalf("cache protection completion = %#v, refresh calls = %d", completed, adapter.refreshCalls.Load())
	}
}

type executorFunc func(context.Context, core.Request) (provider.EventStream, error)

func (f executorFunc) Execute(ctx context.Context, request core.Request) (provider.EventStream, error) {
	return f(ctx, request)
}

type streamStep struct {
	event core.Event
	err   error
}

type scriptedStream struct {
	steps []streamStep
	index int
}

func (s *scriptedStream) Recv() (core.Event, error) {
	if s.index >= len(s.steps) {
		return core.Event{}, io.EOF
	}
	step := s.steps[s.index]
	s.index++
	return step.event, step.err
}

func (s *scriptedStream) Close() error { return nil }

type blockingStream struct {
	closed chan struct{}
}

type cacheAdapterStub struct {
	observation   provider.CacheObservation
	refreshResult provider.RefreshResult
	refreshCalls  atomic.Int64
}

func (s *cacheAdapterStub) Inspect(context.Context, provider.CacheAnchor) provider.CacheCapability {
	return provider.CacheCapability{Supported: true}
}

func (s *cacheAdapterStub) Refresh(context.Context, provider.CacheAnchor) (provider.RefreshResult, error) {
	s.refreshCalls.Add(1)
	return s.refreshResult, nil
}

func (s *cacheAdapterStub) CurrentCacheAnchor(context.Context, core.Request) (provider.CacheAnchor, bool, error) {
	return s.observation.Anchor, false, nil
}

func (s *cacheAdapterStub) BuildCacheAnchor(context.Context, core.Request, core.Response) (provider.CacheObservation, error) {
	return s.observation, nil
}

func newBlockingStream() *blockingStream {
	return &blockingStream{closed: make(chan struct{})}
}

func (s *blockingStream) Recv() (core.Event, error) {
	<-s.closed
	return core.Event{}, io.EOF
}

func (s *blockingStream) Close() error {
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
	return nil
}

func testRoute(id string, executor provider.ResponseExecutor) provider.Route {
	return provider.Route{
		ID: id, Provider: id, Model: "gateway-model", Region: "local", HomeRegion: "local", Healthy: true,
		Executor: executor,
		Profile:  provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{"text": provider.CapabilityNative}},
	}
}
