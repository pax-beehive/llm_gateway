package controlrelay

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type RetentionResult struct {
	RequestedThrough int64
	SafeThrough      int64
	Deleted          int64
	MinimumCursor    int64
}

type Retention struct {
	database   *sql.DB
	now        func() time.Time
	staleAfter time.Duration
}

func NewRetention(database *sql.DB, now func() time.Time, staleAfter time.Duration) (*Retention, error) {
	if database == nil || staleAfter <= 0 {
		return nil, errors.New("Control Event retention requires PostgreSQL and a positive heartbeat window")
	}
	if now == nil {
		now = time.Now
	}
	return &Retention{database: database, now: now, staleAfter: staleAfter}, nil
}

// PruneThrough removes at most limit events, never crossing the cursor of a
// recently observed Gateway. A Gateway older than that window is required to
// use the authoritative bootstrap if it returns.
func (retention *Retention) PruneThrough(ctx context.Context, requested int64, limit int) (RetentionResult, error) {
	if requested < 0 || limit < 1 || limit > 10_000 {
		return RetentionResult{}, errors.New("invalid Control Event retention request")
	}
	transaction, err := retention.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return RetentionResult{}, err
	}
	defer func() { _ = transaction.Rollback() }()
	if _, err := transaction.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('control-event-retention',0))`); err != nil {
		return RetentionResult{}, err
	}
	var current, sourceHead int64
	if err := transaction.QueryRowContext(ctx, `SELECT minimum_cursor FROM control_event_history WHERE singleton=true FOR UPDATE`).Scan(&current); err != nil {
		return RetentionResult{}, err
	}
	if err := transaction.QueryRowContext(ctx, `SELECT GREATEST(COALESCE(max(delivery_sequence),0),$1) FROM control_outbox`, current).Scan(&sourceHead); err != nil {
		return RetentionResult{}, err
	}
	safe := min(requested, sourceHead)
	var activeMinimum sql.NullInt64
	if err := transaction.QueryRowContext(ctx, `SELECT min((consumer->>'last_event_id')::bigint)
		FROM operations_gateway_heartbeats h CROSS JOIN LATERAL jsonb_array_elements(h.consumers) consumer
		WHERE h.received_at >= $1 AND consumer->>'name'='control_event_relay'
		AND COALESCE(consumer->>'last_event_id','') ~ '^[0-9]+$'`, retention.now().UTC().Add(-retention.staleAfter)).Scan(&activeMinimum); err != nil {
		return RetentionResult{}, err
	}
	if activeMinimum.Valid {
		safe = min(safe, activeMinimum.Int64)
	}
	result := RetentionResult{RequestedThrough: requested, SafeThrough: safe, MinimumCursor: current}
	if safe <= current {
		if err := transaction.Commit(); err != nil {
			return RetentionResult{}, err
		}
		return result, nil
	}
	rows, err := transaction.QueryContext(ctx, `WITH candidates AS (
		SELECT event_id,delivery_sequence FROM control_outbox WHERE delivery_sequence<=$1
		ORDER BY delivery_sequence LIMIT $2 FOR UPDATE
	), deleted AS (
		DELETE FROM control_outbox o USING candidates c WHERE o.event_id=c.event_id
		RETURNING c.delivery_sequence
	) SELECT delivery_sequence FROM deleted ORDER BY delivery_sequence`, safe, limit)
	if err != nil {
		return RetentionResult{}, err
	}
	deletedThrough := current
	for rows.Next() {
		if err := rows.Scan(&deletedThrough); err != nil {
			_ = rows.Close()
			return RetentionResult{}, err
		}
		result.Deleted++
	}
	if err := rows.Close(); err != nil {
		return RetentionResult{}, err
	}
	if result.Deleted < int64(limit) {
		deletedThrough = safe
	}
	if deletedThrough < current {
		return RetentionResult{}, fmt.Errorf("Control Event retention floor regressed from %d to %d", current, deletedThrough)
	}
	if _, err := transaction.ExecContext(ctx, `UPDATE control_event_history SET minimum_cursor=$1,updated_at=$2 WHERE singleton=true`, deletedThrough, retention.now().UTC()); err != nil {
		return RetentionResult{}, err
	}
	result.MinimumCursor = deletedThrough
	if err := transaction.Commit(); err != nil {
		return RetentionResult{}, err
	}
	return result, nil
}
