package metering_test

import (
	"context"
	"testing"

	"github.com/toddzheng/llm-gateway/internal/metering"
)

func TestFileExportStoreIsCreateOnlyAndRetrySafe(t *testing.T) {
	store, err := metering.NewFileExportStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), "export.csv", []byte("stable")); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), "export.csv", []byte("stable")); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	if err := store.Put(context.Background(), "export.csv", []byte("different")); err == nil {
		t.Fatal("immutable export was overwritten")
	}
	payload, err := store.Get(context.Background(), "export.csv")
	if err != nil || string(payload) != "stable" {
		t.Fatalf("stored payload=%q/%v", payload, err)
	}
}
