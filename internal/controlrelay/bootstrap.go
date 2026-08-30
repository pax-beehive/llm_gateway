package controlrelay

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/toddzheng/llm-gateway/internal/access"
	"github.com/toddzheng/llm-gateway/internal/controlevent"
	"github.com/toddzheng/llm-gateway/internal/operations"
	"github.com/toddzheng/llm-gateway/internal/providerconnection"
	"github.com/toddzheng/llm-gateway/internal/routingcatalog"
)

const (
	BootstrapPath             = "/internal/v1/control-bootstrap"
	bootstrapSchemaVersion    = 1
	maxBootstrapTenants       = 10_000
	maxBootstrapKeys          = 100_000
	maxBootstrapConnections   = 10_000
	maxBootstrapResponseBytes = 16 << 20
)

var ErrBootstrapTooLarge = errors.New("Control Event bootstrap exceeds bounded limits")

type RoutingBootstrap struct {
	Revision       int64                           `json:"revision"`
	Document       routingcatalog.Document         `json:"document"`
	Validation     routingcatalog.ValidationReport `json:"validation_report"`
	ValidationHash string                          `json:"validation_hash"`
	CreatedAt      time.Time                       `json:"created_at"`
}

type Bootstrap struct {
	SchemaVersion       int                                    `json:"schema_version"`
	SourceCursor        int64                                  `json:"source_cursor"`
	CreatedAt           time.Time                              `json:"created_at"`
	Access              []access.Snapshot                      `json:"access"`
	ProviderConnections []providerconnection.ExecutionSnapshot `json:"provider_connections"`
	RoutingCatalog      *RoutingBootstrap                      `json:"routing_catalog,omitempty"`
}

type BootstrapPublisher interface {
	PublishBootstrap(context.Context, controlevent.Audience) (Bootstrap, error)
}

type PostgresBootstrapPublisher struct {
	database *sql.DB
	now      func() time.Time
}

func NewPostgresBootstrapPublisher(database *sql.DB, now func() time.Time) (*PostgresBootstrapPublisher, error) {
	if database == nil {
		return nil, errors.New("PostgreSQL Control Event bootstrap publisher requires a database")
	}
	if now == nil {
		now = time.Now
	}
	return &PostgresBootstrapPublisher{database: database, now: now}, nil
}

