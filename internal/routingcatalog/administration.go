package routingcatalog

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

const (
	createDraftOperation  = "routing_catalog.create_draft"
	updateDraftOperation  = "routing_catalog.update_draft"
	publishDraftOperation = "routing_catalog.publish_draft"
	restoreOperation      = "routing_catalog.restore"
)

type Service struct {
	database    *sql.DB
	connections ConnectionLookup
	now         func() time.Time
	random      io.Reader
}

func NewService(database *sql.DB, connections ConnectionLookup, now func() time.Time, random io.Reader) (*Service, error) {
	if database == nil || connections == nil {
		return nil, errors.New("Routing Catalog Administration requires PostgreSQL and Provider Connection lookup")
	}
	if now == nil {
		now = time.Now
	}
	if random == nil {
		random = rand.Reader
	}
	return &Service{database: database, connections: connections, now: now, random: random}, nil
}

func (service *Service) CreateDraft(ctx context.Context, actor tenantadmin.ActorEnvelope, idempotencyKey string, command CreateDraftCommand) (DraftResult, error) {
	if err := authorizeCatalogWrite(actor); err != nil {
		return DraftResult{}, err
	}
	if !managedResourceIDPattern.MatchString(command.ID) || command.BaseRevision < 0 || len(command.Document.Routes) == 0 {
		return DraftResult{}, ErrInvalidArgument
	}
	requestHash, err := catalogCommandHash(command, actor.Reason)
	if err != nil {
		return DraftResult{}, err
	}
	transaction, replay, err := service.beginCommand(ctx, actor, createDraftOperation, idempotencyKey, requestHash)
	if err != nil {
		return DraftResult{}, err
	}
	if replay != nil {
		var result DraftResult
		if err := json.Unmarshal(replay, &result); err != nil {
			return DraftResult{}, err
		}
		result.Replay = true
		return result, nil
	}
	defer func() { _ = transaction.Rollback() }()
	document, err := json.Marshal(command.Document)
	if err != nil {
		return DraftResult{}, ErrInvalidArgument
	}
	now := service.now().UTC()
	_, err = transaction.ExecContext(ctx, `INSERT INTO routing_catalog_drafts (
		id,base_revision,document,status,revision,created_by,updated_by,created_at,updated_at
	) VALUES ($1,$2,$3,'draft',1,$4,$4,$5,$5)`, command.ID, command.BaseRevision, document, actor.ID, now)
	if err != nil {
		return DraftResult{}, mapCatalogDatabaseError(err)
	}
	draft, err := getDraftTx(ctx, transaction, command.ID, false)
	if err != nil {
		return DraftResult{}, err
	}
	result := DraftResult{Draft: draft}
	if err := service.recordDraftAudit(ctx, transaction, actor, draft, "RoutingCatalogDraftCreated"); err != nil {
		return DraftResult{}, err
	}
	if err := recordCatalogCommand(ctx, transaction, actor, createDraftOperation, idempotencyKey, requestHash, result); err != nil {
		return DraftResult{}, err
	}
	if err := transaction.Commit(); err != nil {
		return DraftResult{}, err
	}
	return result, nil
}

func (service *Service) ValidateDraft(ctx context.Context, actor tenantadmin.ActorEnvelope, command ValidateDraftCommand) (DraftResult, error) {
	if err := authorizeCatalogWrite(actor); err != nil {
		return DraftResult{}, err
	}
	if !managedResourceIDPattern.MatchString(command.DraftID) || command.ExpectedRevision <= 0 {
		return DraftResult{}, ErrInvalidArgument
	}
	transaction, err := service.database.BeginTx(ctx, nil)
	if err != nil {
		return DraftResult{}, err
	}
	defer func() { _ = transaction.Rollback() }()
	draft, err := getDraftTx(ctx, transaction, command.DraftID, true)
	if err != nil {
		return DraftResult{}, err
	}
	if draft.Revision != command.ExpectedRevision {
		return DraftResult{}, ErrRevisionConflict
	}
	report := Validate(ctx, draft.Document, service.connections)
	encoded, err := json.Marshal(report)
	if err != nil {
		return DraftResult{}, err
	}
	status := DraftOpen
	if report.Valid {
		status = DraftValidated
	}
	now := service.now().UTC()
	if _, err := transaction.ExecContext(ctx, `UPDATE routing_catalog_drafts SET
		status=$1,revision=revision+1,validation_report=$2,validation_hash=$3,updated_by=$4,updated_at=$5
		WHERE id=$6`, status, encoded, report.Hash, actor.ID, now, command.DraftID); err != nil {
		return DraftResult{}, err
	}
	draft, err = getDraftTx(ctx, transaction, command.DraftID, false)
	if err != nil {
		return DraftResult{}, err
	}
	if err := service.recordDraftAudit(ctx, transaction, actor, draft, "RoutingCatalogDraftValidated"); err != nil {
		return DraftResult{}, err
	}
	if err := transaction.Commit(); err != nil {
		return DraftResult{}, err
	}
	return DraftResult{Draft: draft}, nil
}

