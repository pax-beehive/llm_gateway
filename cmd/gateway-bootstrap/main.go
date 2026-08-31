package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/credentialadmin"
	"github.com/toddzheng/llm-gateway/internal/dbtransport"
	"github.com/toddzheng/llm-gateway/internal/provider"
	"github.com/toddzheng/llm-gateway/internal/routingcatalog"
	"github.com/toddzheng/llm-gateway/internal/secretcustody"
	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

const (
	canaryTenantID        = "tenant-gateway-canary"
	canaryProviderModel   = "gpt-5.6-luna"
	canaryConnectionID    = "pc-openai-us-west"
	canaryRouteID         = "route-openai-gpt-5-6-luna-us-west"
	canaryDraftID         = "rcd-gateway-canary-v1"
	canaryPriceSnapshotID = "openai-gpt-5-6-luna-2026-08-30-standard"
	canaryRegion          = "us-west"
)

var secretResourcePart = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,254}$`)

func main() {
	if err := run(context.Background()); err != nil {
		slog.Error("Gateway bootstrap failed", "error", err)
		os.Exit(1)
	}
}

func run(parent context.Context) error {
	if os.Getenv("GATEWAY_BOOTSTRAP_CONFIRM") != "publish" {
		return errors.New("GATEWAY_BOOTSTRAP_CONFIRM=publish is required")
	}
	authorizationID := strings.TrimSpace(os.Getenv("GATEWAY_BOOTSTRAP_AUTHORIZATION_ID"))
	if authorizationID == "" || len(authorizationID) > 128 {
		return errors.New("GATEWAY_BOOTSTRAP_AUTHORIZATION_ID is required and must be at most 128 characters")
	}
	databaseURL := strings.TrimSpace(os.Getenv("CONTROL_PLANE_DATABASE_URL"))
	cloudSQLInstance := strings.TrimSpace(os.Getenv("CONTROL_PLANE_CLOUD_SQL_INSTANCE"))
	if databaseURL == "" || cloudSQLInstance == "" {
		return errors.New("CONTROL_PLANE_DATABASE_URL and CONTROL_PLANE_CLOUD_SQL_INSTANCE are required")
	}
	if err := dbtransport.RequireAuthenticatedTransport(databaseURL, cloudSQLInstance); err != nil {
		return fmt.Errorf("Gateway bootstrap database transport: %w", err)
	}
	pepperRing, err := pepperRingFromEnv()
	if err != nil {
		return err
	}
	defer func() {
		for version := range pepperRing.Peppers {
			clear(pepperRing.Peppers[version])
		}
	}()

	projectID := strings.TrimSpace(os.Getenv("CONTROL_GCP_SECRET_PROJECT"))
	keySecretID := strings.TrimSpace(os.Getenv("GATEWAY_BOOTSTRAP_API_KEY_SECRET"))
	keyStore, err := newGatewayKeyStore(projectID, keySecretID, "", nil, secretcustody.NewMetadataTokenProvider())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	if err := keyStore.RequireSecret(ctx); err != nil {
		return fmt.Errorf("preflight canary Gateway API key Secret: %w", err)
	}
	hasKey, err := keyStore.HasVersion(ctx)
	if err != nil {
		return fmt.Errorf("inspect canary Gateway API key Secret: %w", err)
	}
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

	actor := tenantadmin.ActorEnvelope{
		Type: "system", ID: "gateway-bootstrap",
		Scopes:    []string{tenantadmin.ScopePlatformRead, tenantadmin.ScopePlatformWrite},
		RequestID: authorizationID,
		Reason:    "initial GCP Gateway canary bootstrap",
	}
	tenantService, err := tenantadmin.NewService(database, time.Now)
	if err != nil {
		return err
	}
	credentialService, err := credentialadmin.NewService(database, pepperRing, time.Now, nil)
	if err != nil {
		return err
	}
	tenantPolicy, keyPolicy, document := canaryConfiguration()
	tenantResult, err := tenantService.CreateTenant(ctx, actor, "gateway-bootstrap-tenant-v1", tenantadmin.CreateTenantCommand{
		ID: canaryTenantID, Slug: "gateway-canary", DisplayName: "Gateway production canary", HomeRegion: canaryRegion,
		Metadata: map[string]any{"environment": "production", "purpose": "gateway-validation"}, InitialPolicy: tenantPolicy,
	})
	if err != nil {
		return fmt.Errorf("create canary Tenant: %w", err)
	}
	issued, err := credentialService.Issue(ctx, actor, "gateway-bootstrap-key-v1", credentialadmin.IssueCommand{
		TenantID: canaryTenantID, Name: "Gateway production canary",
		Metadata: map[string]any{"environment": "production", "purpose": "gateway-validation"}, Policy: keyPolicy,
	})
	if err != nil {
		return fmt.Errorf("issue canary Gateway API key: %w", err)
	}
	if issued.RawSecret != "" {
		material := []byte(issued.RawSecret)
		issued.RawSecret = ""
		defer clear(material)
		if hasKey {
			return errors.New("canary Gateway API key Secret already contains a version before first key delivery")
		}
		if err := keyStore.AddVersion(ctx, material); err != nil {
			return fmt.Errorf("deliver one-time canary Gateway API key: %w", err)
		}
		hasKey = true
	}
	if !hasKey {
		return errors.New("issued canary Gateway API key was replayed without a recoverable Secret version")
	}

	connectionLookup, err := routingcatalog.NewPostgresConnectionLookup(database)
	if err != nil {
		return err
	}
	catalog, err := routingcatalog.NewService(database, connectionLookup, time.Now, nil)
	if err != nil {
		return err
	}
	catalogRevision, publicationID, err := ensureCanaryCatalog(ctx, catalog, actor, document)
	if err != nil {
		return err
	}
	slog.Info("Gateway bootstrap completed",
		"tenant_id", tenantResult.Tenant.ID,
		"api_key_id", issued.Credential.ID,
		"public_model", canaryProviderModel,
		"catalog_revision", catalogRevision,
		"publication_id", publicationID,
		"required_region", canaryRegion,
	)
	return nil
}

func canaryConfiguration() (core.TenantPolicy, core.APIKeyPolicy, routingcatalog.Document) {
	stored, cache, inspection := false, false, false
	maxInput, maxOutput, maxCost := int64(4096), int64(256), int64(5_000)
	requestsPerMinute, tokensPerMinute := int64(10), int64(10_000)
	dailySpend, monthlySpend := int64(100_000), int64(1_000_000)
	tenantPolicy := core.TenantPolicy{
		Revision: 1, MaxConcurrentResponses: 2, MaxInputItems: 16,
		AllowStoredResponses: &stored, AllowCacheProtection: &cache, AllowContentInspection: &inspection,
		Limits: core.QuotaLimits{
			MaxInputTokens: &maxInput, MaxOutputTokens: &maxOutput, MaxCostMicros: &maxCost,
			RequestsPerMinute: &requestsPerMinute, TokensPerMinute: &tokensPerMinute,
			DailySpendMicros: &dailySpend, MonthlySpendMicros: &monthlySpend, Currency: "USD",
		},
	}
	models := []string{canaryProviderModel}
	operations := []string{"models", "capabilities", "responses", "chat_completions"}
	regions := []string{canaryRegion}
	maxConcurrent := 1
	keyPolicy := core.APIKeyPolicy{
		Revision: 1, AllowedPublicModels: &models, AllowedOperations: &operations, AllowedRegions: &regions,
		MaxConcurrentResponses: &maxConcurrent,
		Limits: core.QuotaLimits{
			MaxInputTokens: &maxInput, MaxOutputTokens: &maxOutput, MaxCostMicros: &maxCost,
			RequestsPerMinute: &requestsPerMinute, TokensPerMinute: &tokensPerMinute,
			DailySpendMicros: &dailySpend, MonthlySpendMicros: &monthlySpend, Currency: "USD",
		},
	}
	document := routingcatalog.Document{Routes: []routingcatalog.ManagedRoute{{
		ID: canaryRouteID, PublicModel: canaryProviderModel, ProviderConnectionID: canaryConnectionID,
		ProviderModel: canaryProviderModel, ExecutionRegion: canaryRegion, HomeRegion: canaryRegion,
		CapabilityProfileRevision: 1,
		Capabilities: map[string]provider.CapabilitySupport{
			"text": provider.CapabilityNative, "streaming": provider.CapabilityNative,
		},
		ProviderCostSnapshot: core.PriceSnapshot{
			ID: canaryPriceSnapshotID, Provider: "openai", Model: canaryProviderModel, Region: canaryRegion, Currency: "USD",
			InputPerMillionMicros: 200_000, CachedInputPerMillionMicros: 200_000, OutputPerMillionMicros: 1_200_000,
			EffectiveAt: time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC).Unix(), Source: "https://platform.openai.com/docs/models",
		},
		AdministrativeStatus: routingcatalog.RouteActive,
		SelectionPolicy:      routingcatalog.SelectionPolicy{Priority: 10, Weight: 100, MaxConcurrency: 2},
		TenantVisibility: routingcatalog.TenantVisibilityPolicy{
			TenantIDs: []string{canaryTenantID}, LimitPolicyRevisions: map[string]int64{canaryTenantID: 1},
		},
	}}}
	return tenantPolicy, keyPolicy, document
}

func ensureCanaryCatalog(ctx context.Context, catalog *routingcatalog.Service, actor tenantadmin.ActorEnvelope, document routingcatalog.Document) (int64, string, error) {
	current, err := catalog.Current(ctx, actor)
	if err == nil {
		if len(current.Document.Routes) == 1 && current.Document.Routes[0].ID == canaryRouteID && current.Document.Routes[0].ProviderModel == canaryProviderModel {
			return current.Revision, "existing", nil
		}
		return 0, "", errors.New("an unrelated Routing Catalog revision already exists; bootstrap refuses to replace it")
	}
	if !errors.Is(err, routingcatalog.ErrNotFound) {
		return 0, "", fmt.Errorf("read current Routing Catalog: %w", err)
	}
	created, err := catalog.CreateDraft(ctx, actor, "gateway-bootstrap-catalog-draft-v1", routingcatalog.CreateDraftCommand{
		ID: canaryDraftID, BaseRevision: 0, Document: document,
	})
	if err != nil {
		return 0, "", fmt.Errorf("create canary Routing Catalog draft: %w", err)
	}
	validated, err := catalog.ValidateDraft(ctx, actor, routingcatalog.ValidateDraftCommand{
		DraftID: canaryDraftID, ExpectedRevision: created.Draft.Revision,
	})
	if err != nil {
		return 0, "", fmt.Errorf("validate canary Routing Catalog draft: %w", err)
	}
	if !validated.Draft.Validation.Valid {
		return 0, "", errors.New("canary Routing Catalog validation did not produce valid evidence")
	}
	published, err := catalog.PublishDraft(ctx, actor, "gateway-bootstrap-catalog-publish-v1", routingcatalog.PublishDraftCommand{
		DraftID: canaryDraftID, ExpectedRevision: validated.Draft.Revision, RequiredRegions: []string{canaryRegion},
	})
	if err != nil {
		return 0, "", fmt.Errorf("publish canary Routing Catalog: %w", err)
	}
	return published.Revision.Revision, published.Publication.ID, nil
}

func pepperRingFromEnv() (credentialadmin.PepperRing, error) {
	encoded := strings.TrimSpace(os.Getenv("CONTROL_API_KEY_PEPPERS_JSON"))
	currentValue := strings.TrimSpace(os.Getenv("CONTROL_API_KEY_CURRENT_DIGEST_VERSION"))
	current, err := strconv.ParseInt(currentValue, 10, 16)
	if err != nil || current <= 0 || encoded == "" {
		return credentialadmin.PepperRing{}, errors.New("CONTROL_API_KEY_PEPPERS_JSON and a positive CONTROL_API_KEY_CURRENT_DIGEST_VERSION are required")
	}
	var configured map[string]string
	if err := json.Unmarshal([]byte(encoded), &configured); err != nil {
		return credentialadmin.PepperRing{}, errors.New("CONTROL_API_KEY_PEPPERS_JSON must be a version-to-pepper object")
	}
	peppers := make(map[int16][]byte, len(configured))
	for versionValue, pepper := range configured {
		version, err := strconv.ParseInt(versionValue, 10, 16)
		if err != nil || version <= 0 || len(pepper) < 16 {
			return credentialadmin.PepperRing{}, errors.New("Gateway API key peppers require positive versions and at least 16 bytes")
		}
		peppers[int16(version)] = []byte(pepper)
	}
	return credentialadmin.PepperRing{CurrentVersion: int16(current), Peppers: peppers}, nil
}

func requireDatabaseRole(ctx context.Context, database *sql.DB, expected string) error {
	var current string
	if err := database.QueryRowContext(ctx, `SELECT current_user`).Scan(&current); err != nil {
		return fmt.Errorf("inspect Gateway bootstrap database role: %w", err)
	}
	if current != expected {
		return fmt.Errorf("Gateway bootstrap database role mismatch: connected as %q", current)
	}
	return nil
}

type gatewayKeyStore struct {
	projectID string
	secretID  string
	endpoint  *url.URL
	client    *http.Client
	tokens    secretcustody.TokenProvider
}

func newGatewayKeyStore(projectID, secretID, endpointValue string, client *http.Client, tokens secretcustody.TokenProvider) (*gatewayKeyStore, error) {
	if !secretResourcePart.MatchString(projectID) || !secretResourcePart.MatchString(secretID) || tokens == nil {
		return nil, errors.New("Gateway API key Secret requires bounded project, secret ID, and Workload Identity token provider")
	}
	if endpointValue == "" {
		endpointValue = "https://secretmanager.googleapis.com/v1"
	}
	endpoint, err := url.Parse(endpointValue)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil {
		return nil, errors.New("Gateway API key Secret endpoint must be absolute HTTPS")
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	} else {
		copy := *client
		if copy.Timeout <= 0 || copy.Timeout > 10*time.Second {
			copy.Timeout = 10 * time.Second
		}
		client = &copy
	}
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &gatewayKeyStore{projectID: projectID, secretID: secretID, endpoint: endpoint, client: client, tokens: tokens}, nil
}

func (store *gatewayKeyStore) HasVersion(ctx context.Context) (bool, error) {
	status, err := store.request(ctx, http.MethodGet, store.resource()+"/versions/latest", nil)
	if status == http.StatusNotFound {
		return false, nil
	}
	return status >= 200 && status < 300, err
}

func (store *gatewayKeyStore) RequireSecret(ctx context.Context) error {
	status, err := store.request(ctx, http.MethodGet, store.resource(), nil)
	if status == http.StatusNotFound {
		return errors.New("pre-created canary Gateway API key Secret is required")
	}
	return err
}

func (store *gatewayKeyStore) AddVersion(ctx context.Context, material []byte) error {
	if len(material) < 24 || len(material) > 8192 {
		return errors.New("bounded Gateway API key material is required")
	}
	body := map[string]any{"payload": map[string]string{"data": base64.StdEncoding.EncodeToString(material)}}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		status, err := store.request(ctx, http.MethodPost, store.resource()+":addVersion", body)
		if err == nil && status >= 200 && status < 300 {
			return nil
		}
		last = err
		if status > 0 && status < 500 {
			break
		}
	}
	return last
}

func (store *gatewayKeyStore) resource() string {
	return "projects/" + store.projectID + "/secrets/" + store.secretID
}

func (store *gatewayKeyStore) request(ctx context.Context, method, resource string, body any) (int, error) {
	endpoint := *store.endpoint
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/" + resource
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), reader)
	if err != nil {
		return 0, err
	}
	token, err := store.tokens.Token(ctx)
	if err != nil || token.AccessToken == "" {
		return 0, errors.New("obtain Gateway API key Secret Workload Identity token")
	}
	request.Header.Set("Authorization", "Bearer "+token.AccessToken)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := store.client.Do(request)
	if err != nil {
		return 0, errors.New("Gateway API key Secret request failed")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return response.StatusCode, fmt.Errorf("Gateway API key Secret status %d", response.StatusCode)
	}
	return response.StatusCode, nil
}
