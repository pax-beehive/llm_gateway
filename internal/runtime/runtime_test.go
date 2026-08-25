package runtime_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
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

func TestVisibleProviderEOFWithoutTerminalUsageIsFailure(t *testing.T) {
	t.Parallel()
	executor := executorFunc(func(context.Context, core.Request) (provider.EventStream, error) {
		return &scriptedStream{steps: []streamStep{{event: core.Event{Type: "response.output_text.delta", Delta: "partial"}}}}, nil
	})
	responseStore := store.NewMemoryResponseStore()
	engine := gatewayruntime.New(responseStore, provider.NewRouter(testRoute("incomplete", executor)))
	response, err := engine.Execute(context.Background(), core.Request{
		TenantID: "tenant-a", Model: "gateway-model", Store: true, RequestedFeatures: []string{"text"},
	})
	if !errors.Is(err, io.ErrUnexpectedEOF) || response.Status != core.ResponseStatusFailed || response.OutputText() != "partial" {
		t.Fatalf("response/status/error = %#v / %v", response, err)
	}
}

func TestStoreFalseFailureReturnsContentButDoesNotRetainIt(t *testing.T) {
	t.Parallel()

	executor := executorFunc(func(context.Context, core.Request) (provider.EventStream, error) {
		return &scriptedStream{steps: []streamStep{
			{event: core.Event{Type: "response.output_text.delta", Delta: "sensitive partial"}},
			{err: errors.New("sensitive provider failure")},
		}}, nil
	})
	responseStore := store.NewMemoryResponseStore()
	engine := gatewayruntime.New(responseStore, provider.NewRouter(testRoute("ephemeral-failure", executor)))
	response, err := engine.Execute(context.Background(), core.Request{
		TenantID: "tenant-a", Model: "gateway-model", Store: false,
		Input:             []core.Item{{Type: "message", Role: "user", Content: []core.Content{{Type: "input_text", Text: "sensitive prompt"}}}},
		RequestedFeatures: []string{"text"},
	})
	if err == nil || response.OutputText() != "sensitive partial" || response.Error == nil || response.Error.Message != "sensitive provider failure" {
		t.Fatalf("ephemeral failed response/error = %#v / %v", response, err)
	}
	if _, getErr := engine.Get(context.Background(), "tenant-a", response.ID); !errors.Is(getErr, store.ErrNotFound) {
		t.Fatalf("get error = %v, want not found after store:false failure", getErr)
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

func TestTenantPolicyEnforcesConcurrentResponseQuota(t *testing.T) {
	t.Parallel()
	stream := newSignalledBlockingStream()
	executor := executorFunc(func(context.Context, core.Request) (provider.EventStream, error) { return stream, nil })
	engine := gatewayruntime.NewWithOptions(store.NewMemoryResponseStore(), provider.NewRouter(testRoute("quota", executor)), gatewayruntime.Options{
		ProviderIdleTimeout: time.Second,
		TenantPolicies: map[string]core.TenantPolicy{
			"tenant-a": {MaxConcurrentResponses: 1},
		},
	})
	finished := make(chan error, 1)
	go func() {
		_, err := engine.Execute(context.Background(), core.Request{
			TenantID: "tenant-a", Model: "gateway-model", Store: true, RequestedFeatures: []string{"text"},
		})
		finished <- err
	}()
	<-stream.started
	_, err := engine.Execute(context.Background(), core.Request{
		TenantID: "tenant-a", Model: "gateway-model", Store: true, RequestedFeatures: []string{"text"},
	})
	if !errors.Is(err, gatewayruntime.ErrQuotaExceeded) {
		t.Fatalf("second execute error = %v, want quota exceeded", err)
	}
	_ = stream.Close()
	if err := <-finished; err == nil {
		t.Fatal("first blocked execution unexpectedly completed")
	}
}

func TestLostGlobalQuotaLeaseCancelsProviderExecution(t *testing.T) {
	t.Parallel()
	stream := newBlockingStream()
	var coordinationErrors, cacheErrors atomic.Int64
	responseStore := &failingRenewalStore{MemoryResponseStore: store.NewMemoryResponseStore(), renewed: make(chan struct{})}
	engine := gatewayruntime.NewWithOptions(responseStore, provider.NewRouter(testRoute("quota-renewal", executorFunc(
		func(context.Context, core.Request) (provider.EventStream, error) { return stream, nil },
	))), gatewayruntime.Options{
		ProviderIdleTimeout: time.Second, QuotaLeaseTTL: 30 * time.Millisecond, QuotaRenewInterval: 5 * time.Millisecond,
		OnCoordinationError: func(error) { coordinationErrors.Add(1) },
		OnCacheError:        func(error) { cacheErrors.Add(1) },
		TenantPolicies:      map[string]core.TenantPolicy{"tenant-a": {MaxConcurrentResponses: 1}},
	})
	_, err := engine.Execute(context.Background(), core.Request{
		TenantID: "tenant-a", Model: "gateway-model", Store: true, RequestedFeatures: []string{"text"},
	})
	if err == nil {
		t.Fatal("execution continued after global quota lease renewal failed")
	}
	select {
	case <-responseStore.renewed:
	default:
		t.Fatal("global quota lease was never renewed")
	}
	if coordinationErrors.Load() != 1 || cacheErrors.Load() != 0 {
		t.Fatalf("coordination/cache errors = %d/%d, want 1/0", coordinationErrors.Load(), cacheErrors.Load())
	}
}

func TestTenantPolicyRejectsDisallowedCacheContentInspection(t *testing.T) {
	t.Parallel()
	allowCache := true
	engine := gatewayruntime.NewWithOptions(store.NewMemoryResponseStore(), provider.NewRouter(testRoute("policy", provider.NewEchoExecutor())), gatewayruntime.Options{
		CacheProtectionMode: gatewayruntime.CacheProtectionShadowMode,
		TenantPolicies: map[string]core.TenantPolicy{
			"tenant-a": {AllowCacheProtection: &allowCache},
		},
	})
	_, err := engine.Execute(context.Background(), core.Request{
		TenantID: "tenant-a", Model: "gateway-model", Store: true, RequestedFeatures: []string{"text"},
		CacheProtection: &core.CacheProtectionPolicy{Enabled: true, AllowContentInspection: true},
	})
	if err == nil || !strings.Contains(err.Error(), "content inspection") {
		t.Fatalf("execute error = %v, want tenant content-inspection policy rejection", err)
	}
}

func TestCacheProtectionDefaultsOffAndRejectsRequestOptIn(t *testing.T) {
	t.Parallel()
	allowCache := true
	engine := gatewayruntime.NewWithOptions(store.NewMemoryResponseStore(), provider.NewRouter(testRoute("policy", provider.NewEchoExecutor())), gatewayruntime.Options{
		TenantPolicies: map[string]core.TenantPolicy{
			"tenant-a": {AllowCacheProtection: &allowCache},
		},
	})
	_, err := engine.Execute(context.Background(), core.Request{
		TenantID: "tenant-a", Model: "gateway-model", Store: true, RequestedFeatures: []string{"text"},
		CacheProtection: &core.CacheProtectionPolicy{
			Enabled: true, MaxSpendMicros: 1_000_000, MaxRefreshes: 1, MaxProtectionWindowSec: 3600,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "disabled by gateway policy") {
		t.Fatalf("execute error = %v, want gateway-level Cache Protection rejection", err)
	}
}

func TestCacheProtectionRequiresExplicitTenantOptIn(t *testing.T) {
	t.Parallel()
	engine := gatewayruntime.NewWithOptions(store.NewMemoryResponseStore(), provider.NewRouter(testRoute("policy", provider.NewEchoExecutor())), gatewayruntime.Options{
		CacheProtectionMode: gatewayruntime.CacheProtectionShadowMode,
	})
	_, err := engine.Execute(context.Background(), core.Request{
		TenantID: "tenant-a", Model: "gateway-model", Store: true, RequestedFeatures: []string{"text"},
		CacheProtection: &core.CacheProtectionPolicy{
			Enabled: true, MaxSpendMicros: 1_000_000, MaxRefreshes: 1, MaxProtectionWindowSec: 3600,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "tenant policy does not allow") {
		t.Fatalf("execute error = %v, want Tenant-level Cache Protection rejection", err)
	}
}

func TestCacheProtectionOffSkipsCacheLifecycleForOrdinaryRequests(t *testing.T) {
	t.Parallel()
	adapter := &cacheAdapterStub{}
	route := testRoute("cache-route", provider.NewEchoExecutor())
	route.CacheProtector = adapter
	route.CacheAnchorBuilder = adapter
	route.PriceSnapshot = core.PriceSnapshot{
		ID: "price-v1", Provider: route.Provider, Model: "gateway-model", Region: "local", Currency: "USD",
		EffectiveAt: 1, Source: "test",
	}
	engine := gatewayruntime.NewWithOptions(store.NewMemoryResponseStore(), provider.NewRouter(route), gatewayruntime.Options{
		CacheCoordinator: cacheprotection.NewCoordinator(cacheprotection.NewMemoryIntentRepository(), time.Now),
	})
	if _, err := engine.Execute(context.Background(), core.Request{
		TenantID: "tenant-a", Model: "gateway-model", Store: true, RequestedFeatures: []string{"text"},
	}); err != nil {
		t.Fatal(err)
	}
	if adapter.currentAnchorCalls.Load() != 0 || adapter.buildAnchorCalls.Load() != 0 {
		t.Fatalf("current/build Cache Anchor calls = %d/%d, want 0/0 while Cache Protection is off",
			adapter.currentAnchorCalls.Load(), adapter.buildAnchorCalls.Load())
	}
}

func TestStaleExecutionEpochCannotCancelOrDeletePromotedResponse(t *testing.T) {
	t.Parallel()
	responseStore := store.NewMemoryResponseStore()
	response := core.Response{
		ID: "resp-promoted", Object: "response", Status: core.ResponseStatusInProgress,
		HomeRegion: "us-west", ExecutionEpoch: 2, Revision: 1, RetainContent: true,
	}
	if err := responseStore.Create(context.Background(), "tenant-a", response); err != nil {
		t.Fatal(err)
	}
	engine := gatewayruntime.New(responseStore, provider.NewStaticRouter(provider.NewEchoExecutor()))
	if _, err := engine.Cancel(context.Background(), "tenant-a", response.ID, "us-west", 1); err == nil {
		t.Fatal("stale execution epoch cancelled promoted Response")
	}
	if err := engine.Delete(context.Background(), "tenant-a", response.ID, "us-west", 1); err == nil {
		t.Fatal("stale execution epoch deleted promoted Response")
	}
	if _, err := engine.Get(context.Background(), "tenant-a", response.ID); err != nil {
		t.Fatalf("promoted Response disappeared: %v", err)
	}
}

func TestResponseFinalizationRecordsNormalizedUsageAgainstImmutablePriceSnapshot(t *testing.T) {
	t.Parallel()

	usage := core.Usage{
		InputTokens: 1_000_000, CachedInputTokens: 800_000, CacheWriteInputTokens: 100_000,
		OutputTokens: 100_000, TotalTokens: 1_100_000,
	}
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
		CacheWritePerMillionMicros: 12_000_000, OutputPerMillionMicros: 20_000_000,
		EffectiveAt: 1, Source: "test-contract",
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
	if record.AmountMicros != 5_000_000 || record.PriceSnapshot.ID != "price-v1" {
		t.Fatalf("usage amount/snapshot = %d/%q, want 5000000/price-v1", record.AmountMicros, record.PriceSnapshot.ID)
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
	allowCache := true
	engine := gatewayruntime.NewWithOptions(responseStore, provider.NewRouter(route), gatewayruntime.Options{
		CacheCoordinator: coordinator, CacheProtectionMode: gatewayruntime.CacheProtectionAnthropicCanaryMode,
		TenantPolicies: map[string]core.TenantPolicy{"tenant-a": {AllowCacheProtection: &allowCache}},
	})
	response, err := engine.Execute(context.Background(), core.Request{
		TenantID: "tenant-a", Model: "gateway-model", Store: true, RequestedFeatures: []string{"text"},
		CacheProtection: &core.CacheProtectionPolicy{
			Enabled: true, MaxSpendMicros: 1_000_000, MaxRefreshes: 1,
			MaxProtectionWindowSec: 3600, SafetyMarginMicros: 100_000,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	usageRecords := responseStore.UsageRecords("tenant-a", response.ID)
	if len(usageRecords) != 1 || usageRecords[0].HoldoutCohort != "treatment" ||
		!strings.Contains(usageRecords[0].ExperimentRevision, "route-cache-route") {
		t.Fatalf("treatment usage cohort = %#v", usageRecords)
	}
	worker := cacheprotection.NewCoordinator(repository, func() time.Time { return now.Add(4*time.Minute + 55*time.Second) })
	completed, err := worker.RunDue(context.Background(), 10, func(provider.CacheAnchor) provider.CacheProtector { return adapter })
	if err != nil {
		t.Fatal(err)
	}
	if len(completed) != 1 || completed[0].Status != cacheprotection.IntentSucceeded || adapter.refreshCalls.Load() != 1 {
		t.Fatalf("cache protection result = %#v, refresh calls = %d", completed, adapter.refreshCalls.Load())
	}
}

func TestCacheProtectionShadowModeNeverRefreshes(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	usage := core.Usage{InputTokens: 1_000_000, OutputTokens: 10, TotalTokens: 1_000_010}
	executor := executorFunc(func(context.Context, core.Request) (provider.EventStream, error) {
		return &scriptedStream{steps: []streamStep{{event: core.Event{Type: "response.completed", Usage: &usage}}}}, nil
	})
	anchor := provider.CacheAnchor{
		TenantID: "tenant-shadow", RouteID: "cache-route", Provider: "cache-provider", Model: "gateway-model",
		CredentialScope: "tenant-primary", Region: "local", CacheKey: "cache-key", PrefixHash: "sha256:shadow",
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
		ID: "cache-price-shadow", Provider: "cache-provider", Model: "gateway-model", Region: "local", Currency: "USD",
		InputPerMillionMicros: 10_000_000, CachedInputPerMillionMicros: 1_000_000,
		OutputPerMillionMicros: 1_000_000, EffectiveAt: 1, Source: "test",
	}
	repository := cacheprotection.NewMemoryIntentRepository()
	coordinator := cacheprotection.NewCoordinator(repository, time.Now)
	allowCache := true
	engine := gatewayruntime.NewWithOptions(store.NewMemoryResponseStore(), provider.NewRouter(route), gatewayruntime.Options{
		CacheCoordinator: coordinator, CacheProtectionMode: gatewayruntime.CacheProtectionShadowMode,
		TenantPolicies: map[string]core.TenantPolicy{"tenant-shadow": {AllowCacheProtection: &allowCache}},
	})
	_, err := engine.Execute(context.Background(), core.Request{
		TenantID: "tenant-shadow", Model: "gateway-model", Store: true, RequestedFeatures: []string{"text"},
		CacheProtection: &core.CacheProtectionPolicy{
			Enabled: true, MaxSpendMicros: 1_000_000, MaxRefreshes: 3,
			MaxProtectionWindowSec: 3600, SafetyMarginMicros: 100_000,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	worker := cacheprotection.NewCoordinator(repository, func() time.Time { return now.Add(10 * time.Minute) })
	completed, err := worker.RunDue(context.Background(), 10, func(provider.CacheAnchor) provider.CacheProtector { return adapter })
	if err != nil {
		t.Fatal(err)
	}
	if len(completed) != 0 || adapter.refreshCalls.Load() != 0 {
		t.Fatalf("shadow completions/refresh calls = %d/%d, want 0/0", len(completed), adapter.refreshCalls.Load())
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

type signalledBlockingStream struct {
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
}

type cacheAdapterStub struct {
	observation        provider.CacheObservation
	refreshResult      provider.RefreshResult
	refreshCalls       atomic.Int64
	currentAnchorCalls atomic.Int64
	buildAnchorCalls   atomic.Int64
}

type failingRenewalStore struct {
	*store.MemoryResponseStore
	renewed chan struct{}
	once    sync.Once
}

func (s *failingRenewalStore) AcquireResponseSlot(context.Context, string, string, int, time.Time) error {
	return nil
}

func (s *failingRenewalStore) RenewResponseSlot(context.Context, string, string, time.Time) error {
	s.once.Do(func() { close(s.renewed) })
	return errors.New("quota database unavailable")
}

func (s *failingRenewalStore) ReleaseResponseSlot(context.Context, string, string) error {
	return nil
}

func (s *cacheAdapterStub) Inspect(context.Context, provider.CacheAnchor) provider.CacheCapability {
	return provider.CacheCapability{Supported: true}
}

func (s *cacheAdapterStub) Refresh(context.Context, provider.CacheAnchor) (provider.RefreshResult, error) {
	s.refreshCalls.Add(1)
	return s.refreshResult, nil
}

func (s *cacheAdapterStub) CurrentCacheAnchor(context.Context, core.Request) (provider.CacheAnchor, bool, error) {
	s.currentAnchorCalls.Add(1)
	return s.observation.Anchor, false, nil
}

func (s *cacheAdapterStub) BuildCacheAnchor(context.Context, core.Request, core.Response) (provider.CacheObservation, error) {
	s.buildAnchorCalls.Add(1)
	return s.observation, nil
}

func newBlockingStream() *blockingStream {
	return &blockingStream{closed: make(chan struct{})}
}

func newSignalledBlockingStream() *signalledBlockingStream {
	return &signalledBlockingStream{started: make(chan struct{}), closed: make(chan struct{})}
}

func (s *signalledBlockingStream) Recv() (core.Event, error) {
	s.once.Do(func() { close(s.started) })
	<-s.closed
	return core.Event{}, io.EOF
}

func (s *signalledBlockingStream) Close() error {
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
	return nil
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
