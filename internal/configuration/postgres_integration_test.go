//go:build integration

package configuration_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/toddzheng/llm-gateway/internal/configuration"
	"github.com/toddzheng/llm-gateway/internal/store"
)

func TestPostgresConfigurationHistoryUsesCASAndKeepsImmutableRevisions(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := store.NewPostgresResponseStore(db).Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	kind := fmt.Sprintf("routes-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM configuration_heads WHERE kind = $1`, kind)
		_, _ = db.Exec(`DELETE FROM configuration_history WHERE kind = $1`, kind)
	})
	repository := configuration.NewPostgresRepository(db)
	first, err := repository.Publish(ctx, kind, 0, 1, json.RawMessage(`{"route":"a"}`), "test")
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision != 1 {
		t.Fatalf("first revision = %d", first.Revision)
	}
	if _, err := repository.Publish(ctx, kind, 0, 2, json.RawMessage(`{"route":"wrong"}`), "test"); !errors.Is(err, configuration.ErrConflict) {
		t.Fatalf("stale publish error = %v, want conflict", err)
	}
	second, err := repository.Publish(ctx, kind, 1, 2, json.RawMessage(`{"route":"b"}`), "test")
	if err != nil {
		t.Fatal(err)
	}
	current, err := repository.Current(ctx, kind)
	if err != nil {
		t.Fatal(err)
	}
	if current.Revision != second.Revision || string(current.Payload) != `{"route": "b"}` {
		// PostgreSQL jsonb normalizes insignificant whitespace.
		var value map[string]string
		if json.Unmarshal(current.Payload, &value) != nil || value["route"] != "b" {
			t.Fatalf("current snapshot = %#v", current)
		}
	}
	var historyCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM configuration_history WHERE kind = $1`, kind).Scan(&historyCount); err != nil {
		t.Fatal(err)
	}
	if historyCount != 2 {
		t.Fatalf("history count = %d, want 2 immutable revisions", historyCount)
	}
}
