package quota

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	DenialEventType          = "QuotaAdmissionDenied"
	DenialEventSchemaVersion = 1
)

// DenialEvent is content-free evidence that admission failed before a Provider
// side effect. It is an operational fact, never a Usage Ledger row.
type DenialEvent struct {
	EventID              string    `json:"event_id"`
	SchemaVersion        int       `json:"schema_version"`
	Type                 string    `json:"type"`
	TenantID             string    `json:"tenant_id"`
	APIKeyID             string    `json:"api_key_id,omitempty"`
	ResponseID           string    `json:"response_id,omitempty"`
	AttemptID            string    `json:"attempt_id,omitempty"`
	OperationID          string    `json:"operation_id,omitempty"`
	Capability           string    `json:"capability,omitempty"`
	PublicModel          string    `json:"public_model,omitempty"`
	RouteID              string    `json:"route_id,omitempty"`
	Region               string    `json:"region,omitempty"`
	Scope                string    `json:"scope"`
	Dimension            string    `json:"dimension"`
	Currency             string    `json:"currency,omitempty"`
	TenantPolicyRevision int64     `json:"tenant_policy_revision,omitempty"`
	APIKeyPolicyRevision int64     `json:"api_key_policy_revision,omitempty"`
	OccurredAt           time.Time `json:"occurred_at"`
}

type DenialRecorder interface {
	RecordDenial(context.Context, DenialEvent) error
}

func (event DenialEvent) Validate() error {
	if event.EventID == "" || event.SchemaVersion != DenialEventSchemaVersion || event.Type != DenialEventType ||
		event.TenantID == "" || event.Scope == "" || event.Dimension == "" || event.OccurredAt.IsZero() {
		return errors.New("quota denial event requires complete identity, scope, dimension, and time")
	}
	if event.Scope != "tenant" && event.Scope != "api_key" && event.Scope != "effective_policy" && event.Scope != "gateway" {
		return errors.New("quota denial event has invalid scope")
	}
	return nil
}

func (c *PostgresController) RecordDenial(ctx context.Context, event DenialEvent) error {
	if event.EventID == "" {
		id, err := denialID()
		if err != nil {
			return err
		}
		event.EventID = id
	}
	if event.SchemaVersion == 0 {
		event.SchemaVersion = DenialEventSchemaVersion
	}
	if event.Type == "" {
		event.Type = DenialEventType
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = c.now().UTC()
	}
	if err := event.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = c.db.ExecContext(ctx, `INSERT INTO transactional_outbox(
		tenant_id,aggregate_type,aggregate_id,aggregate_revision,event_type,payload,created_at)
		VALUES($1,'quota_denial',$2,1,'quota.denied',$3,$4)`, event.TenantID, event.EventID, payload, event.OccurredAt)
	return err
}

func (c *PostgresController) rejectReservation(ctx context.Context, request ReservationRequest, scope string, denial error) (Reservation, error) {
	region := request.Region
	if region == "" {
		region = request.HomeRegion
	}
	event := DenialEvent{
		TenantID: request.TenantID, APIKeyID: request.APIKeyID, ResponseID: request.ResponseID,
		AttemptID: request.ResponseAttemptID, OperationID: request.CapabilityOperationID, Capability: string(request.Capability),
		PublicModel: request.PublicModel, RouteID: request.RouteID, Region: region,
		Scope: scope, Dimension: denialDimension(denial), Currency: request.Currency,
		TenantPolicyRevision: request.TenantPolicyRevision, APIKeyPolicyRevision: request.APIKeyPolicyRevision,
	}
	if err := c.RecordDenial(ctx, event); err != nil {
		return Reservation{}, errors.Join(denial, fmt.Errorf("record quota denial evidence: %w", err))
	}
	return Reservation{}, denial
}

func (c *PostgresController) rejectRefreshReservation(ctx context.Context, request RefreshReservationRequest, scope string, denial error) (Reservation, error) {
	event := DenialEvent{
		TenantID: request.TenantID, APIKeyID: request.APIKeyID, OperationID: request.CacheRefreshIntentID,
		Capability: "cache_refresh", Scope: scope, Dimension: denialDimension(denial), Currency: request.Currency,
		TenantPolicyRevision: request.TenantPolicyRevision, APIKeyPolicyRevision: request.APIKeyPolicyRevision,
	}
	if err := c.RecordDenial(ctx, event); err != nil {
		return Reservation{}, errors.Join(denial, fmt.Errorf("record quota denial evidence: %w", err))
	}
	return Reservation{}, denial
}

func denialDimension(err error) string {
	message := err.Error()
	if index := strings.LastIndex(message, ": "); index >= 0 {
		message = message[index+2:]
	}
	message = strings.TrimSpace(strings.ToLower(message))
	message = strings.ReplaceAll(message, " ", "_")
	message = strings.ReplaceAll(message, "/", "_")
	if message == "" {
		return "unknown"
	}
	return message
}

func denialID() (string, error) {
	var payload [16]byte
	if _, err := rand.Read(payload[:]); err != nil {
		return "", err
	}
	return "qden_" + hex.EncodeToString(payload[:]), nil
}
