package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/provider"
	"github.com/toddzheng/llm-gateway/internal/store"
)

type Runtime struct {
	store  store.ResponseStore
	router provider.Router
	now    func() time.Time

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

func New(responseStore store.ResponseStore, router provider.Router) *Runtime {
	return &Runtime{store: responseStore, router: router, now: time.Now, cancels: make(map[string]context.CancelFunc)}
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
	if request.TenantID == "" || request.Model == "" {
		return core.Response{}, errors.New("tenant and model are required")
	}
	if request.CompatibilityMode == "" {
		request.CompatibilityMode = core.CompatibilityStrict
	}
	if request.HomeRegion == "" {
		request.HomeRegion = "local"
	}
	if request.PreviousResponseID != "" {
		previous, err := r.store.Get(ctx, request.TenantID, request.PreviousResponseID)
		if err != nil {
			return core.Response{}, fmt.Errorf("previous response: %w", err)
		}
		if previous.HomeRegion != "" && previous.HomeRegion != request.HomeRegion {
			return core.Response{}, errors.New("previous response belongs to a different home region")
		}
		if len(previous.Attempts) > 0 {
			request.PreferredRouteID = previous.Attempts[len(previous.Attempts)-1].RouteID
		}
	}
	now := r.now().UTC()
	response := core.Response{
		ID: newID("resp"), Object: "response", CreatedAt: now.Unix(), Status: core.ResponseStatusInProgress,
		Model: request.Model, PreviousResponseID: request.PreviousResponseID, ConversationID: request.ConversationID,
		Input: append([]core.Item(nil), request.Input...), Output: []core.Item{}, Metadata: request.Metadata,
		HomeRegion: request.HomeRegion, Revision: 1,
	}
	if err := r.store.Create(ctx, request.TenantID, response); err != nil {
		return core.Response{}, fmt.Errorf("create response: %w", err)
	}
	sequence := int64(0)
	emitEvent := func(event core.Event) error {
		if emit == nil {
			return nil
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
		return r.fail(executionCtx, request.TenantID, response, "route_not_found", err)
	}

	var lastErr error
	for _, route := range routes {
		lastErr = nil
		attemptStart := r.now().UTC()
		attempt := core.Attempt{
			ID: newID("attempt"), RouteID: route.ID, Provider: route.Provider, ProviderModel: route.Model,
			Region: route.Region, StartedAt: attemptStart,
		}
		response.Attempts = append(response.Attempts, attempt)
		stream, executeErr := route.Executor.Execute(executionCtx, request)
		if executeErr != nil {
			lastErr = executeErr
			response.Attempts[len(response.Attempts)-1].Error = gatewayError("provider_error", executeErr)
			continue
		}

		visible := false
		var outputText string
		for {
			event, recvErr := stream.Recv()
			if recvErr == io.EOF {
				break
			}
			if recvErr != nil {
				lastErr = recvErr
				response.Attempts[len(response.Attempts)-1].Error = gatewayError("provider_stream_error", recvErr)
				_ = stream.Close()
				if visible {
					if outputText != "" {
						response.Output = append(response.Output, outputMessage(outputText))
					}
					return r.fail(executionCtx, request.TenantID, response, "stream_interrupted", recvErr)
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
				response.Output = append(response.Output, *event.Item)
			}
			if event.Usage != nil {
				response.Usage = *event.Usage
			}
		}
		_ = stream.Close()
		if lastErr != nil && !visible {
			continue
		}
		if outputText != "" {
			response.Output = append(response.Output, outputMessage(outputText))
		}
		completed := r.now().UTC()
		response.Status = core.ResponseStatusCompleted
		completedUnix := completed.Unix()
		response.CompletedAt = &completedUnix
		response.Attempts[len(response.Attempts)-1].CompletedAt = &completed
		if err := r.store.Update(executionCtx, request.TenantID, response, response.Revision); err != nil {
			return core.Response{}, fmt.Errorf("persist completed response: %w", err)
		}
		response.Revision++
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
	return r.fail(executionCtx, request.TenantID, response, "provider_unavailable", lastErr)
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

func (r *Runtime) Delete(ctx context.Context, tenantID, responseID string) error {
	response, err := r.store.Get(ctx, tenantID, responseID)
	if err != nil {
		return err
	}
	return r.store.Delete(ctx, tenantID, responseID, response.Revision)
}

func (r *Runtime) Cancel(ctx context.Context, tenantID, responseID string) (core.Response, error) {
	response, err := r.store.Get(ctx, tenantID, responseID)
	if err != nil {
		return core.Response{}, err
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
	if err := r.store.Update(ctx, tenantID, response, response.Revision); err != nil {
		return core.Response{}, err
	}
	response.Revision++
	return response, nil
}

func (r *Runtime) fail(ctx context.Context, tenantID string, response core.Response, code string, cause error) (core.Response, error) {
	response.Status = core.ResponseStatusFailed
	response.Error = gatewayError(code, cause)
	if err := r.store.Update(ctx, tenantID, response, response.Revision); err != nil {
		return core.Response{}, errors.Join(cause, err)
	}
	response.Revision++
	return response, cause
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