func (service *Service) UpdateDraft(ctx context.Context, actor tenantadmin.ActorEnvelope, idempotencyKey string, command UpdateDraftCommand) (DraftResult, error) {
	if err := authorizeCatalogWrite(actor); err != nil {
		return DraftResult{}, err
	}
	if !managedResourceIDPattern.MatchString(command.DraftID) || command.ExpectedRevision <= 0 || len(command.Document.Routes) == 0 {
		return DraftResult{}, ErrInvalidArgument
	}
	requestHash, err := catalogCommandHash(command, actor.Reason)
	if err != nil {
		return DraftResult{}, err
	}
	transaction, replay, err := service.beginCommand(ctx, actor, updateDraftOperation, idempotencyKey, requestHash)
	if err != nil {
		return DraftResult{}, err
	}
	if replay != nil {
		var result DraftResult
		if err := json.Unmarshal(replay, &result); err != nil {
			return DraftResult{}, err
		}
		result.Replay = true
		return result, nil
	}
	defer func() { _ = transaction.Rollback() }()
	draft, err := getDraftTx(ctx, transaction, command.DraftID, true)
	if err != nil {
		return DraftResult{}, err
	}
	if draft.Revision != command.ExpectedRevision {
		return DraftResult{}, ErrRevisionConflict
	}
	document, err := json.Marshal(command.Document)
	if err != nil {
		return DraftResult{}, ErrInvalidArgument
	}
	now := service.now().UTC()
	if _, err := transaction.ExecContext(ctx, `UPDATE routing_catalog_drafts SET
		document=$1,status='draft',revision=revision+1,validation_report=NULL,validation_hash=NULL,updated_by=$2,updated_at=$3
		WHERE id=$4`, document, actor.ID, now, command.DraftID); err != nil {
		return DraftResult{}, err
	}
	draft, err = getDraftTx(ctx, transaction, command.DraftID, false)
	if err != nil {
		return DraftResult{}, err
	}
	result := DraftResult{Draft: draft}
	if err := service.recordDraftAudit(ctx, transaction, actor, draft, "RoutingCatalogDraftUpdated"); err != nil {
		return DraftResult{}, err
	}
	if err := recordCatalogCommand(ctx, transaction, actor, updateDraftOperation, idempotencyKey, requestHash, result); err != nil {
		return DraftResult{}, err
	}
	if err := transaction.Commit(); err != nil {
		return DraftResult{}, err
	}
	return result, nil
}

