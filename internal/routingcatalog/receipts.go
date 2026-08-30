package routingcatalog

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

const (
	ReceiptApplied  = "applied"
	ReceiptRejected = "rejected"
)

// CollectNextRolloutReceipt promotes one Gateway-owned inbox observation into
// the authoritative control-plane publication workflow. Keeping this step in
// the control-plane process prevents a Gateway database role from directly
// changing publication status.
func (service *Service) CollectNextRolloutReceipt(ctx context.Context) (bool, error) {
	var receipt RolloutReceipt
	err := service.database.QueryRowContext(ctx, `SELECT i.publication_id,i.gateway_id,i.region,
		i.catalog_revision,i.status,COALESCE(i.error_code,''),i.observed_at
		FROM gateway_routing_catalog_inbox i
		JOIN routing_publications p ON p.id=i.publication_id AND p.catalog_revision=i.catalog_revision
		LEFT JOIN routing_rollout_receipts r ON r.publication_id=i.publication_id
			AND r.gateway_id=i.gateway_id AND r.region=i.region
		WHERE r.publication_id IS NULL OR i.observed_at > r.observed_at
		ORDER BY i.observed_at,i.gateway_id,i.event_id LIMIT 1`).Scan(
		&receipt.PublicationID, &receipt.GatewayID, &receipt.Region, &receipt.CatalogRevision,
		&receipt.Status, &receipt.ErrorCode, &receipt.ObservedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, err = service.RecordRolloutReceipt(ctx, receipt)
	return true, err
}

func (service *Service) RecordRolloutReceipt(ctx context.Context, receipt RolloutReceipt) (Publication, error) {
	if !managedResourceIDPattern.MatchString(receipt.PublicationID) || !managedResourceIDPattern.MatchString(receipt.GatewayID) ||
		strings.TrimSpace(receipt.Region) == "" || receipt.CatalogRevision <= 0 ||
		receipt.Status != ReceiptApplied && receipt.Status != ReceiptRejected || receipt.ObservedAt.IsZero() {
		return Publication{}, ErrInvalidArgument
	}
	transaction, err := service.database.BeginTx(ctx, nil)
	if err != nil {
		return Publication{}, err
	}
	defer func() { _ = transaction.Rollback() }()
	publication, err := recordRolloutReceiptTx(ctx, transaction, receipt, service.now().UTC())
	if err != nil {
		return Publication{}, err
	}
	if err := transaction.Commit(); err != nil {
		return Publication{}, err
	}
	return publication, nil
}

func recordRolloutReceiptTx(ctx context.Context, transaction *sql.Tx, receipt RolloutReceipt, updatedAt time.Time) (Publication, error) {
	publication, err := scanPublication(transaction.QueryRowContext(ctx, `SELECT id,catalog_revision,status,validation_hash,
		to_json(required_regions),created_by,created_at,updated_at FROM routing_publications WHERE id=$1 FOR UPDATE`, receipt.PublicationID))
	if err != nil {
		return Publication{}, err
	}
	if publication.CatalogRevision != receipt.CatalogRevision {
		return Publication{}, ErrRevisionConflict
	}
	_, err = transaction.ExecContext(ctx, `INSERT INTO routing_rollout_receipts (
		publication_id,gateway_id,region,catalog_revision,status,error_code,observed_at
	) VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),$7)
	ON CONFLICT (publication_id,gateway_id,region) DO UPDATE SET
		catalog_revision=EXCLUDED.catalog_revision,status=EXCLUDED.status,error_code=EXCLUDED.error_code,observed_at=EXCLUDED.observed_at
	WHERE EXCLUDED.observed_at > routing_rollout_receipts.observed_at`, receipt.PublicationID, receipt.GatewayID, receipt.Region,
		receipt.CatalogRevision, receipt.Status, receipt.ErrorCode, receipt.ObservedAt.UTC())
	if err != nil {
		return Publication{}, err
	}
	receipts, err := listReceiptsTx(ctx, transaction, receipt.PublicationID)
	if err != nil {
		return Publication{}, err
	}
	setReceiptLag(publication.CreatedAt, receipts)
	status := rolloutStatus(publication.RequiredRegions, receipts)
	if _, err := transaction.ExecContext(ctx, `UPDATE routing_publications SET status=$1,updated_at=$2 WHERE id=$3`, status, updatedAt, receipt.PublicationID); err != nil {
		return Publication{}, err
	}
	publication.Status = status
	publication.UpdatedAt = updatedAt
	publication.Receipts = receipts
	return publication, nil
}

func listReceiptsTx(ctx context.Context, transaction *sql.Tx, publicationID string) ([]RolloutReceipt, error) {
	rows, err := transaction.QueryContext(ctx, `SELECT publication_id,gateway_id,region,catalog_revision,status,
		COALESCE(error_code,''),observed_at FROM routing_rollout_receipts WHERE publication_id=$1 ORDER BY region,gateway_id`, publicationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	receipts := make([]RolloutReceipt, 0)
	for rows.Next() {
		var receipt RolloutReceipt
		if err := rows.Scan(&receipt.PublicationID, &receipt.GatewayID, &receipt.Region, &receipt.CatalogRevision, &receipt.Status, &receipt.ErrorCode, &receipt.ObservedAt); err != nil {
			return nil, err
		}
		receipts = append(receipts, receipt)
	}
	return receipts, rows.Err()
}

func rolloutStatus(requiredRegions []string, receipts []RolloutReceipt) PublicationStatus {
	appliedRegions := make(map[string]bool)
	anyApplied := false
	anyRejected := false
	for _, receipt := range receipts {
		if receipt.Status == ReceiptApplied {
			appliedRegions[receipt.Region] = true
			anyApplied = true
		} else if receipt.Status == ReceiptRejected {
			anyRejected = true
		}
	}
	allRequiredApplied := len(requiredRegions) > 0
	for _, region := range requiredRegions {
		if !appliedRegions[region] {
			allRequiredApplied = false
			break
		}
	}
	if allRequiredApplied {
		return PublicationActive
	}
	if len(requiredRegions) == 0 && anyApplied {
		return PublicationActive
	}
	if anyApplied && anyRejected {
		return PublicationPartiallyApplied
	}
	if anyRejected && !anyApplied {
		return PublicationFailed
	}
	return PublicationRollingOut
}

func appendPublicationReceipts(ctx context.Context, database *sql.DB, publication Publication) (Publication, error) {
	rows, err := database.QueryContext(ctx, `SELECT publication_id,gateway_id,region,catalog_revision,status,
		COALESCE(error_code,''),observed_at FROM routing_rollout_receipts WHERE publication_id=$1 ORDER BY region,gateway_id`, publication.ID)
	if err != nil {
		return Publication{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var receipt RolloutReceipt
		if err := rows.Scan(&receipt.PublicationID, &receipt.GatewayID, &receipt.Region, &receipt.CatalogRevision, &receipt.Status, &receipt.ErrorCode, &receipt.ObservedAt); err != nil {
			return Publication{}, err
		}
		publication.Receipts = append(publication.Receipts, receipt)
	}
	if err := rows.Err(); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Publication{}, err
	}
	setReceiptLag(publication.CreatedAt, publication.Receipts)
	return publication, nil
}

func setReceiptLag(publishedAt time.Time, receipts []RolloutReceipt) {
	for index := range receipts {
		lag := receipts[index].ObservedAt.Sub(publishedAt).Milliseconds()
		if lag < 0 {
			lag = 0
		}
		receipts[index].LagMilliseconds = lag
	}
}
