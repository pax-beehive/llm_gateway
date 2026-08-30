package operations

import (
	"context"
	"time"

	"github.com/toddzheng/llm-gateway/internal/routingcatalog"
	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

const (
	CurrentObservationVersion = 3
	MinimumObservationVersion = 2
	CurrentDatabaseSchema     = 22
	MinimumDatabaseSchema     = 21
)

type CheckResult struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

type ReadinessResult struct {
	Ready   bool          `json:"ready"`
	Checks  []CheckResult `json:"checks"`
	Checked time.Time     `json:"checked_at"`
}

type ReadinessChecker interface {
	Check(context.Context) ReadinessResult
}

type ConsumerObservation struct {
	Name            string    `json:"name"`
	LastEventID     string    `json:"last_event_id,omitempty"`
	LagSeconds      float64   `json:"lag_seconds"`
	PendingCount    int64     `json:"pending_count"`
	ErrorCode       string    `json:"error_code,omitempty"`
	LastSucceededAt time.Time `json:"last_succeeded_at,omitempty"`
}

type BacklogSignals struct {
	OldestUnpublishedOutboxAgeSeconds float64    `json:"oldest_unpublished_outbox_age_seconds"`
	OutboxPendingCount                int64      `json:"outbox_pending_count"`
	QuotaReconciliationBacklog        int64      `json:"quota_reconciliation_backlog"`
	CacheRefreshDueBacklog            int64      `json:"cache_refresh_due_backlog"`
	RetentionScrubBacklog             int64      `json:"retention_scrub_backlog"`
	KeyRevocationPropagationSeconds   float64    `json:"key_revocation_propagation_seconds"`
	MeteringProjectionCutoff          *time.Time `json:"metering_projection_cutoff,omitempty"`
	MeteringProjectionStatus          string     `json:"metering_projection_status"`
}

type AccessRolloutReceipt struct {
	EventID           string    `json:"event_id"`
	DeliverySequence  int64     `json:"delivery_sequence"`
	AggregateType     string    `json:"aggregate_type"`
	AggregateID       string    `json:"aggregate_id"`
	AggregateRevision int64     `json:"aggregate_revision"`
	Status            string    `json:"status"`
	ErrorCode         string    `json:"error_code,omitempty"`
	OccurredAt        time.Time `json:"occurred_at"`
	ObservedAt        time.Time `json:"applied_at"`
	LagMilliseconds   int64     `json:"lag_milliseconds"`
}

type GatewayObservation struct {
	EventSchemaVersion       int                             `json:"event_schema_version"`
	GatewayID                string                          `json:"gateway_id"`
	Region                   string                          `json:"region"`
	BuildSHA                 string                          `json:"build_sha"`
	DatabaseSchemaVersion    int                             `json:"database_schema_version"`
	RoutingCatalogRevision   int64                           `json:"routing_catalog_revision"`
	AccessProjectionRevision int64                           `json:"access_projection_revision"`
	ExecutionEpochFloor      int64                           `json:"execution_epoch_floor"`
	LastUsageOutboxID        int64                           `json:"last_usage_outbox_id"`
	StartedAt                time.Time                       `json:"started_at"`
	ObservedAt               time.Time                       `json:"observed_at"`
	Consumers                []ConsumerObservation           `json:"consumers"`
	Backlogs                 BacklogSignals                  `json:"backlogs"`
	RoutingReceipts          []routingcatalog.RolloutReceipt `json:"routing_receipts,omitempty"`
	AccessReceipts           []AccessRolloutReceipt          `json:"access_receipts,omitempty"`
}

type GatewayIdentity struct {
	GatewayID string
	Region    string
}

type GatewaySummary struct {
	GatewayObservation
	ReceivedAt             time.Time `json:"received_at"`
	HeartbeatLagSeconds    float64   `json:"heartbeat_lag_seconds"`
	HeartbeatStatus        string    `json:"heartbeat_status"`
	DesiredRoutingRevision int64     `json:"desired_routing_revision"`
	RoutingRevisionLag     int64     `json:"routing_revision_lag"`
	DesiredAccessRevision  int64     `json:"desired_access_revision"`
	AccessRevisionLag      int64     `json:"access_revision_lag"`
}

type GatewayPage struct {
	Data []GatewaySummary `json:"data"`
}

type OutboxStatus struct {
	PendingCount                int64      `json:"outbox_pending_count"`
	OldestUnpublishedAgeSeconds float64    `json:"oldest_unpublished_outbox_age_seconds"`
	OldestOccurredAt            *time.Time `json:"oldest_occurred_at,omitempty"`
}

type PublicationSummary struct {
	ID              string                           `json:"id"`
	CatalogRevision int64                            `json:"catalog_revision"`
	Status          routingcatalog.PublicationStatus `json:"status"`
	RequiredRegions []string                         `json:"required_regions"`
	CreatedAt       time.Time                        `json:"created_at"`
	UpdatedAt       time.Time                        `json:"updated_at"`
	Receipts        []routingcatalog.RolloutReceipt  `json:"receipts,omitempty"`
}

type PublicationPage struct {
	Data []PublicationSummary `json:"data"`
}

type JobSummary struct {
	ID          string     `json:"id"`
	Kind        string     `json:"kind"`
	RequestedBy string     `json:"requested_by"`
	TenantID    string     `json:"tenant_id,omitempty"`
	Status      string     `json:"status"`
	Progress    int        `json:"progress"`
	ResultRef   string     `json:"result_ref,omitempty"`
	ErrorCode   string     `json:"error_code,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
}

type JobPage struct {
	Data []JobSummary `json:"data"`
}
type ConsumerPage struct {
	Data []GatewaySummary `json:"data"`
}

type QueryService interface {
	ListGateways(context.Context, tenantadmin.ActorEnvelope) (GatewayPage, error)
	GetGateway(context.Context, tenantadmin.ActorEnvelope, string) (GatewaySummary, error)
	ListPublications(context.Context, tenantadmin.ActorEnvelope) (PublicationPage, error)
	GetPublication(context.Context, tenantadmin.ActorEnvelope, string) (PublicationSummary, error)
	GetOutbox(context.Context, tenantadmin.ActorEnvelope) (OutboxStatus, error)
	ListConsumers(context.Context, tenantadmin.ActorEnvelope) (ConsumerPage, error)
	ListJobs(context.Context, tenantadmin.ActorEnvelope) (JobPage, error)
	GetJob(context.Context, tenantadmin.ActorEnvelope, string) (JobSummary, error)
}
