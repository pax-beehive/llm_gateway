package routingcatalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/toddzheng/llm-gateway/internal/controlevent"
	"github.com/toddzheng/llm-gateway/internal/provider"
)

type RuntimeCompiler interface {
	CompileSnapshot(context.Context, Document) (CompiledSnapshot, error)
}

type RuntimeCompilerFunc func(context.Context, Document) ([]provider.Route, error)

type CompiledSnapshot struct {
	Routes         []provider.Route
	ValidationHash string
}

func (function RuntimeCompilerFunc) CompileSnapshot(ctx context.Context, document Document) (CompiledSnapshot, error) {
	routes, err := function(ctx, document)
	return CompiledSnapshot{Routes: routes, ValidationHash: validationHash(document, []ValidationIssue{}, []ValidationIssue{})}, err
}

type ManagedCompiler struct {
	resolver   RuntimeConnectionResolver
	httpClient *http.Client
}

func NewManagedCompiler(resolver RuntimeConnectionResolver, httpClient *http.Client) (*ManagedCompiler, error) {
	if resolver == nil {
		return nil, errors.New("managed Routing Catalog compiler requires Provider Connection resolution")
	}
	return &ManagedCompiler{resolver: resolver, httpClient: httpClient}, nil
}

func (compiler *ManagedCompiler) Compile(ctx context.Context, document Document) ([]provider.Route, error) {
	return Compile(ctx, document, compiler.resolver, compiler.httpClient)
}

func (compiler *ManagedCompiler) CompileSnapshot(ctx context.Context, document Document) (CompiledSnapshot, error) {
	routes, report, err := compileManaged(ctx, document, compiler.resolver, compiler.httpClient, true)
	return CompiledSnapshot{Routes: routes, ValidationHash: report.Hash}, err
}

type PublishedRouter interface {
	Revision() int64
	ReplaceAt(int64, time.Time, []provider.Route) error
}

type Consumer struct {
	database  *sql.DB
	compiler  RuntimeCompiler
	router    PublishedRouter
	gatewayID string
	region    string
	now       func() time.Time
}

func NewConsumer(database *sql.DB, compiler RuntimeCompiler, router PublishedRouter, gatewayID, region string, now func() time.Time) (*Consumer, error) {
	if database == nil || compiler == nil || router == nil || !managedResourceIDPattern.MatchString(gatewayID) || strings.TrimSpace(region) == "" {
		return nil, errors.New("Routing Catalog consumer requires PostgreSQL, compiler, router, Gateway ID, and region")
	}
	if now == nil {
		now = time.Now
	}
	return &Consumer{database: database, compiler: compiler, router: router, gatewayID: gatewayID, region: region, now: now}, nil
}

type publishedEvent struct {
	EventID        string
	Revision       int64
	OccurredAt     time.Time
	PublicationID  string
	ValidationHash string
	Document       Document
	InvalidCode    string
}

func (consumer *Consumer) RunNext(ctx context.Context) (bool, error) {
	event, err := consumer.nextEvent(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return consumer.runConnectionEvent(ctx)
	}
	if err != nil {
		return false, err
	}
	return true, consumer.applyPublishedEvent(ctx, event)
}

func (consumer *Consumer) applyPublishedEvent(ctx context.Context, event publishedEvent) error {
	if event.Revision < consumer.router.Revision() {
		if err := consumer.recordRejected(ctx, event, "catalog_revision_stale"); err != nil {
			return err
		}
		return nil
	}
	if event.InvalidCode != "" {
		if err := consumer.recordRejected(ctx, event, event.InvalidCode); err != nil {
			return err
		}
		return nil
	}
	compiled, compileErr := consumer.compiler.CompileSnapshot(ctx, event.Document)
	if compileErr != nil {
		if errors.Is(compileErr, controlevent.ErrExecutionSecretUnavailable) {
			return compileErr
		}
		if err := consumer.recordRejected(ctx, event, "catalog_compile_failed"); err != nil {
			return err
		}
		return nil
	}
	if event.Revision >= consumer.router.Revision() {
		if err := consumer.router.ReplaceAt(event.Revision, event.OccurredAt, compiled.Routes); err != nil {
			if recordErr := consumer.recordRejected(ctx, event, "catalog_swap_failed"); recordErr != nil {
				return recordErr
			}
			return nil
		}
	}
	// An applied receipt is authoritative rollout evidence, so it must never
	// become visible before the in-process router has installed the snapshot.
	// Persistence after an equal-revision swap is retry-safe.
	if err := consumer.recordApplied(ctx, event); err != nil {
		return err
	}
	return nil
}

