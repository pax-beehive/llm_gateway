package metering

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type ExportStore interface {
	Put(context.Context, string, []byte) error
	Get(context.Context, string) ([]byte, error)
}

type FileExportStore struct{ root string }

func NewFileExportStore(root string) (*FileExportStore, error) {
	if root == "" || !filepath.IsAbs(root) {
		return nil, errors.New("Metering export directory must be absolute")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	return &FileExportStore{root: root}, nil
}

func (store *FileExportStore) path(key string) (string, error) {
	if filepath.Base(key) != key || key == "." || key == "" {
		return "", ErrInvalidArgument
	}
	return filepath.Join(store.root, key), nil
}

func (store *FileExportStore) Put(_ context.Context, key string, payload []byte) error {
	path, err := store.path(key)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(store.root, ".metering-export-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Link(temporary, path); err == nil {
		return nil
	} else if !os.IsExist(err) {
		return err
	}
	existing, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(existing, payload) {
		return errors.New("Metering export object already exists with different content")
	}
	return nil
}

func (store *FileExportStore) Get(_ context.Context, key string) ([]byte, error) {
	path, err := store.path(key)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

func (service *Service) RequestExport(ctx context.Context, filter Filter) (Export, error) {
	if _, _, err := filterSQL(filter, "", 1); err != nil {
		return Export{}, err
	}
	id, err := newID("export")
	if err != nil {
		return Export{}, err
	}
	cutoff := service.now().UTC()
	payload, _ := json.Marshal(filter)
	result := Export{ID: id, TenantID: filter.TenantID, Status: "queued", Filter: payload, Cutoff: cutoff, CreatedAt: cutoff}
	_, err = service.database.ExecContext(ctx, `INSERT INTO metering_exports(id,tenant_id,status,filter,cutoff,created_at)
		VALUES($1,$2,'queued',$3,$4,$4)`, id, filter.TenantID, payload, cutoff)
	return result, err
}

func (service *Service) GetExport(ctx context.Context, tenantID, id string) (Export, error) {
	var result Export
	err := service.database.QueryRowContext(ctx, `SELECT id,tenant_id,status,filter,cutoff,COALESCE(object_key,''),COALESCE(sha256,''),row_count,COALESCE(error_code,''),created_at,completed_at
		FROM metering_exports WHERE tenant_id=$1 AND id=$2`, tenantID, id).Scan(&result.ID, &result.TenantID, &result.Status, &result.Filter, &result.Cutoff, &result.ObjectKey, &result.SHA256, &result.RowCount, &result.ErrorCode, &result.CreatedAt, &result.CompletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Export{}, ErrNotFound
	}
	return result, err
}

func (service *Service) ListExports(ctx context.Context, tenantID string, limit int) ([]Export, error) {
	if tenantID == "" {
		return nil, ErrInvalidArgument
	}
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 100 {
		return nil, ErrInvalidArgument
	}
	rows, err := service.database.QueryContext(ctx, `SELECT id,tenant_id,status,filter,cutoff,COALESCE(object_key,''),COALESCE(sha256,''),row_count,COALESCE(error_code,''),created_at,completed_at
		FROM metering_exports WHERE tenant_id=$1 ORDER BY created_at DESC,id DESC LIMIT $2`, tenantID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Export{}
	for rows.Next() {
		var item Export
		if err := rows.Scan(&item.ID, &item.TenantID, &item.Status, &item.Filter, &item.Cutoff, &item.ObjectKey, &item.SHA256, &item.RowCount, &item.ErrorCode, &item.CreatedAt, &item.CompletedAt); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (service *Service) RunNextExport(ctx context.Context, store ExportStore) (bool, error) {
	if store == nil {
		return false, errors.New("Metering export store is required")
	}
	tx, err := service.database.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var job Export
	err = tx.QueryRowContext(ctx, `SELECT id,tenant_id,status,filter,cutoff,created_at FROM metering_exports
		WHERE status='queued' OR (status='running' AND lease_expires_at<=now())
		ORDER BY created_at,id FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&job.ID, &job.TenantID, &job.Status, &job.Filter, &job.Cutoff, &job.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, tx.Commit()
	}
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE metering_exports SET status='running',lease_expires_at=now()+interval '1 hour',attempt_count=attempt_count+1,error_code=NULL WHERE id=$1`, job.ID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	var filter Filter
	if err := json.Unmarshal(job.Filter, &filter); err != nil {
		return true, service.failExport(ctx, job.ID, "invalid_filter")
	}
	if filter.Through.IsZero() || filter.Through.After(job.Cutoff) {
		filter.Through = job.Cutoff
	}
	payload, rows, err := service.exportCSV(ctx, filter, job.Cutoff)
	if err != nil {
		return true, service.failExport(ctx, job.ID, "query_failed")
	}
	key := job.ID + ".csv"
	if err := store.Put(ctx, key, payload); err != nil {
		return true, service.failExport(ctx, job.ID, "storage_failed")
	}
	digest := sha256.Sum256(payload)
	completed := service.now().UTC()
	_, err = service.database.ExecContext(ctx, `UPDATE metering_exports SET status='succeeded',object_key=$2,sha256=$3,row_count=$4,completed_at=$5,error_code=NULL,lease_expires_at=NULL WHERE id=$1 AND status='running'`, job.ID, key, hex.EncodeToString(digest[:]), rows, completed)
	return true, err
}

func (service *Service) failExport(ctx context.Context, id, code string) error {
	_, err := service.database.ExecContext(ctx, `UPDATE metering_exports SET status='failed',error_code=$2,completed_at=now(),lease_expires_at=NULL WHERE id=$1 AND status='running'`, id, code)
	return err
}

func (service *Service) exportCSV(ctx context.Context, filter Filter, dataCutoff time.Time) ([]byte, int64, error) {
	where, args, err := filterSQL(filter, "f.", 1)
	if err != nil {
		return nil, 0, err
	}
	args = append(args, dataCutoff)
	where += ` AND i.consumed_at <= $` + strconv.Itoa(len(args))
	rows, err := service.database.QueryContext(ctx, `SELECT f.event_id,f.usage_id,f.tenant_id,COALESCE(f.api_key_id,''),COALESCE(f.response_id,''),COALESCE(f.attempt_id,''),
		COALESCE(operation_id,''),COALESCE(capability,''),COALESCE(route_id,''),provider,public_model,provider_model,region,price_snapshot_id,
		input_tokens,cached_input_tokens,cache_write_input_tokens,output_tokens,input_units,documents,amount_micros,currency,outcome,
		COALESCE(corrects_event_id,''),COALESCE(correction_actor_id,''),COALESCE(reason,''),f.occurred_at
		FROM metering_usage_facts f JOIN metering_inbox i USING(event_id) WHERE `+where+` ORDER BY f.occurred_at,f.event_id`, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var buffer bytes.Buffer
	writer := csv.NewWriter(&buffer)
	_ = writer.Write([]string{"event_id", "usage_id", "tenant_id", "api_key_id", "response_id", "attempt_id", "operation_id", "capability", "route_id", "provider", "public_model", "provider_model", "region", "price_snapshot_id", "input_tokens", "cached_input_tokens", "cache_write_input_tokens", "output_tokens", "input_units", "documents", "amount_micros", "currency", "outcome", "corrects_event_id", "correction_actor_id", "reason", "occurred_at"})
	var count int64
	for rows.Next() {
		values := make([]string, 27)
		var nums [7]int64
		var at time.Time
		dest := []any{&values[0], &values[1], &values[2], &values[3], &values[4], &values[5], &values[6], &values[7], &values[8], &values[9], &values[10], &values[11], &values[12], &values[13], &nums[0], &nums[1], &nums[2], &nums[3], &nums[4], &nums[5], &nums[6], &values[21], &values[22], &values[23], &values[24], &values[25], &at}
		if err := rows.Scan(dest...); err != nil {
			return nil, 0, err
		}
		for i, n := range nums {
			values[14+i] = strconv.FormatInt(n, 10)
		}
		values[26] = at.UTC().Format(time.RFC3339Nano)
		_ = writer.Write(values)
		count++
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	writer.Flush()
	return buffer.Bytes(), count, writer.Error()
}