func (service *Service) PublishDraft(ctx context.Context, actor tenantadmin.ActorEnvelope, idempotencyKey string, command PublishDraftCommand) (PublicationResult, error) {
	if err := authorizeCatalogWrite(actor); err != nil {
		return PublicationResult{}, err
	}
	if !managedResourceIDPattern.MatchString(command.DraftID) || command.ExpectedRevision <= 0 || !validRegions(command.RequiredRegions) {
		return PublicationResult{}, ErrInvalidArgument
	}
	requestHash, err := catalogCommandHash(command, actor.Reason)
	if err != nil {
		return PublicationResult{}, err
	}
	transaction, replay, err := service.beginCommand(ctx, actor, publishDraftOperation, idempotencyKey, requestHash)
	if err != nil {
		return PublicationResult{}, err
	}
	if replay != nil {
		var result PublicationResult
		if err := json.Unmarshal(replay, &result); err != nil {
			return PublicationResult{}, err
		}
		result.Replay = true
		return result, nil
	}
	defer func() { _ = transaction.Rollback() }()
	if _, err := transaction.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('routing_catalog_head',0))`); err != nil {
		return PublicationResult{}, err
	}
	draft, err := getDraftTx(ctx, transaction, command.DraftID, true)
	if err != nil {
		return PublicationResult{}, err
	}
	if draft.Revision != command.ExpectedRevision {
		return PublicationResult{}, ErrRevisionConflict
	}
	if draft.Status != DraftValidated || !draft.Validation.Valid || draft.ValidationHash == "" {
		return PublicationResult{}, ErrValidationFailed
	}
	currentValidation := Validate(ctx, draft.Document, service.connections)
	if !currentValidation.Valid || currentValidation.Hash != draft.ValidationHash {
		return PublicationResult{}, ErrValidationFailed
	}
	currentRevision, err := currentRevisionTx(ctx, transaction)
	if err != nil {
		return PublicationResult{}, err
	}
	if currentRevision != draft.BaseRevision {
		return PublicationResult{}, ErrRevisionConflict
	}
	nextRevision := currentRevision + 1
	now := service.now().UTC()
	document, _ := json.Marshal(draft.Document)
	validation, _ := json.Marshal(draft.Validation)
	if _, err := transaction.ExecContext(ctx, `INSERT INTO routing_catalog_revisions (
		revision,document,validation_report,validation_hash,source_revision,created_by,created_at
	) VALUES ($1,$2,$3,$4,NULL,$5,$6)`, nextRevision, document, validation, draft.ValidationHash, actor.ID, now); err != nil {
		return PublicationResult{}, mapCatalogDatabaseError(err)
	}
	if _, err := transaction.ExecContext(ctx, `INSERT INTO routing_catalog_head (singleton,revision,updated_at)
		VALUES (true,$1,$2) ON CONFLICT (singleton) DO UPDATE SET revision=EXCLUDED.revision,updated_at=EXCLUDED.updated_at`, nextRevision, now); err != nil {
		return PublicationResult{}, err
	}
	publicationID, err := catalogID(service.random, "rpub")
	if err != nil {
		return PublicationResult{}, err
	}
	if command.RequiredRegions == nil {
		command.RequiredRegions = []string{}
	}
	if _, err := transaction.ExecContext(ctx, `INSERT INTO routing_publications (
		id,catalog_revision,status,validation_hash,required_regions,created_by,created_at,updated_at
	) VALUES ($1,$2,'published',$3,$4,$5,$6,$6)`, publicationID, nextRevision, draft.ValidationHash, command.RequiredRegions, actor.ID, now); err != nil {
		return PublicationResult{}, err
	}
	revision := Revision{Revision: nextRevision, Document: draft.Document, Validation: draft.Validation, ValidationHash: draft.ValidationHash, CreatedBy: actor.ID, CreatedAt: now}
	publication := Publication{ID: publicationID, CatalogRevision: nextRevision, Status: PublicationPublished, ValidationHash: draft.ValidationHash, RequiredRegions: append([]string(nil), command.RequiredRegions...), CreatedBy: actor.ID, CreatedAt: now, UpdatedAt: now}
	result := PublicationResult{Revision: revision, Publication: publication}
	if err := service.recordPublication(ctx, transaction, actor, result); err != nil {
		return PublicationResult{}, err
	}
	if err := recordCatalogCommand(ctx, transaction, actor, publishDraftOperation, idempotencyKey, requestHash, result); err != nil {
		return PublicationResult{}, err
	}
	if err := transaction.Commit(); err != nil {
		return PublicationResult{}, err
	}
	return result, nil
}

func (service *Service) Current(ctx context.Context, actor tenantadmin.ActorEnvelope) (Revision, error) {
	if err := authorizeCatalogRead(actor); err != nil {
		return Revision{}, err
	}
	return scanRevision(service.database.QueryRowContext(ctx, `SELECT r.revision,r.document,r.validation_report,r.validation_hash,
		r.source_revision,r.created_by,r.created_at FROM routing_catalog_head h
		JOIN routing_catalog_revisions r ON r.revision=h.revision WHERE h.singleton=true`))
}

func (service *Service) GetDraft(ctx context.Context, actor tenantadmin.ActorEnvelope, draftID string) (Draft, error) {
	if err := authorizeCatalogRead(actor); err != nil {
		return Draft{}, err
	}
	if !managedResourceIDPattern.MatchString(draftID) {
		return Draft{}, ErrInvalidArgument
	}
	return scanDraft(service.database.QueryRowContext(ctx, `SELECT id,base_revision,document,status,revision,validation_report,validation_hash,
		created_by,updated_by,created_at,updated_at FROM routing_catalog_drafts WHERE id=$1`, draftID))
}

func (service *Service) GetRevision(ctx context.Context, actor tenantadmin.ActorEnvelope, revision int64) (Revision, error) {
	if err := authorizeCatalogRead(actor); err != nil {
		return Revision{}, err
	}
	if revision <= 0 {
		return Revision{}, ErrInvalidArgument
	}
	return scanRevision(service.database.QueryRowContext(ctx, `SELECT revision,document,validation_report,validation_hash,
		source_revision,created_by,created_at FROM routing_catalog_revisions WHERE revision=$1`, revision))
}

func (service *Service) ListRevisions(ctx context.Context, actor tenantadmin.ActorEnvelope, cursor int64, limit int) (RevisionPage, error) {
	if err := authorizeCatalogRead(actor); err != nil {
		return RevisionPage{}, err
	}
	if cursor < 0 || limit < 0 || limit > 200 {
		return RevisionPage{}, ErrInvalidArgument
	}
	if limit == 0 {
		limit = 50
	}
	rows, err := service.database.QueryContext(ctx, `SELECT revision,document,validation_report,validation_hash,
		source_revision,created_by,created_at FROM routing_catalog_revisions
		WHERE ($1::bigint=0 OR revision < $1) ORDER BY revision DESC LIMIT $2`, cursor, limit+1)
	if err != nil {
		return RevisionPage{}, err
	}
	defer rows.Close()
	page := RevisionPage{Data: make([]Revision, 0, limit)}
	for rows.Next() {
		revision, err := scanRevision(rows)
		if err != nil {
			return RevisionPage{}, err
		}
		if len(page.Data) == limit {
			page.NextCursor = revision.Revision + 1
			break
		}
		page.Data = append(page.Data, revision)
	}
	return page, rows.Err()
}

func (service *Service) GetPublication(ctx context.Context, actor tenantadmin.ActorEnvelope, publicationID string) (Publication, error) {
	if err := authorizeCatalogRead(actor); err != nil {
		return Publication{}, err
	}
	if !managedResourceIDPattern.MatchString(publicationID) {
		return Publication{}, ErrInvalidArgument
	}
	publication, err := scanPublication(service.database.QueryRowContext(ctx, `SELECT id,catalog_revision,status,validation_hash,
		to_json(required_regions),created_by,created_at,updated_at FROM routing_publications WHERE id=$1`, publicationID))
	if err != nil {
		return Publication{}, err
	}
	return appendPublicationReceipts(ctx, service.database, publication)
}

func (service *Service) Restore(ctx context.Context, actor tenantadmin.ActorEnvelope, idempotencyKey string, command RestoreCommand) (PublicationResult, error) {
	if err := authorizeCatalogWrite(actor); err != nil {
		return PublicationResult{}, err
	}
	if command.SourceRevision <= 0 || command.ExpectedHead < 0 || !validRegions(command.RequiredRegions) {
		return PublicationResult{}, ErrInvalidArgument
	}
	requestHash, err := catalogCommandHash(command, actor.Reason)
	if err != nil {
		return PublicationResult{}, err
	}
	transaction, replay, err := service.beginCommand(ctx, actor, restoreOperation, idempotencyKey, requestHash)
	if err != nil {
		return PublicationResult{}, err
	}
	if replay != nil {
		var result PublicationResult
		if err := json.Unmarshal(replay, &result); err != nil {
			return PublicationResult{}, err
		}
		result.Replay = true
		return result, nil
	}
	defer func() { _ = transaction.Rollback() }()
	if _, err := transaction.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('routing_catalog_head',0))`); err != nil {
		return PublicationResult{}, err
	}
	currentRevision, err := currentRevisionTx(ctx, transaction)
	if err != nil {
		return PublicationResult{}, err
	}
	if currentRevision != command.ExpectedHead {
		return PublicationResult{}, ErrRevisionConflict
	}
	source, err := scanRevision(transaction.QueryRowContext(ctx, `SELECT revision,document,validation_report,validation_hash,
		source_revision,created_by,created_at FROM routing_catalog_revisions WHERE revision=$1`, command.SourceRevision))
	if err != nil {
		return PublicationResult{}, err
	}
	currentValidation := Validate(ctx, source.Document, service.connections)
	if !currentValidation.Valid {
		return PublicationResult{}, ErrValidationFailed
	}
	nextRevision := currentRevision + 1
	now := service.now().UTC()
	document, _ := json.Marshal(source.Document)
	validation, _ := json.Marshal(currentValidation)
	if _, err := transaction.ExecContext(ctx, `INSERT INTO routing_catalog_revisions (
		revision,document,validation_report,validation_hash,source_revision,created_by,created_at
	) VALUES ($1,$2,$3,$4,$5,$6,$7)`, nextRevision, document, validation, currentValidation.Hash, source.Revision, actor.ID, now); err != nil {
		return PublicationResult{}, mapCatalogDatabaseError(err)
	}
	if _, err := transaction.ExecContext(ctx, `UPDATE routing_catalog_head SET revision=$1,updated_at=$2 WHERE singleton=true`, nextRevision, now); err != nil {
		return PublicationResult{}, err
	}
	publicationID, err := catalogID(service.random, "rpub")
	if err != nil {
		return PublicationResult{}, err
	}
	if command.RequiredRegions == nil {
		command.RequiredRegions = []string{}
	}
	if _, err := transaction.ExecContext(ctx, `INSERT INTO routing_publications (
		id,catalog_revision,status,validation_hash,required_regions,created_by,created_at,updated_at
	) VALUES ($1,$2,'published',$3,$4,$5,$6,$6)`, publicationID, nextRevision, currentValidation.Hash, command.RequiredRegions, actor.ID, now); err != nil {
		return PublicationResult{}, err
	}
	revision := Revision{Revision: nextRevision, Document: source.Document, Validation: currentValidation, ValidationHash: currentValidation.Hash, SourceRevision: source.Revision, CreatedBy: actor.ID, CreatedAt: now}
	publication := Publication{ID: publicationID, CatalogRevision: nextRevision, Status: PublicationPublished, ValidationHash: currentValidation.Hash, RequiredRegions: append([]string(nil), command.RequiredRegions...), CreatedBy: actor.ID, CreatedAt: now, UpdatedAt: now}
	result := PublicationResult{Revision: revision, Publication: publication}
	if err := service.recordPublication(ctx, transaction, actor, result); err != nil {
		return PublicationResult{}, err
	}
	if err := recordCatalogCommand(ctx, transaction, actor, restoreOperation, idempotencyKey, requestHash, result); err != nil {
		return PublicationResult{}, err
	}
	if err := transaction.Commit(); err != nil {
		return PublicationResult{}, err
	}
	return result, nil
}