type providerConnectionEvent struct {
	EventID      string
	ConnectionID string
	Revision     int64
	EventType    string
	Region       string
}

func (consumer *Consumer) runConnectionEvent(ctx context.Context) (bool, error) {
	var event providerConnectionEvent
	err := consumer.database.QueryRowContext(ctx, `SELECT o.event_id,
		CASE WHEN o.aggregate_type='ProviderConnection' THEN o.aggregate_id ELSE o.payload->>'connection_id' END,
		CASE WHEN o.aggregate_type='ProviderConnection' THEN o.aggregate_revision ELSE (o.payload->>'connection_revision')::bigint END,
		o.event_type
		FROM control_outbox o WHERE ((
			o.aggregate_type='ProviderConnection'
			AND o.event_type IN ('ProviderConnectionChanged','ProviderConnectionEnabled','ProviderConnectionDisabled','ProviderCredentialRotated')
		) OR (
			o.aggregate_type='ProviderOperation' AND o.event_type='ProviderConnectionHealthObserved'
		))
		AND NOT EXISTS (SELECT 1 FROM gateway_provider_connection_inbox i WHERE i.gateway_id=$1 AND i.event_id=o.event_id)
		ORDER BY o.occurred_at,o.event_id LIMIT 1`, consumer.gatewayID).Scan(&event.EventID, &event.ConnectionID, &event.Revision, &event.EventType)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, consumer.applyConnectionEvent(ctx, event)
}

func (consumer *Consumer) applyConnectionEvent(ctx context.Context, event providerConnectionEvent) error {
	projected, err := LoadProjected(ctx, consumer.database)
	if errors.Is(err, ErrNotFound) {
		return consumer.recordConnectionInbox(ctx, event, "ignored", "")
	}
	if err != nil {
		return err
	}
	affected := false
	for _, route := range projected.Document.Routes {
		if route.ProviderConnectionID == event.ConnectionID {
			affected = true
			break
		}
	}
	if !affected {
		return consumer.recordConnectionInbox(ctx, event, "ignored", "")
	}
	document := projected.Document
	if event.EventType == "ProviderConnectionDisabled" || event.Region != "" && event.Region != consumer.region {
		filtered := make([]ManagedRoute, 0, len(document.Routes))
		for _, route := range document.Routes {
			if route.ProviderConnectionID != event.ConnectionID {
				filtered = append(filtered, route)
			}
		}
		document.Routes = filtered
	}
	var routes []provider.Route
	var compileErr error
	if len(document.Routes) > 0 {
		compiled, err := consumer.compiler.CompileSnapshot(ctx, document)
		routes, compileErr = compiled.Routes, err
	}
	if compileErr != nil {
		if errors.Is(compileErr, controlevent.ErrExecutionSecretUnavailable) {
			return compileErr
		}
		return consumer.recordConnectionInbox(ctx, event, "rejected", "provider_connection_recompile_failed")
	}
	if err := consumer.router.ReplaceAt(projected.Revision, consumer.now().UTC(), routes); err != nil {
		return err
	}
	return consumer.recordConnectionInbox(ctx, event, "applied", "")
}

// Consume applies an externally relayed event without reading the control-plane
// database. Unsupported event types are intentionally ignored so one ordered
// stream can feed multiple local projections.
func (consumer *Consumer) Consume(ctx context.Context, envelope controlevent.Event) error {
	switch {
	case envelope.AggregateType == "RoutingCatalog" && envelope.EventType == "RoutingCatalogPublished":
		if envelope.SchemaVersion != 1 {
			return errors.New("unsupported Routing Catalog event schema")
		}
		event := publishedEvent{EventID: envelope.EventID, Revision: envelope.AggregateRevision, OccurredAt: envelope.OccurredAt, PublicationID: envelope.AggregateID}
		consumer.decodePublishedEvent(&event, envelope.AggregateID, envelope.Payload)
		return consumer.applyPublishedEvent(ctx, event)
	case envelope.AggregateType == "ProviderConnection":
		var payload struct {
			ConnectionID string `json:"connection_id"`
			Region       string `json:"region"`
			Revision     int64  `json:"revision"`
		}
		if envelope.SchemaVersion != 3 || json.Unmarshal(envelope.Payload, &payload) != nil || payload.ConnectionID != envelope.AggregateID || payload.Revision != envelope.AggregateRevision {
			return errors.New("invalid Provider Connection event")
		}
		return consumer.applyConnectionEvent(ctx, providerConnectionEvent{EventID: envelope.EventID, ConnectionID: payload.ConnectionID, Revision: payload.Revision, EventType: envelope.EventType, Region: payload.Region})
	case envelope.AggregateType == "ProviderOperation" && envelope.EventType == "ProviderConnectionHealthObserved":
		var payload struct {
			ConnectionID       string `json:"connection_id"`
			ConnectionRevision int64  `json:"connection_revision"`
		}
		if envelope.SchemaVersion != 2 || json.Unmarshal(envelope.Payload, &payload) != nil || payload.ConnectionID == "" || payload.ConnectionRevision <= 0 {
			return errors.New("invalid Provider Connection health event")
		}
		return consumer.applyConnectionEvent(ctx, providerConnectionEvent{EventID: envelope.EventID, ConnectionID: payload.ConnectionID, Revision: payload.ConnectionRevision, EventType: envelope.EventType})
	default:
		return nil
	}
}

