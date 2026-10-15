//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"jungle/internal/wagering/application"
	"jungle/migrations"
)

func onlyAvailableEvent(t *testing.T, f fixture) string {
	t.Helper()

	open(t, f.stores[0])

	if _, err := f.pool.Exec(t.Context(), "UPDATE outbox SET published_at=now()"); err != nil {
		t.Fatal(err)
	}

	var eventID string

	err := f.pool.QueryRow(t.Context(), "SELECT event_id::text FROM outbox ORDER BY occurred_at,event_id LIMIT 1").Scan(&eventID)
	if err != nil {
		t.Fatal(err)
	}

	_, err = f.pool.Exec(t.Context(), `UPDATE outbox SET published_at=NULL,
		next_attempt_at=now()-interval '1 second',lease_until=NULL WHERE event_id=$1`, eventID)
	if err != nil {
		t.Fatal(err)
	}

	return eventID
}

func TestOutboxAvailability(t *testing.T) {
	tests := []struct {
		name      string
		setup     string
		available bool
	}{
		{name: "unpublished due and unleased", available: true},
		{name: "published", setup: "UPDATE outbox SET published_at=now() WHERE event_id=$1"},
		{name: "retry scheduled for later", setup: "UPDATE outbox SET next_attempt_at=now()+interval '1 hour' WHERE event_id=$1"},
		{name: "active lease", setup: "UPDATE outbox SET lease_until=now()+interval '1 hour' WHERE event_id=$1"},
		{name: "expired lease", setup: "UPDATE outbox SET lease_until=now()-interval '1 second' WHERE event_id=$1", available: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := setup(t)
			eventID := onlyAvailableEvent(t, f)

			if tt.setup != "" {
				if _, err := f.pool.Exec(t.Context(), tt.setup, eventID); err != nil {
					t.Fatal(err)
				}
			}

			delivery, err := f.stores[0].ClaimEvent(t.Context(), time.Minute)
			if !tt.available {
				if !errors.Is(err, application.ErrNotFound) {
					t.Fatalf("ClaimEvent() = %+v, %v; want no available event", delivery, err)
				}

				return
			}

			if err != nil || delivery.ID != eventID {
				t.Fatalf("ClaimEvent() = %+v, %v; want %s", delivery, err, eventID)
			}
		})
	}
}