func (service *Service) beginCommand(ctx context.Context, actor tenantadmin.ActorEnvelope, operation, idempotencyKey string, requestHash []byte) (*sql.Tx, []byte, error) {
	if strings.TrimSpace(idempotencyKey) == "" || len(idempotencyKey) > 200 {
		return nil, nil, ErrInvalidArgument
	}
	transaction, err := service.database.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	identity := strings.Join([]string{actor.Type, actor.ID, operation, idempotencyKey}, "\x1f")
	if _, err := transaction.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, identity); err != nil {
		_ = transaction.Rollback()
		return nil, nil, err
	}
	var storedHash, result []byte
	err = transaction.QueryRowContext(ctx, `SELECT request_hash,result FROM control_command_idempotency
		WHERE actor_type=$1 AND actor_id=$2 AND operation=$3 AND idempotency_key=$4`, actor.Type, actor.ID, operation, idempotencyKey).Scan(&storedHash, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return transaction, nil, nil
	}
	if err != nil {
		_ = transaction.Rollback()
		return nil, nil, err
	}
	if !hmac.Equal(storedHash, requestHash) {
		_ = transaction.Rollback()
		return nil, nil, ErrIdempotencyConflict
	}
	if err := transaction.Commit(); err != nil {
		return nil, nil, err
	}
	return nil, result, nil
}

