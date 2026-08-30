package provider

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/toddzheng/llm-gateway/internal/core"
)

var ErrRouteConcurrencyLimit = errors.New("Model Route concurrency limit reached")

// WithConcurrencyLimit applies one shared, process-local admission limit to
// every execution seam exposed by a Model Route. The permit follows a
// streaming Response until the stream is closed or reaches an error/EOF.
func WithConcurrencyLimit(route Route) Route {
	if route.MaxConcurrency <= 0 {
		return route
	}
	limiter := &routeConcurrencyLimiter{limit: int64(route.MaxConcurrency)}
	if route.Executor != nil {
		route.Executor = limitedResponseExecutor{next: route.Executor, limiter: limiter}
	}
	if route.EmbeddingExecutor != nil {
		route.EmbeddingExecutor = limitedEmbeddingExecutor{next: route.EmbeddingExecutor, limiter: limiter}
	}
	if route.ModerationExecutor != nil {
		route.ModerationExecutor = limitedModerationExecutor{next: route.ModerationExecutor, limiter: limiter}
	}
	if route.RerankExecutor != nil {
		route.RerankExecutor = limitedRerankExecutor{next: route.RerankExecutor, limiter: limiter}
	}
	return route
}

type routeConcurrencyLimiter struct {
	limit  int64
	active atomic.Int64
}

func (limiter *routeConcurrencyLimiter) acquire() bool {
	for {
		current := limiter.active.Load()
		if current >= limiter.limit {
			return false
		}
		if limiter.active.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

func (limiter *routeConcurrencyLimiter) release() { limiter.active.Add(-1) }

type limitedResponseExecutor struct {
	next    ResponseExecutor
	limiter *routeConcurrencyLimiter
}

func (executor limitedResponseExecutor) Execute(ctx context.Context, request core.Request) (EventStream, error) {
	if !executor.limiter.acquire() {
		return nil, ErrRouteConcurrencyLimit
	}
	stream, err := executor.next.Execute(ctx, request)
	if err != nil {
		executor.limiter.release()
		return nil, err
	}
	return &limitedEventStream{next: stream, release: executor.limiter.release}, nil
}

type limitedEventStream struct {
	next    EventStream
	release func()
	once    sync.Once
}

func (stream *limitedEventStream) Recv() (core.Event, error) {
	event, err := stream.next.Recv()
	if err != nil {
		stream.once.Do(stream.release)
	}
	return event, err
}

func (stream *limitedEventStream) Close() error {
	stream.once.Do(stream.release)
	return stream.next.Close()
}

type limitedEmbeddingExecutor struct {
	next    EmbeddingExecutor
	limiter *routeConcurrencyLimiter
}

func (executor limitedEmbeddingExecutor) Embed(ctx context.Context, request core.EmbeddingRequest) (core.EmbeddingResult, error) {
	if !executor.limiter.acquire() {
		return core.EmbeddingResult{}, ErrRouteConcurrencyLimit
	}
	defer executor.limiter.release()
	return executor.next.Embed(ctx, request)
}

type limitedModerationExecutor struct {
	next    ModerationExecutor
	limiter *routeConcurrencyLimiter
}

func (executor limitedModerationExecutor) Moderate(ctx context.Context, request core.ModerationRequest) (core.ModerationResult, error) {
	if !executor.limiter.acquire() {
		return core.ModerationResult{}, ErrRouteConcurrencyLimit
	}
	defer executor.limiter.release()
	return executor.next.Moderate(ctx, request)
}

type limitedRerankExecutor struct {
	next    RerankExecutor
	limiter *routeConcurrencyLimiter
}

func (executor limitedRerankExecutor) Rerank(ctx context.Context, request core.RerankRequest) (core.RerankResult, error) {
	if !executor.limiter.acquire() {
		return core.RerankResult{}, ErrRouteConcurrencyLimit
	}
	defer executor.limiter.release()
	return executor.next.Rerank(ctx, request)
}
