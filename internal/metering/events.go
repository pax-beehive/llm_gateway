package metering

import "github.com/toddzheng/llm-gateway/internal/core"

func ResponseUsageEvent(usage core.UsageRecord, response core.Response) UsageEvent {
	return UsageEvent{
		EventID:       "gateway-usage:" + usage.TenantID + ":" + usage.ID,
		SchemaVersion: CurrentEventSchemaVersion, Type: EventUsageRecorded, UsageID: usage.ID,
		TenantID: usage.TenantID, APIKeyID: usage.APIKeyID, ResponseID: usage.ResponseID,
		AttemptID: usage.AttemptID, RouteID: usage.RouteID, Provider: usage.PriceSnapshot.Provider,
		PublicModel: response.Model, ProviderModel: usage.PriceSnapshot.Model, Region: usage.PriceSnapshot.Region,
		PriceSnapshotID: usage.PriceSnapshot.ID, InputTokens: usage.Usage.InputTokens,
		CachedInputTokens: usage.Usage.CachedInputTokens, CacheWriteInputTokens: usage.Usage.CacheWriteInputTokens,
		OutputTokens: usage.Usage.OutputTokens, AmountMicros: usage.AmountMicros, Currency: usage.Currency,
		Outcome: "committed", OccurredAt: usage.CreatedAt,
	}
}

func CapabilityUsageEvent(usage core.CapabilityUsageRecord) UsageEvent {
	publicModel := usage.PublicModel
	if publicModel == "" {
		publicModel = usage.Model
	}
	return UsageEvent{
		EventID:       "gateway-capability-usage:" + usage.TenantID + ":" + usage.ID,
		SchemaVersion: CurrentEventSchemaVersion, Type: EventCapabilityUsageRecorded, UsageID: usage.ID,
		TenantID: usage.TenantID, APIKeyID: usage.APIKeyID, OperationID: usage.OperationID,
		Capability: string(usage.Capability), RouteID: usage.RouteID, Provider: usage.Provider,
		PublicModel: publicModel, ProviderModel: usage.PriceSnapshot.Model, Region: usage.PriceSnapshot.Region,
		PriceSnapshotID: usage.PriceSnapshot.ID, InputUnits: usage.InputUnits, Documents: usage.Documents,
		AmountMicros: usage.AmountMicros, Currency: usage.Currency, Outcome: "committed", OccurredAt: usage.CreatedAt,
	}
}
