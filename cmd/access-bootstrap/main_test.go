package main

import (
	"context"
	"strings"
	"testing"
)

func TestRunRejectsEnvironmentBootstrapInProduction(t *testing.T) {
	t.Setenv("GATEWAY_DATABASE_URL", "postgres://gateway@example.com/gateway?sslmode=verify-full")
	t.Setenv("GATEWAY_ENV", "production")
	if err := run(context.Background()); err == nil || !strings.Contains(err.Error(), "development-only") {
		t.Fatalf("run error = %v, want development-only rejection", err)
	}
}
