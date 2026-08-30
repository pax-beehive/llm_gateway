//go:build integration

package routingcatalog_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/toddzheng/llm-gateway/internal/controlevent"
	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/migrations"
	"github.com/toddzheng/llm-gateway/internal/provider"
	"github.com/toddzheng/llm-gateway/internal/providerconnection"
	"github.com/toddzheng/llm-gateway/internal/routingcatalog"
	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

func TestValidatedDraftPublishesOneImmutableRoutingCatalogRevision(t *testing.T) {
	ctx, _, service, actor := newIntegrationService(t)
	baseRevision := int64(0)
	current, err := service.Current(ctx, actor)
	if err == nil {
		baseRevision = current.Revision
	} else if !errors.Is(err, routingcatalog.ErrNotFound) {
		t.Fatal(err)
	}
	draftID := fmt.Sprintf("rcd-%d", time.Now().UnixNano())
	created, err := service.CreateDraft(ctx, actor, "create-"+draftID, routingcatalog.CreateDraftCommand{
		ID: draftID, BaseRevision: baseRevision, Document: validDocument(),
	})
	if err != nil {
		t.Fatal(err)
	}
	validated, err := service.ValidateDraft(ctx, actor, routingcatalog.ValidateDraftCommand{DraftID: draftID, ExpectedRevision: created.Draft.Revision})
	if err != nil || !validated.Draft.Validation.Valid || validated.Draft.Status != routingcatalog.DraftValidated {
		t.Fatalf("validate draft = %#v / %v", validated, err)
	}
	published, err := service.PublishDraft(ctx, actor, "publish-"+draftID, routingcatalog.PublishDraftCommand{
		DraftID: draftID, ExpectedRevision: validated.Draft.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if published.Publication.CatalogRevision != baseRevision+1 || published.Revision.SourceRevision != 0 || published.Revision.ValidationHash == "" {
		t.Fatalf("publication = %#v", published)
	}
	active, err := service.Current(ctx, actor)
	if err != nil || active.Revision != published.Revision.Revision || active.Document.Routes[0].ProviderConnectionID != "pc-openai-us" {
		t.Fatalf("current revision = %#v / %v", active, err)
	}
}

func TestConcurrentDraftPublicationsFromOneBaseHaveOneWinner(t *testing.T) {
	ctx, _, service, actor := newIntegrationService(t)
	base := int64(0)
	if current, err := service.Current(ctx, actor); err == nil {
		base = current.Revision
	} else if !errors.Is(err, routingcatalog.ErrNotFound) {
		t.Fatal(err)
	}
	validated := make([]routingcatalog.Draft, 2)
	for index := range validated {
		id := fmt.Sprintf("rcd-race-%d-%d", time.Now().UnixNano(), index)
		created, err := service.CreateDraft(ctx, actor, "create-"+id, routingcatalog.CreateDraftCommand{ID: id, BaseRevision: base, Document: validDocument()})
		if err != nil {
			t.Fatal(err)
		}
		result, err := service.ValidateDraft(ctx, actor, routingcatalog.ValidateDraftCommand{DraftID: id, ExpectedRevision: created.Draft.Revision})
		if err != nil {
			t.Fatal(err)
		}
		validated[index] = result.Draft
	}
	first, err := service.PublishDraft(ctx, actor, "publish-"+validated[0].ID, routingcatalog.PublishDraftCommand{DraftID: validated[0].ID, ExpectedRevision: validated[0].Revision})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.PublishDraft(ctx, actor, "publish-"+validated[1].ID, routingcatalog.PublishDraftCommand{DraftID: validated[1].ID, ExpectedRevision: validated[1].Revision})
	if !errors.Is(err, routingcatalog.ErrRevisionConflict) {
		t.Fatalf("second publication error = %v", err)
	}
	current, err := service.Current(ctx, actor)
	if err != nil || current.Revision != first.Revision.Revision {
		t.Fatalf("current revision = %#v / %v", current, err)
	}
}

func TestRestoreCopiesPriorCatalogIntoNewRevision(t *testing.T) {
	ctx, _, service, actor := newIntegrationService(t)
	current, err := service.Current(ctx, actor)
	if errors.Is(err, routingcatalog.ErrNotFound) {
		id := fmt.Sprintf("rcd-restore-source-%d", time.Now().UnixNano())
		created, createErr := service.CreateDraft(ctx, actor, "create-"+id, routingcatalog.CreateDraftCommand{ID: id, Document: validDocument()})
		if createErr != nil {
			t.Fatal(createErr)
		}
		validated, validateErr := service.ValidateDraft(ctx, actor, routingcatalog.ValidateDraftCommand{DraftID: id, ExpectedRevision: created.Draft.Revision})
		if validateErr != nil {
			t.Fatal(validateErr)
		}
		published, publishErr := service.PublishDraft(ctx, actor, "publish-"+id, routingcatalog.PublishDraftCommand{DraftID: id, ExpectedRevision: validated.Draft.Revision})
		if publishErr != nil {
			t.Fatal(publishErr)
		}
		current = published.Revision
		err = nil
	}
	if err != nil {
		t.Fatal(err)
	}
	restored, err := service.Restore(ctx, actor, fmt.Sprintf("restore-%d", time.Now().UnixNano()), routingcatalog.RestoreCommand{
		SourceRevision: current.Revision, ExpectedHead: current.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if restored.Revision.Revision != current.Revision+1 || restored.Revision.SourceRevision != current.Revision ||
		restored.Revision.ValidationHash != current.ValidationHash {
		t.Fatalf("restored revision = %#v, source = %#v", restored, current)
	}
}

func TestUpdatingValidatedDraftInvalidatesItsValidationEvidence(t *testing.T) {
	ctx, _, service, actor := newIntegrationService(t)
	base := int64(0)
	if current, err := service.Current(ctx, actor); err == nil {
		base = current.Revision
	}
	id := fmt.Sprintf("rcd-update-%d", time.Now().UnixNano())
	created, err := service.CreateDraft(ctx, actor, "create-"+id, routingcatalog.CreateDraftCommand{ID: id, BaseRevision: base, Document: validDocument()})
	if err != nil {
		t.Fatal(err)
	}
	validated, err := service.ValidateDraft(ctx, actor, routingcatalog.ValidateDraftCommand{DraftID: id, ExpectedRevision: created.Draft.Revision})
	if err != nil {
		t.Fatal(err)
	}
	changed := validDocument()
	changed.Routes[0].PublicModel = "changed-model"
	updated, err := service.UpdateDraft(ctx, actor, "update-"+id, routingcatalog.UpdateDraftCommand{
		DraftID: id, ExpectedRevision: validated.Draft.Revision, Document: changed,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Draft.Status != routingcatalog.DraftOpen || updated.Draft.ValidationHash != "" || updated.Draft.Validation.Valid {
		t.Fatalf("updated draft = %#v", updated.Draft)
	}
}

func TestPublishRevalidatesProviderConnectionsAfterDraftValidation(t *testing.T) {
	connection := routingcatalog.ConnectionDescriptor{
		ID: "pc-openai-us", Provider: "openai", Region: "us-west", CredentialScope: "organization-a", Enabled: true,
		CapabilityProfile: provider.CapabilityProfile{Revision: 4, Features: map[string]provider.CapabilitySupport{
			"text": provider.CapabilityNative, "streaming": provider.CapabilityNative,
		}},
	}
	lookup := routingcatalog.ConnectionLookupFunc(func(context.Context, string) (routingcatalog.ConnectionDescriptor, error) {
		return connection, nil
	})
	ctx, _, service, actor := newIntegrationServiceWithLookup(t, lookup)
	base := int64(0)
	if current, err := service.Current(ctx, actor); err == nil {
		base = current.Revision
	}
	id := fmt.Sprintf("rcd-revalidate-%d", time.Now().UnixNano())
	created, err := service.CreateDraft(ctx, actor, "create-"+id, routingcatalog.CreateDraftCommand{ID: id, BaseRevision: base, Document: validDocument()})
	if err != nil {
		t.Fatal(err)
	}
	validated, err := service.ValidateDraft(ctx, actor, routingcatalog.ValidateDraftCommand{DraftID: id, ExpectedRevision: created.Draft.Revision})
	if err != nil {
		t.Fatal(err)
	}
	connection.Enabled = false
	_, err = service.PublishDraft(ctx, actor, "publish-"+id, routingcatalog.PublishDraftCommand{DraftID: id, ExpectedRevision: validated.Draft.Revision})
	if !errors.Is(err, routingcatalog.ErrValidationFailed) {
		t.Fatalf("publish after Provider Connection change error = %v", err)
	}
}

func TestRolloutReceiptsAdvancePublicationOnlyAfterRegionalQuorum(t *testing.T) {
	ctx, _, service, actor := newIntegrationService(t)
	base := int64(0)
	if current, err := service.Current(ctx, actor); err == nil {
		base = current.Revision
	}
	id := fmt.Sprintf("rcd-receipts-%d", time.Now().UnixNano())
	created, err := service.CreateDraft(ctx, actor, "create-"+id, routingcatalog.CreateDraftCommand{ID: id, BaseRevision: base, Document: validDocument()})
	if err != nil {
		t.Fatal(err)
	}
	validated, err := service.ValidateDraft(ctx, actor, routingcatalog.ValidateDraftCommand{DraftID: id, ExpectedRevision: created.Draft.Revision})
	if err != nil {
		t.Fatal(err)
	}
	published, err := service.PublishDraft(ctx, actor, "publish-"+id, routingcatalog.PublishDraftCommand{
		DraftID: id, ExpectedRevision: validated.Draft.Revision, RequiredRegions: []string{"eu-west", "us-west"},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	first, err := service.RecordRolloutReceipt(ctx, routingcatalog.RolloutReceipt{
		PublicationID: published.Publication.ID, GatewayID: "gateway-us-1", Region: "us-west",
		CatalogRevision: published.Revision.Revision, Status: routingcatalog.ReceiptApplied, ObservedAt: now,
	})
	if err != nil || first.Status != routingcatalog.PublicationRollingOut {
		t.Fatalf("first receipt = %#v / %v", first, err)
	}
	active, err := service.RecordRolloutReceipt(ctx, routingcatalog.RolloutReceipt{
		PublicationID: published.Publication.ID, GatewayID: "gateway-eu-1", Region: "eu-west",
		CatalogRevision: published.Revision.Revision, Status: routingcatalog.ReceiptApplied, ObservedAt: now.Add(time.Second),
	})
	if err != nil || active.Status != routingcatalog.PublicationActive || len(active.Receipts) != 2 {
		t.Fatalf("active publication = %#v / %v", active, err)
	}
	for _, receipt := range active.Receipts {
		if receipt.LagMilliseconds < 0 {
			t.Fatalf("negative rollout lag = %#v", receipt)
		}
	}
	stale, err := service.RecordRolloutReceipt(ctx, routingcatalog.RolloutReceipt{
		PublicationID: published.Publication.ID, GatewayID: "gateway-us-1", Region: "us-west",
		CatalogRevision: published.Revision.Revision, Status: routingcatalog.ReceiptRejected, ErrorCode: "stale",
		ObservedAt: now.Add(-time.Second),
	})
	if err != nil || stale.Status != routingcatalog.PublicationActive {
		t.Fatalf("stale receipt moved status backward = %#v / %v", stale, err)
	}
}

func TestGatewayConsumerAppliesPublishedCatalogAndReportsRollout(t *testing.T) {
	ctx, db, service, actor := newIntegrationService(t)
	base := int64(0)
	if current, err := service.Current(ctx, actor); err == nil {
		base = current.Revision
	}
	id := fmt.Sprintf("rcd-consume-%d", time.Now().UnixNano())
	created, err := service.CreateDraft(ctx, actor, "create-"+id, routingcatalog.CreateDraftCommand{ID: id, BaseRevision: base, Document: validDocument()})
	if err != nil {
		t.Fatal(err)
	}
	validated, err := service.ValidateDraft(ctx, actor, routingcatalog.ValidateDraftCommand{DraftID: id, ExpectedRevision: created.Draft.Revision})
	if err != nil {
		t.Fatal(err)
	}
	published, err := service.PublishDraft(ctx, actor, "publish-"+id, routingcatalog.PublishDraftCommand{
		DraftID: id, ExpectedRevision: validated.Draft.Revision, RequiredRegions: []string{"us-west"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolver := routingcatalog.RuntimeConnectionResolverFunc(func(context.Context, string) (providerconnection.ResolvedConnection, error) {
		return providerconnection.ResolvedConnection{Connection: providerconnection.ProviderConnection{
			ID: "pc-openai-us", Provider: "openai", BaseURL: "https://api.openai.com/v1", Region: "us-west", CredentialScope: "organization-a",
			AdministrativeStatus: providerconnection.StatusEnabled, CredentialVersion: 1,
			CapabilityDeclaration: provider.CapabilityProfile{Revision: 4, Features: map[string]provider.CapabilitySupport{
				"text": provider.CapabilityNative, "streaming": provider.CapabilityNative,
			}},
		}, Secret: []byte("managed-secret")}, nil
	})
	compiler, err := routingcatalog.NewManagedCompiler(resolver, nil)
	if err != nil {
		t.Fatal(err)
	}
	router := provider.NewVersionedRouter(1, []provider.Route{{
		ID: "legacy", Provider: "echo", Model: "legacy", Region: "local", HomeRegion: "local", Healthy: true,
		Profile: provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{"text": provider.CapabilityNative}},
	}})
	consumer, err := routingcatalog.NewConsumer(db, compiler, router, "gateway-us-test", "us-west", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	for count := 0; count < 100; count++ {
		worked, err := consumer.RunNext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			break
		}
	}
	if router.Revision() != published.Revision.Revision {
		t.Fatalf("router revision = %d, want %d", router.Revision(), published.Revision.Revision)
	}
	models, err := router.ListModels(ctx, provider.ModelCatalogQuery{TenantID: "tenant-a", HomeRegion: "us-west"})
	if err != nil || len(models) != 1 || models[0].ID != "gateway-model" {
		t.Fatalf("managed models = %#v / %v", models, err)
	}
	drainReceiptCollector(t, ctx, service)
	publication, err := service.GetPublication(ctx, actor, published.Publication.ID)
	if err != nil || publication.Status != routingcatalog.PublicationActive {
		t.Fatalf("publication rollout = %#v / %v", publication, err)
	}
	secondRouter := provider.NewVersionedRouter(1, nil)
	secondConsumer, err := routingcatalog.NewConsumer(db, compiler, secondRouter, "gateway-us-test-secondary", "us-west", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	for count := 0; count < 10_000; count++ {
		worked, err := secondConsumer.RunNext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			break
		}
	}
	drainReceiptCollector(t, ctx, service)
	publication, err = service.GetPublication(ctx, actor, published.Publication.ID)
	if err != nil || len(publication.Receipts) != 2 || secondRouter.Revision() != published.Revision.Revision {
		t.Fatalf("per-Gateway rollout receipts = %#v, second revision=%d / %v", publication, secondRouter.Revision(), err)
	}
}

func TestGatewayConsumerRejectsOneRevisionAndContinuesToLaterPublication(t *testing.T) {
	ctx, db, service, actor := newIntegrationService(t)
	base := int64(0)
	if current, err := service.Current(ctx, actor); err == nil {
		base = current.Revision
	}
	publish := func(model string, baseRevision int64) routingcatalog.PublicationResult {
		id := fmt.Sprintf("rcd-%s-%d", model, time.Now().UnixNano())
		document := validDocument()
		document.Routes[0].PublicModel = model
		created, err := service.CreateDraft(ctx, actor, "create-"+id, routingcatalog.CreateDraftCommand{ID: id, BaseRevision: baseRevision, Document: document})
		if err != nil {
			t.Fatal(err)
		}
		validated, err := service.ValidateDraft(ctx, actor, routingcatalog.ValidateDraftCommand{DraftID: id, ExpectedRevision: created.Draft.Revision})
		if err != nil {
			t.Fatal(err)
		}
		result, err := service.PublishDraft(ctx, actor, "publish-"+id, routingcatalog.PublishDraftCommand{
			DraftID: id, ExpectedRevision: validated.Draft.Revision, RequiredRegions: []string{"us-west"},
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	rejected := publish("reject-me", base)
	applied := publish("apply-me", rejected.Revision.Revision)
	compiler := routingcatalog.RuntimeCompilerFunc(func(_ context.Context, document routingcatalog.Document) ([]provider.Route, error) {
		if document.Routes[0].PublicModel == "reject-me" {
			return nil, errors.New("deterministic invalid runtime snapshot")
		}
		return []provider.Route{{
			ID: "applied", Provider: "echo", Model: document.Routes[0].PublicModel, Region: "us-west", HomeRegion: "us-west", Healthy: true,
			Profile: provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{"text": provider.CapabilityNative}},
		}}, nil
	})
	startRevision := base
	if startRevision <= 0 {
		startRevision = 1
	}
	router := provider.NewVersionedRouter(startRevision, []provider.Route{{
		ID: "last-valid", Provider: "echo", Model: "last-valid", Region: "us-west", HomeRegion: "us-west", Healthy: true,
		Profile: provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{"text": provider.CapabilityNative}},
	}})
	consumer, err := routingcatalog.NewConsumer(db, compiler, router, "gateway-recovery-test", "us-west", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	for count := 0; count < 10_000; count++ {
		worked, err := consumer.RunNext(ctx)
		if err != nil || !worked {
			t.Fatalf("consume through rejected revision = %v / %v", worked, err)
		}
		drainReceiptCollector(t, ctx, service)
		status, getErr := service.GetPublication(ctx, actor, rejected.Publication.ID)
		if getErr == nil && status.Status == routingcatalog.PublicationFailed {
			break
		}
	}
	if router.Revision() == rejected.Revision.Revision {
		t.Fatalf("rejected revision replaced last valid snapshot: %d", router.Revision())
	}
	for count := 0; count < 10_000 && router.Revision() < applied.Revision.Revision; count++ {
		worked, err := consumer.RunNext(ctx)
		if err != nil || !worked {
			t.Fatalf("apply later revision = %v / %v", worked, err)
		}
	}
	if router.Revision() != applied.Revision.Revision {
		t.Fatalf("router revision = %d, want %d", router.Revision(), applied.Revision.Revision)
	}
	rejectedStatus, err := service.GetPublication(ctx, actor, rejected.Publication.ID)
	if err != nil || rejectedStatus.Status != routingcatalog.PublicationFailed || rejectedStatus.Receipts[0].ErrorCode != "catalog_compile_failed" {
		t.Fatalf("rejected publication = %#v / %v", rejectedStatus, err)
	}
}

func TestGatewayConsumerRetriesTemporaryExecutionSecretFailure(t *testing.T) {
	ctx, db, service, actor := newIntegrationService(t)
	base := int64(0)
	if current, err := service.Current(ctx, actor); err == nil {
		base = current.Revision
	}
	id := fmt.Sprintf("rcd-secret-retry-%d", time.Now().UnixNano())
	created, err := service.CreateDraft(ctx, actor, "create-"+id, routingcatalog.CreateDraftCommand{ID: id, BaseRevision: base, Document: validDocument()})
	if err != nil {
		t.Fatal(err)
	}
	validated, err := service.ValidateDraft(ctx, actor, routingcatalog.ValidateDraftCommand{DraftID: id, ExpectedRevision: created.Draft.Revision})
	if err != nil {
		t.Fatal(err)
	}
	published, err := service.PublishDraft(ctx, actor, "publish-"+id, routingcatalog.PublishDraftCommand{DraftID: id, ExpectedRevision: validated.Draft.Revision})
	if err != nil {
		t.Fatal(err)
	}
	compiler := routingcatalog.RuntimeCompilerFunc(func(context.Context, routingcatalog.Document) ([]provider.Route, error) {
		return nil, fmt.Errorf("secret relay: %w", controlevent.ErrExecutionSecretUnavailable)
	})
	gatewayID := fmt.Sprintf("gateway-secret-retry-%d", time.Now().UnixNano())
	consumer, err := routingcatalog.NewConsumer(db, compiler, provider.NewVersionedRouter(1, nil), gatewayID, "us-west", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	var envelope controlevent.Event
	if err := db.QueryRowContext(ctx, `SELECT event_id,delivery_sequence,schema_version,aggregate_type,aggregate_id,
		aggregate_revision,COALESCE(tenant_id,''),event_type,occurred_at,payload FROM control_outbox
		WHERE event_type='RoutingCatalogPublished' AND aggregate_id=$1`, published.Publication.ID).Scan(
		&envelope.EventID, &envelope.DeliverySequence, &envelope.SchemaVersion, &envelope.AggregateType, &envelope.AggregateID,
		&envelope.AggregateRevision, &envelope.TenantID, &envelope.EventType, &envelope.OccurredAt, &envelope.Payload); err != nil {
		t.Fatal(err)
	}
	if err := consumer.Consume(ctx, envelope); !errors.Is(err, controlevent.ErrExecutionSecretUnavailable) {
		t.Fatalf("temporary secret compilation = %v", err)
	}
	var acknowledged int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM gateway_routing_catalog_inbox WHERE gateway_id=$1`, gatewayID).Scan(&acknowledged); err != nil {
		t.Fatal(err)
	}
	if acknowledged != 0 {
		t.Fatalf("temporary secret failure was acknowledged: %d rows", acknowledged)
	}
}

func drainReceiptCollector(t *testing.T, ctx context.Context, service *routingcatalog.Service) {
	t.Helper()
	for count := 0; count < 10_000; count++ {
		worked, err := service.CollectNextRolloutReceipt(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			return
		}
	}
	t.Fatal("Routing Catalog receipt collector did not drain")
}

func TestGatewayConsumerRejectsInvalidEventEnvelopeAndContinues(t *testing.T) {
	ctx, db, service, actor := newIntegrationService(t)
	base := int64(0)
	if current, err := service.Current(ctx, actor); err == nil {
		base = current.Revision
	}
	badEventID := fmt.Sprintf("revt-invalid-%d", time.Now().UnixNano())
	if _, err := db.ExecContext(ctx, `INSERT INTO control_outbox (
		event_id,schema_version,aggregate_type,aggregate_id,aggregate_revision,tenant_id,event_type,occurred_at,payload
	) VALUES ($1,1,'RoutingCatalog','global',$2,NULL,'RoutingCatalogPublished',$3,'{}'::jsonb)`,
		badEventID, base+1, time.Now().UTC().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("rcd-after-invalid-%d", time.Now().UnixNano())
	created, err := service.CreateDraft(ctx, actor, "create-"+id, routingcatalog.CreateDraftCommand{ID: id, BaseRevision: base, Document: validDocument()})
	if err != nil {
		t.Fatal(err)
	}
	validated, err := service.ValidateDraft(ctx, actor, routingcatalog.ValidateDraftCommand{DraftID: id, ExpectedRevision: created.Draft.Revision})
	if err != nil {
		t.Fatal(err)
	}
	published, err := service.PublishDraft(ctx, actor, "publish-"+id, routingcatalog.PublishDraftCommand{DraftID: id, ExpectedRevision: validated.Draft.Revision})
	if err != nil {
		t.Fatal(err)
	}
	compiler := routingcatalog.RuntimeCompilerFunc(func(_ context.Context, document routingcatalog.Document) ([]provider.Route, error) {
		return []provider.Route{{
			ID: "valid-after-envelope", Provider: "echo", Model: document.Routes[0].PublicModel, Region: "us-west", HomeRegion: "us-west", Healthy: true,
			Profile: provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{"text": provider.CapabilityNative}},
		}}, nil
	})
	router := provider.NewVersionedRouter(1, nil)
	gatewayID := fmt.Sprintf("gateway-invalid-envelope-%d", time.Now().UnixNano())
	consumer, err := routingcatalog.NewConsumer(db, compiler, router, gatewayID, "us-west", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	for count := 0; count < 10_000 && router.Revision() < published.Revision.Revision; count++ {
		worked, err := consumer.RunNext(ctx)
		if err != nil || !worked {
			t.Fatalf("consumer progress = %v / %v", worked, err)
		}
	}
	var status, errorCode string
	if err := db.QueryRowContext(ctx, `SELECT status,error_code FROM gateway_routing_catalog_inbox WHERE gateway_id=$1 AND event_id=$2`, gatewayID, badEventID).Scan(&status, &errorCode); err != nil {
		t.Fatal(err)
	}
	if status != routingcatalog.ReceiptRejected || errorCode != "catalog_event_identity_invalid" || router.Revision() != published.Revision.Revision {
		t.Fatalf("invalid inbox status/error/revision = %q/%q/%d", status, errorCode, router.Revision())
	}
}

func TestGatewayConsumerRejectsCatalogWhoseDocumentDoesNotMatchValidationHash(t *testing.T) {
	ctx, db, service, actor := newIntegrationService(t)
	base := int64(0)
	if current, err := service.Current(ctx, actor); err == nil {
		base = current.Revision
	}
	id := fmt.Sprintf("rcd-hash-%d", time.Now().UnixNano())
	created, err := service.CreateDraft(ctx, actor, "create-"+id, routingcatalog.CreateDraftCommand{ID: id, BaseRevision: base, Document: validDocument()})
	if err != nil {
		t.Fatal(err)
	}
	validated, err := service.ValidateDraft(ctx, actor, routingcatalog.ValidateDraftCommand{DraftID: id, ExpectedRevision: created.Draft.Revision})
	if err != nil {
		t.Fatal(err)
	}
	published, err := service.PublishDraft(ctx, actor, "publish-"+id, routingcatalog.PublishDraftCommand{DraftID: id, ExpectedRevision: validated.Draft.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE control_outbox SET payload=jsonb_set(
		payload,'{document,routes,0,public_model}',to_jsonb('tampered-model'::text),false
	) WHERE event_type='RoutingCatalogPublished' AND aggregate_id=$1`, published.Publication.ID); err != nil {
		t.Fatal(err)
	}
	compiler := routingcatalog.RuntimeCompilerFunc(func(_ context.Context, document routingcatalog.Document) ([]provider.Route, error) {
		if document.Routes[0].PublicModel == "tampered-model" {
			t.Fatal("tampered Catalog reached runtime compilation")
		}
		return []provider.Route{{
			ID: document.Routes[0].ID, Provider: "echo", Model: document.Routes[0].PublicModel, Region: "us-west", HomeRegion: "us-west", Healthy: true,
			Profile: provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{"text": provider.CapabilityNative}},
		}}, nil
	})
	router := provider.NewVersionedRouter(max(base, 1), nil)
	gatewayID := fmt.Sprintf("gateway-hash-%d", time.Now().UnixNano())
	consumer, err := routingcatalog.NewConsumer(db, compiler, router, gatewayID, "us-west", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	for count := 0; count < 10_000; count++ {
		worked, err := consumer.RunNext(ctx)
		if err != nil || !worked {
			t.Fatalf("consume tampered Catalog = %v / %v", worked, err)
		}
		var status, code string
		err = db.QueryRowContext(ctx, `SELECT status,error_code FROM gateway_routing_catalog_inbox WHERE gateway_id=$1 AND publication_id=$2`, gatewayID, published.Publication.ID).Scan(&status, &code)
		if err == nil {
			if status != routingcatalog.ReceiptRejected || code != "catalog_validation_hash_mismatch" {
				t.Fatalf("tampered receipt = %q / %q", status, code)
			}
			break
		}
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
	}
	drainReceiptCollector(t, ctx, service)
	publication, err := service.GetPublication(ctx, actor, published.Publication.ID)
	if err != nil || publication.Status != routingcatalog.PublicationFailed {
		t.Fatalf("tampered publication = %#v / %v", publication, err)
	}
}

func TestGatewayConsumerRejectsStaleCatalogRevision(t *testing.T) {
	ctx, db, service, actor := newIntegrationService(t)
	base := int64(0)
	if current, err := service.Current(ctx, actor); err == nil {
		base = current.Revision
	}
	id := fmt.Sprintf("rcd-stale-%d", time.Now().UnixNano())
	created, err := service.CreateDraft(ctx, actor, "create-"+id, routingcatalog.CreateDraftCommand{ID: id, BaseRevision: base, Document: validDocument()})
	if err != nil {
		t.Fatal(err)
	}
	validated, err := service.ValidateDraft(ctx, actor, routingcatalog.ValidateDraftCommand{DraftID: id, ExpectedRevision: created.Draft.Revision})
	if err != nil {
		t.Fatal(err)
	}
	published, err := service.PublishDraft(ctx, actor, "publish-"+id, routingcatalog.PublishDraftCommand{DraftID: id, ExpectedRevision: validated.Draft.Revision})
	if err != nil {
		t.Fatal(err)
	}
	compiler := routingcatalog.RuntimeCompilerFunc(func(context.Context, routingcatalog.Document) ([]provider.Route, error) {
		t.Fatal("stale Catalog reached runtime compilation")
		return nil, nil
	})
	router := provider.NewVersionedRouter(published.Revision.Revision+1, nil)
	gatewayID := fmt.Sprintf("gateway-stale-%d", time.Now().UnixNano())
	consumer, err := routingcatalog.NewConsumer(db, compiler, router, gatewayID, "us-west", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	for count := 0; count < 10_000; count++ {
		worked, err := consumer.RunNext(ctx)
		if err != nil || !worked {
			t.Fatalf("consume stale Catalog = %v / %v", worked, err)
		}
		var status, code string
		err = db.QueryRowContext(ctx, `SELECT status,error_code FROM gateway_routing_catalog_inbox WHERE gateway_id=$1 AND publication_id=$2`, gatewayID, published.Publication.ID).Scan(&status, &code)
		if err == nil {
			if status != routingcatalog.ReceiptRejected || code != "catalog_revision_stale" {
				t.Fatalf("stale receipt = %q / %q", status, code)
			}
			break
		}
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
	}
}

type failingPublishedRouter struct {
	revision int64
}

func (router *failingPublishedRouter) Revision() int64 {
	return router.revision
}

func (*failingPublishedRouter) ReplaceAt(int64, time.Time, []provider.Route) error {
	return errors.New("atomic route swap failed")
}

func TestGatewayConsumerDoesNotRecordAppliedBeforeRouterSwap(t *testing.T) {
	ctx, db, service, actor := newIntegrationService(t)
	base := int64(0)
	if current, err := service.Current(ctx, actor); err == nil {
		base = current.Revision
	}
	id := fmt.Sprintf("rcd-swap-failure-%d", time.Now().UnixNano())
	created, err := service.CreateDraft(ctx, actor, "create-"+id, routingcatalog.CreateDraftCommand{ID: id, BaseRevision: base, Document: validDocument()})
	if err != nil {
		t.Fatal(err)
	}
	validated, err := service.ValidateDraft(ctx, actor, routingcatalog.ValidateDraftCommand{DraftID: id, ExpectedRevision: created.Draft.Revision})
	if err != nil {
		t.Fatal(err)
	}
	published, err := service.PublishDraft(ctx, actor, "publish-"+id, routingcatalog.PublishDraftCommand{DraftID: id, ExpectedRevision: validated.Draft.Revision})
	if err != nil {
		t.Fatal(err)
	}
	compiler := routingcatalog.RuntimeCompilerFunc(func(context.Context, routingcatalog.Document) ([]provider.Route, error) {
		return []provider.Route{{ID: "compiled"}}, nil
	})
	gatewayID := fmt.Sprintf("gateway-swap-failure-%d", time.Now().UnixNano())
	consumer, err := routingcatalog.NewConsumer(db, compiler, &failingPublishedRouter{revision: base}, gatewayID, "us-west", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	for count := 0; count < 10_000; count++ {
		worked, runErr := consumer.RunNext(ctx)
		if runErr != nil || !worked {
			t.Fatalf("consume through swap failure = %v / %v", worked, runErr)
		}
		var status, code string
		err = db.QueryRowContext(ctx, `SELECT status,COALESCE(error_code,'') FROM gateway_routing_catalog_inbox
			WHERE gateway_id=$1 AND publication_id=$2`, gatewayID, published.Publication.ID).Scan(&status, &code)
		if err == nil {
			if status != routingcatalog.ReceiptRejected || code != "catalog_swap_failed" {
				t.Fatalf("swap failure receipt = %q / %q", status, code)
			}
			return
		}
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
	}
	t.Fatal("swap failure publication was not consumed")
}

func TestProviderCredentialEventRecompilesAffectedRoutesAtSameCatalogRevision(t *testing.T) {
	ctx, db, service, actor := newIntegrationService(t)
	base := int64(0)
	if current, err := service.Current(ctx, actor); err == nil {
		base = current.Revision
	}
	id := fmt.Sprintf("rcd-credential-%d", time.Now().UnixNano())
	created, err := service.CreateDraft(ctx, actor, "create-"+id, routingcatalog.CreateDraftCommand{ID: id, BaseRevision: base, Document: validDocument()})
	if err != nil {
		t.Fatal(err)
	}
	validated, err := service.ValidateDraft(ctx, actor, routingcatalog.ValidateDraftCommand{DraftID: id, ExpectedRevision: created.Draft.Revision})
	if err != nil {
		t.Fatal(err)
	}
	published, err := service.PublishDraft(ctx, actor, "publish-"+id, routingcatalog.PublishDraftCommand{DraftID: id, ExpectedRevision: validated.Draft.Revision})
	if err != nil {
		t.Fatal(err)
	}
	compileCount := 0
	compiler := routingcatalog.RuntimeCompilerFunc(func(_ context.Context, document routingcatalog.Document) ([]provider.Route, error) {
		compileCount++
		return []provider.Route{{
			ID: document.Routes[0].ID, Provider: fmt.Sprintf("credential-%d", compileCount), Model: document.Routes[0].PublicModel,
			Region: "us-west", HomeRegion: "us-west", Healthy: true,
			Profile: provider.CapabilityProfile{Revision: 1, Features: map[string]provider.CapabilitySupport{"text": provider.CapabilityNative}},
		}}, nil
	})
	router := provider.NewVersionedRouter(1, []provider.Route{{ID: "legacy", Provider: "legacy", Model: "legacy", Healthy: true, Profile: provider.CapabilityProfile{Features: map[string]provider.CapabilitySupport{"text": provider.CapabilityNative}}}})
	consumer, err := routingcatalog.NewConsumer(db, compiler, router, "gateway-credential-test", "us-west", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	for {
		worked, err := consumer.RunNext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			break
		}
	}
	before, err := router.Candidates(ctx, core.Request{TenantID: "tenant-a", Model: "gateway-model", HomeRegion: "us-west", RequestedFeatures: []string{"text"}})
	if err != nil {
		t.Fatal(err)
	}
	eventID := fmt.Sprintf("provider-event-%d", time.Now().UnixNano())
	if _, err := db.ExecContext(ctx, `INSERT INTO control_outbox (
		event_id,schema_version,aggregate_type,aggregate_id,aggregate_revision,tenant_id,event_type,occurred_at,payload
	) VALUES ($1,2,'ProviderConnection','pc-openai-us',$2,NULL,'ProviderCredentialRotated',$3,'{}'::jsonb)`,
		eventID, time.Now().UnixNano(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	worked, err := consumer.RunNext(ctx)
	if err != nil || !worked {
		t.Fatalf("consume credential rotation = %v / %v", worked, err)
	}
	after, err := router.Candidates(ctx, core.Request{TenantID: "tenant-a", Model: "gateway-model", HomeRegion: "us-west", RequestedFeatures: []string{"text"}})
	if err != nil || before[0].Provider == after[0].Provider || router.Revision() != published.Revision.Revision {
		t.Fatalf("routes before/after = %#v / %#v, revision=%d, err=%v", before, after, router.Revision(), err)
	}
}

func newIntegrationService(t *testing.T) (context.Context, *sql.DB, *routingcatalog.Service, tenantadmin.ActorEnvelope) {
	return newIntegrationServiceWithLookup(t, nil)
}

func newIntegrationServiceWithLookup(t *testing.T, lookup routingcatalog.ConnectionLookup) (context.Context, *sql.DB, *routingcatalog.Service, tenantadmin.ActorEnvelope) {
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
	if err := tenantadmin.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := routingcatalog.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if lookup == nil {
		lookup = routingcatalog.ConnectionLookupFunc(func(context.Context, string) (routingcatalog.ConnectionDescriptor, error) {
			return routingcatalog.ConnectionDescriptor{
				ID: "pc-openai-us", Provider: "openai", Region: "us-west", CredentialScope: "organization-a", Enabled: true,
				CapabilityProfile: provider.CapabilityProfile{Revision: 4, Features: map[string]provider.CapabilitySupport{
					"text": provider.CapabilityNative, "streaming": provider.CapabilityNative,
				}},
			}, nil
		})
	}
	service, err := routingcatalog.NewService(db, lookup, time.Now, nil)
	if err != nil {
		t.Fatal(err)
	}
	actor := tenantadmin.ActorEnvelope{Type: "human", ID: "routing-admin", Scopes: []string{tenantadmin.ScopePlatformWrite}, RequestID: "request-routing", Reason: "publish managed routes"}
	return ctx, db, service, actor
}
