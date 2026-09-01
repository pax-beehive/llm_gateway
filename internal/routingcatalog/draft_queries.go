package routingcatalog

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

type draftCursor struct {
	UpdatedAt time.Time `json:"updated_at"`
	ID        string    `json:"id"`
}

func (service *Service) ListDrafts(ctx context.Context, actor tenantadmin.ActorEnvelope, filter DraftFilter) (DraftPage, error) {
	if err := authorizeCatalogRead(actor); err != nil {
		return DraftPage{}, err
	}
	if filter.Limit == 0 {
		filter.Limit = 50
	}
	if filter.Limit < 1 || filter.Limit > 100 || filter.Status != "" && filter.Status != DraftOpen && filter.Status != DraftValidated {
		return DraftPage{}, ErrInvalidArgument
	}
	cursor, hasCursor, err := decodeDraftCursor(filter.Cursor)
	if err != nil {
		return DraftPage{}, err
	}
	rows, err := service.database.QueryContext(ctx, `SELECT id,base_revision,document,status,revision,validation_report,validation_hash,
		created_by,updated_by,created_at,updated_at FROM routing_catalog_drafts
		WHERE ($1='' OR status=$1) AND (NOT $2 OR (updated_at,id)<($3,$4))
		ORDER BY updated_at DESC,id DESC LIMIT $5`, filter.Status, hasCursor, cursor.UpdatedAt, cursor.ID, filter.Limit+1)
	if err != nil {
		return DraftPage{}, err
	}
	defer rows.Close()
	data := make([]Draft, 0, filter.Limit+1)
	for rows.Next() {
		draft, err := scanDraft(rows)
		if err != nil {
			return DraftPage{}, err
		}
		data = append(data, draft)
	}
	if err := rows.Err(); err != nil {
		return DraftPage{}, err
	}
	page := DraftPage{Data: data}
	if len(data) > filter.Limit {
		page.Data = data[:filter.Limit]
		page.NextCursor, err = encodeDraftCursor(page.Data[len(page.Data)-1])
		if err != nil {
			return DraftPage{}, err
		}
	}
	return page, nil
}

func encodeDraftCursor(draft Draft) (string, error) {
	payload, err := json.Marshal(draftCursor{UpdatedAt: draft.UpdatedAt.UTC(), ID: draft.ID})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

func decodeDraftCursor(value string) (draftCursor, bool, error) {
	if value == "" {
		return draftCursor{}, false, nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return draftCursor{}, false, fmt.Errorf("%w: invalid draft cursor", ErrInvalidArgument)
	}
	var cursor draftCursor
	if err := json.Unmarshal(payload, &cursor); err != nil || cursor.UpdatedAt.IsZero() || !managedResourceIDPattern.MatchString(cursor.ID) {
		return draftCursor{}, false, fmt.Errorf("%w: invalid draft cursor", ErrInvalidArgument)
	}
	return cursor, true, nil
}
