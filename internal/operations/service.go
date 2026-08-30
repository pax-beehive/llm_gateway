package operations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/toddzheng/llm-gateway/internal/routingcatalog"
	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

var (
	ErrNotFound        = errors.New("Operations resource not found")
	ErrInvalidArgument = errors.New("invalid Operations argument")
	ErrPolicyDenied    = errors.New("Operations policy denied")
	safeCode           = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

type ReceiptRecorder interface {
	RecordRolloutReceipt(context.Context, routingcatalog.RolloutReceipt) (routingcatalog.Publication, error)
}

type Service struct {
	database     *sql.DB
	receipts     ReceiptRecorder
	now          func() time.Time
	staleAfter   time.Duration
	heartbeatLag metric.Float64Histogram
	routingLag   metric.Int64Histogram
	accessLag    metric.Int64Histogram
	backlog      metric.Int64Histogram
	consumerLag  metric.Float64Histogram
	oldestOutbox metric.Float64Histogram
	revocation   metric.Float64Histogram
}

func NewService(database *sql.DB, receipts ReceiptRecorder, now func() time.Time) (*Service, error) {
	if database == nil {
		return nil, errors.New("Operations requires PostgreSQL")
	}
	if now == nil {
		now = time.Now
	}
	meter := otel.Meter("llm-gateway/operations")
	heartbeatLag, _ := meter.Float64Histogram("gateway.operations.heartbeat_lag_seconds")
	routingLag, _ := meter.Int64Histogram("gateway.operations.routing_revision_lag")
	accessLag, _ := meter.Int64Histogram("gateway.operations.access_revision_lag")
	backlog, _ := meter.Int64Histogram("gateway.operations.backlog")
	consumerLag, _ := meter.Float64Histogram("gateway.operations.consumer_lag_seconds")
	oldestOutbox, _ := meter.Float64Histogram("gateway.operations.oldest_unpublished_outbox_age_seconds")
	revocation, _ := meter.Float64Histogram("gateway.operations.key_revocation_propagation_seconds")
	return &Service{database: database, receipts: receipts, now: now, staleAfter: 15 * time.Second,
		heartbeatLag: heartbeatLag, routingLag: routingLag, accessLag: accessLag, backlog: backlog, consumerLag: consumerLag,
		oldestOutbox: oldestOutbox, revocation: revocation}, nil
}

func (service *Service) RecordGatewayObservation(ctx context.Context, identity GatewayIdentity, observation GatewayObservation) error {
	if err := validateObservation(identity, observation, service.now().UTC()); err != nil {
		return err
	}
	observation.ObservedAt = observation.ObservedAt.UTC().Truncate(time.Microsecond)
	consumers, _ := json.Marshal(observation.Consumers)
	backlogs, _ := json.Marshal(observation.Backlogs)
	receivedAt := service.now().UTC()
	result, err := service.database.ExecContext(ctx, `INSERT INTO operations_gateway_heartbeats (
		gateway_id,region,build_sha,database_schema_version,routing_catalog_revision,access_projection_revision,
		execution_epoch_floor,last_usage_outbox_id,started_at,observed_at,received_at,consumers,backlogs
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
	ON CONFLICT (gateway_id) DO UPDATE SET region=EXCLUDED.region,build_sha=EXCLUDED.build_sha,
		database_schema_version=EXCLUDED.database_schema_version,
		routing_catalog_revision=EXCLUDED.routing_catalog_revision,
		access_projection_revision=EXCLUDED.access_projection_revision,
		execution_epoch_floor=EXCLUDED.execution_epoch_floor,
		last_usage_outbox_id=EXCLUDED.last_usage_outbox_id,
		started_at=EXCLUDED.started_at,observed_at=EXCLUDED.observed_at,
		received_at=EXCLUDED.received_at,consumers=EXCLUDED.consumers,backlogs=EXCLUDED.backlogs
	WHERE EXCLUDED.observed_at > operations_gateway_heartbeats.observed_at
	  AND EXCLUDED.routing_catalog_revision >= operations_gateway_heartbeats.routing_catalog_revision
	  AND EXCLUDED.access_projection_revision >= operations_gateway_heartbeats.access_projection_revision
	  AND EXCLUDED.last_usage_outbox_id >= operations_gateway_heartbeats.last_usage_outbox_id`,
		identity.GatewayID, identity.Region, observation.BuildSHA, observation.DatabaseSchemaVersion,
		observation.RoutingCatalogRevision, observation.AccessProjectionRevision, observation.ExecutionEpochFloor,
		observation.LastUsageOutboxID, observation.StartedAt.UTC(), observation.ObservedAt.UTC(), receivedAt, consumers, backlogs)
	if err != nil {
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	accepted := rowsAffected == 1
	if !accepted {
		var storedObservedAt time.Time
		var storedRouting, storedAccess, storedUsage int64
		if err := service.database.QueryRowContext(ctx, `SELECT observed_at,routing_catalog_revision,access_projection_revision,last_usage_outbox_id
			FROM operations_gateway_heartbeats WHERE gateway_id=$1`, identity.GatewayID).Scan(
			&storedObservedAt, &storedRouting, &storedAccess, &storedUsage); err != nil {
			return err
		}
		if !storedObservedAt.Equal(observation.ObservedAt) || storedRouting != observation.RoutingCatalogRevision ||
			storedAccess != observation.AccessProjectionRevision || storedUsage != observation.LastUsageOutboxID {
			// Heartbeat soft state is monotonic, but durable receipts have their
			// own immutable identity and must still drain after an instance restart.
			accepted = false
		}
	}
	if accepted {
		service.heartbeatLag.Record(ctx, nonnegativeSeconds(receivedAt.Sub(observation.ObservedAt)))
		service.oldestOutbox.Record(ctx, observation.Backlogs.OldestUnpublishedOutboxAgeSeconds)
		service.revocation.Record(ctx, observation.Backlogs.KeyRevocationPropagationSeconds)
		for kind, value := range map[string]int64{
			"outbox":               observation.Backlogs.OutboxPendingCount,
			"quota_reconciliation": observation.Backlogs.QuotaReconciliationBacklog,
			"cache_refresh_due":    observation.Backlogs.CacheRefreshDueBacklog,
			"retention_scrub":      observation.Backlogs.RetentionScrubBacklog,
		} {
			service.backlog.Record(ctx, value, metric.WithAttributes(attribute.String("kind", kind)))
		}
		for _, consumer := range observation.Consumers {
			service.consumerLag.Record(ctx, consumer.LagSeconds, metric.WithAttributes(attribute.String("consumer", consumer.Name)))
		}
	}
	if service.receipts != nil {
		for _, receipt := range observation.RoutingReceipts {
			receipt.GatewayID = identity.GatewayID
			receipt.Region = identity.Region
			if receipt.ObservedAt.IsZero() {
				receipt.ObservedAt = observation.ObservedAt
			}
			if _, err := service.receipts.RecordRolloutReceipt(ctx, receipt); err != nil && !errors.Is(err, routingcatalog.ErrNotFound) {
				return err
			}
		}
	}
	for _, receipt := range observation.AccessReceipts {
		_, err := service.database.ExecContext(ctx, `INSERT INTO operations_access_rollout_receipts (
			gateway_id,region,event_id,delivery_sequence,aggregate_type,aggregate_id,aggregate_revision,
			status,error_code,occurred_at,observed_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),$10,$11)
		ON CONFLICT DO NOTHING`,
			identity.GatewayID, identity.Region, receipt.EventID, receipt.DeliverySequence, receipt.AggregateType,
			receipt.AggregateID, receipt.AggregateRevision, receipt.Status, receipt.ErrorCode,
			receipt.OccurredAt.UTC(), receipt.ObservedAt.UTC())
		if err != nil {
			return err
		}
	}
	return nil
}

func validateObservation(identity GatewayIdentity, observation GatewayObservation, now time.Time) error {
	observedSkew := now.Sub(observation.ObservedAt)
	if !resourceID.MatchString(identity.GatewayID) || strings.TrimSpace(identity.Region) == "" || observation.GatewayID != identity.GatewayID || observation.Region != identity.Region ||
		(observation.EventSchemaVersion < MinimumObservationVersion || observation.EventSchemaVersion > CurrentObservationVersion) || !resourceID.MatchString(observation.BuildSHA) ||
		observation.DatabaseSchemaVersion <= 0 || observation.RoutingCatalogRevision < 0 || observation.AccessProjectionRevision < 0 || observation.ExecutionEpochFloor < 0 ||
		observation.LastUsageOutboxID < 0 || observation.StartedAt.IsZero() || observation.ObservedAt.IsZero() || observation.StartedAt.After(observation.ObservedAt) ||
		observedSkew > 5*time.Minute || observedSkew < -5*time.Minute || len(observation.Consumers) > 32 || len(observation.RoutingReceipts) > 128 || len(observation.AccessReceipts) > 128 ||
		observation.Backlogs.OldestUnpublishedOutboxAgeSeconds < 0 || observation.Backlogs.OutboxPendingCount < 0 ||
		observation.Backlogs.QuotaReconciliationBacklog < 0 || observation.Backlogs.CacheRefreshDueBacklog < 0 ||
		observation.Backlogs.RetentionScrubBacklog < 0 || observation.Backlogs.KeyRevocationPropagationSeconds < 0 {
		return ErrInvalidArgument
	}
	for _, consumer := range observation.Consumers {
		if !resourceID.MatchString(consumer.Name) || consumer.LastEventID != "" && !resourceID.MatchString(consumer.LastEventID) ||
			consumer.LagSeconds < 0 || consumer.PendingCount < 0 || consumer.ErrorCode != "" && !safeCode.MatchString(consumer.ErrorCode) {
			return ErrInvalidArgument
		}
	}
	for _, receipt := range observation.RoutingReceipts {
		if !resourceID.MatchString(receipt.PublicationID) || receipt.GatewayID != identity.GatewayID || receipt.Region != identity.Region ||
			receipt.CatalogRevision <= 0 || (receipt.Status != routingcatalog.ReceiptApplied && receipt.Status != routingcatalog.ReceiptRejected) ||
			receipt.ErrorCode != "" && !safeCode.MatchString(receipt.ErrorCode) || receipt.ObservedAt.IsZero() || receipt.ObservedAt.After(observation.ObservedAt) {
			return ErrInvalidArgument
		}
	}
	for _, receipt := range observation.AccessReceipts {
		if !resourceID.MatchString(receipt.EventID) || receipt.DeliverySequence <= 0 ||
			(receipt.AggregateType != "Tenant" && receipt.AggregateType != "GatewayAPIKey") || !resourceID.MatchString(receipt.AggregateID) ||
			receipt.AggregateRevision <= 0 || (receipt.Status != "applied" && receipt.Status != "rejected") ||
			receipt.ErrorCode != "" && !safeCode.MatchString(receipt.ErrorCode) || receipt.OccurredAt.IsZero() || receipt.ObservedAt.IsZero() ||
			receipt.ObservedAt.Before(receipt.OccurredAt) || receipt.ObservedAt.After(observation.ObservedAt) {
			return ErrInvalidArgument
		}
	}
	if observation.EventSchemaVersion == CurrentObservationVersion &&
		observation.Backlogs.MeteringProjectionStatus != "unavailable" && observation.Backlogs.MeteringProjectionStatus != "current" &&
		observation.Backlogs.MeteringProjectionStatus != "degraded" {
		return ErrInvalidArgument
	}
	if observation.Backlogs.MeteringProjectionCutoff != nil && observation.Backlogs.MeteringProjectionCutoff.After(observation.ObservedAt) {
		return ErrInvalidArgument
	}
	return nil
}

func (service *Service) ListGateways(ctx context.Context, actor tenantadmin.ActorEnvelope) (GatewayPage, error) {
	if !canRead(actor) {
		return GatewayPage{}, ErrPolicyDenied
	}
	rows, err := service.database.QueryContext(ctx, heartbeatSelect+` ORDER BY region,gateway_id LIMIT 500`)
	if err != nil {
		return GatewayPage{}, err
	}
	defer rows.Close()
	page := GatewayPage{Data: []GatewaySummary{}}
	for rows.Next() {
		summary, err := scanGateway(rows)
		if err != nil {
			return GatewayPage{}, err
		}
		page.Data = append(page.Data, summary)
	}
	if err := rows.Err(); err != nil {
		return GatewayPage{}, err
	}
	if err := service.loadAccessReceipts(ctx, page.Data); err != nil {
		return GatewayPage{}, err
	}
	desiredRouting, desiredAccess, err := service.desiredRevisionsByRegion(ctx, page.Data)
	if err != nil {
		return GatewayPage{}, err
	}
	service.finishGatewaySummaries(ctx, page.Data, desiredRouting, desiredAccess)
	return page, nil
}

func (service *Service) GetGateway(ctx context.Context, actor tenantadmin.ActorEnvelope, gatewayID string) (GatewaySummary, error) {
	if !canRead(actor) {
		return GatewaySummary{}, ErrPolicyDenied
	}
	if !resourceID.MatchString(gatewayID) {
		return GatewaySummary{}, ErrInvalidArgument
	}
	summary, err := scanGateway(service.database.QueryRowContext(ctx, heartbeatSelect+` WHERE gateway_id=$1`, gatewayID))
	if errors.Is(err, sql.ErrNoRows) {
		return GatewaySummary{}, ErrNotFound
	}
	if err != nil {
		return GatewaySummary{}, err
	}
	routing, accessByRegion, err := service.desiredRevisionsByRegion(ctx, []GatewaySummary{summary})
	if err != nil {
		return GatewaySummary{}, err
	}
	values := []GatewaySummary{summary}
	if err := service.loadAccessReceipts(ctx, values); err != nil {
		return GatewaySummary{}, err
	}
	service.finishGatewaySummaries(ctx, values, routing, accessByRegion)
	return values[0], nil
}

func (service *Service) loadAccessReceipts(ctx context.Context, values []GatewaySummary) error {
	if len(values) == 0 {
		return nil
	}
	ids := make([]string, 0, len(values))
	byID := make(map[string]*GatewaySummary, len(values))
	for index := range values {
		ids = append(ids, values[index].GatewayID)
		byID[values[index].GatewayID] = &values[index]
	}
	rows, err := service.database.QueryContext(ctx, `SELECT gateway_id,event_id,delivery_sequence,aggregate_type,aggregate_id,
		aggregate_revision,status,COALESCE(error_code,''),occurred_at,observed_at
		FROM (SELECT *,row_number() OVER (PARTITION BY gateway_id ORDER BY delivery_sequence DESC,event_id DESC) AS receipt_rank
			FROM operations_access_rollout_receipts WHERE gateway_id=ANY($1)) receipts
		WHERE receipt_rank<=128 ORDER BY gateway_id,delivery_sequence DESC,event_id DESC`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var gatewayID string
		var receipt AccessRolloutReceipt
		if err := rows.Scan(&gatewayID, &receipt.EventID, &receipt.DeliverySequence, &receipt.AggregateType, &receipt.AggregateID,
			&receipt.AggregateRevision, &receipt.Status, &receipt.ErrorCode, &receipt.OccurredAt, &receipt.ObservedAt); err != nil {
			return err
		}
		receipt.LagMilliseconds = max64(receipt.ObservedAt.Sub(receipt.OccurredAt).Milliseconds(), 0)
		if summary := byID[gatewayID]; summary != nil {
			summary.AccessReceipts = append(summary.AccessReceipts, receipt)
		}
	}
	return rows.Err()
}

const heartbeatSelect = `SELECT gateway_id,region,build_sha,database_schema_version,routing_catalog_revision,
	access_projection_revision,execution_epoch_floor,last_usage_outbox_id,started_at,observed_at,received_at,consumers,backlogs
	FROM operations_gateway_heartbeats`

type rowScanner interface{ Scan(...any) error }

func scanGateway(row rowScanner) (GatewaySummary, error) {
	var summary GatewaySummary
	var consumers, backlogs []byte
	err := row.Scan(&summary.GatewayID, &summary.Region, &summary.BuildSHA, &summary.DatabaseSchemaVersion,
		&summary.RoutingCatalogRevision, &summary.AccessProjectionRevision, &summary.ExecutionEpochFloor,
		&summary.LastUsageOutboxID, &summary.StartedAt, &summary.ObservedAt, &summary.ReceivedAt, &consumers, &backlogs)
	if err != nil {
		return GatewaySummary{}, err
	}
	summary.EventSchemaVersion = CurrentObservationVersion
	if err := json.Unmarshal(consumers, &summary.Consumers); err != nil {
		return GatewaySummary{}, err
	}
	if err := json.Unmarshal(backlogs, &summary.Backlogs); err != nil {
		return GatewaySummary{}, err
	}
	return summary, nil
}

func (service *Service) desiredRevisionsByRegion(ctx context.Context, gateways []GatewaySummary) (int64, map[string]int64, error) {
	var routing int64
	if err := service.database.QueryRowContext(ctx, `SELECT COALESCE(max(revision),0) FROM routing_catalog_revisions`).Scan(&routing); err != nil {
		return 0, nil, err
	}
	desired := make(map[string]int64)
	for _, gateway := range gateways {
		if _, exists := desired[gateway.Region]; exists {
			continue
		}
		var access int64
		if err := service.database.QueryRowContext(ctx, `SELECT COALESCE(max(delivery_sequence),0) FROM control_outbox
			WHERE aggregate_type IN ('Tenant','GatewayAPIKey') AND schema_version=2
			AND COALESCE(payload->>'home_region'=$1,false)`, gateway.Region).Scan(&access); err != nil {
			return 0, nil, err
		}
		desired[gateway.Region] = access
	}
	return routing, desired, nil
}

func (service *Service) finishGatewaySummaries(ctx context.Context, values []GatewaySummary, desiredRouting int64, desiredAccessByRegion map[string]int64) {
	now := service.now().UTC()
	for index := range values {
		value := &values[index]
		value.DesiredRoutingRevision = desiredRouting
		value.DesiredAccessRevision = desiredAccessByRegion[value.Region]
		value.RoutingRevisionLag = max64(desiredRouting-value.RoutingCatalogRevision, 0)
		value.AccessRevisionLag = max64(value.DesiredAccessRevision-value.AccessProjectionRevision, 0)
		for _, consumer := range value.Consumers {
			switch consumer.Name {
			case "access_projection":
				value.AccessRevisionLag = max64(value.AccessRevisionLag, consumer.PendingCount)
				if consumer.ErrorCode != "" {
					value.AccessRevisionLag = max64(value.AccessRevisionLag, 1)
				}
			case "routing_catalog":
				value.RoutingRevisionLag = max64(value.RoutingRevisionLag, consumer.PendingCount)
				if consumer.ErrorCode != "" {
					value.RoutingRevisionLag = max64(value.RoutingRevisionLag, 1)
				}
			}
		}
		value.HeartbeatLagSeconds = nonnegativeSeconds(now.Sub(value.ObservedAt))
		value.HeartbeatStatus = "current"
		if now.Sub(value.ObservedAt) > service.staleAfter {
			value.HeartbeatStatus = "stale"
		}
		service.routingLag.Record(ctx, value.RoutingRevisionLag)
		service.accessLag.Record(ctx, value.AccessRevisionLag)
	}
}

func (service *Service) GetOutbox(ctx context.Context, actor tenantadmin.ActorEnvelope) (OutboxStatus, error) {
	if !canRead(actor) {
		return OutboxStatus{}, ErrPolicyDenied
	}
	var result OutboxStatus
	err := service.database.QueryRowContext(ctx, `SELECT count(*),min(occurred_at),GREATEST(COALESCE(EXTRACT(EPOCH FROM (now()-min(occurred_at))),0),0) FROM control_outbox WHERE published_at IS NULL`).Scan(&result.PendingCount, &result.OldestOccurredAt, &result.OldestUnpublishedAgeSeconds)
	if err != nil {
		return OutboxStatus{}, err
	}
	return result, nil
}

func (service *Service) ListConsumers(ctx context.Context, actor tenantadmin.ActorEnvelope) (ConsumerPage, error) {
	page, err := service.ListGateways(ctx, actor)
	return ConsumerPage{Data: page.Data}, err
}

func (service *Service) ListPublications(ctx context.Context, actor tenantadmin.ActorEnvelope) (PublicationPage, error) {
	if !canRead(actor) {
		return PublicationPage{}, ErrPolicyDenied
	}
	rows, err := service.database.QueryContext(ctx, `SELECT id,catalog_revision,status,to_json(required_regions),created_at,updated_at FROM routing_publications ORDER BY catalog_revision DESC LIMIT 200`)
	if err != nil {
		return PublicationPage{}, err
	}
	defer rows.Close()
	page := PublicationPage{Data: []PublicationSummary{}}
	for rows.Next() {
		item, err := scanPublicationSummary(rows)
		if err != nil {
			return PublicationPage{}, err
		}
		page.Data = append(page.Data, item)
	}
	return page, rows.Err()
}

func (service *Service) GetPublication(ctx context.Context, actor tenantadmin.ActorEnvelope, id string) (PublicationSummary, error) {
	if !canRead(actor) {
		return PublicationSummary{}, ErrPolicyDenied
	}
	if !resourceID.MatchString(id) {
		return PublicationSummary{}, ErrInvalidArgument
	}
	item, err := scanPublicationSummary(service.database.QueryRowContext(ctx, `SELECT id,catalog_revision,status,to_json(required_regions),created_at,updated_at FROM routing_publications WHERE id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return PublicationSummary{}, ErrNotFound
	}
	if err != nil {
		return PublicationSummary{}, err
	}
	rows, err := service.database.QueryContext(ctx, `SELECT publication_id,gateway_id,region,catalog_revision,status,COALESCE(error_code,''),observed_at FROM routing_rollout_receipts WHERE publication_id=$1 ORDER BY region,gateway_id`, id)
	if err != nil {
		return PublicationSummary{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var receipt routingcatalog.RolloutReceipt
		if err := rows.Scan(&receipt.PublicationID, &receipt.GatewayID, &receipt.Region, &receipt.CatalogRevision, &receipt.Status, &receipt.ErrorCode, &receipt.ObservedAt); err != nil {
			return PublicationSummary{}, err
		}
		receipt.LagMilliseconds = max64(receipt.ObservedAt.Sub(item.CreatedAt).Milliseconds(), 0)
		item.Receipts = append(item.Receipts, receipt)
	}
	return item, rows.Err()
}

func scanPublicationSummary(row rowScanner) (PublicationSummary, error) {
	var item PublicationSummary
	var regions []byte
	err := row.Scan(&item.ID, &item.CatalogRevision, &item.Status, &regions, &item.CreatedAt, &item.UpdatedAt)
	if err != nil {
		return PublicationSummary{}, err
	}
	if err := json.Unmarshal(regions, &item.RequiredRegions); err != nil {
		return PublicationSummary{}, err
	}
	return item, nil
}

func (service *Service) ListJobs(ctx context.Context, actor tenantadmin.ActorEnvelope) (JobPage, error) {
	if !canRead(actor) {
		return JobPage{}, ErrPolicyDenied
	}
	rows, err := service.database.QueryContext(ctx, jobSelect+` ORDER BY created_at DESC,id LIMIT 200`)
	if err != nil {
		return JobPage{}, err
	}
	defer rows.Close()
	page := JobPage{Data: []JobSummary{}}
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return JobPage{}, err
		}
		page.Data = append(page.Data, job)
	}
	return page, rows.Err()
}
func (service *Service) GetJob(ctx context.Context, actor tenantadmin.ActorEnvelope, id string) (JobSummary, error) {
	if !canRead(actor) {
		return JobSummary{}, ErrPolicyDenied
	}
	if !resourceID.MatchString(id) {
		return JobSummary{}, ErrInvalidArgument
	}
	job, err := scanJob(service.database.QueryRowContext(ctx, jobSelect+` WHERE id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return JobSummary{}, ErrNotFound
	}
	return job, err
}

const jobSelect = `SELECT id,operation_type,actor_id,status,COALESCE(error_code,''),created_at,started_at,completed_at FROM provider_operations`

func scanJob(row rowScanner) (JobSummary, error) {
	var job JobSummary
	if err := row.Scan(&job.ID, &job.Kind, &job.RequestedBy, &job.Status, &job.ErrorCode, &job.CreatedAt, &job.StartedAt, &job.FinishedAt); err != nil {
		return JobSummary{}, err
	}
	job.Progress = 0
	if job.Status == "running" {
		job.Progress = 50
	} else if job.Status == "succeeded" || job.Status == "failed" || job.Status == "uncertain" {
		job.Progress = 100
	}
	if job.Status == "succeeded" {
		job.ResultRef = "provider-operation:" + job.ID
	}
	return job, nil
}

func canRead(actor tenantadmin.ActorEnvelope) bool {
	if actor.Type == "" || actor.ID == "" {
		return false
	}
	for _, scope := range actor.Scopes {
		if scope == tenantadmin.ScopePlatformRead || scope == tenantadmin.ScopePlatformWrite {
			return true
		}
	}
	return false
}
func nonnegativeSeconds(value time.Duration) float64 {
	if value < 0 {
		return 0
	}
	return value.Seconds()
}
func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func SortConsumers(values []ConsumerObservation) {
	sort.Slice(values, func(i, j int) bool { return values[i].Name < values[j].Name })
}
func SchemaVersion(ctx context.Context, database *sql.DB, component string) (int, error) {
	var version int
	err := database.QueryRowContext(ctx, `SELECT current_version FROM gateway_schema_metadata WHERE component=$1`, component).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return version, nil
}
