package store

import (
	"context"
	"errors"
	"sync"

	"github.com/toddzheng/llm-gateway/internal/core"
)

var (
	ErrNotFound = errors.New("response not found")
	ErrConflict = errors.New("response revision conflict")
)

type ResponseStore interface {
	Create(context.Context, string, core.Response) error
	Get(context.Context, string, string) (core.Response, error)
	Update(context.Context, string, core.Response, int64) error
	Delete(context.Context, string, string, int64) error
	ListInputItems(context.Context, string, string) ([]core.Item, error)
}

type MemoryResponseStore struct {
	mu        sync.RWMutex
	responses map[string]map[string]core.Response
}

func NewMemoryResponseStore() *MemoryResponseStore {
	return &MemoryResponseStore{responses: make(map[string]map[string]core.Response)}
}

func (s *MemoryResponseStore) Create(_ context.Context, tenantID string, response core.Response) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.responses[tenantID] == nil {
		s.responses[tenantID] = make(map[string]core.Response)
	}
	if _, exists := s.responses[tenantID][response.ID]; exists {
		return ErrConflict
	}
	s.responses[tenantID][response.ID] = cloneResponse(response)
	return nil
}

func (s *MemoryResponseStore) Get(_ context.Context, tenantID, responseID string) (core.Response, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	response, exists := s.responses[tenantID][responseID]
	if !exists || response.Status == core.ResponseStatusDeleted {
		return core.Response{}, ErrNotFound
	}
	return cloneResponse(response), nil
}

func (s *MemoryResponseStore) Update(_ context.Context, tenantID string, response core.Response, expectedRevision int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.responses[tenantID][response.ID]
	if !exists {
		return ErrNotFound
	}
	if current.Revision != expectedRevision {
		return ErrConflict
	}
	response.Revision = expectedRevision + 1
	s.responses[tenantID][response.ID] = cloneResponse(response)
	return nil
}

func (s *MemoryResponseStore) Delete(_ context.Context, tenantID, responseID string, expectedRevision int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.responses[tenantID][responseID]
	if !exists || current.Status == core.ResponseStatusDeleted {
		return ErrNotFound
	}
	if current.Revision != expectedRevision {
		return ErrConflict
	}
	current.Status = core.ResponseStatusDeleted
	current.Input = nil
	current.Output = nil
	current.Revision++
	s.responses[tenantID][responseID] = current
	return nil
}

func (s *MemoryResponseStore) ListInputItems(ctx context.Context, tenantID, responseID string) ([]core.Item, error) {
	response, err := s.Get(ctx, tenantID, responseID)
	if err != nil {
		return nil, err
	}
	return append([]core.Item(nil), response.Input...), nil
}

func cloneResponse(response core.Response) core.Response {
	response.Input = append([]core.Item(nil), response.Input...)
	response.Output = append([]core.Item(nil), response.Output...)
	response.Attempts = append([]core.Attempt(nil), response.Attempts...)
	if response.Metadata != nil {
		response.Metadata = make(map[string]string, len(response.Metadata))
		for key, value := range response.Metadata {
			response.Metadata[key] = value
		}
	}
	return response
}
