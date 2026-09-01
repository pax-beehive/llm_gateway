package providerconnection

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

type operationCursor struct {
	CreatedAt time.Time `json:"created_at"`
	ID        string    `json:"id"`
}

func (service *Service) ListOperations(ctx context.Context, actor tenantadmin.ActorEnvelope, filter OperationFilter) (OperationPage, error) {
	if err := authorizeRead(actor); err != nil {
		return OperationPage{}, err
	}
	if filter.Limit == 0 {
		filter.Limit = 50
	}
	if filter.Limit < 1 || filter.Limit > 100 ||
		filter.ConnectionID != "" && !resourceIDPattern.MatchString(filter.ConnectionID) ||
		filter.Type != "" && !validOperationType(filter.Type) ||
		filter.Status != "" && !validOperationStatus(filter.Status) {
		return OperationPage{}, ErrInvalidArgument
	}
	cursor, hasCursor, err := decodeOperationCursor(filter.Cursor)
	if err != nil {
		return OperationPage{}, err
	}
	rows, err := service.database.QueryContext(ctx, operationSelect+`
		WHERE ($1='' OR connection_id=$1) AND ($2='' OR operation_type=$2) AND ($3='' OR status=$3)
		  AND (NOT $4 OR (created_at,id)<($5,$6))
		ORDER BY created_at DESC,id DESC LIMIT $7`, filter.ConnectionID, filter.Type, filter.Status,
		hasCursor, cursor.CreatedAt, cursor.ID, filter.Limit+1)
	if err != nil {
		return OperationPage{}, err
	}
	defer rows.Close()
	data := make([]Operation, 0, filter.Limit+1)
	for rows.Next() {
		operation, err := scanOperation(rows)
		if err != nil {
			return OperationPage{}, err
		}
		data = append(data, publicOperation(operation))
	}
	if err := rows.Err(); err != nil {
		return OperationPage{}, err
	}
	page := OperationPage{Data: data}
	if len(data) > filter.Limit {
		page.Data = data[:filter.Limit]
		page.NextCursor, err = encodeOperationCursor(page.Data[len(page.Data)-1])
		if err != nil {
			return OperationPage{}, err
		}
	}
	return page, nil
}

func validOperationType(value OperationType) bool {
	return value == OperationProbe || value == OperationModelDiscovery || value == OperationCredentialRotation
}

func validOperationStatus(value OperationStatus) bool {
	return value == OperationQueued || value == OperationRunning || value == OperationSucceeded || value == OperationFailed || value == OperationUncertain
}

func encodeOperationCursor(operation Operation) (string, error) {
	payload, err := json.Marshal(operationCursor{CreatedAt: operation.CreatedAt.UTC(), ID: operation.ID})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

func decodeOperationCursor(value string) (operationCursor, bool, error) {
	if value == "" {
		return operationCursor{}, false, nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return operationCursor{}, false, fmt.Errorf("%w: invalid operation cursor", ErrInvalidArgument)
	}
	var cursor operationCursor
	if err := json.Unmarshal(payload, &cursor); err != nil || cursor.CreatedAt.IsZero() || !resourceIDPattern.MatchString(cursor.ID) {
		return operationCursor{}, false, fmt.Errorf("%w: invalid operation cursor", ErrInvalidArgument)
	}
	return cursor, true, nil
}
