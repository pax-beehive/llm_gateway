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

func (DeterministicOperator) Probe(_ context.Context, connection ProviderConnection, secret []byte) (ProbeResult, error) {
	if len(secret) == 0 {
		return ProbeResult{}, &OperationError{Code: "authentication_failed"}
	}
	digest := sha256.Sum256([]byte(connection.Provider + "\x1f" + connection.BaseURL + "\x1fprobe"))
	return ProbeResult{ObservedModelCount: 2, RawResponseHash: hex.EncodeToString(digest[:])}, nil
}

func (DeterministicOperator) Discover(_ context.Context, connection ProviderConnection, secret []byte) (DiscoveryResult, error) {
	if len(secret) == 0 {
		return DiscoveryResult{}, &OperationError{Code: "authentication_failed"}
	}
	digest := sha256.Sum256([]byte(connection.Provider + "\x1f" + connection.BaseURL + "\x1fdiscovery"))
	return DiscoveryResult{
		Models: []ObservedModel{{ID: connection.Provider + "-deterministic-small", OwnedBy: connection.Provider},
			{ID: connection.Provider + "-deterministic-large", OwnedBy: connection.Provider}},
		RawResponseHash: hex.EncodeToString(digest[:]),
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

func (operator *ModelDiscoveryOperator) Probe(ctx context.Context, connection ProviderConnection, secret []byte) (ProbeResult, error) {
	observation, err := operator.observe(ctx, connection, secret)
	if err != nil {
		return ProbeResult{}, err
	}
	return ProbeResult{ObservedModelCount: len(observation.Models), RawResponseHash: observation.RawResponseHash}, nil
}

func (operator *ModelDiscoveryOperator) Discover(ctx context.Context, connection ProviderConnection, secret []byte) (DiscoveryResult, error) {
	observation, err := operator.observe(ctx, connection, secret)
	if err != nil {
		return DiscoveryResult{}, err
	}
	models := make([]ObservedModel, 0, len(observation.Models))
	for _, model := range observation.Models {
		models = append(models, ObservedModel{ID: model.ID, OwnedBy: model.OwnedBy})
	}
	return DiscoveryResult{Models: models, RawResponseHash: observation.RawResponseHash}, nil
}

func (operator *ModelDiscoveryOperator) observe(ctx context.Context, connection ProviderConnection, secret []byte) (modeldiscovery.Observation, error) {
	client, err := modeldiscovery.New(modeldiscovery.Config{
		Provider: modeldiscovery.Provider(connection.Provider), BaseURL: connection.BaseURL,
		APIKey: string(secret), HTTPClient: operator.httpClient,
	})
	if err != nil {
		return modeldiscovery.Observation{}, &OperationError{Code: "invalid_connection"}
	}
	observation, err := client.ListObserved(ctx)
	if err == nil {
		return observation, nil
	}
	var requestError *modeldiscovery.RequestError
	if errors.As(err, &requestError) {
		return modeldiscovery.Observation{}, &OperationError{Code: requestError.Code}
	}
	return modeldiscovery.Observation{}, &OperationError{Code: "provider_operation_failed"}
}
