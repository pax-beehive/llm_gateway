package providerconnection

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

// DiscoveredModelPage is an immutable inventory from one completed discovery.
// Its cursor is bound to that operation, so refreshing cannot mix snapshots.
type DiscoveredModelPage struct {
	Data       []ObservedModel `json:"data"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

func (service *Service) ListDiscoveredModels(ctx context.Context, actor tenantadmin.ActorEnvelope, operationID, cursor string, limit int) (DiscoveredModelPage, error) {
	operation, err := service.GetOperation(ctx, actor, operationID)
	if err != nil {
		return DiscoveredModelPage{}, err
	}
	if operation.Type != OperationModelDiscovery || operation.Status != OperationSucceeded {
		return DiscoveredModelPage{}, ErrInvalidArgument
	}
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 100 {
		return DiscoveredModelPage{}, ErrInvalidArgument
	}
	after := ""
	if cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		parts := strings.SplitN(string(raw), "\x00", 2)
		if err != nil || len(parts) != 2 || parts[0] != operationID || parts[1] == "" || len(parts[1]) > 512 {
			return DiscoveredModelPage{}, ErrInvalidArgument
		}
		after = parts[1]
	}
	rows, err := service.database.QueryContext(ctx, `SELECT provider_model_id, owned_by, capabilities FROM provider_model_observations WHERE operation_id=$1 AND provider_model_id>$2 ORDER BY provider_model_id LIMIT $3`, operationID, after, limit+1)
	if err != nil {
		return DiscoveredModelPage{}, err
	}
	defer rows.Close()
	page := DiscoveredModelPage{Data: make([]ObservedModel, 0)}
	for rows.Next() {
		var model ObservedModel
		var capabilities []byte
		if err := rows.Scan(&model.ID, &model.OwnedBy, &capabilities); err != nil {
			return DiscoveredModelPage{}, err
		}
		if err := json.Unmarshal(capabilities, &model.Capabilities); err != nil {
			return DiscoveredModelPage{}, err
		}
		page.Data = append(page.Data, model)
	}
	if err := rows.Err(); err != nil {
		return DiscoveredModelPage{}, err
	}
	if len(page.Data) > limit {
		page.Data = page.Data[:limit]
		page.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(operationID + "\x00" + page.Data[limit-1].ID))
	}
	return page, nil
}
