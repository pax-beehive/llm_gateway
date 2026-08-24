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
	ErrConversationBusy    = errors.New("conversation already has an active response")
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

type ConversationStore interface {
	CreateConversation(context.Context, string, core.Conversation) error
	GetConversation(context.Context, string, string) (core.Conversation, error)
	AppendConversationItems(context.Context, string, string, []core.Item, int64) (core.Conversation, error)
	DeleteConversation(context.Context, string, string, int64) error
}

type FinancialResponseStore interface {
	CompleteWithUsage(context.Context, string, core.Response, int64, core.UsageRecord) error
}

type idempotencyRecord struct {
	requestHash []byte
	responseID  string
}

type MemoryResponseStore struct {
	mu            sync.RWMutex
	responses     map[string]map[string]core.Response
	idempotency   map[string]map[string]idempotencyRecord
	conversations map[string]map[string]core.Conversation
	usageRecords  map[string]map[string]core.UsageRecord
}

func NewMemoryResponseStore() *MemoryResponseStore {
	return &MemoryResponseStore{
		responses:     make(map[string]map[string]core.Response),
		idempotency:   make(map[string]map[string]idempotencyRecord),
		conversations: make(map[string]map[string]core.Conversation),
		usageRecords:  make(map[string]map[string]core.UsageRecord),
	}
}

func (s *MemoryResponseStore) CompleteWithUsage(_ context.Context, tenantID string, response core.Response, expectedRevision int64, usage core.UsageRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.responses[tenantID][response.ID]
	if !exists {
		return ErrNotFound
	}
	if current.Revision != expectedRevision {
		return ErrConflict
	}
	if response.Status != core.ResponseStatusCompleted {
		return errors.New("financial completion requires completed response")
	}
	if err := s.finishConversationLocked(tenantID, response); err != nil {
		return err
	}
	if s.usageRecords[tenantID] == nil {
		s.usageRecords[tenantID] = make(map[string]core.UsageRecord)
	}
	if _, exists := s.usageRecords[tenantID][usage.ID]; exists {
		return ErrConflict
	}
	response.Revision = expectedRevision + 1
	s.responses[tenantID][response.ID] = cloneResponse(response)
	s.usageRecords[tenantID][usage.ID] = cloneUsageRecord(usage)
	return nil
}

func (s *MemoryResponseStore) UsageRecords(tenantID, responseID string) []core.UsageRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var records []core.UsageRecord
	for _, record := range s.usageRecords[tenantID] {
		if record.ResponseID == responseID {
			records = append(records, cloneUsageRecord(record))
		}
	}
	return records
}

func (s *MemoryResponseStore) CreateConversation(_ context.Context, tenantID string, conversation core.Conversation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conversations[tenantID] == nil {
		s.conversations[tenantID] = make(map[string]core.Conversation)
	}
	if _, exists := s.conversations[tenantID][conversation.ID]; exists {
		return ErrConflict
	}
	s.conversations[tenantID][conversation.ID] = cloneConversation(conversation)
	return nil
}

func (s *MemoryResponseStore) GetConversation(_ context.Context, tenantID, conversationID string) (core.Conversation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	conversation, exists := s.conversations[tenantID][conversationID]
	if !exists {
		return core.Conversation{}, ErrNotFound
	}
	return cloneConversation(conversation), nil
}

func (s *MemoryResponseStore) AppendConversationItems(_ context.Context, tenantID, conversationID string, items []core.Item, expectedRevision int64) (core.Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	conversation, exists := s.conversations[tenantID][conversationID]
	if !exists {
		return core.Conversation{}, ErrNotFound
	}
	if conversation.Revision != expectedRevision {
		return core.Conversation{}, ErrConflict
	}
	if conversation.ActiveResponseID != "" {
		return core.Conversation{}, ErrConversationBusy
	}
	conversation.Items = append(conversation.Items, cloneItems(items)...)
	conversation.Revision++
	s.conversations[tenantID][conversationID] = conversation
	return cloneConversation(conversation), nil
}

func (s *MemoryResponseStore) DeleteConversation(_ context.Context, tenantID, conversationID string, expectedRevision int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	conversation, exists := s.conversations[tenantID][conversationID]
	if !exists {
		return ErrNotFound
	}
	if conversation.Revision != expectedRevision {
		return ErrConflict
	}
	if conversation.ActiveResponseID != "" {
		return ErrConversationBusy
	}
	delete(s.conversations[tenantID], conversationID)
	return nil
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
	if err := s.beginConversationLocked(tenantID, response); err != nil {
		return core.Response{}, false, err
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
	if err := s.beginConversationLocked(tenantID, response); err != nil {
		return err
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
	if isTerminal(response.Status) && !isTerminal(current.Status) {
		if err := s.finishConversationLocked(tenantID, response); err != nil {
			return err
		}
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
	response.Input = cloneItems(response.Input)
	response.Output = cloneItems(response.Output)
	response.Attempts = append([]core.Attempt(nil), response.Attempts...)
	if response.Metadata != nil {
		response.Metadata = make(map[string]string, len(response.Metadata))
		for key, value := range response.Metadata {
			response.Metadata[key] = value
		}
	}
	return response
}

func (s *MemoryResponseStore) beginConversationLocked(tenantID string, response core.Response) error {
	if response.ConversationID == "" {
		return nil
	}
	conversation, exists := s.conversations[tenantID][response.ConversationID]
	if !exists {
		return ErrNotFound
	}
	if conversation.HomeRegion != response.HomeRegion {
		return ErrConflict
	}
	if conversation.ActiveResponseID != "" {
		return ErrConversationBusy
	}
	conversation.Items = append(conversation.Items, cloneItems(response.Input)...)
	conversation.ActiveResponseID = response.ID
	conversation.Revision++
	s.conversations[tenantID][response.ConversationID] = conversation
	return nil
}

func (s *MemoryResponseStore) finishConversationLocked(tenantID string, response core.Response) error {
	if response.ConversationID == "" {
		return nil
	}
	conversation, exists := s.conversations[tenantID][response.ConversationID]
	if !exists {
		return ErrNotFound
	}
	if conversation.ActiveResponseID != response.ID {
		return ErrConflict
	}
	conversation.Items = append(conversation.Items, cloneItems(response.Output)...)
	conversation.ActiveResponseID = ""
	conversation.Revision++
	s.conversations[tenantID][response.ConversationID] = conversation
	return nil
}

func isTerminal(status core.ResponseStatus) bool {
	return status == core.ResponseStatusCompleted || status == core.ResponseStatusFailed || status == core.ResponseStatusCancelled
}

func cloneConversation(conversation core.Conversation) core.Conversation {
	conversation.Items = cloneItems(conversation.Items)
	if conversation.Metadata != nil {
		metadata := make(map[string]string, len(conversation.Metadata))
		for key, value := range conversation.Metadata {
			metadata[key] = value
		}
		conversation.Metadata = metadata
	}
	return conversation
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

func cloneUsageRecord(record core.UsageRecord) core.UsageRecord {
	record.ProviderUsage = append([]byte(nil), record.ProviderUsage...)
	if record.ProtectedHit != nil {
		protected := *record.ProtectedHit
		record.ProtectedHit = &protected
	}
	return record
}
