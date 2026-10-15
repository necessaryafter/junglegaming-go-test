//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"jungle/internal/wagering/domain/wager"
	"jungle/internal/wagering/domain/wallet"
	"jungle/internal/wagering/postgres"
	"jungle/migrations"
)

type permissionFixture struct {
	admin         *pgx.Conn
	migrator      *pgx.Conn
	runtime       *pgxpool.Pool
	store         *postgres.Store
	migrationRole string
	wallet        wallet.Snapshot
}

func permissionSQL(t *testing.T, name string) string {
	t.Helper()

	data, err := os.ReadFile("../../../docker/postgres/" + name + ".sql")
	if err != nil {
		t.Fatal(err)
	}

	return string(data)
}

func setupPermissions(t *testing.T, existing bool) permissionFixture {
	t.Helper()

	address := os.Getenv("TEST_DATABASE_URL")
	if address == "" {
		t.Skip("TEST_DATABASE_URL is required for real PostgreSQL integration tests")
	}

	config, err := pgx.ParseConfig(address)
	if err != nil {
		t.Fatal(err)
	}

	admin, err := pgx.ConnectConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}

	suffix := strings.ReplaceAll(id(t), "-", "")
	databaseName := "permissions_" + suffix
	migrationRole := "migration_" + suffix
	runtimeRole := "runtime_" + suffix
	migrationPassword := id(t)
	runtimePassword := id(t)
	fixture := permissionFixture{migrationRole: migrationRole}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		if fixture.runtime != nil {
			fixture.runtime.Close()
		}

		for _, connection := range []*pgx.Conn{fixture.migrator, fixture.admin} {
			if connection != nil {
				if err := connection.Close(ctx); err != nil {
					t.Errorf("close permission fixture connection: %v", err)
				}
			}
		}

		if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{databaseName}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("drop permission fixture database: %v", err)
		}

		for _, role := range []string{migrationRole, runtimeRole} {
			if _, err := admin.Exec(ctx, "DROP ROLE IF EXISTS "+pgx.Identifier{role}.Sanitize()); err != nil {
				t.Errorf("drop permission fixture role: %v", err)
			}
		}

		if err := admin.Close(ctx); err != nil {
			t.Errorf("close permission fixture administrator: %v", err)
		}
	})

	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+pgx.Identifier{databaseName}.Sanitize()); err != nil {
		t.Fatal(err)
	}

	databaseConfig := config.Copy()
	databaseConfig.Database = databaseName
	databaseConfig.RuntimeParams["search_path"] = "public"
	fixture.admin, err = pgx.ConnectConfig(t.Context(), databaseConfig)
	if err != nil {
		t.Fatal(err)
	}

	if existing {
		if err := migrations.Apply(t.Context(), fixture.admin, "up"); err != nil {
			t.Fatal(err)
		}

		poolConfig, err := pgxpool.ParseConfig(address)
		if err != nil {
			t.Fatal(err)
		}

		poolConfig.ConnConfig = databaseConfig.Copy()
		pool, err := pgxpool.NewWithConfig(t.Context(), poolConfig)
		if err != nil {
			t.Fatal(err)
		}

		fixture.wallet = open(t, postgres.New(pool, slog.Default()))
		pool.Close()
	}

	const settings = `
    SELECT
        set_config('jungle.migration_role', $1, false),
        set_config('jungle.runtime_role', $2, false),
        set_config('jungle.migration_password', $3, false),
        set_config('jungle.runtime_password', $4, false)`

	if _, err := fixture.admin.Exec(t.Context(), settings, migrationRole, runtimeRole, migrationPassword, runtimePassword); err != nil {
		t.Fatal(err)
	}

	for attempt := range 2 {
		if _, err := fixture.admin.Exec(t.Context(), permissionSQL(t, "roles")); err != nil {
			t.Fatal(err)
		}

		if attempt == 0 {
			// Reprovisioning must clear an earlier administrator's direct grant
			// and login default that would otherwise bypass financial triggers.
			query := "GRANT SET ON PARAMETER session_replication_role TO " + pgx.Identifier{runtimeRole}.Sanitize()
			if _, err := fixture.admin.Exec(t.Context(), query); err != nil {
				t.Fatal(err)
			}

			query = "ALTER ROLE " + pgx.Identifier{runtimeRole}.Sanitize() + " SET session_replication_role = replica"
			if _, err := fixture.admin.Exec(t.Context(), query); err != nil {
				t.Fatal(err)
			}
		}
	}

	migrationConfig := databaseConfig.Copy()
	migrationConfig.User = migrationRole
	migrationConfig.Password = migrationPassword
	fixture.migrator, err = pgx.ConnectConfig(t.Context(), migrationConfig)
	if err != nil {
		t.Fatal(err)
	}

	if err := migrations.Apply(t.Context(), fixture.migrator, "up"); err != nil {
		t.Fatalf("migrate with non-superuser owner: %v", err)
	}

	if _, err := fixture.migrator.Exec(t.Context(), "SELECT set_config('jungle.runtime_role', $1, false)", runtimeRole); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		if _, err := fixture.migrator.Exec(t.Context(), permissionSQL(t, "runtime")); err != nil {
			t.Fatal(err)
		}
	}

	runtimeConfig := databaseConfig.Copy()
	runtimeConfig.User = runtimeRole
	runtimeConfig.Password = runtimePassword
	poolConfig, err := pgxpool.ParseConfig(runtimeConfig.ConnString())
	if err != nil {
		t.Fatal(err)
	}

	poolConfig.ConnConfig = runtimeConfig
	fixture.runtime, err = pgxpool.NewWithConfig(t.Context(), poolConfig)
	if err != nil {
		t.Fatal(err)
	}

	fixture.store = postgres.New(fixture.runtime, slog.Default())

	return fixture
}

