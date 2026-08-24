package configuration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var (
	ErrNotFound = errors.New("configuration not found")
	ErrConflict = errors.New("configuration revision conflict")
)

type Snapshot struct {
	Kind      string
	Revision  int64
	Payload   json.RawMessage
	CreatedBy string
	CreatedAt time.Time
}

type Repository interface {
	Current(context.Context, string) (Snapshot, error)
	Publish(context.Context, string, int64, int64, json.RawMessage, string) (Snapshot, error)
}

type PostgresRepository struct {
	db *sql.DB
}

func NewPostgresRepository(db *sql.DB) *PostgresRepository {
	return &PostgresRepository{db: db}
}

func (r *PostgresRepository) Current(ctx context.Context, kind string) (Snapshot, error) {
	var snapshot Snapshot
	err := r.db.QueryRowContext(ctx, `
		SELECT h.kind, h.revision, v.payload, v.created_by, v.created_at
		FROM configuration_heads h
		JOIN configuration_history v ON v.kind = h.kind AND v.revision = h.revision
		WHERE h.kind = $1`, kind).Scan(
		&snapshot.Kind, &snapshot.Revision, &snapshot.Payload, &snapshot.CreatedBy, &snapshot.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, ErrNotFound
	}
	return snapshot, err
}

func (r *PostgresRepository) Publish(ctx context.Context, kind string, expectedRevision, revision int64, payload json.RawMessage, actor string) (Snapshot, error) {
	if kind == "" || revision <= 0 || revision <= expectedRevision || actor == "" || !json.Valid(payload) {
		return Snapshot{}, errors.New("invalid versioned configuration publication")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Snapshot{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "configuration\x1f"+kind); err != nil {
		return Snapshot{}, err
	}
	var currentRevision int64
	err = tx.QueryRowContext(ctx, `SELECT revision FROM configuration_heads WHERE kind = $1 FOR UPDATE`, kind).Scan(&currentRevision)
	if errors.Is(err, sql.ErrNoRows) {
		currentRevision = 0
	} else if err != nil {
		return Snapshot{}, err
	}
	if currentRevision != expectedRevision {
		return Snapshot{}, ErrConflict
	}
	var snapshot Snapshot
	err = tx.QueryRowContext(ctx, `
		INSERT INTO configuration_history (kind, revision, payload, created_by)
		VALUES ($1, $2, $3, $4)
		RETURNING kind, revision, payload, created_by, created_at`, kind, revision, payload, actor).Scan(
		&snapshot.Kind, &snapshot.Revision, &snapshot.Payload, &snapshot.CreatedBy, &snapshot.CreatedAt,
	)
	if err != nil {
		return Snapshot{}, fmt.Errorf("insert configuration revision: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO configuration_heads (kind, revision) VALUES ($1, $2)
		ON CONFLICT (kind) DO UPDATE SET revision = EXCLUDED.revision, updated_at = now()`, kind, revision); err != nil {
		return Snapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

func Watch(ctx context.Context, repository Repository, kind string, interval time.Duration, apply func(Snapshot) error) error {
	if interval <= 0 || apply == nil {
		return errors.New("configuration watch interval and apply callback are required")
	}
	currentRevision := int64(0)
	load := func() error {
		snapshot, err := repository.Current(ctx, kind)
		if err != nil {
			return err
		}
		if snapshot.Revision <= currentRevision {
			return nil
		}
		if err := apply(snapshot); err != nil {
			return err
		}
		currentRevision = snapshot.Revision
		return nil
	}
	if err := load(); err != nil {
		return err
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := load(); err != nil {
				return err
			}
		}
	}
}
