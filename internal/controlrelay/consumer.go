package controlrelay

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/toddzheng/llm-gateway/internal/controlevent"
)

//go:embed migrations/000001_control_event_relay.sql
var gatewayMigration string

type Fetcher interface {
	Fetch(context.Context, int64, int) (controlevent.Batch, error)
}

type Consumer struct {
	database   *sql.DB
	streamName string
	fetcher    Fetcher
	consumers  []controlevent.Consumer
	now        func() time.Time
	mu         sync.Mutex
}

type Status struct {
	Cursor           int64
	SourceHead       int64
	LastFetchedAt    *time.Time
	LastSucceededAt  *time.Time
	LastAttemptAt    *time.Time
	FailureStartedAt *time.Time
	LastErrorCode    string
}

func Migrate(ctx context.Context, database *sql.DB) error {
	if database == nil {
		return errors.New("Control Event relay migration requires PostgreSQL")
	}
	if _, err := database.ExecContext(ctx, gatewayMigration); err != nil {
		return fmt.Errorf("migrate Control Event relay cursor: %w", err)
	}
	return nil
}

func NewConsumer(database *sql.DB, streamName string, fetcher Fetcher, consumers []controlevent.Consumer, now func() time.Time) (*Consumer, error) {
	if database == nil || strings.TrimSpace(streamName) == "" || fetcher == nil || len(consumers) == 0 {
		return nil, errors.New("Control Event relay consumer requires PostgreSQL, stream, fetcher, and projections")
	}
	for _, consumer := range consumers {
		if consumer == nil {
			return nil, errors.New("Control Event relay projection is nil")
		}
	}
	if now == nil {
		now = time.Now
	}
	return &Consumer{database: database, streamName: streamName, fetcher: fetcher, consumers: append([]controlevent.Consumer(nil), consumers...), now: now}, nil
}

func (consumer *Consumer) RunNext(ctx context.Context, limit int) (bool, error) {
	if limit < 1 || limit > 256 {
		return false, errors.New("Control Event relay batch limit must be between 1 and 256")
	}
	consumer.mu.Lock()
	defer consumer.mu.Unlock()
	status, err := consumer.Status(ctx)
	if err != nil {
		return false, err
	}
	cursor := status.Cursor
	batch, err := consumer.fetcher.Fetch(ctx, cursor, limit)
	if err != nil {
		if persistErr := consumer.recordFailure(ctx, status, "control_event_fetch_failed"); persistErr != nil {
			return false, errors.Join(err, persistErr)
		}
		return false, err
	}
	if batch.NextCursor < cursor {
		batchErr := errors.New("Control Event relay cursor regressed")
		if persistErr := consumer.recordFailure(ctx, status, "control_event_batch_invalid"); persistErr != nil {
			return false, errors.Join(batchErr, persistErr)
		}
		return false, batchErr
	}
	if batch.SourceHead < batch.NextCursor {
		batchErr := errors.New("Control Event relay source head regressed")
		if persistErr := consumer.recordFailure(ctx, status, "control_event_batch_invalid"); persistErr != nil {
			return false, errors.Join(batchErr, persistErr)
		}
		return false, batchErr
	}
	observedAt := consumer.now().UTC()
	if err := consumer.recordFetch(ctx, cursor, batch.SourceHead, observedAt); err != nil {
		return false, err
	}
	previous := cursor
	for _, event := range batch.Events {
		if event.DeliverySequence <= previous || event.DeliverySequence > batch.NextCursor {
			batchErr := errors.New("Control Event relay batch ordering is invalid")
			failureStatus := status
			failureStatus.SourceHead = batch.SourceHead
			failureStatus.LastFetchedAt = &observedAt
			if persistErr := consumer.recordFailure(ctx, failureStatus, "control_event_batch_invalid"); persistErr != nil {
				return false, errors.Join(batchErr, persistErr)
			}
			return false, batchErr
		}
		previous = event.DeliverySequence
		for _, projection := range consumer.consumers {
			if err := projection.Consume(ctx, event); err != nil {
				if persistErr := consumer.recordFailure(ctx, Status{Cursor: cursor, SourceHead: batch.SourceHead, LastFetchedAt: &observedAt}, "control_event_projection_failed"); persistErr != nil {
					return false, errors.Join(err, persistErr)
				}
				return false, err
			}
		}
	}
	result, err := consumer.database.ExecContext(ctx, `UPDATE gateway_control_event_offsets
		SET cursor=$1,last_succeeded_at=$2,failure_started_at=NULL,last_error_code=NULL,updated_at=$2
		WHERE stream_name=$3 AND cursor=$4`, batch.NextCursor, observedAt, consumer.streamName, cursor)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if count != 1 {
		return false, errors.New("Control Event relay cursor changed concurrently")
	}
	return batch.NextCursor > cursor, nil
}

