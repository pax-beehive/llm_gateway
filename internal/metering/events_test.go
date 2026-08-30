package metering_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/metering"
)

func TestGatewayUsageEventHasStableIdentityAndNoContentFields(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	event := metering.ResponseUsageEvent(core.UsageRecord{
		ID: "usage-a", TenantID: "tenant-a", APIKeyID: "key-a", ResponseID: "response-a",
		AttemptID: "attempt-a", RouteID: "route-a",
		PriceSnapshot: core.PriceSnapshot{ID: "price-a", Provider: "openai", Model: "gpt-provider", Region: "us"},
		Usage:         core.Usage{InputTokens: 10, OutputTokens: 5}, AmountMicros: 25, Currency: "USD", CreatedAt: now,
	}, core.Response{Model: "public-model"})
	if event.EventID != "gateway-usage:tenant-a:usage-a" {
		t.Fatalf("event ID = %q", event.EventID)
	}
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{`"input"`, `"output"`, `"metadata"`, `"provider_usage"`, `"credential"`} {
		if strings.Contains(string(payload), forbidden) {
			t.Fatalf("usage event contains forbidden content field %s: %s", forbidden, payload)
		}
	}
}

func TestOriginalUsageEventRejectsNegativeFinancialValues(t *testing.T) {
	event := metering.UsageEvent{
		EventID: "event-a", SchemaVersion: 1, Type: metering.EventUsageRecorded, UsageID: "usage-a",
		TenantID: "tenant-a", ResponseID: "response-a", AttemptID: "attempt-a", Provider: "provider",
		PublicModel: "public", ProviderModel: "provider-model", Region: "us", PriceSnapshotID: "price-a",
		AmountMicros: -1, Currency: "USD", Outcome: "committed", OccurredAt: time.Now().UTC(),
	}
	if err := event.Validate(); err == nil {
		t.Fatal("negative original usage unexpectedly validated")
	}
}

func TestCapabilityUsageEventSeparatesPublicAndProviderModels(t *testing.T) {
	event := metering.CapabilityUsageEvent(core.CapabilityUsageRecord{
		ID: "usage-a", TenantID: "tenant-a", OperationID: "operation-a", Capability: core.CapabilityEmbeddings,
		RouteID: "route-a", Provider: "provider", Model: "provider-result-model", PublicModel: "public-alias",
		PriceSnapshot: core.PriceSnapshot{ID: "price-a", Model: "provider-priced-model", Region: "us"},
		Currency:      "USD", CreatedAt: time.Now().UTC(),
	})
	if event.PublicModel != "public-alias" || event.ProviderModel != "provider-priced-model" {
		t.Fatalf("public/provider model attribution = %q/%q", event.PublicModel, event.ProviderModel)
	}
}
