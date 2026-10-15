//go:build integration

package postgres_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"jungle/internal/wagering/application"
	"jungle/internal/wagering/domain/wager"
	"jungle/internal/wagering/postgres"
)

func TestReferenceAttemptLogs(t *testing.T) {
	tests := []struct {
		name      string
		resolve   bool
		failWrite bool
		status    wager.Status
	}{
		{name: "still pending", status: wager.PendingReference},
		{name: "resolved", resolve: true, status: wager.Processed},
		{name: "failed write", failWrite: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := setup(t)
			var output bytes.Buffer
			store := postgres.New(fixture.pool, slog.New(slog.NewJSONHandler(&output, nil)), nil)
			state := open(t, store)
			request := command(t, state, wager.Refund, "25.00")
			request.Input.ReferenceExternalTransactionID = "reference-for-logs"
			pending, err := store.Submit(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}

			if tt.resolve {
				original := command(t, state, wager.Bet, "25.00")
				original.Input.ExternalTransactionID = "reference-for-logs"

				if _, err := store.Submit(t.Context(), original); err != nil {
					t.Fatal(err)
				}
			}

			const dueQuery = `
    UPDATE wager_transactions
    SET next_attempt_at = NOW()
    WHERE id = $1`

			if _, err := fixture.pool.Exec(t.Context(), dueQuery, pending.TransactionID); err != nil {
				t.Fatal(err)
			}

			if tt.failWrite {
				const failureQuery = `
    CREATE FUNCTION fail_reference_log_test() RETURNS trigger AS $$
    BEGIN
        RAISE EXCEPTION 'sensitive-reference-secret';
    END;
    $$ LANGUAGE plpgsql;

    CREATE TRIGGER fail_reference_log_test
    BEFORE UPDATE ON wager_transactions
    FOR EACH ROW EXECUTE FUNCTION fail_reference_log_test()`

				if _, err := fixture.pool.Exec(t.Context(), failureQuery); err != nil {
					t.Fatal(err)
				}
			}

			output.Reset()
			err = store.RetryReference(t.Context())
			if (err != nil) != tt.failWrite {
				t.Fatalf("reference retry = %v, want failed write %t", err, tt.failWrite)
			}

			logged := output.String()
			decoder := json.NewDecoder(strings.NewReader(logged))
			var record map[string]any
			if err := decoder.Decode(&record); err != nil {
				t.Fatal(err)
			}

			for key, want := range map[string]any{
				"operation": "reference_retry", "walletId": state.ID,
				"transactionId": pending.TransactionID, "providerId": "provider-a",
				"correlationId": request.Input.ExternalTransactionID,
			} {
				if record[key] != want {
					t.Errorf("%s = %v, want %v", key, record[key], want)
				}
			}

			if tt.failWrite {
				if record["msg"] != "reference retry failed" || record["level"] != "ERROR" {
					t.Fatalf("write failure was logged as successful: %v", record)
				}
			} else if record["status"] != string(tt.status) {
				t.Fatalf("logged status = %v, want %s", record["status"], tt.status)
			}

			for _, forbidden := range []string{"sensitive-reference-secret", "money", "balance", "payload", "playerId"} {
				if strings.Contains(logged, forbidden) {
					t.Fatalf("log exposed %s", forbidden)
				}
			}

			if err := decoder.Decode(&record); !errors.Is(err, io.EOF) {
				t.Fatalf("unexpected duplicate log: %v", err)
			}

			if tt.failWrite {
				return
			}

			output.Reset()

			if err := store.RetryReference(t.Context()); !errors.Is(err, application.ErrNotFound) {
				t.Fatalf("empty reference poll = %v", err)
			}

			if output.Len() != 0 {
				t.Fatal("empty polls should not produce logs")
			}
		})
	}
}
