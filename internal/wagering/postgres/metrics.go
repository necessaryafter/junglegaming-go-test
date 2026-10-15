package postgres

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

func (s *Store) observeConflict(err error) {
	var pgErr *pgconn.PgError

	if !errors.As(err, &pgErr) {
		return
	}

	switch pgErr.Code {
	case "23505":
		s.metrics.RecordDatabaseConflict("uniqueness")
	case "40001":
		s.metrics.RecordDatabaseConflict("serialization")
	case "40P01":
		s.metrics.RecordDatabaseConflict("deadlock")
	case "55P03":
		s.metrics.RecordDatabaseConflict("lock_unavailable")
	}
}