func TestOutboxLeaseDurationAndPublicationCounters(t *testing.T) {
	f := setup(t)
	eventID := onlyAvailableEvent(t, f)
	var before, after, leaseUntil time.Time

	if err := f.pool.QueryRow(t.Context(), "SELECT clock_timestamp()").Scan(&before); err != nil {
		t.Fatal(err)
	}

	leaseDuration := 2500 * time.Millisecond
	first, err := f.stores[0].ClaimEvent(t.Context(), leaseDuration)
	if err != nil {
		t.Fatal(err)
	}

	if err := f.pool.QueryRow(t.Context(), "SELECT clock_timestamp(),lease_until FROM outbox WHERE event_id=$1", eventID).Scan(&after, &leaseUntil); err != nil {
		t.Fatal(err)
	}

	if leaseUntil.Before(before.Add(leaseDuration)) || leaseUntil.After(after.Add(leaseDuration)) {
		t.Fatalf("lease expiration %v is inconsistent with duration %v", leaseUntil, leaseDuration)
	}

	if first.ProcessingAttempts != 1 || first.PublicationFailures != 0 {
		t.Fatalf("initial counters = %+v", first)
	}

	if _, err := f.pool.Exec(t.Context(), "UPDATE outbox SET lease_until=now()-interval '1 second' WHERE event_id=$1", eventID); err != nil {
		t.Fatal(err)
	}

	recovered, err := f.stores[1].ClaimEvent(t.Context(), leaseDuration)
	if err != nil {
		t.Fatal(err)
	}

	if recovered.ProcessingAttempts != 2 || recovered.PublicationFailures != 0 {
		t.Fatalf("abandoned claim recovery counters = %+v", recovered)
	}

	if err := f.stores[0].FinishEvent(t.Context(), first, false); err != nil {
		t.Fatal(err)
	}

	if err := f.stores[1].FinishEvent(t.Context(), recovered, false); err != nil {
		t.Fatal(err)
	}

	var attempts, failures int
	var leaseReleased bool
	var nextAttempt time.Time

	err = f.pool.QueryRow(t.Context(), `SELECT processing_attempts,publication_failures,lease_id IS NULL,next_attempt_at
		FROM outbox WHERE event_id=$1`, eventID).Scan(&attempts, &failures, &leaseReleased, &nextAttempt)
	if err != nil {
		t.Fatal(err)
	}

	if attempts != 2 || failures != 1 {
		t.Fatalf("failure completion: attempts=%d, failures=%d", attempts, failures)
	}

	if !leaseReleased {
		t.Fatal("failure completion did not release the lease")
	}

	if !nextAttempt.After(after) {
		t.Fatalf("failure completion did not schedule a retry: next=%v", nextAttempt)
	}

	if _, err := f.pool.Exec(t.Context(), "UPDATE outbox SET next_attempt_at=now() WHERE event_id=$1", eventID); err != nil {
		t.Fatal(err)
	}

	retry, err := f.stores[2].ClaimEvent(t.Context(), leaseDuration)
	if err != nil {
		t.Fatal(err)
	}

	if retry.ProcessingAttempts != 3 || retry.PublicationFailures != 1 {
		t.Fatalf("retry counters = %+v", retry)
	}

	if err := f.stores[2].FinishEvent(t.Context(), retry, true); err != nil {
		t.Fatal(err)
	}

	var published bool

	err = f.pool.QueryRow(t.Context(), "SELECT published_at IS NOT NULL,publication_failures FROM outbox WHERE event_id=$1", eventID).Scan(&published, &failures)
	if err != nil {
		t.Fatal(err)
	}

	if !published || failures != 1 {
		t.Fatalf("successful publication: published=%v, failures=%d", published, failures)
	}
}

func TestOutboxSkipsLockedEvent(t *testing.T) {
	f := setup(t)
	open(t, f.stores[0])
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tx.Rollback(t.Context()); err != nil {
			t.Errorf("release locked event: %v", err)
		}
	}()

	var lockedID string

	if err := tx.QueryRow(t.Context(), "SELECT event_id::text FROM outbox ORDER BY occurred_at,event_id LIMIT 1 FOR UPDATE").Scan(&lockedID); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	delivery, err := f.stores[1].ClaimEvent(ctx, time.Minute)
	if err != nil || delivery.ID == lockedID {
		t.Fatalf("claim must skip another transaction's row lock: %+v, %v", delivery, err)
	}
}

func TestOutboxMigrationPreservesClaimHistory(t *testing.T) {
	f := setup(t)
	connection, err := f.pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Release()

	if err := migrations.Apply(t.Context(), connection.Conn(), "down"); err != nil {
		t.Fatal(err)
	}

	base, err := os.ReadFile("../../../migrations/000001_base.up.sql")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := connection.Exec(t.Context(), string(base)); err != nil {
		t.Fatal(err)
	}

	if _, err := connection.Exec(t.Context(), "INSERT INTO schema_migrations(version, dirty) VALUES(1, false)"); err != nil {
		t.Fatal(err)
	}

	if err := f.stores[0].Ready(t.Context()); err == nil {
		t.Fatal("readiness accepted the schema before the required outbox migration")
	}

	state := open(t, f.stores[0])

	if _, err := connection.Exec(t.Context(), "UPDATE outbox SET attempts=7"); err != nil {
		t.Fatal(err)
	}

	if err := migrations.Apply(t.Context(), connection.Conn(), "up"); err != nil {
		t.Fatal(err)
	}

	if err := f.stores[0].Ready(t.Context()); err != nil {
		t.Fatalf("readiness after upgrading: %v", err)
	}

	delivery, err := f.stores[0].ClaimEvent(t.Context(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	if delivery.ProcessingAttempts != 8 || delivery.PublicationFailures != 0 {
		t.Fatalf("upgraded counters = %+v", delivery)
	}

	assertBalance(t, f.stores[0], state.ID, "100.00", 1)
}
