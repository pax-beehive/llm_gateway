//go:build integration

package controlrelay_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/toddzheng/llm-gateway/internal/accessprojection"
	"github.com/toddzheng/llm-gateway/internal/controlevent"
	"github.com/toddzheng/llm-gateway/internal/controlrelay"
	"github.com/toddzheng/llm-gateway/internal/migrations"
	"github.com/toddzheng/llm-gateway/internal/operations"
	"github.com/toddzheng/llm-gateway/internal/provider"
	"github.com/toddzheng/llm-gateway/internal/providerconnection"
	"github.com/toddzheng/llm-gateway/internal/routingcatalog"
	"github.com/toddzheng/llm-gateway/internal/secretcustody"
	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

type handlerTransport struct{ handler http.Handler }

type fetcherFunc func(context.Context, int64, int) (controlevent.Batch, error)

func (function fetcherFunc) Fetch(ctx context.Context, after int64, limit int) (controlevent.Batch, error) {
	return function(ctx, after, limit)
}

func (transport handlerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	transport.handler.ServeHTTP(recorder, request)
	return recorder.Result(), nil
}

func TestRelayProjectsAndResolvesProviderConnectionAcrossIsolatedSchemas(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	admin, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	suffix := time.Now().UnixNano()
	controlSchema := fmt.Sprintf("relay_control_%d", suffix)
	gatewaySchema := fmt.Sprintf("relay_gateway_%d", suffix)
	if _, err := admin.ExecContext(ctx, fmt.Sprintf(`CREATE SCHEMA %s; CREATE SCHEMA %s`, controlSchema, gatewaySchema)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), fmt.Sprintf(`DROP SCHEMA %s CASCADE; DROP SCHEMA %s CASCADE`, controlSchema, gatewaySchema))
	})
	openSchema := func(schema string) *sql.DB {
		parsed, parseErr := url.Parse(databaseURL)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		query := parsed.Query()
		query.Set("search_path", schema)
		parsed.RawQuery = query.Encode()
		database, openErr := sql.Open("pgx", parsed.String())
		if openErr != nil {
			t.Fatal(openErr)
		}
		t.Cleanup(func() { _ = database.Close() })
		return database
	}
	controlDB := openSchema(controlSchema)
	gatewayDB := openSchema(gatewaySchema)
	for _, migrate := range []func(context.Context, *sql.DB) error{migrations.Migrate, tenantadmin.Migrate, providerconnection.Migrate, routingcatalog.Migrate, operations.Migrate} {
		if err := migrate(ctx, controlDB); err != nil {
			t.Fatal(err)
		}
	}
	for _, migrate := range []func(context.Context, *sql.DB) error{migrations.Migrate, accessprojection.Migrate, providerconnection.MigrateGatewayProjection, controlrelay.Migrate} {
		if err := migrate(ctx, gatewayDB); err != nil {
			t.Fatal(err)
		}
	}
	firstTransaction, err := controlDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := firstTransaction.ExecContext(ctx, `INSERT INTO control_outbox (
		event_id,schema_version,aggregate_type,aggregate_id,aggregate_revision,tenant_id,event_type,occurred_at,payload
	) VALUES ('cevt-serialized-first',1,'Other','serialized-first',1,NULL,'OtherChanged',now(),'{}')`); err != nil {
		t.Fatal(err)
	}
	secondCommitted := make(chan error, 1)
	go func() {
		_, insertErr := controlDB.ExecContext(ctx, `INSERT INTO control_outbox (
			event_id,schema_version,aggregate_type,aggregate_id,aggregate_revision,tenant_id,event_type,occurred_at,payload
		) VALUES ('cevt-serialized-second',1,'Other','serialized-second',1,NULL,'OtherChanged',now(),'{}')`)
		secondCommitted <- insertErr
	}()
	select {
	case err := <-secondCommitted:
		t.Fatalf("later Control Event committed before the earlier transaction: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := firstTransaction.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-secondCommitted:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	custody := secretcustody.NewMemory()
	reference, err := custody.Put(ctx, "cross-database-provider", []byte("provider-secret-material"))
	if err != nil {
		t.Fatal(err)
	}
	capability := provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{"responses": provider.CapabilityNative}}
	capabilityJSON, _ := json.Marshal(capability)
	if _, err := controlDB.ExecContext(ctx, `INSERT INTO provider_connections (
		id,provider,display_name,base_url,region,credential_scope,secret_ref,secret_external_version,
		administrative_status,capability_declaration,credential_version,revision
	) VALUES ('pc-openai','openai','OpenAI','https://api.openai.com/v1','us-west1','projected',
		$1,$2,'enabled',$3,1,1)`, reference.Name, reference.Version, capabilityJSON); err != nil {
		t.Fatal(err)
	}
	eventPayload, _ := json.Marshal(map[string]any{
		"connection_id": "pc-openai", "provider": "openai", "base_url": "https://api.openai.com/v1",
		"region": "us-west1", "credential_scope": "projected", "administrative_status": "enabled",
		"capability_declaration": capability, "credential_version": 1, "revision": 1,
	})
	if _, err := controlDB.ExecContext(ctx, `INSERT INTO control_outbox (
		event_id,schema_version,aggregate_type,aggregate_id,aggregate_revision,tenant_id,event_type,occurred_at,payload
	) VALUES ('cevt-provider',3,'ProviderConnection','pc-openai',1,NULL,'ProviderConnectionEnabled',now(),$1)`, eventPayload); err != nil {
		t.Fatal(err)
	}
	if _, err := controlDB.ExecContext(ctx, `INSERT INTO control_outbox (
		event_id,schema_version,aggregate_type,aggregate_id,aggregate_revision,tenant_id,event_type,occurred_at,payload
	) VALUES ('cevt-unsupported',1,'Other','other',1,NULL,'OtherChanged',now(),'{}')`); err != nil {
		t.Fatal(err)
	}
	routingDocument := routingcatalog.Document{Routes: []routingcatalog.ManagedRoute{}}
	bootstrapCompiler := routingcatalog.RuntimeCompilerFunc(func(context.Context, routingcatalog.Document) ([]provider.Route, error) {
		return []provider.Route{}, nil
	})
	compiledRouting, err := bootstrapCompiler.CompileSnapshot(ctx, routingDocument)
	if err != nil {
		t.Fatal(err)
	}
	routingJSON, _ := json.Marshal(routingDocument)
	routingValidationJSON, _ := json.Marshal(routingcatalog.ValidationReport{
		Valid: true, Hash: compiledRouting.ValidationHash,
		Errors: []routingcatalog.ValidationIssue{}, Warnings: []routingcatalog.ValidationIssue{},
	})
	if _, err := controlDB.ExecContext(ctx, `INSERT INTO routing_catalog_revisions (
		revision,document,validation_report,validation_hash,source_revision,created_by,created_at
	) VALUES (2,$1,$2,$3,NULL,'integration',now())`, routingJSON, routingValidationJSON, compiledRouting.ValidationHash); err != nil {
		t.Fatal(err)
	}
	if _, err := controlDB.ExecContext(ctx, `INSERT INTO routing_catalog_head (singleton,revision,updated_at) VALUES (true,2,now())`); err != nil {
		t.Fatal(err)
	}

	key := []byte("cross-database-relay-hmac-key-00000001")
	verifier, err := operations.NewHMACVerifier(map[string]string{"gateway-a": string(key)}, map[string]string{"gateway-a": "us-west1"}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	eventPublisher, _ := controlrelay.NewPostgresPublisher(controlDB)
	bootstrapPublisher, _ := controlrelay.NewPostgresBootstrapPublisher(controlDB, time.Now)
	secretPublisher, _ := controlrelay.NewPostgresSecretPublisher(controlDB, custody)
	eventHandler, _ := controlrelay.NewHandler(eventPublisher, verifier)
	bootstrapHandler, _ := controlrelay.NewBootstrapHandler(bootstrapPublisher, verifier)
	secretHandler, _ := controlrelay.NewSecretHandler(secretPublisher, verifier)
	mux := http.NewServeMux()
	mux.Handle(controlrelay.EventPath, eventHandler)
	mux.Handle(controlrelay.BootstrapPath, bootstrapHandler)
	mux.Handle(controlrelay.SecretPathPrefix, secretHandler)
	httpClient := &http.Client{Transport: handlerTransport{handler: mux}}
	client, err := controlrelay.NewClient("https://control.example.test", "gateway-a", key, httpClient, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := providerconnection.NewProjection(gatewayDB, "us-west1", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	relay, err := controlrelay.NewConsumer(gatewayDB, "control-plane", client, []controlevent.Consumer{projection}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	worked, err := relay.RunNext(ctx, 256)
	if err != nil || !worked {
		t.Fatalf("relay worked=%v err=%v", worked, err)
	}
	status, err := relay.Status(ctx)
	if err != nil || status.Cursor <= 0 || status.SourceHead < status.Cursor || status.LastFetchedAt == nil {
		t.Fatalf("relay status = %#v err=%v", status, err)
	}
	resolver, err := providerconnection.NewProjectedGatewayResolver(gatewayDB, "us-west1", client)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolver.Resolve(ctx, "pc-openai")
	if err != nil {
		t.Fatal(err)
	}
	defer clear(resolved.Secret)
	if string(resolved.Secret) != "provider-secret-material" || resolved.Connection.Revision != 1 {
		t.Fatalf("resolved = %#v secret=%q", resolved.Connection, resolved.Secret)
	}
	if _, err := controlDB.ExecContext(ctx, `UPDATE provider_connections SET revision=2 WHERE id='pc-openai'`); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Resolve(ctx, "pc-openai"); !errors.Is(err, providerconnection.ErrNotFound) {
		t.Fatalf("stale local execution projection error = %v, want Provider Connection not found", err)
	}
	compiler, err := routingcatalog.NewManagedCompiler(resolver, nil)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := compiler.CompileSnapshot(ctx, routingcatalog.Document{Routes: []routingcatalog.ManagedRoute{{ProviderConnectionID: "pc-openai"}}})
	if err != nil || len(compiled.Routes) != 0 {
		t.Fatalf("historical catalog with stale Provider projection = %#v err=%v", compiled, err)
	}
	movePayload, _ := json.Marshal(map[string]any{
		"connection_id": "pc-moved", "provider": "openai", "base_url": "https://api.openai.com/v1",
		"region": "us-east1", "previous_region": "us-west1", "credential_scope": "moved",
		"administrative_status": "enabled", "capability_declaration": capability,
		"credential_version": 2, "revision": 7,
	})
	if err := projection.Consume(ctx, controlevent.Event{
		EventID: "cevt-move-away", DeliverySequence: 9_000_000_000, SchemaVersion: 3,
		AggregateType: "ProviderConnection", AggregateID: "pc-moved", AggregateRevision: 7,
		EventType: "ProviderConnectionChanged", OccurredAt: time.Now().UTC(), Payload: movePayload,
	}); err != nil {
		t.Fatalf("bootstrap move-away tombstone: %v", err)
	}
	var movedRegion string
	var movedRevision int64
	if err := gatewayDB.QueryRowContext(ctx, `SELECT region,revision FROM gateway_provider_connection_projection WHERE id='pc-moved'`).Scan(&movedRegion, &movedRevision); err != nil {
		t.Fatal(err)
	}
	if movedRegion != "us-east1" || movedRevision != 7 {
		t.Fatalf("move-away tombstone = %s/%d", movedRegion, movedRevision)
	}
	var leaked int
	if err := gatewayDB.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.tables
		WHERE table_schema=current_schema() AND table_name='control_outbox'`).Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Fatal("Gateway database unexpectedly contains the control-plane outbox")
	}
	var storedColumns string
	if err := gatewayDB.QueryRowContext(ctx, `SELECT string_agg(column_name,',' ORDER BY column_name) FROM information_schema.columns
		WHERE table_schema=current_schema() AND table_name='gateway_provider_connection_projection'`).Scan(&storedColumns); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(storedColumns, "secret") {
		t.Fatalf("projection columns leak secret metadata: %s", storedColumns)
	}

	bootstrap, err := client.FetchBootstrap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	encodedBootstrap, _ := json.Marshal(bootstrap)
	if bootstrap.SourceCursor != status.SourceHead || len(bootstrap.ProviderConnections) != 1 || bootstrap.RoutingCatalog == nil ||
		bootstrap.ProviderConnections[0].ConnectionID != "pc-openai" || strings.Contains(string(encodedBootstrap), reference.Name) ||
		strings.Contains(string(encodedBootstrap), "provider-secret-material") {
		t.Fatalf("bootstrap = %#v", bootstrap)
	}
	if err := projection.ReplaceSnapshots(ctx, bootstrap.ProviderConnections); err != nil {
		t.Fatal(err)
	}
	bootstrapRouter := provider.NewVersionedRouter(1, nil)
	bootstrapRoutingConsumer, err := routingcatalog.NewConsumer(gatewayDB, bootstrapCompiler, bootstrapRouter, "gateway-bootstrap", "us-west1", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if err := bootstrapRoutingConsumer.ReplaceSnapshot(ctx, routingcatalog.Revision{
		Revision: bootstrap.RoutingCatalog.Revision, Document: bootstrap.RoutingCatalog.Document,
		Validation:     bootstrap.RoutingCatalog.Validation,
		ValidationHash: bootstrap.RoutingCatalog.ValidationHash, CreatedAt: bootstrap.RoutingCatalog.CreatedAt,
	}); err != nil {
		t.Fatal(err)
	}
	projectedRouting, err := routingcatalog.LoadProjected(ctx, gatewayDB)
	if err != nil || projectedRouting.Revision != 2 || bootstrapRouter.Revision() != 2 {
		t.Fatalf("bootstrapped Routing Catalog = %#v router=%d err=%v", projectedRouting, bootstrapRouter.Revision(), err)
	}
	retention, err := controlrelay.NewRetention(controlDB, time.Now, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controlDB.ExecContext(ctx, `INSERT INTO operations_gateway_heartbeats (
		gateway_id,region,build_sha,database_schema_version,routing_catalog_revision,access_projection_revision,
		execution_epoch_floor,last_usage_outbox_id,started_at,observed_at,received_at,consumers,backlogs
	) VALUES ('gateway-a','us-west1','integration',23,0,0,0,0,now(),now(),now(),$1,'{}')`,
		fmt.Sprintf(`[{"name":"control_event_relay","last_event_id":"%d","lag_seconds":0,"pending_count":1}]`, bootstrap.SourceCursor-1)); err != nil {
		t.Fatal(err)
	}
	retained, err := retention.PruneThrough(ctx, bootstrap.SourceCursor, 256)
	if err != nil || retained.SafeThrough != bootstrap.SourceCursor-1 || retained.MinimumCursor != bootstrap.SourceCursor-1 || retained.Deleted == 0 {
		t.Fatalf("retention = %#v/%v", retained, err)
	}
	if _, err := client.Fetch(ctx, 0, 256); err == nil {
		t.Fatal("pruned Control Event history did not require bootstrap")
	} else {
		var historyErr *controlrelay.HistoryUnavailableError
		if !errors.As(err, &historyErr) || historyErr.MinimumCursor != bootstrap.SourceCursor-1 {
			t.Fatalf("history error = %v", err)
		}
	}
	if batch, err := client.Fetch(ctx, bootstrap.SourceCursor-1, 256); err != nil || batch.NextCursor != bootstrap.SourceCursor || batch.SourceHead != bootstrap.SourceCursor {
		t.Fatalf("retained tail = %#v/%v", batch, err)
	}
	if _, err := controlDB.ExecContext(ctx, `UPDATE operations_gateway_heartbeats SET
		consumers=$1,observed_at=now(),received_at=now() WHERE gateway_id='gateway-a'`,
		fmt.Sprintf(`[{"name":"control_event_relay","last_event_id":"%d","lag_seconds":0,"pending_count":0}]`, bootstrap.SourceCursor)); err != nil {
		t.Fatal(err)
	}
	retained, err = retention.PruneThrough(ctx, bootstrap.SourceCursor, 256)
	if err != nil || retained.MinimumCursor != bootstrap.SourceCursor || retained.Deleted == 0 {
		t.Fatalf("final retention = %#v/%v", retained, err)
	}
	postRetentionBootstrap, err := client.FetchBootstrap(ctx)
	if err != nil || postRetentionBootstrap.SourceCursor != bootstrap.SourceCursor {
		t.Fatalf("post-retention bootstrap = %#v/%v", postRetentionBootstrap, err)
	}
	if err := relay.BootstrapCursor(ctx, bootstrap.SourceCursor); err != nil {
		t.Fatal(err)
	}
	if cursor, err := relay.Cursor(ctx); err != nil || cursor != bootstrap.SourceCursor {
		t.Fatalf("bootstrapped cursor = %d/%v", cursor, err)
	}

	retryNow := time.Now().UTC()
	retryRelay, err := controlrelay.NewConsumer(gatewayDB, "retryable-control-plane", fetcherFunc(func(_ context.Context, after int64, _ int) (controlevent.Batch, error) {
		return controlevent.Batch{Events: []controlevent.Event{{EventID: "cevt-retry", DeliverySequence: after + 1}}, NextCursor: after + 1, SourceHead: after + 1}, nil
	}), []controlevent.Consumer{controlevent.ConsumerFunc(func(context.Context, controlevent.Event) error {
		return controlevent.ErrExecutionSecretUnavailable
	})}, func() time.Time { return retryNow })
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := retryRelay.RunNext(ctx, 1); worked || !errors.Is(err, controlevent.ErrExecutionSecretUnavailable) {
		t.Fatalf("retryable relay result = %v/%v", worked, err)
	}
	if cursor, err := retryRelay.Cursor(ctx); err != nil || cursor != 0 {
		t.Fatalf("retryable relay cursor = %d/%v, want zero", cursor, err)
	}
	retryStatus, err := retryRelay.Status(ctx)
	if err != nil || retryStatus.SourceHead != 1 || retryStatus.LastFetchedAt == nil || retryStatus.LastErrorCode != "control_event_projection_failed" {
		t.Fatalf("retryable relay status = %#v/%v", retryStatus, err)
	}
	firstFailureAt := retryStatus.FailureStartedAt
	retryNow = retryNow.Add(5 * time.Second)
	if worked, err := retryRelay.RunNext(ctx, 1); worked || !errors.Is(err, controlevent.ErrExecutionSecretUnavailable) {
		t.Fatalf("repeated retryable relay result = %v/%v", worked, err)
	}
	retryStatus, err = retryRelay.Status(ctx)
	if err != nil || firstFailureAt == nil || retryStatus.FailureStartedAt == nil || !retryStatus.FailureStartedAt.Equal(*firstFailureAt) {
		t.Fatalf("repeated retryable relay status = %#v/%v, first failure %v", retryStatus, err, firstFailureAt)
	}

	fetchFailureRelay, err := controlrelay.NewConsumer(gatewayDB, "failed-control-plane", fetcherFunc(func(context.Context, int64, int) (controlevent.Batch, error) {
		return controlevent.Batch{}, errors.New("relay unavailable")
	}), []controlevent.Consumer{controlevent.ConsumerFunc(func(context.Context, controlevent.Event) error { return nil })}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := fetchFailureRelay.RunNext(ctx, 1); worked || err == nil {
		t.Fatalf("failed fetch result = %v/%v", worked, err)
	}
	fetchFailureStatus, err := fetchFailureRelay.Status(ctx)
	if err != nil || fetchFailureStatus.Cursor != 0 || fetchFailureStatus.LastAttemptAt == nil || fetchFailureStatus.LastErrorCode != "control_event_fetch_failed" {
		t.Fatalf("failed fetch status = %#v/%v", fetchFailureStatus, err)
	}
	invalidBatchRelay, err := controlrelay.NewConsumer(gatewayDB, "invalid-control-plane", fetcherFunc(func(context.Context, int64, int) (controlevent.Batch, error) {
		return controlevent.Batch{NextCursor: 2, SourceHead: 1}, nil
	}), []controlevent.Consumer{controlevent.ConsumerFunc(func(context.Context, controlevent.Event) error { return nil })}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := invalidBatchRelay.RunNext(ctx, 1); worked || err == nil {
		t.Fatalf("invalid batch result = %v/%v", worked, err)
	}
	invalidStatus, err := invalidBatchRelay.Status(ctx)
	if err != nil || invalidStatus.LastErrorCode != "control_event_batch_invalid" || invalidStatus.FailureStartedAt == nil {
		t.Fatalf("invalid batch status = %#v/%v", invalidStatus, err)
	}

	observedAt := time.Now().UTC()
	if _, err := gatewayDB.ExecContext(ctx, `UPDATE gateway_control_event_offsets SET
		source_head=cursor+1,last_fetched_at=$2,last_succeeded_at=$1,last_attempt_at=$2,
		failure_started_at=$1,last_error_code='control_event_fetch_failed'
		WHERE stream_name='control-plane'`, observedAt.Add(-5*time.Second), observedAt); err != nil {
		t.Fatal(err)
	}
	observation, err := operations.ObserveGateway(ctx, gatewayDB, "gateway-a", "us-west1", "relay-test-build", observedAt.Add(-time.Minute), 1, observedAt)
	if err != nil {
		t.Fatal(err)
	}
	var relayObservation *operations.ConsumerObservation
	for index := range observation.Consumers {
		if observation.Consumers[index].Name == "control_event_relay" {
			relayObservation = &observation.Consumers[index]
			break
		}
	}
	if relayObservation == nil || relayObservation.PendingCount != 1 || relayObservation.ErrorCode != "control_event_fetch_failed" || relayObservation.LagSeconds < 5 {
		t.Fatalf("relay Operations observation = %#v", relayObservation)
	}
}
