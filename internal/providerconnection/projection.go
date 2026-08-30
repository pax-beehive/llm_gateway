package providerconnection

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/toddzheng/llm-gateway/internal/controlevent"
	"github.com/toddzheng/llm-gateway/internal/provider"
)

var ErrProjectionGap = errors.New("Gateway Provider Connection projection revision gap")

type Projection struct {
	database *sql.DB
	region   string
	now      func() time.Time
}

func NewProjection(database *sql.DB, region string, now func() time.Time) (*Projection, error) {
	if database == nil || strings.TrimSpace(region) == "" {
		return nil, errors.New("Gateway Provider Connection projection requires PostgreSQL and region")
	}
	if now == nil {
		now = time.Now
	}
	return &Projection{database: database, region: region, now: now}, nil
}

type ExecutionSnapshot struct {
	ConnectionID         string                     `json:"connection_id"`
	Provider             string                     `json:"provider"`
	BaseURL              string                     `json:"base_url"`
	Region               string                     `json:"region"`
	PreviousRegion       string                     `json:"previous_region"`
	CredentialScope      string                     `json:"credential_scope"`
	Capability           provider.CapabilityProfile `json:"capability_declaration"`
	AdministrativeStatus AdministrativeStatus       `json:"administrative_status"`
	CredentialVersion    int64                      `json:"credential_version"`
	Revision             int64                      `json:"revision"`
	ObservedHealthy      *bool                      `json:"observed_healthy,omitempty"`
	ObservedAt           time.Time                  `json:"observed_at"`
}