// ReplaceSnapshot installs an authoritative Routing Catalog bootstrap without
// manufacturing a rollout receipt. The startup caller advances the shared
// relay cursor only after all bootstrap projections succeed.
func (consumer *Consumer) ReplaceSnapshot(ctx context.Context, revision Revision) error {
	if revision.Revision <= 0 || revision.ValidationHash == "" || revision.CreatedAt.IsZero() {
		return ErrInvalidArgument
	}
	validationErrors := revision.Validation.Errors
	if validationErrors == nil {
		validationErrors = []ValidationIssue{}
	}
	validationWarnings := revision.Validation.Warnings
	if validationWarnings == nil {
		validationWarnings = []ValidationIssue{}
	}
	if !revision.Validation.Valid || len(validationErrors) != 0 ||
		validationHash(revision.Document, validationErrors, validationWarnings) != revision.ValidationHash ||
		revision.Validation.Hash != revision.ValidationHash {
		return errors.New("Routing Catalog bootstrap validation evidence mismatch")
	}
	compiled, err := consumer.compiler.CompileSnapshot(ctx, revision.Document)
	if err != nil {
		return err
	}
	if revision.Revision < consumer.router.Revision() {
		return errors.New("Routing Catalog bootstrap revision is stale")
	}
	if err := consumer.router.ReplaceAt(revision.Revision, revision.CreatedAt, compiled.Routes); err != nil {
		return err
	}
	document, err := json.Marshal(revision.Document)
	if err != nil {
		return err
	}
	transaction, err := consumer.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	if _, err := transaction.ExecContext(ctx, `INSERT INTO gateway_routing_catalog_history (
		revision,publication_id,document,validation_hash,applied_at
	) VALUES ($1,$2,$3,$4,$5) ON CONFLICT (revision) DO NOTHING`, revision.Revision,
		fmt.Sprintf("bootstrap-%d", revision.Revision), document, revision.ValidationHash, consumer.now().UTC()); err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx, `INSERT INTO gateway_routing_catalog_head (singleton,revision,updated_at)
		VALUES (true,$1,$2) ON CONFLICT (singleton) DO UPDATE SET revision=EXCLUDED.revision,updated_at=EXCLUDED.updated_at
		WHERE gateway_routing_catalog_head.revision<=EXCLUDED.revision`, revision.Revision, consumer.now().UTC()); err != nil {
		return err
	}
	return transaction.Commit()
}

func (consumer *Consumer) recordConnectionInbox(ctx context.Context, event providerConnectionEvent, status, errorCode string) error {
	_, err := consumer.database.ExecContext(ctx, `INSERT INTO gateway_provider_connection_inbox (
		gateway_id,event_id,connection_id,connection_revision,status,error_code,observed_at
	) VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),$7) ON CONFLICT (gateway_id,event_id) DO NOTHING`,
		consumer.gatewayID, event.EventID, event.ConnectionID, event.Revision, status, errorCode, consumer.now().UTC())
	return err
}

func (consumer *Consumer) nextEvent(ctx context.Context) (publishedEvent, error) {
	var event publishedEvent
	var payload []byte
	var aggregateID string
	err := consumer.database.QueryRowContext(ctx, `SELECT o.event_id,o.aggregate_id,o.aggregate_revision,o.occurred_at,o.payload
		FROM control_outbox o WHERE o.event_type='RoutingCatalogPublished'
		AND NOT EXISTS (SELECT 1 FROM gateway_routing_catalog_inbox i WHERE i.gateway_id=$1 AND i.event_id=o.event_id)
		ORDER BY o.aggregate_revision,o.occurred_at,o.event_id LIMIT 1`, consumer.gatewayID).Scan(&event.EventID, &aggregateID, &event.Revision, &event.OccurredAt, &payload)
	if err != nil {
		return publishedEvent{}, err
	}
	consumer.decodePublishedEvent(&event, aggregateID, payload)
	return event, nil
}

