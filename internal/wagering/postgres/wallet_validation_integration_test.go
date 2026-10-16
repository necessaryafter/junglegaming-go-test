//go:build integration

package postgres_test

import (
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"jungle/internal/wagering/domain/wager"
)

func TestWalletValidationMigrationPreservesFinancialState(t *testing.T) {
	f := setup(t)
	state := open(t, f.stores[0])
	input := command(t, state, wager.Bet, "20.00")

	result, err := f.stores[0].Submit(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}

	for _, direction := range []string{"down", "up"} {
		t.Run(direction, func(t *testing.T) {
			// Reverse only migration 4 on populated tables, then reinstall it.
			// Full runner up/down and legacy adoption are covered separately.
			sql, err := os.ReadFile("../../../migrations/000004_wallet_validation." + direction + ".sql")
			if err != nil {
				t.Fatal(err)
			}

			if _, err := f.pool.Exec(t.Context(), string(sql)); err != nil {
				t.Fatal(err)
			}

			var indexed bool
			if err := f.pool.QueryRow(t.Context(), "SELECT to_regclass('transaction_wallet') IS NOT NULL").Scan(&indexed); err != nil {
				t.Fatal(err)
			}

			if indexed != (direction == "up") {
				t.Fatalf("wallet index exists = %t after %s", indexed, direction)
			}

			// Both function versions must reject a balance without its ledger.
			if _, err := f.pool.Exec(t.Context(), "UPDATE wallets SET balance = balance + 1 WHERE id = $1", state.ID); err == nil {
				t.Fatal("accepted balance corruption")
			}

			const missingLedgerQuery = `
    INSERT INTO wager_transactions (
        id, wallet_id, player_id, currency, amount, kind, provider_id,
        external_id, idempotency_key, payload_hash, request, status,
        result_balance, created_at, updated_at
    )
    SELECT
        $1, wallet_id, player_id, currency, amount, kind, provider_id,
        $2, $3, payload_hash, request, status,
        result_balance, created_at, updated_at
    FROM wager_transactions
    WHERE id = $4`

			_, err = f.pool.Exec(t.Context(), missingLedgerQuery, id(t), id(t), id(t), result.TransactionID)
			var constraint *pgconn.PgError

			if !errors.As(err, &constraint) {
				t.Fatalf("missing ledger error = %v, want PostgreSQL constraint error", err)
			}

			if constraint.Code != "23514" {
				t.Fatalf("missing ledger SQLSTATE = %s, want 23514", constraint.Code)
			}

			if constraint.Message != "processed movement requires a ledger entry" {
				t.Fatalf("missing ledger rejected by unexpected rule: %s", constraint.Message)
			}

			assertBalance(t, f.stores[0], state.ID, "80.00", 2)

			replay, err := f.stores[1].Submit(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}

			if !replay.IdempotentReplay {
				t.Fatal("lost persisted idempotency across migration")
			}
		})
	}
}