// ReplaceSnapshots installs one complete regional execution projection. It is
// used only by the startup bootstrap path before the Gateway accepts traffic.
func (projection *Projection) ReplaceSnapshots(ctx context.Context, snapshots []ExecutionSnapshot) error {
	transaction, err := projection.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	if _, err := transaction.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('gateway-provider-connection-bootstrap',0))`); err != nil {
		return err
	}
	ids := make([]string, 0, len(snapshots))
	seen := make(map[string]struct{}, len(snapshots))
	for _, snapshot := range snapshots {
		if !resourceIDPattern.MatchString(snapshot.ConnectionID) || snapshot.Region != projection.region || snapshot.Revision <= 0 ||
			snapshot.CredentialVersion <= 0 || snapshot.ObservedAt.IsZero() || validateBaseURL(snapshot.Provider, snapshot.BaseURL) != nil ||
			(snapshot.AdministrativeStatus != StatusEnabled && snapshot.AdministrativeStatus != StatusDisabled) {
			return ErrInvalidArgument
		}
		if _, duplicate := seen[snapshot.ConnectionID]; duplicate {
			return ErrInvalidArgument
		}
		seen[snapshot.ConnectionID] = struct{}{}
		var current int64
		err := transaction.QueryRowContext(ctx, `SELECT revision FROM gateway_provider_connection_projection WHERE id=$1 FOR UPDATE`, snapshot.ConnectionID).Scan(&current)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if current > snapshot.Revision {
			return fmt.Errorf("%w: snapshot revision %d is behind projection revision %d", ErrProjectionGap, snapshot.Revision, current)
		}
		capability, err := json.Marshal(snapshot.Capability)
		if err != nil {
			return err
		}
		if _, err := transaction.ExecContext(ctx, `INSERT INTO gateway_provider_connection_projection (
			id,provider,base_url,region,credential_scope,capability_declaration,administrative_status,
			revision,credential_version,observed_healthy,event_occurred_at,applied_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (id) DO UPDATE SET provider=EXCLUDED.provider,base_url=EXCLUDED.base_url,region=EXCLUDED.region,
		credential_scope=EXCLUDED.credential_scope,capability_declaration=EXCLUDED.capability_declaration,
		administrative_status=EXCLUDED.administrative_status,revision=EXCLUDED.revision,
		credential_version=EXCLUDED.credential_version,observed_healthy=EXCLUDED.observed_healthy,
		event_occurred_at=EXCLUDED.event_occurred_at,applied_at=EXCLUDED.applied_at`, snapshot.ConnectionID,
			snapshot.Provider, snapshot.BaseURL, snapshot.Region, snapshot.CredentialScope, capability,
			snapshot.AdministrativeStatus, snapshot.Revision, snapshot.CredentialVersion, snapshot.ObservedHealthy,
			snapshot.ObservedAt, projection.now().UTC()); err != nil {
			return err
		}
		if _, err := transaction.ExecContext(ctx, `DELETE FROM gateway_provider_connection_projection_gaps WHERE connection_id=$1`, snapshot.ConnectionID); err != nil {
			return err
		}
		ids = append(ids, snapshot.ConnectionID)
	}
	if _, err := transaction.ExecContext(ctx, `DELETE FROM gateway_provider_connection_projection
		WHERE region=$1 AND NOT (id=ANY($2))`, projection.region, ids); err != nil {
		return err
	}
	return transaction.Commit()
}

func (projection *Projection) Consume(ctx context.Context, event controlevent.Event) error {
	if event.AggregateType == "ProviderOperation" && event.EventType == "ProviderConnectionHealthObserved" {
		return projection.consumeHealth(ctx, event)
	}
	if event.AggregateType != "ProviderConnection" {
		return nil
	}
	if event.SchemaVersion != 3 || event.EventID == "" || event.DeliverySequence <= 0 ||
		!resourceIDPattern.MatchString(event.AggregateID) || event.AggregateRevision <= 0 || event.OccurredAt.IsZero() {
		return ErrInvalidArgument
	}
	var payload ExecutionSnapshot
	if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.ConnectionID != event.AggregateID ||
		payload.Revision != event.AggregateRevision || payload.CredentialVersion <= 0 || payload.Region == "" ||
		payload.Region != projection.region && payload.PreviousRegion != projection.region ||
		payload.AdministrativeStatus != StatusEnabled && payload.AdministrativeStatus != StatusDisabled ||
		validateBaseURL(payload.Provider, payload.BaseURL) != nil {
		return ErrInvalidArgument
	}
	transaction, err := projection.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	if _, err := transaction.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "gateway-provider-connection\x1f"+payload.ConnectionID); err != nil {
		return err
	}
	var duplicate bool
	if err := transaction.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM gateway_provider_connection_projection_inbox WHERE event_id=$1)`, event.EventID).Scan(&duplicate); err != nil {
		return err
	}
	if duplicate {
		return transaction.Commit()
	}
	var current int64
	err = transaction.QueryRowContext(ctx, `SELECT revision FROM gateway_provider_connection_projection WHERE id=$1 FOR UPDATE`, payload.ConnectionID).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		current = 0
	} else if err != nil {
		return err
	}
	if event.AggregateRevision <= current {
		if err := projection.recordInbox(ctx, transaction, event, "stale"); err != nil {
			return err
		}
		return transaction.Commit()
	}
	// A region move is a full execution-metadata snapshot for the destination
	// region. That region intentionally did not receive earlier revisions, so it
	// may establish its first local head from the move revision.
	regionBootstrap := current == 0 && (payload.Region == projection.region || payload.PreviousRegion == projection.region)
	if event.AggregateRevision != current+1 && !regionBootstrap {
		if _, err := transaction.ExecContext(ctx, `INSERT INTO gateway_provider_connection_projection_gaps (
			connection_id,expected_revision,received_revision,event_id,delivery_sequence,detected_at
		) VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (connection_id) DO UPDATE SET
			expected_revision=EXCLUDED.expected_revision,received_revision=GREATEST(gateway_provider_connection_projection_gaps.received_revision,EXCLUDED.received_revision),
			event_id=EXCLUDED.event_id,delivery_sequence=EXCLUDED.delivery_sequence,detected_at=LEAST(gateway_provider_connection_projection_gaps.detected_at,EXCLUDED.detected_at)`,
			payload.ConnectionID, current+1, event.AggregateRevision, event.EventID, event.DeliverySequence, projection.now().UTC()); err != nil {
			return err
		}
		if err := transaction.Commit(); err != nil {
			return err
		}
		return fmt.Errorf("%w: %s expected %d received %d", ErrProjectionGap, payload.ConnectionID, current+1, event.AggregateRevision)
	}
	capability, err := json.Marshal(payload.Capability)
	if err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx, `INSERT INTO gateway_provider_connection_projection (
		id,provider,base_url,region,credential_scope,capability_declaration,administrative_status,
		revision,credential_version,observed_healthy,event_occurred_at,applied_at
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NULL,$10,$11)
	ON CONFLICT (id) DO UPDATE SET provider=EXCLUDED.provider,base_url=EXCLUDED.base_url,
		region=EXCLUDED.region,credential_scope=EXCLUDED.credential_scope,capability_declaration=EXCLUDED.capability_declaration,
		administrative_status=EXCLUDED.administrative_status,revision=EXCLUDED.revision,
		credential_version=EXCLUDED.credential_version,observed_healthy=NULL,
		event_occurred_at=EXCLUDED.event_occurred_at,applied_at=EXCLUDED.applied_at`,
		payload.ConnectionID, payload.Provider, payload.BaseURL, payload.Region, payload.CredentialScope, capability,
		payload.AdministrativeStatus, payload.Revision, payload.CredentialVersion, event.OccurredAt, projection.now().UTC()); err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx, `DELETE FROM gateway_provider_connection_projection_gaps WHERE connection_id=$1`, payload.ConnectionID); err != nil {
		return err
	}
	if err := projection.recordInbox(ctx, transaction, event, "applied"); err != nil {
		return err
	}
	return transaction.Commit()
}