func (consumer *Consumer) recordFetch(ctx context.Context, cursor, sourceHead int64, observedAt time.Time) error {
	result, err := consumer.database.ExecContext(ctx, `INSERT INTO gateway_control_event_offsets (
		stream_name,cursor,source_head,last_fetched_at,last_succeeded_at,last_attempt_at,failure_started_at,last_error_code,updated_at
	) VALUES ($1,$2,$3,$4,NULL,$4,NULL,NULL,$4) ON CONFLICT (stream_name) DO UPDATE SET
		source_head=GREATEST(gateway_control_event_offsets.source_head,EXCLUDED.source_head),
		last_fetched_at=EXCLUDED.last_fetched_at,last_attempt_at=EXCLUDED.last_attempt_at,
		updated_at=EXCLUDED.updated_at
		WHERE gateway_control_event_offsets.cursor=$2`, consumer.streamName, cursor, sourceHead, observedAt)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("Control Event relay cursor changed concurrently")
	}
	return nil
}

func (consumer *Consumer) recordFailure(ctx context.Context, status Status, errorCode string) error {
	attemptedAt := consumer.now().UTC()
	result, err := consumer.database.ExecContext(ctx, `INSERT INTO gateway_control_event_offsets (
		stream_name,cursor,source_head,last_fetched_at,last_succeeded_at,last_attempt_at,failure_started_at,last_error_code,updated_at
	) VALUES ($1,$2,$3,$4,$5,$6,$6,$7,$6) ON CONFLICT (stream_name) DO UPDATE SET
		source_head=GREATEST(gateway_control_event_offsets.source_head,EXCLUDED.source_head),
		last_attempt_at=EXCLUDED.last_attempt_at,
		failure_started_at=CASE WHEN gateway_control_event_offsets.last_error_code IS NULL
			THEN EXCLUDED.failure_started_at ELSE gateway_control_event_offsets.failure_started_at END,
		last_error_code=EXCLUDED.last_error_code,updated_at=EXCLUDED.updated_at
		WHERE gateway_control_event_offsets.cursor=$2`, consumer.streamName, status.Cursor, status.SourceHead, status.LastFetchedAt, status.LastSucceededAt, attemptedAt, errorCode)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("Control Event relay cursor changed concurrently")
	}
	return nil
}

func (consumer *Consumer) Cursor(ctx context.Context) (int64, error) {
	status, err := consumer.Status(ctx)
	return status.Cursor, err
}

func (consumer *Consumer) Status(ctx context.Context) (Status, error) {
	var status Status
	err := consumer.database.QueryRowContext(ctx, `SELECT cursor,source_head,last_fetched_at,last_succeeded_at,last_attempt_at,failure_started_at,COALESCE(last_error_code,'')
		FROM gateway_control_event_offsets WHERE stream_name=$1`, consumer.streamName).Scan(
		&status.Cursor, &status.SourceHead, &status.LastFetchedAt, &status.LastSucceededAt, &status.LastAttemptAt, &status.FailureStartedAt, &status.LastErrorCode)
	if errors.Is(err, sql.ErrNoRows) {
		return Status{}, nil
	}
	return status, err
}
