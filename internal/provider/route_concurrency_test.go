package provider_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/provider"
)

func TestRouteConcurrencyPermitFollowsStreamingResponseLifetime(t *testing.T) {
	route := provider.WithConcurrencyLimit(provider.Route{MaxConcurrency: 1, Executor: heldExecutor{}})
	first, err := route.Executor.Execute(context.Background(), core.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := route.Executor.Execute(context.Background(), core.Request{}); !errors.Is(err, provider.ErrRouteConcurrencyLimit) {
		t.Fatalf("concurrent execution error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := route.Executor.Execute(context.Background(), core.Request{})
	if err != nil {
		t.Fatalf("execution after release = %v", err)
	}
	_ = second.Close()
}

type heldExecutor struct{}

func (heldExecutor) Execute(context.Context, core.Request) (provider.EventStream, error) {
	return &heldStream{}, nil
}

type heldStream struct{}

func (*heldStream) Recv() (core.Event, error) { return core.Event{}, io.EOF }
func (*heldStream) Close() error              { return nil }