func (projection *Projection) consumeHealth(ctx context.Context, event controlevent.Event) error {
	if event.SchemaVersion != 2 || event.EventID == "" || event.DeliverySequence <= 0 || event.OccurredAt.IsZero() {
		return ErrInvalidArgument
	}
	var payload struct {
		ConnectionID       string `json:"connection_id"`
		ConnectionRevision int64  `json:"connection_revision"`
		ObservedStatus     string `json:"observed_status"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil || !resourceIDPattern.MatchString(payload.ConnectionID) ||
		payload.ConnectionRevision <= 0 || payload.ObservedStatus != "healthy" && payload.ObservedStatus != "unhealthy" {
		return ErrInvalidArgument
	}
	transaction, err := projection.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	var duplicate bool
	if err := transaction.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM gateway_provider_connection_projection_inbox WHERE event_id=$1)`, event.EventID).Scan(&duplicate); err != nil {
		return err
	}
	if duplicate {
		return transaction.Commit()
	}
	result, err := transaction.ExecContext(ctx, `UPDATE gateway_provider_connection_projection SET observed_healthy=$2,applied_at=$3
		WHERE id=$1 AND revision=$4`, payload.ConnectionID, payload.ObservedStatus == "healthy", projection.now().UTC(), payload.ConnectionRevision)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	disposition := "applied"
	if count == 0 {
		disposition = "stale"
	}
	if err := projection.recordInbox(ctx, transaction, event, disposition); err != nil {
		return err
	}
	return transaction.Commit()
}

func (projection *Projection) recordInbox(ctx context.Context, transaction *sql.Tx, event controlevent.Event, disposition string) error {
	_, err := transaction.ExecContext(ctx, `INSERT INTO gateway_provider_connection_projection_inbox (
		event_id,delivery_sequence,aggregate_type,aggregate_id,aggregate_revision,disposition,received_at
	) VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (event_id) DO NOTHING`, event.EventID, event.DeliverySequence,
		event.AggregateType, event.AggregateID, event.AggregateRevision, disposition, projection.now().UTC())
	return err
}
