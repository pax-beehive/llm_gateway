package providerconnection

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"github.com/toddzheng/llm-gateway/internal/provider/modeldiscovery"
)

type DeterministicOperator struct{}

func NewDeterministicOperator() DeterministicOperator { return DeterministicOperator{} }

func (DeterministicOperator) Probe(_ context.Context, connection ProviderConnection, secret []byte, maxRequests int) (ProbeResult, error) {
	if len(secret) == 0 || maxRequests < 1 {
		return ProbeResult{}, &OperationError{Code: "authentication_failed"}
	}
	digest := sha256.Sum256([]byte(connection.Provider + "\x1f" + connection.BaseURL + "\x1fprobe"))
	return ProbeResult{ObservedModelCount: 2, RawResponseHash: hex.EncodeToString(digest[:]), ProviderRequests: 1}, nil
}

func (DeterministicOperator) Discover(_ context.Context, connection ProviderConnection, secret []byte, maxRequests int) (DiscoveryResult, error) {
	if len(secret) == 0 || maxRequests < 1 {
		return DiscoveryResult{}, &OperationError{Code: "authentication_failed"}
	}
	digest := sha256.Sum256([]byte(connection.Provider + "\x1f" + connection.BaseURL + "\x1fdiscovery"))
	return DiscoveryResult{
		Models: []ObservedModel{{ID: connection.Provider + "-deterministic-small", OwnedBy: connection.Provider},
			{ID: connection.Provider + "-deterministic-large", OwnedBy: connection.Provider}},
		RawResponseHash:  hex.EncodeToString(digest[:]),
		ProviderRequests: 1,
	}, nil
}

type ModelDiscoveryOperator struct {
	httpClient *http.Client
}

func NewModelDiscoveryOperator(httpClient *http.Client) *ModelDiscoveryOperator {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 8 * time.Second}
	} else {
		copy := *httpClient
		if copy.Timeout <= 0 || copy.Timeout > 8*time.Second {
			copy.Timeout = 8 * time.Second
		}
		httpClient = &copy
	}
	return &ModelDiscoveryOperator{httpClient: httpClient}
}

func (operator *ModelDiscoveryOperator) Probe(ctx context.Context, connection ProviderConnection, secret []byte, maxRequests int) (ProbeResult, error) {
	client, err := operator.client(connection, secret, maxRequests)
	if err != nil {
		return ProbeResult{}, err
	}
	observation, err := client.ProbeObserved(ctx)
	if err != nil {
		return ProbeResult{}, classifyDiscoveryError(err)
	}
	return ProbeResult{ObservedModelCount: len(observation.Models), RawResponseHash: observation.RawResponseHash, ProviderRequests: observation.RequestCount}, nil
}

func (operator *ModelDiscoveryOperator) Discover(ctx context.Context, connection ProviderConnection, secret []byte, maxRequests int) (DiscoveryResult, error) {
	observation, err := operator.observe(ctx, connection, secret, maxRequests)
	if err != nil {
		return DiscoveryResult{}, err
	}
	models := make([]ObservedModel, 0, len(observation.Models))
	for _, model := range observation.Models {
		models = append(models, ObservedModel{ID: model.ID, OwnedBy: model.OwnedBy})
	}
	return DiscoveryResult{Models: models, RawResponseHash: observation.RawResponseHash, ProviderRequests: observation.RequestCount}, nil
}

func (operator *ModelDiscoveryOperator) observe(ctx context.Context, connection ProviderConnection, secret []byte, maxRequests int) (modeldiscovery.Observation, error) {
	client, err := operator.client(connection, secret, maxRequests)
	if err != nil {
		return modeldiscovery.Observation{}, err
	}
	observation, err := client.ListObserved(ctx)
	if err == nil {
		return observation, nil
	}
	return modeldiscovery.Observation{}, classifyDiscoveryError(err)
}

func (operator *ModelDiscoveryOperator) client(connection ProviderConnection, secret []byte, maxRequests int) (*modeldiscovery.Client, error) {
	if err := validateBaseURL(connection.Provider, connection.BaseURL); err != nil {
		return nil, &OperationError{Code: "endpoint_not_authorized"}
	}
	client, err := modeldiscovery.New(modeldiscovery.Config{
		Provider: modeldiscovery.Provider(connection.Provider), BaseURL: connection.BaseURL,
		APIKey: string(secret), HTTPClient: operator.httpClient, MaxRequests: maxRequests,
	})
	if err != nil {
		return nil, &OperationError{Code: "invalid_connection"}
	}
	return client, nil
}

func classifyDiscoveryError(err error) error {
	var requestError *modeldiscovery.RequestError
	if errors.As(err, &requestError) {
		return &OperationError{Code: requestError.Code}
	}
	return &OperationError{Code: "provider_operation_failed"}
}
