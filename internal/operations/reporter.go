package operations

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/toddzheng/llm-gateway/internal/routingcatalog"
)

const observationPath = "/internal/v1/operations/gateway-observations"

type Reporter struct {
	endpoint  *url.URL
	gatewayID string
	region    string
	key       []byte
	client    *http.Client
	now       func() time.Time
}

func NewReporter(endpoint, gatewayID, region string, key []byte, client *http.Client, now func() time.Time) (*Reporter, error) {
	parsed, err := url.Parse(strings.TrimRight(endpoint, "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || !resourceID.MatchString(gatewayID) || strings.TrimSpace(region) == "" || len(key) < 32 {
		return nil, errors.New("Gateway Operations reporter configuration is invalid")
	}
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	if now == nil {
		now = time.Now
	}
	return &Reporter{endpoint: parsed, gatewayID: gatewayID, region: region, key: append([]byte(nil), key...), client: client, now: now}, nil
}

func (reporter *Reporter) Report(ctx context.Context, observation GatewayObservation) error {
	observation.EventSchemaVersion = CurrentObservationVersion
	observation.GatewayID = reporter.gatewayID
	observation.Region = reporter.region
	if observation.Backlogs.MeteringProjectionStatus == "" {
		observation.Backlogs.MeteringProjectionStatus = "unavailable"
	}
	if observation.ObservedAt.IsZero() {
		observation.ObservedAt = reporter.now().UTC()
	}
	body, err := json.Marshal(observation)
	if err != nil {
		return err
	}
	target := *reporter.endpoint
	target.Path = strings.TrimRight(target.Path, "/") + observationPath
	target.RawQuery = ""
	authorization, err := GatewayAuthorization(reporter.key, reporter.gatewayID, reporter.now().UTC(), http.MethodPost, target.RequestURI(), body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", authorization)
	request.Header.Set("Content-Type", "application/json")
	response, err := reporter.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusAccepted {
		return fmt.Errorf("Gateway Operations observation status %d", response.StatusCode)
	}
	return nil
}

func ObserveGateway(ctx context.Context, database *sql.DB, gatewayID, region, buildSHA string, startedAt time.Time, routingRevision int64, now time.Time) (GatewayObservation, error) {
	if database == nil || !resourceID.MatchString(gatewayID) || strings.TrimSpace(region) == "" || strings.TrimSpace(buildSHA) == "" || startedAt.IsZero() || routingRevision < 0 {
		return GatewayObservation{}, ErrInvalidArgument
	}
	observation := GatewayObservation{EventSchemaVersion: CurrentObservationVersion, GatewayID: gatewayID, Region: region,
		BuildSHA: buildSHA, RoutingCatalogRevision: routingRevision, StartedAt: startedAt.UTC(), ObservedAt: now.UTC(), Consumers: []ConsumerObservation{}}
	observation.Backlogs.MeteringProjectionStatus = "unavailable"
	if err := database.QueryRowContext(ctx, `SELECT current_version FROM gateway_schema_metadata WHERE component='gateway'`).Scan(&observation.DatabaseSchemaVersion); err != nil {
		return GatewayObservation{}, err
	}
	if err := database.QueryRowContext(ctx, `SELECT COALESCE(max(delivery_sequence),0) FROM gateway_access_inbox`).Scan(&observation.AccessProjectionRevision); err != nil {
		return GatewayObservation{}, err
	}
	if err := database.QueryRowContext(ctx, `SELECT COALESCE(min(execution_epoch),0) FROM gateway_access_projection WHERE tenant_status='active'`).Scan(&observation.ExecutionEpochFloor); err != nil {
		return GatewayObservation{}, err
	}
	if err := database.QueryRowContext(ctx, `SELECT COALESCE(max(id) FILTER (WHERE event_type IN ('usage.recorded','capability.usage_recorded','cache_refresh.usage_recorded')),0),
		count(*) FILTER (WHERE published_at IS NULL),
		COALESCE(EXTRACT(EPOCH FROM (now()-min(created_at) FILTER (WHERE published_at IS NULL))),0) FROM transactional_outbox`).Scan(
		&observation.LastUsageOutboxID, &observation.Backlogs.OutboxPendingCount, &observation.Backlogs.OldestUnpublishedOutboxAgeSeconds); err != nil {
		return GatewayObservation{}, err
	}
	if err := database.QueryRowContext(ctx, `SELECT count(*) FROM quota_reservations WHERE status='reserved' AND expires_at<=now()`).Scan(&observation.Backlogs.QuotaReconciliationBacklog); err != nil {
		return GatewayObservation{}, err
	}
	if err := database.QueryRowContext(ctx, `SELECT count(*) FROM cache_refresh_intents WHERE status='planned' AND scheduled_for<=now()`).Scan(&observation.Backlogs.CacheRefreshDueBacklog); err != nil {
		return GatewayObservation{}, err
	}
	if err := database.QueryRowContext(ctx, `SELECT count(*) FROM responses r JOIN tenants t ON t.id=r.tenant_id AND t.home_region=$1
		WHERE r.deleted_at IS NULL AND r.status IN ('completed','failed','cancelled') AND r.payload?'content_expires_at'
		AND (r.payload->>'content_expires_at')::bigint<=extract(epoch FROM now())::bigint`, region).Scan(&observation.Backlogs.RetentionScrubBacklog); err != nil {
		return GatewayObservation{}, err
	}
	if err := database.QueryRowContext(ctx, `SELECT COALESCE(max(apply_lag_seconds),0) FROM gateway_access_inbox
		WHERE event_type='GatewayAPIKeyRevoked' AND disposition='applied'`).Scan(&observation.Backlogs.KeyRevocationPropagationSeconds); err != nil {
		return GatewayObservation{}, err
	}
	accessConsumer, err := observeAccessConsumer(ctx, database, now)
	if err != nil {
		return GatewayObservation{}, err
	}
	routingConsumer, err := observeRoutingConsumer(ctx, database, gatewayID, now)
	if err != nil {
		return GatewayObservation{}, err
	}
	observation.Consumers = []ConsumerObservation{accessConsumer, routingConsumer}
	SortConsumers(observation.Consumers)
	rows, err := database.QueryContext(ctx, `SELECT publication_id,catalog_revision,status,COALESCE(error_code,''),observed_at
		FROM gateway_routing_catalog_inbox WHERE gateway_id=$1 ORDER BY observed_at DESC,event_id DESC LIMIT 128`, gatewayID)
	if err != nil {
		return GatewayObservation{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var receipt routingcatalog.RolloutReceipt
		receipt.GatewayID = gatewayID
		receipt.Region = region
		if err := rows.Scan(&receipt.PublicationID, &receipt.CatalogRevision, &receipt.Status, &receipt.ErrorCode, &receipt.ObservedAt); err != nil {
			return GatewayObservation{}, err
		}
		observation.RoutingReceipts = append(observation.RoutingReceipts, receipt)
	}
	if err := rows.Err(); err != nil {
		return GatewayObservation{}, err
	}
	accessRows, err := database.QueryContext(ctx, `SELECT event_id,delivery_sequence,aggregate_type,aggregate_id,
		aggregate_revision,status,COALESCE(error_code,''),event_occurred_at,observed_at
		FROM gateway_access_rollout_receipts WHERE reported_at IS NULL
		ORDER BY observed_at,event_id,status LIMIT 128`)
	if err != nil {
		return GatewayObservation{}, err
	}
	defer accessRows.Close()
	for accessRows.Next() {
		var receipt AccessRolloutReceipt
		if err := accessRows.Scan(&receipt.EventID, &receipt.DeliverySequence, &receipt.AggregateType, &receipt.AggregateID,
			&receipt.AggregateRevision, &receipt.Status, &receipt.ErrorCode, &receipt.OccurredAt, &receipt.ObservedAt); err != nil {
			return GatewayObservation{}, err
		}
		receipt.LagMilliseconds = max64(receipt.ObservedAt.Sub(receipt.OccurredAt).Milliseconds(), 0)
		observation.AccessReceipts = append(observation.AccessReceipts, receipt)
	}
	return observation, accessRows.Err()
}

func MarkAccessReceiptsReported(ctx context.Context, database *sql.DB, observation GatewayObservation, reportedAt time.Time) error {
	if database == nil {
		return ErrInvalidArgument
	}
	for _, receipt := range observation.AccessReceipts {
		if _, err := database.ExecContext(ctx, `UPDATE gateway_access_rollout_receipts SET reported_at=$1
			WHERE event_id=$2 AND status=$3 AND reported_at IS NULL`, reportedAt.UTC(), receipt.EventID, receipt.Status); err != nil {
			return err
		}
	}
	return nil
}

func observeAccessConsumer(ctx context.Context, database *sql.DB, now time.Time) (ConsumerObservation, error) {
	value := ConsumerObservation{Name: "access_projection"}
	var lastAt, oldestGap, oldestPending *time.Time
	var gapCount, pending int64
	err := database.QueryRowContext(ctx, `SELECT COALESCE((SELECT event_id FROM gateway_access_inbox ORDER BY received_at DESC,event_id DESC LIMIT 1),''),
		COALESCE((SELECT count(*) FROM gateway_access_gaps),0),(SELECT max(received_at) FROM gateway_access_inbox),
		(SELECT min(detected_at) FROM gateway_access_gaps),
		(SELECT count(*) FROM control_outbox o WHERE o.aggregate_type IN ('Tenant','GatewayAPIKey') AND o.schema_version=2
			AND NOT EXISTS (SELECT 1 FROM gateway_access_inbox i WHERE i.event_id=o.event_id)),
		(SELECT min(occurred_at) FROM control_outbox o WHERE o.aggregate_type IN ('Tenant','GatewayAPIKey') AND o.schema_version=2
			AND NOT EXISTS (SELECT 1 FROM gateway_access_inbox i WHERE i.event_id=o.event_id))`).Scan(
		&value.LastEventID, &gapCount, &lastAt, &oldestGap, &pending, &oldestPending)
	if err != nil {
		return ConsumerObservation{}, err
	}
	if lastAt != nil {
		value.LastSucceededAt = *lastAt
	}
	value.PendingCount = pending
	oldest := oldestPending
	if oldestGap != nil && (oldest == nil || oldestGap.Before(*oldest)) {
		oldest = oldestGap
	}
	if oldest != nil {
		value.LagSeconds = nonnegativeSeconds(now.Sub(*oldest))
	}
	if gapCount > 0 {
		value.ErrorCode = "revision_gap"
	}
	return value, nil
}

func observeRoutingConsumer(ctx context.Context, database *sql.DB, gatewayID string, now time.Time) (ConsumerObservation, error) {
	value := ConsumerObservation{Name: "routing_catalog"}
	var lastAt, oldestPending *time.Time
	var status, errorCode string
	err := database.QueryRowContext(ctx, `SELECT
		COALESCE((SELECT event_id FROM gateway_routing_catalog_inbox WHERE gateway_id=$1 ORDER BY observed_at DESC,event_id DESC LIMIT 1),''),
		COALESCE((SELECT status FROM gateway_routing_catalog_inbox WHERE gateway_id=$1 ORDER BY observed_at DESC,event_id DESC LIMIT 1),''),
		COALESCE((SELECT error_code FROM gateway_routing_catalog_inbox WHERE gateway_id=$1 ORDER BY observed_at DESC,event_id DESC LIMIT 1),''),
		(SELECT max(observed_at) FROM gateway_routing_catalog_inbox WHERE gateway_id=$1 AND status='applied'),
		(SELECT count(*) FROM control_outbox o WHERE o.aggregate_type='RoutingCatalog' AND o.schema_version=1
			AND NOT EXISTS (SELECT 1 FROM gateway_routing_catalog_inbox i WHERE i.gateway_id=$1 AND i.event_id=o.event_id)),
		(SELECT min(occurred_at) FROM control_outbox o WHERE o.aggregate_type='RoutingCatalog' AND o.schema_version=1
			AND NOT EXISTS (SELECT 1 FROM gateway_routing_catalog_inbox i WHERE i.gateway_id=$1 AND i.event_id=o.event_id))`, gatewayID).Scan(
		&value.LastEventID, &status, &errorCode, &lastAt, &value.PendingCount, &oldestPending)
	if err != nil {
		return ConsumerObservation{}, err
	}
	if lastAt != nil {
		value.LastSucceededAt = *lastAt
	}
	if oldestPending != nil {
		value.LagSeconds = nonnegativeSeconds(now.Sub(*oldestPending))
	}
	if status == routingcatalog.ReceiptRejected {
		value.ErrorCode = errorCode
		if value.ErrorCode == "" {
			value.ErrorCode = "catalog_rejected"
		}
	}
	return value, nil
}
