package secretcustody

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
)

var (
	ErrConflict = errors.New("secret custody idempotency conflict")
	ErrNotFound = errors.New("secret custody reference not found")
)

type Reference struct {
	Name    string
	Version string
}

type Store interface {
	Put(context.Context, string, []byte) (Reference, error)
	Access(context.Context, Reference) ([]byte, error)
}

type Memory struct {
	mu      sync.RWMutex
	secrets map[string][]byte
}

func NewMemory() *Memory {
	return &Memory{secrets: make(map[string][]byte)}
}

func (store *Memory) Put(_ context.Context, key string, material []byte) (Reference, error) {
	if key == "" || len(material) == 0 {
		return Reference{}, errors.New("secret custody key and material are required")
	}
	digest := sha256.Sum256([]byte(key))
	name := "memory://provider-connection/" + hex.EncodeToString(digest[:])
	store.mu.Lock()
	defer store.mu.Unlock()
	if current, exists := store.secrets[name]; exists {
		if !bytes.Equal(current, material) {
			return Reference{}, ErrConflict
		}
		return Reference{Name: name, Version: "1"}, nil
	}
	store.secrets[name] = append([]byte(nil), material...)
	return Reference{Name: name, Version: "1"}, nil
}

func (store *Memory) Access(_ context.Context, reference Reference) ([]byte, error) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	material, exists := store.secrets[reference.Name]
	if !exists {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, reference.Name)
	}
	return append([]byte(nil), material...), nil
}
