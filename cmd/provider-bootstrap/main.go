package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/toddzheng/llm-gateway/internal/dbtransport"
	"github.com/toddzheng/llm-gateway/internal/provider"
	"github.com/toddzheng/llm-gateway/internal/providerconnection"
	"github.com/toddzheng/llm-gateway/internal/secretcustody"
	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

type providerSpec struct {
	EnvironmentKey string
	ID             string
	Provider       string
	DisplayName    string
	BaseURL        string
	Credential     []byte
}

var supportedProviders = []providerSpec{
	{EnvironmentKey: "OPENAI_API_KEY", ID: "pc-openai-us-west", Provider: "openai", DisplayName: "OpenAI production", BaseURL: "https://api.openai.com/v1"},
	{EnvironmentKey: "DEEPSEEK_API_KEY", ID: "pc-deepseek-us-west", Provider: "deepseek", DisplayName: "DeepSeek production", BaseURL: "https://api.deepseek.com"},
	{EnvironmentKey: "ANTHROPIC_API_KEY", ID: "pc-anthropic-us-west", Provider: "anthropic", DisplayName: "Anthropic production", BaseURL: "https://api.anthropic.com/v1"},
	{EnvironmentKey: "GEMINI_API_KEY", ID: "pc-gemini-us-west", Provider: "gemini", DisplayName: "Gemini production", BaseURL: "https://generativelanguage.googleapis.com/v1beta"},
}

func main() {
	if err := run(context.Background()); err != nil {
		slog.Error("Provider bootstrap failed", "error", err)
		os.Exit(1)
	}
}

func run(parent context.Context) error {
	if os.Getenv("PROVIDER_BOOTSTRAP_CONFIRM") != "register" {
		return errors.New("PROVIDER_BOOTSTRAP_CONFIRM=register is required")
	}
	authorizationID := strings.TrimSpace(os.Getenv("PROVIDER_BOOTSTRAP_AUTHORIZATION_ID"))
	if authorizationID == "" || len(authorizationID) > 128 {
		return errors.New("PROVIDER_BOOTSTRAP_AUTHORIZATION_ID is required and must be at most 128 characters")
	}
	specs, canary, err := parseProviderInput(os.Getenv("PROVIDER_BOOTSTRAP_INPUT_JSON"), os.Getenv("PROVIDER_BOOTSTRAP_CANARY"))
	_ = os.Unsetenv("PROVIDER_BOOTSTRAP_INPUT_JSON")
	if err != nil {
		return err
	}
	defer func() {
		for index := range specs {
			clear(specs[index].Credential)
		}
	}()

	databaseURL := strings.TrimSpace(os.Getenv("CONTROL_PLANE_DATABASE_URL"))
	cloudSQLInstance := strings.TrimSpace(os.Getenv("CONTROL_PLANE_CLOUD_SQL_INSTANCE"))
	if databaseURL == "" || cloudSQLInstance == "" {
		return errors.New("CONTROL_PLANE_DATABASE_URL and CONTROL_PLANE_CLOUD_SQL_INSTANCE are required")
	}
	if err := dbtransport.RequireAuthenticatedTransport(databaseURL, cloudSQLInstance); err != nil {
		return fmt.Errorf("Provider bootstrap database transport: %w", err)
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	database, cleanupTransport, err := dbtransport.Open(ctx, databaseURL, cloudSQLInstance)
	if err != nil {
		return err
	}
	defer cleanupTransport()
	defer database.Close()
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	if err := database.PingContext(ctx); err != nil {
		return fmt.Errorf("connect PostgreSQL: %w", err)
	}
	if err := requireDatabaseRole(ctx, database, "llm_gateway_control"); err != nil {
		return err
	}

	projectID := strings.TrimSpace(os.Getenv("CONTROL_GCP_SECRET_PROJECT"))
	if projectID == "" {
		return errors.New("CONTROL_GCP_SECRET_PROJECT is required")
	}
	custody, err := secretcustody.NewGCP(secretcustody.GCPConfig{
		ProjectID: projectID, TokenProvider: secretcustody.NewMetadataTokenProvider(),
	})
	if err != nil {
		return err
	}
	connections, err := providerconnection.NewService(
		database,
		custody,
		providerconnection.NewModelDiscoveryOperator(nil),
		time.Now,
		nil,
		providerconnection.StaticLiveOperationPolicy{
			Source: authorizationID, ProbeMaxRequests: 1, DiscoveryMaxRequests: 1,
		},
	)
	if err != nil {
		return err
	}
	actor := tenantadmin.ActorEnvelope{
		Type: "system", ID: "provider-bootstrap",
		Scopes:    []string{tenantadmin.ScopePlatformRead, tenantadmin.ScopePlatformWrite},
		RequestID: authorizationID,
		Reason:    "initial GCP Provider credential bootstrap",
	}

	registered := make(map[string]providerconnection.ProviderConnection, len(specs))
	for _, spec := range specs {
		result, err := connections.Register(ctx, actor, "provider-bootstrap-register-v1-"+spec.Provider, providerconnection.RegisterCommand{
			ID: spec.ID, Provider: spec.Provider, DisplayName: spec.DisplayName, BaseURL: spec.BaseURL,
			Region: "us-west", CredentialScope: "production-primary", Secret: spec.Credential,
			CapabilityDeclaration: provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{
				"text": provider.CapabilityNative, "streaming": provider.CapabilityNative,
			}},
		})
		if err != nil {
			return fmt.Errorf("register %s Provider Connection: %w", spec.Provider, err)
		}
		current, err := connections.Get(ctx, actor, result.Connection.ID)
		if err != nil {
			return fmt.Errorf("read %s Provider Connection: %w", spec.Provider, err)
		}
		registered[spec.Provider] = current
	}

	canaryConnection := registered[canary]
	probe, err := connections.RequestProbe(ctx, actor, "provider-bootstrap-probe-v1-"+canary, providerconnection.OperationCommand{
		ConnectionID: canaryConnection.ID, ExpectedRevision: canaryConnection.Revision,
	})
	if err != nil {
		return fmt.Errorf("request %s Provider probe: %w", canary, err)
	}
	for probe.Operation.Status == providerconnection.OperationQueued || probe.Operation.Status == providerconnection.OperationRunning {
		worked, runErr := connections.RunNext(ctx)
		if runErr != nil {
			return fmt.Errorf("run %s Provider probe: %w", canary, runErr)
		}
		if !worked {
			break
		}
		operation, getErr := connections.GetOperation(ctx, actor, probe.Operation.ID)
		if getErr != nil {
			return fmt.Errorf("read %s Provider probe: %w", canary, getErr)
		}
		probe.Operation = operation
	}
	if probe.Operation.Status != providerconnection.OperationSucceeded {
		return fmt.Errorf("%s Provider probe ended with status %s and code %s", canary, probe.Operation.Status, probe.Operation.ErrorCode)
	}
	if canaryConnection.AdministrativeStatus == providerconnection.StatusDisabled {
		result, err := connections.Enable(ctx, actor, "provider-bootstrap-enable-v1-"+canary, providerconnection.StatusCommand{
			ConnectionID: canaryConnection.ID, ExpectedRevision: canaryConnection.Revision,
		})
		if err != nil {
			return fmt.Errorf("enable %s Provider Connection: %w", canary, err)
		}
		canaryConnection = result.Connection
	}

	slog.Info("Provider bootstrap completed",
		"connection_count", len(registered),
		"canary", canary,
		"canary_revision", canaryConnection.Revision,
		"probe_status", probe.Operation.Status,
		"observed_model_count", probe.Operation.Result["observed_model_count"],
	)
	return nil
}