func getDraftTx(ctx context.Context, transaction *sql.Tx, id string, forUpdate bool) (Draft, error) {
	query := `SELECT id,base_revision,document,status,revision,validation_report,validation_hash,
		created_by,updated_by,created_at,updated_at FROM routing_catalog_drafts WHERE id=$1`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	return scanDraft(transaction.QueryRowContext(ctx, query, id))
}

func scanDraft(scanner rowScanner) (Draft, error) {
	var draft Draft
	var document, validation []byte
	var validationHash sql.NullString
	err := scanner.Scan(&draft.ID, &draft.BaseRevision, &document, &draft.Status, &draft.Revision,
		&validation, &validationHash, &draft.CreatedBy, &draft.UpdatedBy, &draft.CreatedAt, &draft.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Draft{}, ErrNotFound
	}
	if err != nil {
		return Draft{}, err
	}
	if err := json.Unmarshal(document, &draft.Document); err != nil {
		return Draft{}, err
	}
	if len(validation) > 0 {
		if err := json.Unmarshal(validation, &draft.Validation); err != nil {
			return Draft{}, err
		}
	}
	draft.ValidationHash = validationHash.String
	return draft, nil
}

func currentRevisionTx(ctx context.Context, transaction *sql.Tx) (int64, error) {
	var revision int64
	err := transaction.QueryRowContext(ctx, `SELECT revision FROM routing_catalog_head WHERE singleton=true FOR UPDATE`).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return revision, err
}