func TestDatabaseRoleSeparation(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "fresh database"
		if existing {
			name = "existing database preserves data"
		}

		t.Run(name, func(t *testing.T) {
			f := setupPermissions(t, existing)
			state := f.wallet

			if !existing {
				state = open(t, f.store)
			}

			if err := f.store.Ready(t.Context()); err != nil {
				t.Fatalf("runtime readiness: %v", err)
			}

			bet := command(t, state, wager.Bet, "20.00")
			bet.MessageID = id(t)
			bet.MessageHash = strings.Repeat("a", 64)
			processed, err := f.store.Submit(t.Context(), bet)
			if err != nil {
				t.Fatalf("runtime financial processing with inbox: %v", err)
			}

			if processed.Status != wager.Processed {
				t.Fatalf("runtime bet = %+v", processed)
			}

			replay, err := f.store.Submit(t.Context(), bet)
			if err != nil {
				t.Fatal(err)
			}

			if !replay.IdempotentReplay {
				t.Fatal("runtime did not recognize persistent replay")
			}

			refund := command(t, state, wager.Refund, "20.00")
			refund.Input.ReferenceExternalTransactionID = bet.Input.ExternalTransactionID
			if _, err := f.store.Submit(t.Context(), refund); err != nil {
				t.Fatalf("runtime reversal: %v", err)
			}

			pending := command(t, state, wager.Refund, "1.00")
			pending.Input.ReferenceExternalTransactionID = "missing"
			waiting, err := f.store.Submit(t.Context(), pending)
			if err != nil {
				t.Fatal(err)
			}

			if _, err := f.admin.Exec(t.Context(), "UPDATE wager_transactions SET next_attempt_at = now() WHERE id = $1", waiting.TransactionID); err != nil {
				t.Fatal(err)
			}

			if err := f.store.RetryReference(t.Context()); err != nil {
				t.Fatalf("runtime reference worker: %v", err)
			}

			delivery, err := f.store.ClaimEvent(t.Context(), time.Minute)
			if err != nil {
				t.Fatalf("runtime publisher claim: %v", err)
			}

			if err := f.store.FinishEvent(t.Context(), delivery, true); err != nil {
				t.Fatalf("runtime publisher completion: %v", err)
			}

			if _, err := f.store.Statistics(t.Context()); err != nil {
				t.Fatalf("runtime statistics: %v", err)
			}

			if _, err := f.store.Ledger(t.Context(), state.ID, "", 100); err != nil {
				t.Fatalf("runtime ledger query: %v", err)
			}

			const futureObjects = `
            CREATE TABLE future_table (id INTEGER);
            CREATE FUNCTION future_function() RETURNS INTEGER LANGUAGE SQL AS 'SELECT 1'`

			if _, err := f.migrator.Exec(t.Context(), futureObjects); err != nil {
				t.Fatalf("migration owner DDL: %v", err)
			}

			denied := []struct {
				name string
				sql  string
			}{
				{name: "create table", sql: "CREATE TABLE runtime_ddl (id INTEGER)"},
				{name: "create schema", sql: "CREATE SCHEMA runtime_ddl"},
				{name: "create temporary table", sql: "CREATE TEMP TABLE runtime_ddl (id INTEGER)"},
				{name: "alter table", sql: "ALTER TABLE wallets ADD COLUMN bypass INTEGER"},
				{name: "drop table", sql: "DROP TABLE wallet_ledger"},
				{name: "disable triggers", sql: "ALTER TABLE wallet_ledger DISABLE TRIGGER ALL"},
				{name: "drop trigger", sql: "DROP TRIGGER ledger_immutable ON wallet_ledger"},
				{name: "create trigger", sql: "CREATE TRIGGER runtime_trigger BEFORE INSERT ON wallet_ledger FOR EACH ROW EXECUTE FUNCTION reject_financial_mutation()"},
				{name: "replace trigger function", sql: "CREATE OR REPLACE FUNCTION check_wallet_ledger() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NULL; END'"},
				{name: "disable replication triggers", sql: "SET session_replication_role = replica"},
				{name: "assume migration owner", sql: "SET ROLE " + pgx.Identifier{f.migrationRole}.Sanitize()},
				{name: "create role", sql: "CREATE ROLE runtime_escalation"},
				{name: "alter ledger", sql: "UPDATE wallet_ledger SET amount = 1"},
				{name: "delete ledger", sql: "DELETE FROM wallet_ledger"},
				{name: "truncate ledger", sql: "TRUNCATE wallet_ledger"},
				{name: "delete outbox", sql: "DELETE FROM outbox"},
				{name: "alter migration history", sql: "UPDATE schema_migrations SET dirty = false"},
				{name: "read future table", sql: "SELECT * FROM future_table"},
				{name: "execute future function", sql: "SELECT future_function()"},
			}

			for _, tt := range denied {
				t.Run(tt.name, func(t *testing.T) {
					_, err := f.runtime.Exec(t.Context(), tt.sql)
					var pgErr *pgconn.PgError

					if !errors.As(err, &pgErr) {
						t.Fatalf("restricted operation error = %v, want PostgreSQL permission denial", err)
					}

					if pgErr.Code != "42501" {
						t.Fatalf("restricted operation SQLSTATE = %s, want 42501", pgErr.Code)
					}
				})
			}

			_, err = f.runtime.Exec(t.Context(), "UPDATE wallets SET balance = balance + 1 WHERE id = $1", state.ID)
			var pgErr *pgconn.PgError

			if !errors.As(err, &pgErr) {
				t.Fatalf("financial consistency trigger error = %v", err)
			}

			if pgErr.Code != "23514" {
				t.Fatalf("financial consistency trigger SQLSTATE = %s, want 23514", pgErr.Code)
			}

			assertBalance(t, f.store, state.ID, "100.00", 3)

			for _, direction := range []string{"down", "up"} {
				if err := migrations.Apply(t.Context(), f.migrator, direction); err != nil {
					t.Fatalf("migration owner %s: %v", direction, err)
				}
			}

			if _, err := f.migrator.Exec(t.Context(), permissionSQL(t, "runtime")); err != nil {
				t.Fatal(err)
			}

			if err := f.store.Ready(t.Context()); err != nil {
				t.Fatalf("runtime readiness after migration round trip: %v", err)
			}

			fresh := open(t, f.store)
			assertBalance(t, f.store, fresh.ID, "100.00", 1)
		})
	}
}
