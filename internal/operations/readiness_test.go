package operations_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/toddzheng/llm-gateway/internal/operations"
)

func TestReadyzIsBoundedAndNamesUnavailableDependenciesWithoutLeakingErrors(t *testing.T) {
	now := time.Date(2026, 8, 29, 20, 0, 0, 0, time.UTC)
	probe := operations.NewProbe(20*time.Millisecond, func() time.Time { return now }, map[string]operations.Check{
		"database":        func(context.Context) error { return nil },
		"routing_catalog": func(context.Context) error { return errors.New("secret provider body") },
		"stuck":           func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
	})
	handler := operations.Handler(http.NotFoundHandler(), probe)
	started := time.Now()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("readiness exceeded bound: %v", elapsed)
	}
	if response.Code != http.StatusServiceUnavailable || contains(response.Body.String(), "secret provider body") {
		t.Fatalf("status/body = %d / %s", response.Code, response.Body.String())
	}
}

func TestHealthzDoesNotCallReadinessDependencies(t *testing.T) {
	called := false
	probe := operations.NewProbe(time.Second, time.Now, map[string]operations.Check{"database": func(context.Context) error { called = true; return nil }})
	response := httptest.NewRecorder()
	operations.Handler(http.NotFoundHandler(), probe).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if response.Code != http.StatusOK || called {
		t.Fatalf("status/called = %d/%v", response.Code, called)
	}
}

func TestSchemaCompatibilitySupportsOneRollingVersion(t *testing.T) {
	if !operations.SchemaCompatible(21, 21, 21) || operations.SchemaCompatible(20, 21, 21) || operations.SchemaCompatible(22, 21, 21) {
		t.Fatal("schema compatibility range is incorrect")
	}
}

func contains(value, part string) bool {
	for index := 0; index+len(part) <= len(value); index++ {
		if value[index:index+len(part)] == part {
			return true
		}
	}
	return false
}
