package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/store"
)

func TestMemoryStoreScrubsResponseContentAfterRetentionExpiry(t *testing.T) {
	t.Parallel()
	expired := time.Now().Add(-time.Second).Unix()
	responseStore := store.NewMemoryResponseStore()
	response := core.Response{
		ID: "resp-expired", Object: "response", Status: core.ResponseStatusCompleted, Revision: 1,
		Input:    []core.Item{{Type: "message", Content: []core.Content{{Type: "input_text", Text: "secret"}}}},
		Output:   []core.Item{{Type: "message", Content: []core.Content{{Type: "output_text", Text: "secret"}}}},
		Metadata: map[string]string{"secret": "secret"}, ContentExpiresAt: &expired,
	}
	if err := responseStore.Create(context.Background(), "tenant-a", response); err != nil {
		t.Fatal(err)
	}
	got, err := responseStore.Get(context.Background(), "tenant-a", response.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Input) != 0 || len(got.Output) != 0 || len(got.Metadata) != 0 || got.Revision != 2 {
		t.Fatalf("expired Response retained content: %#v", got)
	}
}