type rowScanner interface{ Scan(...any) error }

func scanRevision(scanner rowScanner) (Revision, error) {
	var revision Revision
	var document, validation []byte
	var source sql.NullInt64
	err := scanner.Scan(&revision.Revision, &document, &validation, &revision.ValidationHash, &source, &revision.CreatedBy, &revision.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Revision{}, ErrNotFound
	}
	if err != nil {
		return Revision{}, err
	}
	if err := json.Unmarshal(document, &revision.Document); err != nil {
		return Revision{}, err
	}
	if err := json.Unmarshal(validation, &revision.Validation); err != nil {
		return Revision{}, err
	}
	if source.Valid {
		revision.SourceRevision = source.Int64
	}
	return revision, nil
}

func scanPublication(scanner rowScanner) (Publication, error) {
	var publication Publication
	var requiredRegions []byte
	err := scanner.Scan(&publication.ID, &publication.CatalogRevision, &publication.Status, &publication.ValidationHash,
		&requiredRegions, &publication.CreatedBy, &publication.CreatedAt, &publication.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Publication{}, ErrNotFound
	}
	if err != nil {
		return Publication{}, err
	}
	if err := json.Unmarshal(requiredRegions, &publication.RequiredRegions); err != nil {
		return Publication{}, err
	}
	return publication, nil
}

func (service *Service) recordDraftAudit(ctx context.Context, transaction *sql.Tx, actor tenantadmin.ActorEnvelope, draft Draft, action string) error {
	auditID, err := catalogID(service.random, "raud")
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{"draft_id": draft.ID, "base_revision": draft.BaseRevision, "status": draft.Status, "validation_hash": draft.ValidationHash})
	_, err = transaction.ExecContext(ctx, `INSERT INTO control_audit_events (
		event_id,tenant_id,actor_type,actor_id,acting_tenant_id,scopes,request_id,reason,action,
		aggregate_type,aggregate_id,aggregate_revision,payload,occurred_at
	) VALUES ($1,NULL,$2,$3,NULLIF($4,''),$5,$6,$7,$8,'RoutingCatalogDraft',$9,$10,$11,$12)`,
		auditID, actor.Type, actor.ID, actor.ActingTenantID, actor.Scopes, actor.RequestID, actor.Reason, action, draft.ID, draft.Revision, payload, service.now().UTC())
	return err
}

