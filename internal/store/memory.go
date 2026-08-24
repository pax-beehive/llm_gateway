package store

import (
	"context"
	"errors"
	"sync"

	"github.com/toddzheng/llm-gateway/internal/core"
)

var (
	ErrNotFound            = errors.New("response not found")
	ErrConflict            = errors.New("response revision conflict")
	ErrIdempotencyMismatch = errors.New("idempotency key was already used with a different request")
)

type ResponseStore interface {
	Create(context.Context, string, core.Response) error
	Get(context.Context, string, string) (core.Response, error)
	Update(context.Context, string, core.Response, int64) error
	Delete(context.Context, string, string, int64) error
	ListInputItems(context.Context, string, string) ([]core.Item, error)
}

type IdempotentResponseStore interface {
	CreateIdempotent(context.Context, string, core.Response, string, string, []byte) (core.Response, bool, error)
}

type idempotencyRecord struct {
	requestHash []byte
	responseID  string
}

type MemoryResponseStore struct {
	mu          sync.RWMutex
	responses   map[string]map[string]core.Response
	idempotency map[string]map[string]idempotencyRecord
}

func NewMemoryResponseStore() *MemoryResponseStore {
	return &MemoryResponseStore{
		responses:   make(map[string]map[string]core.Response),
		idempotency: make(map[string]map[string]idempotencyRecord),
	}
}

func (s *MemoryResponseStore) CreateIdempotent(_ context.Context, tenantID string, response core.Response, operation, key string, requestHash []byte) (core.Response, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idempotency[tenantID] == nil {
		s.idempotency[tenantID] = make(map[string]idempotencyRecord)
	}
	lookupKey := operation + "\x00" + key
	if record, exists := s.idempotency[tenantID][lookupKey]; exists {
		if !equalBytes(record.requestHash, requestHash) {
			return core.Response{}, false, ErrIdempotencyMismatch
		}
		return cloneResponse(s.responses[tenantID][record.responseID]), false, nil
	}
	if s.responses[tenantID] == nil {
		s.responses[tenantID] = make(map[string]core.Response)
	}
	if _, exists := s.responses[tenantID][response.ID]; exists {
		return core.Response{}, false, ErrConflict
	}
	s.responses[tenantID][response.ID] = cloneResponse(response)
	s.idempotency[tenantID][lookupKey] = idempotencyRecord{
		requestHash: append([]byte(nil), requestHash...), responseID: response.ID,
	}
	return response, true, nil
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

func equalBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var different byte
	for index := range left {
		different |= left[index] ^ right[index]
	}
	return different == 0
}
