//go:build integration

package postgres_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"jungle/internal/wagering/domain/wager"
)

func TestFailedPolicyAndReplay(t *testing.T) {
	f := setup(t)
	state := open(t, f.stores[0])
	request := command(t, state, wager.Refund, "1.00")
	request.Input.ReferenceExternalTransactionID = "missing"
	pending, err := f.stores[0].Submit(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}

	if pending.Status != wager.PendingReference {
		t.Fatalf("status = %s, want PENDING_REFERENCE", pending.Status)
	}

	const failTransaction = `
    UPDATE wager_transactions
    SET
        status = 'FAILED',
        failure_code = $2
    WHERE id = $1`

	for _, code := range []string{"RETRIES_EXHAUSTED", "DATABASE_UNAVAILABLE", "INSUFFICIENT_BALANCE"} {
		t.Run(code, func(t *testing.T) {
			var pgErr *pgconn.PgError

			_, err := f.pool.Exec(t.Context(), failTransaction, pending.TransactionID, code)
			if !errors.As(err, &pgErr) {
				t.Fatalf("unsupported FAILED cause error = %v", err)
			}

			if pgErr.ConstraintName != "transaction_failed_policy" {
				t.Fatalf("constraint = %s, want transaction_failed_policy", pgErr.ConstraintName)
			}
		})
	}

	// Simulate an audited operational decision after confirming irrecoverable
	// loss. No automatic database-error or retry-count classifier can do this.
	_, err = f.pool.Exec(t.Context(), failTransaction, pending.TransactionID, wager.InfrastructureDataLoss)
	if err != nil {
		t.Fatal(err)
	}

	for i, store := range f.stores {
		if i > 0 {
			request.MessageID = id(t)
			request.MessageHash = strings.Repeat("a", 64)
		}

		replay, err := store.Submit(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}

		if !replay.IdempotentReplay {
			t.Fatalf("FAILED result was not recognized as a replay: %+v", replay)
		}

		if replay.Status != wager.Failed {
			t.Fatalf("FAILED replay = %+v", replay)
		}

		if replay.TransactionID != pending.TransactionID {
			t.Fatalf("FAILED replay changed identity: %+v", replay)
		}

		if replay.FailureCode != wager.InfrastructureDataLoss {
			t.Fatalf("FAILED replay changed cause: %+v", replay)
		}

		if replay.Balance != pending.Balance {
			t.Fatalf("FAILED replay changed original balance: %+v", replay)
		}
	}

	const reopenTransaction = `
    UPDATE wager_transactions
    SET
        status = 'PENDING',
        failure_code = ''
    WHERE id = $1`

	_, err = f.pool.Exec(t.Context(), reopenTransaction, pending.TransactionID)
	if err == nil {
		t.Fatal("FAILED transaction was reopened")
	}

	assertBalance(t, f.stores[0], state.ID, "100.00", 1)
}
