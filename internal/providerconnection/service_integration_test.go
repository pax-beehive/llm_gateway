//go:build integration

package providerconnection_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/toddzheng/llm-gateway/internal/migrations"
	"github.com/toddzheng/llm-gateway/internal/provider"
	"github.com/toddzheng/llm-gateway/internal/providerconnection"
	"github.com/toddzheng/llm-gateway/internal/secretcustody"
	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

func TestRegisterProviderConnectionStoresOnlySecretReferenceAndEmitsEvidence(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	if err := migrations.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := providerconnection.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	custody := secretcustody.NewMemory()
	service, err := providerconnection.NewService(db, custody, nil, time.Now, nil)
	if err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("pc-openai-%d", time.Now().UnixNano())
	actor := tenantadmin.ActorEnvelope{
		Type: "human", ID: "platform-operator", Scopes: []string{tenantadmin.ScopePlatformWrite},
		RequestID: "request-register-provider", Reason: "configure upstream account",
	}
	result, err := service.Register(ctx, actor, "register-"+id, providerconnection.RegisterCommand{
		ID: id, Provider: "openai", DisplayName: "OpenAI production", BaseURL: "https://api.openai.com/v1",
		Region: "us-west1", CredentialScope: "organization-a", Secret: []byte("provider-secret-material"),
		CapabilityDeclaration: provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{"text": provider.CapabilityNative}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Replay || result.Connection.ID != id || result.Connection.Revision != 1 ||
		result.Connection.AdministrativeStatus != providerconnection.StatusDisabled || result.Connection.SecretRef != "" {
		t.Fatalf("registered connection = %#v", result)
	}
	replayed, err := service.Register(ctx, actor, "register-"+id, providerconnection.RegisterCommand{
		ID: id, Provider: "openai", DisplayName: "OpenAI production", BaseURL: "https://api.openai.com/v1",
		Region: "us-west1", CredentialScope: "organization-a", Secret: []byte("provider-secret-material"),
		CapabilityDeclaration: provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{"text": provider.CapabilityNative}},
	})
	if err != nil || !replayed.Replay || replayed.Connection.ID != id {
		t.Fatalf("replayed connection = %#v err=%v", replayed, err)
	}
	var secretRef string
	var auditCount, outboxCount int
	if err := db.QueryRowContext(ctx, `SELECT secret_ref FROM provider_connections WHERE id = $1`, id).Scan(&secretRef); err != nil {
		t.Fatal(err)
	}
	if secretRef == "" || secretRef == "provider-secret-material" {
		t.Fatalf("persisted secret reference = %q", secretRef)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM control_audit_events WHERE action = 'provider_connection.register' AND aggregate_id = $1`, id).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM control_outbox WHERE aggregate_type = 'ProviderConnection' AND aggregate_id = $1 AND event_type = 'ProviderConnectionRegistered'`, id).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 || outboxCount != 1 {
		t.Fatalf("audit/outbox counts = %d/%d", auditCount, outboxCount)
	}
	var eventSchema int
	var executionPayload []byte
	if err := db.QueryRowContext(ctx, `SELECT schema_version,payload FROM control_outbox
		WHERE aggregate_type='ProviderConnection' AND aggregate_id=$1 AND event_type='ProviderConnectionRegistered'`, id).Scan(&eventSchema, &executionPayload); err != nil {
		t.Fatal(err)
	}
	if eventSchema != 3 || !bytes.Contains(executionPayload, []byte(`"base_url"`)) || !bytes.Contains(executionPayload, []byte(`"capability_declaration"`)) ||
		bytes.Contains(executionPayload, []byte("secret_ref")) || bytes.Contains(executionPayload, []byte("provider-secret-material")) {
		t.Fatalf("execution projection event schema/payload = %d/%s", eventSchema, executionPayload)
	}
	for _, table := range []string{"provider_connections", "provider_connection_credential_versions", "control_audit_events", "control_outbox", "control_command_idempotency"} {
		var leaked bool
		if err := db.QueryRowContext(ctx, fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM %s WHERE row_to_json(%s)::text LIKE '%%provider-secret-material%%')`, table, table)).Scan(&leaked); err != nil {
			t.Fatal(err)
		}
		if leaked {
			t.Fatalf("secret leaked into %s", table)
		}
	}
}

func TestProviderConnectionQueriesUpdatesAndLifecycleUseStablePaginationAndCAS(t *testing.T) {
	db, ctx := providerConnectionDatabase(t)
	custody := secretcustody.NewMemory()
	service, err := providerconnection.NewService(db, custody, nil, time.Now, nil)
	if err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("pc-query-%d", time.Now().UnixNano())
	actor := tenantadmin.ActorEnvelope{
		Type: "human", ID: "platform-operator", Scopes: []string{tenantadmin.ScopePlatformRead, tenantadmin.ScopePlatformWrite},
		RequestID: "request-provider-query", Reason: "integration test",
	}
	for _, suffix := range []string{"a", "b", "c"} {
		if _, err := service.Register(ctx, actor, "register-"+prefix+suffix, providerconnection.RegisterCommand{
			ID: prefix + "-" + suffix, Provider: "anthropic", DisplayName: "Anthropic " + suffix,
			BaseURL: "https://api.anthropic.com/v1", Region: prefix, CredentialScope: "scope-" + suffix,
			Secret: []byte("provider-secret-" + suffix), CapabilityDeclaration: nativeTextProfile(1),
		}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := service.List(ctx, actor, providerconnection.ConnectionFilter{Provider: "anthropic", Region: prefix, Limit: 2})
	if err != nil || len(first.Data) != 2 || first.NextCursor == "" {
		t.Fatalf("first page = %#v err=%v", first, err)
	}
	second, err := service.List(ctx, actor, providerconnection.ConnectionFilter{Provider: "anthropic", Region: prefix, Limit: 2, Cursor: first.NextCursor})
	if err != nil || len(second.Data) != 1 || second.NextCursor != "" || second.Data[0].ID == first.Data[1].ID {
		t.Fatalf("second page = %#v err=%v", second, err)
	}
	observed, err := service.Get(ctx, actor, prefix+"-a")
	if err != nil || observed.SecretRef != "" || observed.CredentialVersion != 1 {
		t.Fatalf("observed connection = %#v err=%v", observed, err)
	}
	changedName := "Anthropic primary"
	changedScope := "organization-primary"
	updated, err := service.Update(ctx, actor, "update-"+prefix, providerconnection.UpdateCommand{
		ConnectionID: prefix + "-a", ExpectedRevision: observed.Revision,
		DisplayName: &changedName, CredentialScope: &changedScope,
	})
	if err != nil || updated.Connection.Revision != 2 || updated.Connection.DisplayName != changedName || updated.Connection.CredentialScope != changedScope {
		t.Fatalf("updated connection = %#v err=%v", updated, err)
	}
	if _, err := service.Update(ctx, actor, "stale-update-"+prefix, providerconnection.UpdateCommand{
		ConnectionID: prefix + "-a", ExpectedRevision: observed.Revision, DisplayName: &changedName,
	}); !errors.Is(err, providerconnection.ErrRevisionConflict) {
		t.Fatalf("stale update error = %v", err)
	}
	enabled, err := service.Enable(ctx, actor, "enable-"+prefix, providerconnection.StatusCommand{
		ConnectionID: prefix + "-a", ExpectedRevision: updated.Connection.Revision,
	})
	if err != nil || enabled.Connection.AdministrativeStatus != providerconnection.StatusEnabled || enabled.Connection.Revision != 3 {
		t.Fatalf("enabled connection = %#v err=%v", enabled, err)
	}
	disabled, err := service.Disable(ctx, actor, "disable-"+prefix, providerconnection.StatusCommand{
		ConnectionID: prefix + "-a", ExpectedRevision: enabled.Connection.Revision,
	})
	if err != nil || disabled.Connection.AdministrativeStatus != providerconnection.StatusDisabled || disabled.Connection.Revision != 4 {
		t.Fatalf("disabled connection = %#v err=%v", disabled, err)
	}
	tenantActor := actor
	tenantActor.Scopes = []string{tenantadmin.ScopeTenantRead, tenantadmin.ScopeTenantWrite}
	tenantActor.ActingTenantID = "tenant-a"
	if _, err := service.Get(ctx, tenantActor, prefix+"-a"); !errors.Is(err, providerconnection.ErrPolicyDenied) {
		t.Fatalf("Tenant-scoped Provider Connection read error = %v", err)
	}
}

func TestAsyncProviderOperationsAreSecretSafeAndDoNotPublishRoutes(t *testing.T) {
	db, ctx := providerConnectionDatabase(t)
	custody := secretcustody.NewMemory()
	operator := &fakeProviderOperator{
		probe: providerconnection.ProbeResult{ObservedModelCount: 2, RawResponseHash: strings.Repeat("a", 64), ProviderRequests: 1},
		discovery: providerconnection.DiscoveryResult{
			Models:           []providerconnection.ObservedModel{{ID: "gpt-test", OwnedBy: "provider"}, {ID: "gpt-test-mini", OwnedBy: "provider"}},
			RawResponseHash:  strings.Repeat("b", 64),
			ProviderRequests: 1,
		},
	}
	service, err := providerconnection.NewService(db, custody, operator, time.Now, nil, providerconnection.StaticLiveOperationPolicy{
		Source: "integration-test-authorization", ProbeMaxRequests: 1, DiscoveryMaxRequests: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("pc-operations-%d", time.Now().UnixNano())
	actor := tenantadmin.ActorEnvelope{
		Type: "human", ID: "platform-operator", Scopes: []string{tenantadmin.ScopePlatformRead, tenantadmin.ScopePlatformWrite},
		RequestID: "request-provider-operations", Reason: "verify upstream account",
	}
	registered, err := service.Register(ctx, actor, "register-"+id, providerconnection.RegisterCommand{
		ID: id, Provider: "openai", DisplayName: "OpenAI operations", BaseURL: "https://api.openai.com/v1",
		Region: "us-west1", CredentialScope: "operations", Secret: []byte("initial-provider-secret"),
		CapabilityDeclaration: nativeTextProfile(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	deniedService, err := providerconnection.NewService(db, custody, operator, time.Now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deniedService.RequestProbe(ctx, actor, "unauthorized-probe-"+id, providerconnection.OperationCommand{
		ConnectionID: id, ExpectedRevision: 1,
	}); !errors.Is(err, providerconnection.ErrPolicyDenied) {
		t.Fatalf("unauthorized live probe error = %v", err)
	}
	probe, err := service.RequestProbe(ctx, actor, "probe-"+id, providerconnection.OperationCommand{
		ConnectionID: id, ExpectedRevision: registered.Connection.Revision,
	})
	if err != nil || probe.Operation.Status != providerconnection.OperationQueued || probe.Operation.PendingSecretRef != "" {
		t.Fatalf("queued probe = %#v err=%v", probe, err)
	}
	if worked, err := service.RunNext(ctx); err != nil || !worked {
		t.Fatalf("run probe = %v/%v", worked, err)
	}
	completedProbe, err := service.GetOperation(ctx, actor, probe.Operation.ID)
	if err != nil || completedProbe.Status != providerconnection.OperationSucceeded || completedProbe.Result["observed_model_count"] != float64(2) {
		t.Fatalf("completed probe = %#v err=%v", completedProbe, err)
	}
	replayedProbe, err := service.RequestProbe(ctx, actor, "probe-"+id, providerconnection.OperationCommand{
		ConnectionID: id, ExpectedRevision: registered.Connection.Revision,
	})
	if err != nil || !replayedProbe.Replay || replayedProbe.Operation.Status != providerconnection.OperationSucceeded {
		t.Fatalf("replayed completed probe = %#v err=%v", replayedProbe, err)
	}
	unchanged, err := service.Get(ctx, actor, id)
	if err != nil || unchanged.AdministrativeStatus != providerconnection.StatusDisabled || unchanged.Revision != 1 {
		t.Fatalf("probe changed administrative state = %#v err=%v", unchanged, err)
	}
	discovery, err := service.RequestDiscovery(ctx, actor, "discover-"+id, providerconnection.OperationCommand{
		ConnectionID: id, ExpectedRevision: unchanged.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := service.RunNext(ctx); err != nil || !worked {
		t.Fatalf("run discovery = %v/%v", worked, err)
	}
	completedDiscovery, err := service.GetOperation(ctx, actor, discovery.Operation.ID)
	if err != nil || completedDiscovery.Status != providerconnection.OperationSucceeded || completedDiscovery.Result["model_count"] != float64(2) {
		t.Fatalf("completed discovery = %#v err=%v", completedDiscovery, err)
	}
	first, err := service.ListDiscoveredModels(ctx, actor, discovery.Operation.ID, "", 1)
	if err != nil || len(first.Data) != 1 || first.Data[0].ID != "gpt-test" || first.NextCursor == "" {
		t.Fatalf("first model page = %#v err=%v", first, err)
	}
	second, err := service.ListDiscoveredModels(ctx, actor, discovery.Operation.ID, first.NextCursor, 1)
	if err != nil || len(second.Data) != 1 || second.Data[0].ID != "gpt-test-mini" || second.NextCursor != "" {
		t.Fatalf("second model page = %#v err=%v", second, err)
	}
	if _, err := service.ListDiscoveredModels(ctx, tenantadmin.ActorEnvelope{Type: "human", ID: "denied"}, discovery.Operation.ID, "", 1); !errors.Is(err, providerconnection.ErrPolicyDenied) {
		t.Fatalf("unauthorized inventory read = %v", err)
	}
	if _, err := service.ListDiscoveredModels(ctx, actor, discovery.Operation.ID, "invalid!", 1); !errors.Is(err, providerconnection.ErrInvalidArgument) {
		t.Fatalf("invalid model cursor = %v", err)
	}
	if _, err := service.ListDiscoveredModels(ctx, actor, completedProbe.ID, first.NextCursor, 1); !errors.Is(err, providerconnection.ErrInvalidArgument) {
		t.Fatalf("non-discovery model read = %v", err)
	}
	var observations, routePublications int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM provider_model_observations WHERE operation_id=$1`, discovery.Operation.ID).Scan(&observations); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM control_outbox WHERE event_type='RoutingCatalogPublished' AND occurred_at >= $1`, discovery.Operation.CreatedAt).Scan(&routePublications); err != nil {
		t.Fatal(err)
	}
	if observations != 2 || routePublications != 0 {
		t.Fatalf("observations/route publications = %d/%d", observations, routePublications)
	}
	rotationSecret := []byte("rotated-provider-secret")
	rotation, err := service.RequestRotation(ctx, actor, "rotate-"+id, providerconnection.RotationCommand{
		ConnectionID: id, ExpectedRevision: unchanged.Revision, Secret: rotationSecret,
	})
	if err != nil || rotation.Operation.PendingSecretRef != "" {
		t.Fatalf("queued rotation = %#v err=%v", rotation, err)
	}
	var leaked bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM provider_operations WHERE row_to_json(provider_operations)::text LIKE '%rotated-provider-secret%')`).Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked {
		t.Fatal("rotation secret leaked into provider_operations")
	}
	if worked, err := service.RunNext(ctx); err != nil || !worked {
		t.Fatalf("run rotation = %v/%v", worked, err)
	}
	rotated, err := service.Get(ctx, actor, id)
	if err != nil || rotated.Revision != 2 || rotated.CredentialVersion != 2 || rotated.SecretRef != "" {
		t.Fatalf("rotated connection = %#v err=%v", rotated, err)
	}
	enabled, err := service.Enable(ctx, actor, "enable-after-rotation-"+id, providerconnection.StatusCommand{ConnectionID: id, ExpectedRevision: 2})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := providerconnection.NewGatewayResolver(db, custody)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolver.Resolve(ctx, enabled.Connection.ID)
	if err != nil || string(resolved.Secret) != string(rotationSecret) || resolved.Connection.SecretRef != "" {
		t.Fatalf("resolved execution credential = %#v err=%v", resolved, err)
	}
	operator.probeErr = fmt.Errorf("provider reflected secret %s", rotationSecret)
	failingProbe, err := service.RequestProbe(ctx, actor, "failing-probe-"+id, providerconnection.OperationCommand{
		ConnectionID: id, ExpectedRevision: enabled.Connection.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := service.RunNext(ctx); err != nil || !worked {
		t.Fatalf("run failed probe = %v/%v", worked, err)
	}
	failed, err := service.GetOperation(ctx, actor, failingProbe.Operation.ID)
	if err != nil || failed.Status != providerconnection.OperationFailed || strings.Contains(failed.ErrorMessage, string(rotationSecret)) || len(failed.ErrorMessage) > 256 {
		t.Fatalf("failed probe = %#v err=%v", failed, err)
	}
	stillEnabled, err := service.Get(ctx, actor, id)
	if err != nil || stillEnabled.AdministrativeStatus != providerconnection.StatusEnabled {
		t.Fatalf("failed probe changed administrative intent = %#v err=%v", stillEnabled, err)
	}
	firstOperations, err := service.ListOperations(ctx, actor, providerconnection.OperationFilter{ConnectionID: id, Limit: 2})
	if err != nil || len(firstOperations.Data) != 2 || firstOperations.NextCursor == "" {
		t.Fatalf("first operation page = %#v err=%v", firstOperations, err)
	}
	secondOperations, err := service.ListOperations(ctx, actor, providerconnection.OperationFilter{ConnectionID: id, Cursor: firstOperations.NextCursor, Limit: 2})
	if err != nil || len(secondOperations.Data) == 0 || secondOperations.Data[0].ID == firstOperations.Data[1].ID {
		t.Fatalf("second operation page = %#v err=%v", secondOperations, err)
	}
	failedOnly, err := service.ListOperations(ctx, actor, providerconnection.OperationFilter{ConnectionID: id, Status: providerconnection.OperationFailed, Limit: 10})
	if err != nil || len(failedOnly.Data) != 1 || failedOnly.Data[0].ID != failed.ID {
		t.Fatalf("failed operation page = %#v err=%v", failedOnly, err)
	}
}

type fakeProviderOperator struct {
	probe       providerconnection.ProbeResult
	probeErr    error
	discovery   providerconnection.DiscoveryResult
	discoverErr error
}

func (operator *fakeProviderOperator) Probe(_ context.Context, _ providerconnection.ProviderConnection, secret []byte, _ int) (providerconnection.ProbeResult, error) {
	if len(secret) == 0 {
		return providerconnection.ProbeResult{}, errors.New("missing secret")
	}
	return operator.probe, operator.probeErr
}

func (operator *fakeProviderOperator) Discover(_ context.Context, _ providerconnection.ProviderConnection, secret []byte, _ int) (providerconnection.DiscoveryResult, error) {
	if len(secret) == 0 {
		return providerconnection.DiscoveryResult{}, errors.New("missing secret")
	}
	return operator.discovery, operator.discoverErr
}

func providerConnectionDatabase(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	if err := migrations.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := providerconnection.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	return db, ctx
}

func nativeTextProfile(revision int64) provider.CapabilityProfile {
	return provider.CapabilityProfile{Revision: revision, Features: map[string]provider.CapabilitySupport{"text": provider.CapabilityNative}}
}
