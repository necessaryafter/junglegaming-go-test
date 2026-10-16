//go:build integration

package postgres_test

import (
	"errors"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"

	"jungle/migrations"
)

func TestMigrationAdoptsLegacyHistory(t *testing.T) {
	f := setup(t)
	connection, err := f.pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	defer connection.Release()

	if err := migrations.Apply(t.Context(), connection.Conn(), "down"); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"000001_base.up.sql", "000002_outbox_attempts.up.sql"} {
		sql, err := os.ReadFile("../../../migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := connection.Exec(t.Context(), string(sql)); err != nil {
			t.Fatal(err)
		}
	}

	const legacy = `
    DROP TABLE schema_migrations;
    CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY);
    INSERT INTO schema_migrations (version) VALUES (1), (2)`

	if _, err := connection.Exec(t.Context(), legacy); err != nil {
		t.Fatal(err)
	}

	state := open(t, f.stores[0])

	if err := migrations.Apply(t.Context(), connection.Conn(), "up"); err != nil {
		t.Fatal(err)
	}

	var version int
	var dirty bool
	var count int

	const historyQuery = `
    SELECT
        version,
        dirty,
        (
            SELECT COUNT(*)
            FROM schema_migrations
        )
    FROM schema_migrations`

	err = connection.QueryRow(t.Context(), historyQuery).Scan(
		&version,
		&dirty,
		&count,
	)
	if err != nil {
		t.Fatal(err)
	}

	if version != 4 {
		t.Fatalf("adopted history version = %d, want 4", version)
	}

	if dirty {
		t.Fatal("adopted history is dirty")
	}

	if count != 1 {
		t.Fatalf("adopted history rows = %d, want 1", count)
	}

	assertBalance(t, f.stores[0], state.ID, "100.00", 1)
}

func TestFailedMigrationRequiresInvestigation(t *testing.T) {
	f := setup(t)
	connection, err := f.pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	defer connection.Release()

	// Reapplying migration 3 while its constraint exists deliberately fails.
	if _, err := connection.Exec(t.Context(), "UPDATE schema_migrations SET version = 2"); err != nil {
		t.Fatal(err)
	}

	if err := migrations.Apply(t.Context(), connection.Conn(), "up"); err == nil {
		t.Fatal("migration failure was ignored")
	}

	if err := f.stores[0].Ready(t.Context()); err == nil {
		t.Fatal("readiness accepted dirty migration history")
	}

	for _, direction := range []string{"up", "down"} {
		var dirty migrate.ErrDirty

		err := migrations.Apply(t.Context(), connection.Conn(), direction)
		if !errors.As(err, &dirty) {
			t.Fatalf("migration %s error = %v, want ErrDirty", direction, err)
		}

		if dirty.Version != 3 {
			t.Fatalf("dirty version = %d, want 3", dirty.Version)
		}
	}
}

func TestReadinessRequiresFailedPolicy(t *testing.T) {
	f := setup(t)

	if err := f.stores[0].Ready(t.Context()); err != nil {
		t.Fatalf("readiness after migrations: %v", err)
	}

	const removeFailurePolicy = `
    ALTER TABLE wager_transactions
    DROP CONSTRAINT transaction_failed_policy`

	_, err := f.pool.Exec(t.Context(), removeFailurePolicy)
	if err != nil {
		t.Fatal(err)
	}

	if err := f.stores[0].Ready(t.Context()); err == nil {
		t.Fatal("readiness accepted a schema without FAILED policy enforcement")
	}
}