func (publisher *PostgresBootstrapPublisher) PublishBootstrap(ctx context.Context, audience controlevent.Audience) (Bootstrap, error) {
	if audience.GatewayID == "" || audience.Region == "" {
		return Bootstrap{}, errors.New("invalid Control Event bootstrap audience")
	}
	transaction, err := publisher.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return Bootstrap{}, err
	}
	defer func() { _ = transaction.Rollback() }()
	result := Bootstrap{SchemaVersion: bootstrapSchemaVersion, CreatedAt: publisher.now().UTC(), Access: []access.Snapshot{}, ProviderConnections: []providerconnection.ExecutionSnapshot{}}
	if err := transaction.QueryRowContext(ctx, `SELECT GREATEST(COALESCE(max(delivery_sequence),0),
		(SELECT minimum_cursor FROM control_event_history WHERE singleton=true)) FROM control_outbox`).Scan(&result.SourceCursor); err != nil {
		return Bootstrap{}, err
	}
	rows, err := transaction.QueryContext(ctx, `SELECT id,status,revision,home_region,execution_epoch,policy_revision,policy
		FROM tenants WHERE home_region=$1 ORDER BY id LIMIT $2`, audience.Region, maxBootstrapTenants+1)
	if err != nil {
		return Bootstrap{}, err
	}
	tenantIndex := map[string]int{}
	for rows.Next() {
		var snapshot access.Snapshot
		var policy []byte
		if err := rows.Scan(&snapshot.Tenant.ID, &snapshot.Tenant.Status, &snapshot.Tenant.Revision, &snapshot.Tenant.HomeRegion,
			&snapshot.Tenant.ExecutionEpoch, &snapshot.Tenant.Policy.Revision, &policy); err != nil {
			_ = rows.Close()
			return Bootstrap{}, err
		}
		if err := json.Unmarshal(policy, &snapshot.Tenant.Policy); err != nil {
			_ = rows.Close()
			return Bootstrap{}, err
		}
		snapshot.CreatedAt = result.CreatedAt
		snapshot.Keys = []access.KeySnapshot{}
		tenantIndex[snapshot.Tenant.ID] = len(result.Access)
		result.Access = append(result.Access, snapshot)
	}
	if err := rows.Close(); err != nil {
		return Bootstrap{}, err
	}
	if len(result.Access) > maxBootstrapTenants {
		return Bootstrap{}, ErrBootstrapTooLarge
	}
	if len(result.Access) > 0 {
		tenantIDs := make([]string, 0, len(result.Access))
		for _, snapshot := range result.Access {
			tenantIDs = append(tenantIDs, snapshot.Tenant.ID)
		}
		keyRows, err := transaction.QueryContext(ctx, `SELECT tenant_id,id,key_prefix,secret_digest,digest_version,status,revision,
			policy_revision,policy,expires_at,revoked_at,last_used_at FROM api_keys
			WHERE tenant_id=ANY($1) ORDER BY tenant_id,id LIMIT $2`, tenantIDs, maxBootstrapKeys+1)
		if err != nil {
			return Bootstrap{}, err
		}
		keyCount := 0
		for keyRows.Next() {
			var tenantID string
			var key access.KeySnapshot
			var policy []byte
			if err := keyRows.Scan(&tenantID, &key.ID, &key.Prefix, &key.SecretDigest, &key.DigestVersion, &key.Status,
				&key.Revision, &key.Policy.Revision, &policy, &key.ExpiresAt, &key.RevokedAt, &key.LastUsedAt); err != nil {
				_ = keyRows.Close()
				return Bootstrap{}, err
			}
			if err := json.Unmarshal(policy, &key.Policy); err != nil {
				_ = keyRows.Close()
				return Bootstrap{}, err
			}
			index, ok := tenantIndex[tenantID]
			if !ok {
				_ = keyRows.Close()
				return Bootstrap{}, errors.New("Control Event bootstrap key has no regional Tenant")
			}
			result.Access[index].Keys = append(result.Access[index].Keys, key)
			keyCount++
		}
		if err := keyRows.Close(); err != nil {
			return Bootstrap{}, err
		}
		if keyCount > maxBootstrapKeys {
			return Bootstrap{}, ErrBootstrapTooLarge
		}
	}
	connectionRows, err := transaction.QueryContext(ctx, `SELECT c.id,c.provider,c.base_url,c.region,c.credential_scope,c.capability_declaration,
		c.administrative_status,c.credential_version,c.revision,h.observed_status,COALESCE(h.observed_at,c.updated_at)
		FROM provider_connections c LEFT JOIN provider_connection_health h ON h.connection_id=c.id
		WHERE c.region=$1 ORDER BY c.id LIMIT $2`, audience.Region, maxBootstrapConnections+1)
	if err != nil {
		return Bootstrap{}, err
	}
	for connectionRows.Next() {
		var snapshot providerconnection.ExecutionSnapshot
		var capability []byte
		var observedStatus sql.NullString
		if err := connectionRows.Scan(&snapshot.ConnectionID, &snapshot.Provider, &snapshot.BaseURL, &snapshot.Region,
			&snapshot.CredentialScope, &capability, &snapshot.AdministrativeStatus, &snapshot.CredentialVersion,
			&snapshot.Revision, &observedStatus, &snapshot.ObservedAt); err != nil {
			_ = connectionRows.Close()
			return Bootstrap{}, err
		}
		if err := json.Unmarshal(capability, &snapshot.Capability); err != nil {
			_ = connectionRows.Close()
			return Bootstrap{}, err
		}
		if observedStatus.Valid {
			healthy := observedStatus.String == "healthy"
			snapshot.ObservedHealthy = &healthy
		}
		result.ProviderConnections = append(result.ProviderConnections, snapshot)
	}
	if err := connectionRows.Close(); err != nil {
		return Bootstrap{}, err
	}
	if len(result.ProviderConnections) > maxBootstrapConnections {
		return Bootstrap{}, ErrBootstrapTooLarge
	}
	var routing RoutingBootstrap
	var document, validation []byte
	err = transaction.QueryRowContext(ctx, `SELECT r.revision,r.document,r.validation_report,r.validation_hash,r.created_at
		FROM routing_catalog_head h JOIN routing_catalog_revisions r ON r.revision=h.revision WHERE h.singleton=true`).Scan(
		&routing.Revision, &document, &validation, &routing.ValidationHash, &routing.CreatedAt)
	if err == nil {
		if err := json.Unmarshal(document, &routing.Document); err != nil {
			return Bootstrap{}, err
		}
		if err := json.Unmarshal(validation, &routing.Validation); err != nil {
			return Bootstrap{}, err
		}
		result.RoutingCatalog = &routing
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Bootstrap{}, err
	}
	if err := transaction.Commit(); err != nil {
		return Bootstrap{}, err
	}
	return result, nil
}

