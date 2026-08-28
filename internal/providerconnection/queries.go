package providerconnection

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

func (service *Service) Get(ctx context.Context, actor tenantadmin.ActorEnvelope, connectionID string) (ProviderConnection, error) {
	if err := authorizeRead(actor); err != nil {
		return ProviderConnection{}, err
	}
	if !resourceIDPattern.MatchString(connectionID) {
		return ProviderConnection{}, ErrInvalidArgument
	}
	connection, err := scanConnection(service.database.QueryRowContext(ctx, connectionSelect+` WHERE id=$1`, connectionID))
	return publicConnection(connection), err
}

func (service *Service) List(ctx context.Context, actor tenantadmin.ActorEnvelope, filter ConnectionFilter) (ConnectionPage, error) {
	if err := authorizeRead(actor); err != nil {
		return ConnectionPage{}, err
	}
	if filter.Limit == 0 {
		filter.Limit = 50
	}
	if filter.Limit < 1 || filter.Limit > 100 {
		return ConnectionPage{}, fmt.Errorf("%w: limit must be between 1 and 100", ErrInvalidArgument)
	}
	if filter.Provider != "" && !validProvider(filter.Provider) || filter.Status != "" && !validStatus(filter.Status) {
		return ConnectionPage{}, ErrInvalidArgument
	}
	cursor, err := decodeCursor(filter.Cursor)
	if err != nil {
		return ConnectionPage{}, err
	}
	rows, err := service.database.QueryContext(ctx, connectionSelect+`
		WHERE ($1='' OR provider=$1) AND ($2='' OR region=$2) AND ($3='' OR administrative_status=$3)
		  AND id > $4 ORDER BY id LIMIT $5`, filter.Provider, filter.Region, filter.Status, cursor, filter.Limit+1)
	if err != nil {
		return ConnectionPage{}, err
	}
	defer rows.Close()
	data := make([]ProviderConnection, 0, filter.Limit+1)
	for rows.Next() {
		connection, err := scanConnection(rows)
		if err != nil {
			return ConnectionPage{}, err
		}
		data = append(data, publicConnection(connection))
	}
	if err := rows.Err(); err != nil {
		return ConnectionPage{}, err
	}
	page := ConnectionPage{Data: data}
	if len(data) > filter.Limit {
		page.Data = data[:filter.Limit]
		page.NextCursor = encodeCursor(page.Data[len(page.Data)-1].ID)
	}
	return page, nil
}

func authorizeRead(actor tenantadmin.ActorEnvelope) error {
	if actor.Type == "" || actor.ID == "" {
		return ErrInvalidArgument
	}
	for _, scope := range actor.Scopes {
		if scope == tenantadmin.ScopePlatformRead || scope == tenantadmin.ScopePlatformWrite {
			return nil
		}
	}
	return ErrPolicyDenied
}

func encodeCursor(id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(id))
}

func decodeCursor(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || !resourceIDPattern.Match(decoded) || strings.ContainsRune(string(decoded), '\x00') {
		return "", fmt.Errorf("%w: invalid cursor", ErrInvalidArgument)
	}
	return string(decoded), nil
}