func parseProviderInput(encoded, canaryValue string) ([]providerSpec, string, error) {
	if strings.TrimSpace(encoded) == "" {
		return nil, "", errors.New("PROVIDER_BOOTSTRAP_INPUT_JSON is required")
	}
	var input map[string]string
	if err := json.Unmarshal([]byte(encoded), &input); err != nil {
		return nil, "", errors.New("PROVIDER_BOOTSTRAP_INPUT_JSON must be a JSON object of supported Provider keys")
	}
	if len(input) != len(supportedProviders) {
		return nil, "", errors.New("Provider bootstrap requires exactly the four supported Provider keys")
	}
	specs := make([]providerSpec, 0, len(supportedProviders))
	for _, supported := range supportedProviders {
		credential, exists := input[supported.EnvironmentKey]
		if !exists || strings.TrimSpace(credential) == "" || len(credential) > 8192 {
			return nil, "", fmt.Errorf("%s is missing or invalid", supported.EnvironmentKey)
		}
		supported.Credential = []byte(credential)
		specs = append(specs, supported)
		delete(input, supported.EnvironmentKey)
	}
	if len(input) != 0 {
		return nil, "", errors.New("Provider bootstrap input contains unsupported keys")
	}
	canary := strings.TrimSpace(canaryValue)
	if canary == "" {
		canary = "openai"
	}
	for _, spec := range specs {
		if spec.Provider == canary {
			return specs, canary, nil
		}
	}
	return nil, "", errors.New("PROVIDER_BOOTSTRAP_CANARY must name a supported Provider")
}

func requireDatabaseRole(ctx context.Context, database *sql.DB, expected string) error {
	var current string
	if err := database.QueryRowContext(ctx, `SELECT current_user`).Scan(&current); err != nil {
		return fmt.Errorf("inspect Provider bootstrap database role: %w", err)
	}
	if current != expected {
		return fmt.Errorf("Provider bootstrap database role mismatch: connected as %q", current)
	}
	return nil
}
