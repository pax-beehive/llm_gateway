package controlrelay

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/toddzheng/llm-gateway/internal/controlevent"
	"github.com/toddzheng/llm-gateway/internal/operations"
	"github.com/toddzheng/llm-gateway/internal/secretcustody"
)

const SecretPathPrefix = "/internal/v1/provider-connection-secrets/"

var (
	ErrExecutionSecretNotFound = controlevent.ErrExecutionSecretNotFound
	resourceIDPattern          = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~-]{0,127}$`)
)

type SecretPublisher interface {
	PublishExecutionSecret(context.Context, controlevent.Audience, string, int64, int64) ([]byte, error)
}

type SecretHandler struct {
	publisher SecretPublisher
	verifier  operations.GatewayVerifier
}

func NewSecretHandler(publisher SecretPublisher, verifier operations.GatewayVerifier) (*SecretHandler, error) {
	if publisher == nil || verifier == nil {
		return nil, errors.New("execution secret relay requires publisher and Gateway verifier")
	}
	return &SecretHandler{publisher: publisher, verifier: verifier}, nil
}

func (handler *SecretHandler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet || !strings.HasPrefix(request.URL.Path, SecretPathPrefix) {
		http.NotFound(response, request)
		return
	}
	connectionID := strings.TrimPrefix(request.URL.Path, SecretPathPrefix)
	if !resourceIDPattern.MatchString(connectionID) {
		http.Error(response, "invalid Provider Connection", http.StatusBadRequest)
		return
	}
	revision, revisionErr := parseBoundedInt(request.URL.Query().Get("revision"), 0, 1, 1<<62)
	credentialVersion, versionErr := parseBoundedInt(request.URL.Query().Get("credential_version"), 0, 1, 1<<62)
	if revisionErr != nil || versionErr != nil {
		http.Error(response, "invalid immutable version", http.StatusBadRequest)
		return
	}
	identity, err := handler.verifier.Verify(request.Context(), request.Header.Get("Authorization"), request.Method, request.URL.RequestURI(), nil)
	if err != nil {
		http.Error(response, "unauthorized", http.StatusUnauthorized)
		return
	}
	material, err := handler.publisher.PublishExecutionSecret(request.Context(), controlevent.Audience{
		GatewayID: identity.GatewayID, Region: identity.Region,
	}, connectionID, revision, credentialVersion)
	if errors.Is(err, ErrExecutionSecretNotFound) {
		http.Error(response, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(response, "execution secret relay unavailable", http.StatusServiceUnavailable)
		return
	}
	defer clear(material)
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(struct {
		ConnectionID      string `json:"connection_id"`
		Revision          int64  `json:"revision"`
		CredentialVersion int64  `json:"credential_version"`
		Material          []byte `json:"material"`
	}{ConnectionID: connectionID, Revision: revision, CredentialVersion: credentialVersion, Material: material})
}

type PostgresSecretPublisher struct {
	database *sql.DB
	custody  secretcustody.Store
}

func NewPostgresSecretPublisher(database *sql.DB, custody secretcustody.Store) (*PostgresSecretPublisher, error) {
	if database == nil || custody == nil {
		return nil, errors.New("PostgreSQL execution secret publisher requires database and Secret Custody")
	}
	return &PostgresSecretPublisher{database: database, custody: custody}, nil
}

func (publisher *PostgresSecretPublisher) PublishExecutionSecret(ctx context.Context, audience controlevent.Audience, connectionID string, revision, credentialVersion int64) ([]byte, error) {
	if strings.TrimSpace(audience.GatewayID) == "" || strings.TrimSpace(audience.Region) == "" ||
		!resourceIDPattern.MatchString(connectionID) || revision <= 0 || credentialVersion <= 0 {
		return nil, ErrExecutionSecretNotFound
	}
	var reference secretcustody.Reference
	err := publisher.database.QueryRowContext(ctx, `SELECT secret_ref,secret_external_version FROM provider_connections
		WHERE id=$1 AND region=$2 AND administrative_status='enabled' AND revision=$3 AND credential_version=$4`,
		connectionID, audience.Region, revision, credentialVersion).Scan(&reference.Name, &reference.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrExecutionSecretNotFound
	}
	if err != nil {
		return nil, err
	}
	material, err := publisher.custody.Access(ctx, reference)
	return material, err
}

func (client *Client) FetchExecutionSecret(ctx context.Context, connectionID string, revision, credentialVersion int64) ([]byte, error) {
	if !resourceIDPattern.MatchString(connectionID) || revision <= 0 || credentialVersion <= 0 {
		return nil, ErrExecutionSecretNotFound
	}
	target := *client.endpoint
	target.Path = strings.TrimRight(target.Path, "/") + SecretPathPrefix + url.PathEscape(connectionID)
	query := target.Query()
	query.Set("revision", strconv.FormatInt(revision, 10))
	query.Set("credential_version", strconv.FormatInt(credentialVersion, 10))
	target.RawQuery = query.Encode()
	authorization, err := operations.GatewayAuthorization(client.key, client.gatewayID, client.now().UTC(), http.MethodGet, target.RequestURI(), nil)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", authorization)
	request.Header.Set("Accept", "application/json")
	response, err := client.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: fetch Provider Connection execution secret", controlevent.ErrExecutionSecretUnavailable)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, ErrExecutionSecretNotFound
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: Provider Connection execution secret relay status %d", controlevent.ErrExecutionSecretUnavailable, response.StatusCode)
	}
	var payload struct {
		ConnectionID      string `json:"connection_id"`
		Revision          int64  `json:"revision"`
		CredentialVersion int64  `json:"credential_version"`
		Material          []byte `json:"material"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 128<<10))
	if err := decoder.Decode(&payload); err != nil || payload.ConnectionID != connectionID || payload.Revision != revision ||
		payload.CredentialVersion != credentialVersion || len(payload.Material) == 0 || len(payload.Material) > 64<<10 {
		clear(payload.Material)
		return nil, fmt.Errorf("%w: invalid Provider Connection execution secret response", controlevent.ErrExecutionSecretUnavailable)
	}
	return payload.Material, nil
}
