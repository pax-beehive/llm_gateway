package routingcatalog

import (
	"errors"
	"time"
)

var (
	ErrNotFound            = errors.New("Routing Catalog record not found")
	ErrAlreadyExists       = errors.New("Routing Catalog record already exists")
	ErrRevisionConflict    = errors.New("Routing Catalog revision conflict")
	ErrIdempotencyConflict = errors.New("Routing Catalog idempotency conflict")
	ErrPolicyDenied        = errors.New("Routing Catalog policy denied")
	ErrInvalidArgument     = errors.New("Routing Catalog invalid argument")
	ErrValidationFailed    = errors.New("Routing Catalog validation failed")
)

type DraftStatus string

const (
	DraftOpen      DraftStatus = "draft"
	DraftValidated DraftStatus = "validated"
)

type PublicationStatus string

const (
	PublicationPublished        PublicationStatus = "published"
	PublicationRollingOut       PublicationStatus = "rolling_out"
	PublicationActive           PublicationStatus = "active"
	PublicationPartiallyApplied PublicationStatus = "partially_applied"
	PublicationFailed           PublicationStatus = "failed"
)

type Draft struct {
	ID             string           `json:"id"`
	BaseRevision   int64            `json:"base_revision"`
	Document       Document         `json:"document"`
	Status         DraftStatus      `json:"status"`
	Revision       int64            `json:"revision"`
	Validation     ValidationReport `json:"validation_report"`
	ValidationHash string           `json:"validation_hash,omitempty"`
	CreatedBy      string           `json:"created_by"`
	UpdatedBy      string           `json:"updated_by"`
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
}

type Revision struct {
	Revision       int64            `json:"revision"`
	Document       Document         `json:"document"`
	Validation     ValidationReport `json:"validation_report"`
	ValidationHash string           `json:"validation_hash"`
	SourceRevision int64            `json:"source_revision,omitempty"`
	CreatedBy      string           `json:"created_by"`
	CreatedAt      time.Time        `json:"created_at"`
}

type Publication struct {
	ID              string            `json:"id"`
	CatalogRevision int64             `json:"catalog_revision"`
	Status          PublicationStatus `json:"status"`
	ValidationHash  string            `json:"validation_hash"`
	RequiredRegions []string          `json:"required_regions"`
	CreatedBy       string            `json:"created_by"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
	Receipts        []RolloutReceipt  `json:"receipts,omitempty"`
}

type RolloutReceipt struct {
	PublicationID   string    `json:"publication_id"`
	GatewayID       string    `json:"gateway_id"`
	Region          string    `json:"region"`
	CatalogRevision int64     `json:"catalog_revision"`
	Status          string    `json:"status"`
	ErrorCode       string    `json:"error_code,omitempty"`
	ObservedAt      time.Time `json:"applied_at"`
	LagMilliseconds int64     `json:"lag_milliseconds"`
}

type DraftResult struct {
	Draft  Draft `json:"draft"`
	Replay bool  `json:"-"`
}

type PublicationResult struct {
	Revision    Revision    `json:"revision"`
	Publication Publication `json:"publication"`
	Replay      bool        `json:"-"`
}

type RevisionPage struct {
	Data       []Revision `json:"data"`
	NextCursor int64      `json:"next_cursor,omitempty"`
}

type CreateDraftCommand struct {
	ID           string
	BaseRevision int64
	Document     Document
}

type ValidateDraftCommand struct {
	DraftID          string
	ExpectedRevision int64
}

type PublishDraftCommand struct {
	DraftID          string
	ExpectedRevision int64
	RequiredRegions  []string
}

type UpdateDraftCommand struct {
	DraftID          string
	ExpectedRevision int64
	Document         Document
}

type RestoreCommand struct {
	SourceRevision  int64
	ExpectedHead    int64
	RequiredRegions []string
}
