package controlaudit

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

type Service struct {
	database *sql.DB
}

type cursor struct {
	OccurredAt time.Time `json:"occurred_at"`
	ID         string    `json:"id"`
}

func NewService(database *sql.DB) (*Service, error) {
	if database == nil {
		return nil, errors.New("Control Audit requires PostgreSQL")
	}
	return &Service{database: database}, nil
}

func (service *Service) List(ctx context.Context, actor tenantadmin.ActorEnvelope, filter Filter) (Page, error) {
	platform := hasScope(actor, tenantadmin.ScopePlatformRead) || hasScope(actor, tenantadmin.ScopePlatformWrite)
	tenant := hasScope(actor, tenantadmin.ScopeTenantRead) || hasScope(actor, tenantadmin.ScopeTenantWrite)
	if actor.Type == "" || actor.ID == "" || !platform && (!tenant || actor.ActingTenantID == "") {
		return Page{}, ErrPolicyDenied
	}
	if !platform {
		if filter.TenantID != "" && filter.TenantID != actor.ActingTenantID {
			return Page{}, ErrPolicyDenied
		}
		filter.TenantID = actor.ActingTenantID
	}
	if filter.Limit == 0 {
		filter.Limit = 50
	}
	if filter.Limit < 1 || filter.Limit > 200 || !validFilterValue(filter.TenantID) ||
		!validFilterValue(filter.AggregateType) || !validFilterValue(filter.AggregateID) ||
		!validFilterValue(filter.ActorType) || !validFilterValue(filter.ActorID) || !validFilterValue(filter.Action) ||
		!filter.From.IsZero() && !filter.Through.IsZero() && !filter.From.Before(filter.Through) {
		return Page{}, ErrInvalidArgument
	}
	position, hasCursor, err := decodeCursor(filter.Cursor)
	if err != nil {
		return Page{}, err
	}
	rows, err := service.database.QueryContext(ctx, `SELECT event_id,COALESCE(tenant_id,''),actor_type,actor_id,
		COALESCE(acting_tenant_id,''),to_json(scopes),request_id,reason,action,COALESCE(aggregate_type,''),
		COALESCE(aggregate_id,''),aggregate_revision,payload,occurred_at
		FROM control_audit_events
		WHERE ($1='' OR tenant_id=$1) AND ($2='' OR aggregate_type=$2) AND ($3='' OR aggregate_id=$3)
		  AND ($4='' OR actor_type=$4) AND ($5='' OR actor_id=$5) AND ($6='' OR action=$6)
		  AND (NOT $7 OR occurred_at >= $8) AND (NOT $9 OR occurred_at < $10)
		  AND (NOT $11 OR (occurred_at,event_id)<($12,$13))
		ORDER BY occurred_at DESC,event_id DESC LIMIT $14`, filter.TenantID, filter.AggregateType, filter.AggregateID,
		filter.ActorType, filter.ActorID, filter.Action, !filter.From.IsZero(), filter.From, !filter.Through.IsZero(), filter.Through,
		hasCursor, position.OccurredAt, position.ID, filter.Limit+1)
	if err != nil {
		return Page{}, err
	}
	defer rows.Close()
	data := make([]Event, 0, filter.Limit+1)
	for rows.Next() {
		var event Event
		var scopes []byte
		if err := rows.Scan(&event.ID, &event.TenantID, &event.ActorType, &event.ActorID, &event.ActingTenantID,
			&scopes, &event.RequestID, &event.Reason, &event.Action, &event.AggregateType, &event.AggregateID,
			&event.AggregateRevision, &event.Payload, &event.OccurredAt); err != nil {
			return Page{}, err
		}
		if err := json.Unmarshal(scopes, &event.Scopes); err != nil {
			return Page{}, err
		}
		data = append(data, event)
	}
	if err := rows.Err(); err != nil {
		return Page{}, err
	}
	page := Page{Data: data}
	if len(data) > filter.Limit {
		page.Data = data[:filter.Limit]
		page.NextCursor, err = encodeCursor(page.Data[len(page.Data)-1])
		if err != nil {
			return Page{}, err
		}
	}
	return page, nil
}

func validFilterValue(value string) bool {
	return len(value) <= 255 && !strings.ContainsRune(value, '\x00')
}

func hasScope(actor tenantadmin.ActorEnvelope, wanted string) bool {
	for _, scope := range actor.Scopes {
		if scope == wanted {
			return true
		}
	}
	return false
}

func encodeCursor(event Event) (string, error) {
	payload, err := json.Marshal(cursor{OccurredAt: event.OccurredAt.UTC(), ID: event.ID})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

func decodeCursor(value string) (cursor, bool, error) {
	if value == "" {
		return cursor{}, false, nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return cursor{}, false, fmt.Errorf("%w: invalid cursor", ErrInvalidArgument)
	}
	var position cursor
	if err := json.Unmarshal(payload, &position); err != nil || position.OccurredAt.IsZero() || position.ID == "" || !validFilterValue(position.ID) {
		return cursor{}, false, fmt.Errorf("%w: invalid cursor", ErrInvalidArgument)
	}
	return position, true, nil
}