func (consumer *Consumer) decodePublishedEvent(event *publishedEvent, aggregateID string, payload []byte) {
	event.PublicationID = aggregateID
	var decoded struct {
		PublicationID   string           `json:"publication_id"`
		CatalogRevision int64            `json:"catalog_revision"`
		ValidationHash  string           `json:"validation_hash"`
		Validation      ValidationReport `json:"validation_report"`
		Document        Document         `json:"document"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		event.InvalidCode = "catalog_event_invalid"
		return
	}
	if decoded.Validation.Errors == nil {
		decoded.Validation.Errors = []ValidationIssue{}
	}
	if decoded.Validation.Warnings == nil {
		decoded.Validation.Warnings = []ValidationIssue{}
	}
	computedValidationHash := validationHash(decoded.Document, decoded.Validation.Errors, decoded.Validation.Warnings)
	if decoded.CatalogRevision != event.Revision || !managedResourceIDPattern.MatchString(decoded.PublicationID) ||
		decoded.ValidationHash == "" ||
		aggregateID != "global" && aggregateID != decoded.PublicationID {
		event.InvalidCode = "catalog_event_identity_invalid"
		return
	}
	if computedValidationHash != decoded.ValidationHash {
		event.InvalidCode = "catalog_validation_hash_mismatch"
		return
	}
	event.PublicationID = decoded.PublicationID
	event.ValidationHash = decoded.ValidationHash
	event.Document = decoded.Document
}

func (consumer *Consumer) recordApplied(ctx context.Context, event publishedEvent) error {
	transaction, err := consumer.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	if _, err := transaction.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('gateway_routing_catalog_head',0))`); err != nil {
		return err
	}
	var current int64
	err = transaction.QueryRowContext(ctx, `SELECT revision FROM gateway_routing_catalog_head WHERE singleton=true FOR UPDATE`).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		current = 0
	} else if err != nil {
		return err
	}
	if event.Revision > current {
		document, _ := json.Marshal(event.Document)
		if _, err := transaction.ExecContext(ctx, `INSERT INTO gateway_routing_catalog_history (
			revision,publication_id,document,validation_hash,applied_at
		) VALUES ($1,$2,$3,$4,$5) ON CONFLICT (revision) DO NOTHING`, event.Revision, event.PublicationID, document, event.ValidationHash, consumer.now().UTC()); err != nil {
			return err
		}
		if _, err := transaction.ExecContext(ctx, `INSERT INTO gateway_routing_catalog_head (singleton,revision,updated_at)
			VALUES (true,$1,$2) ON CONFLICT (singleton) DO UPDATE SET revision=EXCLUDED.revision,updated_at=EXCLUDED.updated_at`, event.Revision, consumer.now().UTC()); err != nil {
			return err
		}
	}
	if err := consumer.recordInbox(ctx, transaction, event, ReceiptApplied, ""); err != nil {
		return err
	}
	return transaction.Commit()
}

func (consumer *Consumer) recordRejected(ctx context.Context, event publishedEvent, errorCode string) error {
	transaction, err := consumer.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	if err := consumer.recordInbox(ctx, transaction, event, ReceiptRejected, errorCode); err != nil {
		return err
	}
	return transaction.Commit()
}

func (consumer *Consumer) recordInbox(ctx context.Context, transaction *sql.Tx, event publishedEvent, status, errorCode string) error {
	observedAt := consumer.now().UTC()
	if _, err := transaction.ExecContext(ctx, `INSERT INTO gateway_routing_catalog_inbox (
		gateway_id,region,event_id,catalog_revision,publication_id,status,error_code,observed_at
	) VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8) ON CONFLICT (gateway_id,event_id) DO NOTHING`,
		consumer.gatewayID, consumer.region, event.EventID, event.Revision, event.PublicationID, status, errorCode, observedAt); err != nil {
		return err
	}
	return nil
}

func LoadProjected(ctx context.Context, database *sql.DB) (Revision, error) {
	if database == nil {
		return Revision{}, errors.New("Routing Catalog projection load requires PostgreSQL")
	}
	var revision Revision
	var document []byte
	err := database.QueryRowContext(ctx, `SELECT r.revision,r.document,r.validation_hash,r.applied_at
		FROM gateway_routing_catalog_head h JOIN gateway_routing_catalog_history r ON r.revision=h.revision
		WHERE h.singleton=true`).Scan(&revision.Revision, &document, &revision.ValidationHash, &revision.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Revision{}, ErrNotFound
	}
	if err != nil {
		return Revision{}, err
	}
	if err := json.Unmarshal(document, &revision.Document); err != nil {
		return Revision{}, err
	}
	return revision, nil
}
