package main

import (
	"testing"
	"time"
)

func TestConfigFromEnvRequiresExplicitConfirmationAndBounds(t *testing.T) {
	t.Setenv("CONTROL_EVENT_RETENTION_DATABASE_URL", "postgres://example.test/gateway")
	t.Setenv("CONTROL_EVENT_RETENTION_THROUGH", "42")
	if _, err := configFromEnv(); err == nil {
		t.Fatal("missing confirmation was accepted")
	}
	t.Setenv("CONTROL_EVENT_RETENTION_CONFIRM", "prune-control-events")
	t.Setenv("CONTROL_EVENT_RETENTION_LIMIT", "256")
	t.Setenv("CONTROL_EVENT_RETENTION_GATEWAY_STALE_AFTER", "15m")
	configuration, err := configFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if configuration.through != 42 || configuration.limit != 256 || configuration.staleAfter != 15*time.Minute {
		t.Fatalf("config = %#v", configuration)
	}
}

func TestConfigFromEnvRejectsInvalidCursor(t *testing.T) {
	t.Setenv("CONTROL_EVENT_RETENTION_DATABASE_URL", "postgres://example.test/gateway")
	t.Setenv("CONTROL_EVENT_RETENTION_THROUGH", "-1")
	t.Setenv("CONTROL_EVENT_RETENTION_CONFIRM", "prune-control-events")
	if _, err := configFromEnv(); err == nil {
		t.Fatal("negative cursor was accepted")
	}
}
