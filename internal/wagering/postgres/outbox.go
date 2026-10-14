package postgres

import (
	"context"
	"errors"
	"time"
)

var ErrInvalidLease = errors.New("postgres: lease must be a positive whole number of milliseconds")

// Delivery is a leased outbox record. ProcessingAttempts counts claims, including
// recovery after a crash; PublicationFailures counts failures acknowledged by
// the current lease owner. A stale publisher cannot complete another owner's work.
type Delivery struct {
	ID                  string
	AggregateID         string
	Payload             []byte
	LeaseID             string
	ProcessingAttempts  int
	PublicationFailures int
}

// ClaimEvent atomically reserves one available event for the requested duration.
func (s *Store) ClaimEvent(ctx context.Context, leaseDuration time.Duration) (Delivery, error) {
	if leaseDuration <= 0 || leaseDuration%time.Millisecond != 0 {
		return Delivery{}, ErrInvalidLease
	}

	leaseID, err := newID()
	if err != nil {
		return Delivery{}, err
	}

	const query = `
    WITH claim_clock AS (
        SELECT NOW() AS claimed_at
    ),
    available_event_to_claim AS (
        SELECT o.event_id
        FROM outbox AS o
        CROSS JOIN claim_clock AS clock
        CROSS JOIN LATERAL (
            SELECT
                o.published_at IS NULL AS awaiting_publication,
                o.next_attempt_at <= clock.claimed_at AS retry_due,
                (o.lease_until IS NULL OR o.lease_until < clock.claimed_at) AS lease_available
        ) AS availability
        WHERE availability.awaiting_publication
          AND availability.retry_due
          AND availability.lease_available
        ORDER BY o.occurred_at, o.event_id
        FOR UPDATE OF o SKIP LOCKED
        LIMIT 1
    )
    UPDATE outbox AS o
    SET
        lease_id = $1,
        lease_until = clock.claimed_at + $2::bigint * INTERVAL '1 millisecond',
        processing_attempts = o.processing_attempts + 1
    FROM available_event_to_claim AS available
    CROSS JOIN claim_clock AS clock
    WHERE o.event_id = available.event_id
    RETURNING
        o.event_id::text,
        o.aggregate_id::text,
        o.payload,
        o.lease_id::text,
        o.processing_attempts,
        o.publication_failures`

	var delivery Delivery

	err = s.pool.QueryRow(ctx, query, leaseID, leaseDuration.Milliseconds()).Scan(
		&delivery.ID,
		&delivery.AggregateID,
		&delivery.Payload,
		&delivery.LeaseID,
		&delivery.ProcessingAttempts,
		&delivery.PublicationFailures,
	)
	if err != nil {
		return Delivery{}, classify(err)
	}

	return delivery, nil
}

// FinishEvent records publication results only while this publisher owns the
// lease. Failed publications increase their own counter and determine backoff;
// abandoned claims increase ProcessingAttempts without inventing a failure.
func (s *Store) FinishEvent(ctx context.Context, delivery Delivery, published bool) error {
	const query = `
    WITH completion_clock AS (
        SELECT NOW() AS completed_at
    )
    UPDATE outbox AS o
    SET
        published_at = CASE WHEN $3 THEN clock.completed_at ELSE NULL END,
        publication_failures = o.publication_failures + CASE WHEN $3 THEN 0 ELSE 1 END,
        next_attempt_at = CASE
            WHEN $3 THEN o.next_attempt_at
            ELSE clock.completed_at + MAKE_INTERVAL(
                secs => LEAST(300, POWER(2, LEAST(o.publication_failures + 1, 8)))::double precision
            )
        END,
        lease_id = NULL,
        lease_until = NULL
    FROM completion_clock AS clock
    WHERE o.event_id = $1
      AND o.lease_id = $2`

	_, err := s.pool.Exec(ctx, query, delivery.ID, delivery.LeaseID, published)

	return err
}
