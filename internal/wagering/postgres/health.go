package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// Ready verifies the schema as well as connectivity.
func (s *Store) Ready(ctx context.Context) error {
	var columnCount int

	const query = `
    SELECT COUNT(*)
    FROM pg_attribute
    WHERE attrelid = to_regclass('outbox')
      AND attname IN ('processing_attempts', 'publication_failures')
      AND NOT attisdropped`

	if err := s.pool.QueryRow(ctx, query).Scan(&columnCount); err != nil {
		return err
	}

	if columnCount != 2 {
		return errors.New("postgres: migrations have not been applied")
	}

	return nil
}

type Statistics struct {
	OutboxPending       int64
	OutboxOldestSeconds int64
	PendingReferences   int64
}

func (s *Store) Statistics(ctx context.Context) (Statistics, error) {
	var stats Statistics

	err := s.pool.QueryRow(ctx, `SELECT count(*),COALESCE(EXTRACT(EPOCH FROM now()-min(occurred_at))::bigint,0)
		FROM outbox WHERE published_at IS NULL`).Scan(&stats.OutboxPending, &stats.OutboxOldestSeconds)
	if err != nil {
		return Statistics{}, err
	}

	err = s.pool.QueryRow(ctx, "SELECT count(*) FROM wager_transactions WHERE status='PENDING_REFERENCE'").Scan(&stats.PendingReferences)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Statistics{}, err
	}

	return stats, nil
}
