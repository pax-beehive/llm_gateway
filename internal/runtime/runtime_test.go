package runtime_test

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"

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

func testRoute(id string, executor provider.ResponseExecutor) provider.Route {
	return provider.Route{
		ID: id, Provider: id, Model: "gateway-model", Region: "local", HomeRegion: "local", Healthy: true,
		Executor: executor,
		Profile:  provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{"text": provider.CapabilityNative}},
	}
}