func (service *Service) recordPublication(ctx context.Context, transaction *sql.Tx, actor tenantadmin.ActorEnvelope, result PublicationResult) error {
	auditID, err := catalogID(service.random, "raud")
	if err != nil {
		return err
	}
	eventID, err := catalogID(service.random, "revt")
	if err != nil {
		return err
	}
	auditPayload, _ := json.Marshal(map[string]any{"publication_id": result.Publication.ID, "catalog_revision": result.Revision.Revision, "validation_hash": result.Revision.ValidationHash})
	if _, err := transaction.ExecContext(ctx, `INSERT INTO control_audit_events (
		event_id,tenant_id,actor_type,actor_id,acting_tenant_id,scopes,request_id,reason,action,
		aggregate_type,aggregate_id,aggregate_revision,payload,occurred_at
	) VALUES ($1,NULL,$2,$3,NULLIF($4,''),$5,$6,$7,'RoutingCatalogPublished','RoutingCatalog',$8,$9,$10,$11)`,
		auditID, actor.Type, actor.ID, actor.ActingTenantID, actor.Scopes, actor.RequestID, actor.Reason, result.Publication.ID, result.Revision.Revision, auditPayload, result.Revision.CreatedAt); err != nil {
		return err
	}
	eventPayload, _ := json.Marshal(map[string]any{
		"publication_id": result.Publication.ID, "catalog_revision": result.Revision.Revision,
		"validation_hash": result.Revision.ValidationHash, "validation_report": result.Revision.Validation,
		"document": result.Revision.Document,
	})
	_, err = transaction.ExecContext(ctx, `INSERT INTO control_outbox (
		event_id,schema_version,aggregate_type,aggregate_id,aggregate_revision,tenant_id,event_type,occurred_at,payload
	) VALUES ($1,1,'RoutingCatalog',$2,$3,NULL,'RoutingCatalogPublished',$4,$5)`, eventID, result.Publication.ID, result.Revision.Revision, result.Revision.CreatedAt, eventPayload)
	return err
}

func recordCatalogCommand(ctx context.Context, transaction *sql.Tx, actor tenantadmin.ActorEnvelope, operation, idempotencyKey string, requestHash []byte, result any) error {
	payload, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = transaction.ExecContext(ctx, `INSERT INTO control_command_idempotency (
		actor_type,actor_id,operation,idempotency_key,request_hash,result
	) VALUES ($1,$2,$3,$4,$5,$6)`, actor.Type, actor.ID, operation, idempotencyKey, requestHash, payload)
	return err
}

func authorizeCatalogWrite(actor tenantadmin.ActorEnvelope) error {
	if actor.Type == "" || actor.ID == "" || actor.RequestID == "" || strings.TrimSpace(actor.Reason) == "" || !actorHasScope(actor, tenantadmin.ScopePlatformWrite) {
		return ErrPolicyDenied
	}
	return nil
}

func authorizeCatalogRead(actor tenantadmin.ActorEnvelope) error {
	if actor.Type == "" || actor.ID == "" || !actorHasScope(actor, tenantadmin.ScopePlatformRead) && !actorHasScope(actor, tenantadmin.ScopePlatformWrite) {
		return ErrPolicyDenied
	}
	return nil
}

func actorHasScope(actor tenantadmin.ActorEnvelope, wanted string) bool {
	for _, scope := range actor.Scopes {
		if scope == wanted {
			return true
		}
	}
	return false
}

func validRegions(regions []string) bool {
	seen := make(map[string]struct{}, len(regions))
	for _, region := range regions {
		if strings.TrimSpace(region) == "" {
			return false
		}
		if _, duplicate := seen[region]; duplicate {
			return false
		}
		seen[region] = struct{}{}
	}
	return true
}

func catalogCommandHash(command any, reason string) ([]byte, error) {
	payload, err := json.Marshal(struct {
		Command any    `json:"command"`
		Reason  string `json:"reason"`
	}{Command: command, Reason: reason})
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(payload)
	return digest[:], nil
}

func catalogID(random io.Reader, prefix string) (string, error) {
	data := make([]byte, 16)
	if _, err := io.ReadFull(random, data); err != nil {
		return "", err
	}
	return prefix + "_" + hex.EncodeToString(data), nil
}

func mapCatalogDatabaseError(err error) error {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) && postgresError.Code == "23505" {
		return ErrAlreadyExists
	}
	return fmt.Errorf("persist Routing Catalog: %w", err)
}
