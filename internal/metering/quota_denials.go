package metering

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/toddzheng/llm-gateway/internal/quota"
)

type quotaDenialCursor struct {
	OccurredAt time.Time `json:"occurred_at"`
	EventID    string    `json:"event_id"`
}

func (service *Service) QuotaDenials(ctx context.Context, filter QuotaDenialFilter) (QuotaDenialPage, error) {
	if filter.TenantID == "" && !filter.AllTenants {
		return QuotaDenialPage{}, fmt.Errorf("%w: Tenant scope is required", ErrInvalidArgument)
	}
	if filter.Limit == 0 {
		filter.Limit = 50
	}
	if filter.Limit < 1 || filter.Limit > 200 || len(filter.Scope) > 64 || len(filter.Dimension) > 128 ||
		strings.ContainsRune(filter.Scope, '\x00') || strings.ContainsRune(filter.Dimension, '\x00') ||
		!filter.From.IsZero() && !filter.Through.IsZero() && filter.Through.Before(filter.From) {
		return QuotaDenialPage{}, ErrInvalidArgument
	}
	cursor, hasCursor, err := decodeQuotaDenialCursor(filter.Cursor)
	if err != nil {
		return QuotaDenialPage{}, err
	}
	where := []string{"TRUE"}
	args := []any{}
	addEqual := func(column string, value any, present bool) {
		if !present {
			return
		}
		args = append(args, value)
		where = append(where, fmt.Sprintf("d.%s=$%d", column, len(args)))
	}
	addEqual("tenant_id", filter.TenantID, filter.TenantID != "")
	addEqual("api_key_id", filter.APIKeyID, filter.APIKeyID != "")
	addEqual("response_id", filter.ResponseID, filter.ResponseID != "")
	addEqual("route_id", filter.RouteID, filter.RouteID != "")
	addEqual("public_model", filter.PublicModel, filter.PublicModel != "")
	addEqual("denial_scope", filter.Scope, filter.Scope != "")
	addEqual("dimension", filter.Dimension, filter.Dimension != "")
	if !filter.From.IsZero() {
		args = append(args, filter.From.UTC())
		where = append(where, fmt.Sprintf("d.occurred_at >= $%d", len(args)))
	}
	if !filter.Through.IsZero() {
		args = append(args, filter.Through.UTC())
		where = append(where, fmt.Sprintf("d.occurred_at <= $%d", len(args)))
	}
	if hasCursor {
		args = append(args, cursor.OccurredAt, cursor.EventID)
		where = append(where, fmt.Sprintf("(d.occurred_at,d.event_id)<($%d,$%d)", len(args)-1, len(args)))
	}
	args = append(args, filter.Limit+1)
	rows, err := service.database.QueryContext(ctx, `SELECT i.payload FROM metering_quota_denials d
		JOIN metering_inbox i USING(event_id) WHERE `+strings.Join(where, " AND ")+fmt.Sprintf(`
		ORDER BY d.occurred_at DESC,d.event_id DESC LIMIT $%d`, len(args)), args...)
	if err != nil {
		return QuotaDenialPage{}, err
	}
	defer rows.Close()
	page := QuotaDenialPage{Data: []quota.DenialEvent{}}
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return QuotaDenialPage{}, err
		}
		var event quota.DenialEvent
		if err := json.Unmarshal(payload, &event); err != nil {
			return QuotaDenialPage{}, err
		}
		page.Data = append(page.Data, event)
	}
	if err := rows.Err(); err != nil {
		return QuotaDenialPage{}, err
	}
	if len(page.Data) > filter.Limit {
		page.Data = page.Data[:filter.Limit]
		last := page.Data[len(page.Data)-1]
		page.NextCursor, err = encodeQuotaDenialCursor(last)
		if err != nil {
			return QuotaDenialPage{}, err
		}
	}
	_ = service.database.QueryRowContext(ctx, `SELECT COALESCE(max(occurred_at),'epoch') FROM metering_quota_denials`).Scan(&page.DataCutoff)
	return page, nil
}

func encodeQuotaDenialCursor(event quota.DenialEvent) (string, error) {
	payload, err := json.Marshal(quotaDenialCursor{OccurredAt: event.OccurredAt.UTC(), EventID: event.EventID})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

func decodeQuotaDenialCursor(value string) (quotaDenialCursor, bool, error) {
	if value == "" {
		return quotaDenialCursor{}, false, nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return quotaDenialCursor{}, false, fmt.Errorf("%w: invalid cursor", ErrInvalidArgument)
	}
	var cursor quotaDenialCursor
	if err := json.Unmarshal(payload, &cursor); err != nil || cursor.OccurredAt.IsZero() || cursor.EventID == "" || strings.ContainsRune(cursor.EventID, '\x00') {
		return quotaDenialCursor{}, false, fmt.Errorf("%w: invalid cursor", ErrInvalidArgument)
	}
	return cursor, true, nil
}