type BootstrapHandler struct {
	publisher BootstrapPublisher
	verifier  operations.GatewayVerifier
}

func NewBootstrapHandler(publisher BootstrapPublisher, verifier operations.GatewayVerifier) (*BootstrapHandler, error) {
	if publisher == nil || verifier == nil {
		return nil, errors.New("Control Event bootstrap requires publisher and Gateway verifier")
	}
	return &BootstrapHandler{publisher: publisher, verifier: verifier}, nil
}

func (handler *BootstrapHandler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet || request.URL.Path != BootstrapPath {
		http.NotFound(response, request)
		return
	}
	response.Header().Set("Cache-Control", "no-store")
	identity, err := handler.verifier.Verify(request.Context(), request.Header.Get("Authorization"), request.Method, request.URL.RequestURI(), nil)
	if err != nil {
		http.Error(response, "unauthorized", http.StatusUnauthorized)
		return
	}
	bootstrap, err := handler.publisher.PublishBootstrap(request.Context(), controlevent.Audience{GatewayID: identity.GatewayID, Region: identity.Region})
	if errors.Is(err, ErrBootstrapTooLarge) {
		http.Error(response, "control bootstrap exceeds bounded limits", http.StatusRequestEntityTooLarge)
		return
	}
	if err != nil {
		http.Error(response, "control bootstrap unavailable", http.StatusServiceUnavailable)
		return
	}
	payload, err := json.Marshal(bootstrap)
	if err != nil || len(payload) > maxBootstrapResponseBytes {
		http.Error(response, "control bootstrap exceeds bounded limits", http.StatusRequestEntityTooLarge)
		return
	}
	response.Header().Set("Content-Type", "application/json")
	_, _ = response.Write(payload)
}

func (client *Client) FetchBootstrap(ctx context.Context) (Bootstrap, error) {
	target := *client.endpoint
	target.Path = strings.TrimRight(target.Path, "/") + BootstrapPath
	target.RawQuery = ""
	authorization, err := operations.GatewayAuthorization(client.key, client.gatewayID, client.now().UTC(), http.MethodGet, target.RequestURI(), nil)
	if err != nil {
		return Bootstrap{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return Bootstrap{}, err
	}
	request.Header.Set("Authorization", authorization)
	request.Header.Set("Accept", "application/json")
	response, err := client.client.Do(request)
	if err != nil {
		return Bootstrap{}, fmt.Errorf("fetch Control Event bootstrap: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return Bootstrap{}, fmt.Errorf("Control Event bootstrap status %d", response.StatusCode)
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxBootstrapResponseBytes+1))
	if err != nil || len(payload) > maxBootstrapResponseBytes {
		return Bootstrap{}, errors.New("invalid Control Event bootstrap response")
	}
	var bootstrap Bootstrap
	if err := json.Unmarshal(payload, &bootstrap); err != nil || bootstrap.SchemaVersion != bootstrapSchemaVersion || bootstrap.SourceCursor < 0 || bootstrap.CreatedAt.IsZero() {
		return Bootstrap{}, errors.New("invalid Control Event bootstrap response")
	}
	return bootstrap, nil
}
