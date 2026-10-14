//go:build integration

package postgres_test

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"jungle/internal/wagering/application"
	"jungle/internal/wagering/domain/wager"
	"jungle/internal/wagering/domain/wallet"
)

type reversalStep struct {
	kind      wager.Kind
	amount    string
	reference int // One-based index of an earlier step; zero means no reference.
	code      string
	direction wallet.Direction
}

func TestReversalMatrix(t *testing.T) {
	tests := []struct {
		name    string
		steps   []reversalStep
		balance string
		entries int64
	}{
		{
			name: "refund bet",
			steps: []reversalStep{
				{wager.Bet, "20.00", 0, "", wallet.Debit},
				{wager.Refund, "20.00", 1, "", wallet.Credit},
			},
			balance: "100.00",
			entries: 3,
		},
		{
			name: "rollback bet",
			steps: []reversalStep{
				{wager.Bet, "20.00", 0, "", wallet.Debit},
				{wager.Rollback, "20.00", 1, "", wallet.Credit},
			},
			balance: "100.00",
			entries: 3,
		},
		{
			name: "rollback win",
			steps: []reversalStep{
				{wager.Win, "20.00", 0, "", wallet.Credit},
				{wager.Rollback, "20.00", 1, "", wallet.Debit},
			},
			balance: "100.00",
			entries: 3,
		},
		{
			name: "rollback refund",
			steps: []reversalStep{
				{wager.Bet, "20.00", 0, "", wallet.Debit},
				{wager.Refund, "20.00", 1, "", wallet.Credit},
				{wager.Rollback, "20.00", 2, "", wallet.Debit},
			},
			balance: "80.00",
			entries: 4,
		},
		{
			name: "duplicate refund",
			steps: []reversalStep{
				{wager.Bet, "20.00", 0, "", wallet.Debit},
				{wager.Refund, "20.00", 1, "", wallet.Credit},
				{wager.Refund, "20.00", 1, "REFERENCE_ALREADY_REVERSED", ""},
			},
			balance: "100.00",
			entries: 3,
		},
		{
			name: "duplicate rollback",
			steps: []reversalStep{
				{wager.Win, "20.00", 0, "", wallet.Credit},
				{wager.Rollback, "20.00", 1, "", wallet.Debit},
				{wager.Rollback, "20.00", 1, "REFERENCE_ALREADY_REVERSED", ""},
			},
			balance: "100.00",
			entries: 3,
		},
		{
			name: "refund then rollback same bet",
			steps: []reversalStep{
				{wager.Bet, "20.00", 0, "", wallet.Debit},
				{wager.Refund, "20.00", 1, "", wallet.Credit},
				{wager.Rollback, "20.00", 1, "REFERENCE_ALREADY_REVERSED", ""},
			},
			balance: "100.00",
			entries: 3,
		},
		{
			name: "rollback then refund same bet",
			steps: []reversalStep{
				{wager.Bet, "20.00", 0, "", wallet.Debit},
				{wager.Rollback, "20.00", 1, "", wallet.Credit},
				{wager.Refund, "20.00", 1, "REFERENCE_ALREADY_REVERSED", ""},
			},
			balance: "100.00",
			entries: 3,
		},
		{
			name: "rollback refund does not reopen bet",
			steps: []reversalStep{
				{wager.Bet, "20.00", 0, "", wallet.Debit},
				{wager.Refund, "20.00", 1, "", wallet.Credit},
				{wager.Rollback, "20.00", 2, "", wallet.Debit},
				{wager.Refund, "20.00", 1, "REFERENCE_ALREADY_REVERSED", ""},
				{wager.Rollback, "20.00", 1, "REFERENCE_ALREADY_REVERSED", ""},
			},
			balance: "80.00",
			entries: 4,
		},
		{
			name: "refund win rejected",
			steps: []reversalStep{
				{wager.Win, "20.00", 0, "", wallet.Credit},
				{wager.Refund, "20.00", 1, "REFERENCE_KIND_INVALID", ""},
			},
			balance: "120.00",
			entries: 2,
		},
		{
			name: "rollback rollback rejected",
			steps: []reversalStep{
				{wager.Bet, "20.00", 0, "", wallet.Debit},
				{wager.Rollback, "20.00", 1, "", wallet.Credit},
				{wager.Rollback, "20.00", 2, "REFERENCE_KIND_INVALID", ""},
			},
			balance: "100.00",
			entries: 3,
		},
		{
			name: "rollback loss rejected",
			steps: []reversalStep{
				{wager.Loss, "0.00", 0, "", ""},
				{wager.Rollback, "20.00", 1, "REFERENCE_KIND_INVALID", ""},
			},
			balance: "100.00",
			entries: 1,
		},
		{
			name: "partial refund rejected then full refund",
			steps: []reversalStep{
				{wager.Bet, "20.00", 0, "", wallet.Debit},
				{wager.Refund, "10.00", 1, "REFERENCE_AMOUNT_MISMATCH", ""},
				{wager.Refund, "20.00", 1, "", wallet.Credit},
			},
			balance: "100.00",
			entries: 3,
		},
		{
			name: "excess rollback rejected",
			steps: []reversalStep{
				{wager.Win, "20.00", 0, "", wallet.Credit},
				{wager.Rollback, "30.00", 1, "REFERENCE_AMOUNT_MISMATCH", ""},
			},
			balance: "120.00",
			entries: 2,
		},
		{
			name: "rejected reference",
			steps: []reversalStep{
				{wager.Bet, "101.00", 0, "INSUFFICIENT_BALANCE", ""},
				{wager.Refund, "101.00", 1, "REFERENCE_UNSUCCESSFUL", ""},
				{wager.Rollback, "101.00", 1, "REFERENCE_UNSUCCESSFUL", ""},
			},
			balance: "100.00",
			entries: 1,
		},
		{
			name: "win reversal insufficient balance",
			steps: []reversalStep{
				{wager.Win, "20.00", 0, "", wallet.Credit},
				{wager.Bet, "110.00", 0, "", wallet.Debit},
				{wager.Rollback, "20.00", 1, "REVERSAL_INSUFFICIENT_BALANCE", ""},
			},
			balance: "10.00",
			entries: 3,
		},
		{
			name: "refund reversal insufficient balance",
			steps: []reversalStep{
				{wager.Bet, "20.00", 0, "", wallet.Debit},
				{wager.Refund, "20.00", 1, "", wallet.Credit},
				{wager.Bet, "90.00", 0, "", wallet.Debit},
				{wager.Rollback, "20.00", 2, "REVERSAL_INSUFFICIENT_BALANCE", ""},
			},
			balance: "10.00",
			entries: 4,
		},
		{
			name: "failed reversal does not reserve reference",
			steps: []reversalStep{
				{wager.Win, "20.00", 0, "", wallet.Credit},
				{wager.Bet, "110.00", 0, "", wallet.Debit},
				{wager.Rollback, "20.00", 1, "REVERSAL_INSUFFICIENT_BALANCE", ""},
				{wager.Win, "10.00", 0, "", wallet.Credit},
				{wager.Rollback, "20.00", 1, "", wallet.Debit},
			},
			balance: "0.00",
			entries: 5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := setup(t)
			state := open(t, f.stores[0])
			var commands []application.Command
			var results []application.Result

			for i, step := range tt.steps {
				request := command(t, state, step.kind, step.amount)

				if step.reference > 0 {
					request.Input.ReferenceExternalTransactionID = commands[step.reference-1].Input.ExternalTransactionID
				}

				result, err := f.stores[i%3].Submit(t.Context(), request)
				if err != nil {
					t.Fatal(err)
				}

				status := wager.Processed

				if step.code != "" {
					status = wager.Rejected
				}

				if result.Status != status || result.FailureCode != step.code {
					t.Fatalf("step %d: result = %+v; want %s, %q", i, result, status, step.code)
				}

				assertReversalEffects(t, f, result, step.direction)
				commands = append(commands, request)
				results = append(results, result)
			}

			// Replay after later movements must retain each original result and add no effects.
			for i, request := range commands {
				replay, err := f.stores[2].Submit(t.Context(), request)
				if err != nil {
					t.Fatal(err)
				}

				original := results[i]
				original.IdempotentReplay = true

				if replay != original {
					t.Fatalf("replay = %+v; want %+v", replay, original)
				}

				assertReversalEffects(t, f, replay, tt.steps[i].direction)
			}

			assertBalance(t, f.stores[0], state.ID, tt.balance, tt.entries)
			current, err := f.stores[1].Wallet(t.Context(), state.ID)
			if err != nil {
				t.Fatal(err)
			}

			if current.Version != tt.entries {
				t.Fatalf("wallet version = %d; want %d", current.Version, tt.entries)
			}
		})
	}
}

func assertReversalEffects(
	t *testing.T,
	f fixture,
	result application.Result,
	direction wallet.Direction,
) {
	t.Helper()

	persisted, err := f.stores[1].Transaction(t.Context(), result.TransactionID, "provider-a", "")
	if err != nil {
		t.Fatal(err)
	}

	if persisted.Status != result.Status || persisted.FailureCode != result.FailureCode {
		t.Fatalf("persisted transaction = %+v; result = %+v", persisted, result)
	}

	var entries int
	var actualDirection string
	err = f.pool.QueryRow(t.Context(), `SELECT count(*), COALESCE(min(direction),'')
  FROM wallet_ledger WHERE transaction_id=$1`, result.TransactionID).Scan(&entries, &actualDirection)
	if err != nil {
		t.Fatal(err)
	}

	wantEntries := 0

	if direction != "" {
		wantEntries = 1
	}

	if entries != wantEntries || actualDirection != string(direction) {
		t.Fatalf("ledger = %d entries, %q; want %d, %q", entries, actualDirection, wantEntries, direction)
	}

	var processed, rejected, changed int
	err = f.pool.QueryRow(t.Context(), `SELECT
  count(*) FILTER (WHERE event_type='WagerTransactionProcessed'),
  count(*) FILTER (WHERE event_type='WagerTransactionRejected' AND payload->'data'->>'failureCode'=$2),
  count(*) FILTER (WHERE event_type='WalletBalanceChanged')
  FROM outbox WHERE payload->'data'->>'transactionId'=$1`, result.TransactionID, result.FailureCode).Scan(
		&processed,
		&rejected,
		&changed,
	)
	if err != nil {
		t.Fatal(err)
	}

	wantProcessed, wantRejected := 0, 0

	switch result.Status {
	case wager.Processed:
		wantProcessed = 1
	case wager.Rejected:
		wantRejected = 1
	}

	if processed != wantProcessed || rejected != wantRejected {
		t.Fatalf("transaction events = %d processed, %d rejected; want %d, %d", processed, rejected, wantProcessed, wantRejected)
	}

	if changed != wantEntries {
		t.Fatalf("balance events = %d; want %d", changed, wantEntries)
	}
}

func TestCompetingReversals(t *testing.T) {
	for _, kinds := range [][2]wager.Kind{{wager.Refund, wager.Refund}, {wager.Rollback, wager.Rollback}, {wager.Refund, wager.Rollback}} {
		t.Run(string(kinds[0])+" against "+string(kinds[1]), func(t *testing.T) {
			f := setup(t)
			state := open(t, f.stores[0])
			bet := command(t, state, wager.Bet, "20.00")
			if _, err := f.stores[0].Submit(t.Context(), bet); err != nil {
				t.Fatal(err)
			}

			requests := make([]application.Command, 2)
			results := make([]application.Result, 2)
			failures := make([]error, 2)
			start := make(chan struct{})
			var group sync.WaitGroup

			for i, kind := range kinds {
				requests[i] = command(t, state, kind, "20.00")
				requests[i].Input.ReferenceExternalTransactionID = bet.Input.ExternalTransactionID
				group.Add(1)

				go func() {
					defer group.Done()
					<-start

					results[i], failures[i] = f.stores[i+1].Submit(t.Context(), requests[i])
				}()
			}

			close(start)
			group.Wait()
			processed, rejected := 0, 0

			for i, result := range results {
				if failures[i] != nil {
					t.Fatal(failures[i])
				}

				switch result.Status {
				case wager.Processed:
					processed++
					assertReversalEffects(t, f, result, wallet.Credit)
				case wager.Rejected:
					rejected++

					if result.FailureCode != "REFERENCE_ALREADY_REVERSED" {
						t.Fatalf("rejection = %+v", result)
					}

					assertReversalEffects(t, f, result, "")
				default:
					t.Fatalf("unexpected result = %+v", result)
				}
			}

			if processed != 1 || rejected != 1 {
				t.Fatalf("processed/rejected = %d/%d; want 1/1", processed, rejected)
			}

			assertBalance(t, f.stores[0], state.ID, "100.00", 3)
		})
	}
}

// Insert at a historical time rather than waiting 24 hours or disabling immutable identity triggers.
func insertExpiredReference(
	t *testing.T,
	f fixture,
	request application.Command,
) string {
	t.Helper()

	payload, err := json.Marshal(request.Input)
	if err != nil {
		t.Fatal(err)
	}

	hash, err := request.Input.Hash()
	if err != nil {
		t.Fatal(err)
	}

	units, err := request.Input.Money.MinorUnits()
	if err != nil {
		t.Fatal(err)
	}

	transactionID := id(t)
	_, err = f.pool.Exec(t.Context(), `INSERT INTO wager_transactions
  (id,wallet_id,player_id,currency,amount,kind,provider_id,external_id,
   idempotency_key,payload_hash,request,status,result_balance,created_at,updated_at)
  SELECT $1,w.id,w.player_id,w.currency,$3,$4,$5,$6,$7,$8,$9,
   'PENDING_REFERENCE',w.balance,now()-interval '25 hours',now()-interval '25 hours'
  FROM wallets AS w WHERE w.id=$2`,
		transactionID,
		request.Input.WalletID,
		units,
		request.Input.Kind,
		request.Input.ProviderID,
		request.Input.ExternalTransactionID,
		request.Key,
		hash,
		payload,
	)
	if err != nil {
		t.Fatal(err)
	}

	return transactionID
}

func TestReferenceRetryMatrix(t *testing.T) {
	for _, kind := range []wager.Kind{wager.Refund, wager.Rollback} {
		t.Run(string(kind), func(t *testing.T) {
			for _, scenario := range []string{"missing", "arrives", "rejected", "pending", "expired missing", "expired pending", "expired arrives"} {
				t.Run(scenario, func(t *testing.T) {
					f := setup(t)
					state := open(t, f.stores[0])
					bet := command(t, state, wager.Bet, "20.00")
					request := command(t, state, kind, "20.00")
					request.Input.ReferenceExternalTransactionID = bet.Input.ExternalTransactionID
					var transactionID string

					switch scenario {
					case "expired missing", "expired pending", "expired arrives":
						transactionID = insertExpiredReference(t, f, request)
					default:
						result, err := f.stores[0].Submit(t.Context(), request)
						if err != nil {
							t.Fatal(err)
						}

						if result.Status != wager.PendingReference {
							t.Fatalf("initial result = %+v", result)
						}

						transactionID = result.TransactionID
					}

					switch scenario {
					case "arrives", "expired arrives", "rejected":
						if scenario == "rejected" {
							drain := command(t, state, wager.Bet, "90.00")
							if _, err := f.stores[0].Submit(t.Context(), drain); err != nil {
								t.Fatal(err)
							}
						}

						if _, err := f.stores[1].Submit(t.Context(), bet); err != nil {
							t.Fatal(err)
						}

					case "pending", "expired pending":
						// A reference can itself await an earlier operation.
						bet.Input.Kind = wager.Rollback
						bet.Input.ReferenceExternalTransactionID = id(t)
						if _, err := f.stores[1].Submit(t.Context(), bet); err != nil {
							t.Fatal(err)
						}

						if _, err := f.pool.Exec(t.Context(), `UPDATE wager_transactions SET next_attempt_at=now()+interval '1 hour'
       WHERE provider_id=$1 AND external_id=$2`, bet.Input.ProviderID, bet.Input.ExternalTransactionID); err != nil {
							t.Fatal(err)
						}
					}

					if _, err := f.pool.Exec(t.Context(), "UPDATE wager_transactions SET next_attempt_at=now() WHERE id=$1", transactionID); err != nil {
						t.Fatal(err)
					}

					before := time.Now()

					if err := f.stores[2].RetryReference(t.Context()); err != nil {
						t.Fatal(err)
					}

					current, err := f.stores[0].Transaction(t.Context(), transactionID, "provider-a", "")
					if err != nil {
						t.Fatal(err)
					}

					wantStatus, wantCode, balance, entries := wager.PendingReference, "", "100.00", int64(1)

					switch scenario {
					case "arrives", "expired arrives":
						wantStatus, entries = wager.Processed, 3
					case "rejected":
						wantStatus, wantCode, balance, entries = wager.Rejected, "REFERENCE_UNSUCCESSFUL", "10.00", 2
					case "expired missing", "expired pending":
						wantStatus, wantCode = wager.Rejected, "REFERENCE_NOT_FOUND"
					}

					if current.Status != wantStatus || current.FailureCode != wantCode {
						t.Fatalf("retry state = %s, %q; want %s, %q", current.Status, current.FailureCode, wantStatus, wantCode)
					}

					direction := wallet.Direction("")

					if wantStatus == wager.Processed {
						direction = wallet.Credit
					}

					assertReversalEffects(t, f, application.Result{
						TransactionID: transactionID,
						Status:        current.Status,
						FailureCode:   current.FailureCode,
					}, direction)
					assertBalance(t, f.stores[0], state.ID, balance, entries)

					var attempts int
					var next time.Time
					err = f.pool.QueryRow(t.Context(), "SELECT attempts,next_attempt_at FROM wager_transactions WHERE id=$1", transactionID).Scan(&attempts, &next)
					if err != nil {
						t.Fatal(err)
					}

					wantAttempts := 2
					switch scenario {
					case "expired missing", "expired pending", "expired arrives":
						wantAttempts = 1
					}

					if attempts != wantAttempts {
						t.Fatalf("attempts = %d; want %d", attempts, wantAttempts)
					}

					wantDelay := time.Second
					if wantAttempts == 2 {
						wantDelay = 2 * time.Second
					}

					if next.Sub(current.UpdatedAt) != wantDelay {
						t.Fatalf("backoff = %s; want %s", next.Sub(current.UpdatedAt), wantDelay)
					}

					var pendingEvents int
					err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM outbox
                        WHERE event_type='WagerTransactionPendingReference'
                        AND payload->'data'->>'transactionId'=$1`, transactionID).Scan(&pendingEvents)
					if err != nil {
						t.Fatal(err)
					}

					wantPendingEvents := 1
					if wantAttempts == 1 {
						wantPendingEvents = 0 // Historical fixtures were inserted without transport events.
					}

					if pendingEvents != wantPendingEvents {
						t.Fatalf("pending events = %d; want %d", pendingEvents, wantPendingEvents)
					}

					if !next.After(before) {
						t.Fatalf("retry not scheduled in the future: %s", next)
					}

					if err := f.stores[1].RetryReference(t.Context()); !errors.Is(err, application.ErrNotFound) {
						t.Fatalf("immediate retry = %v; want no due work", err)
					}

					if wantStatus == wager.Rejected {
						// A late reference cannot resurrect a terminal rejection.
						if scenario == "expired missing" {
							if _, err := f.stores[1].Submit(t.Context(), bet); err != nil {
								t.Fatal(err)
							}
						}

						replay, err := f.stores[2].Submit(t.Context(), request)
						if err != nil {
							t.Fatal(err)
						}

						if !replay.IdempotentReplay {
							t.Fatal("terminal result was not replayed")
						}

						if replay.Status != wager.Rejected || replay.FailureCode != wantCode {
							t.Fatalf("terminal replay = %+v", replay)
						}

						if replay.Balance != current.Balance {
							t.Fatalf("terminal replay changed original balance: %+v", replay)
						}

						assertReversalEffects(t, f, replay, "")
					}
				})
			}
		})
	}
}

func TestReversalReferenceMismatch(t *testing.T) {
	for _, kind := range []wager.Kind{wager.Refund, wager.Rollback} {
		t.Run(string(kind), func(t *testing.T) {
			for _, mismatch := range []string{"round", "wallet and player", "partial amount", "excess amount", "provider scope"} {
				t.Run(mismatch, func(t *testing.T) {
					f := setup(t)
					originalWallet := open(t, f.stores[0])
					targetWallet := originalWallet
					bet := command(t, originalWallet, wager.Bet, "20.00")
					if _, err := f.stores[0].Submit(t.Context(), bet); err != nil {
						t.Fatal(err)
					}

					if mismatch == "wallet and player" {
						targetWallet = open(t, f.stores[1])
					}

					request := command(t, targetWallet, kind, "20.00")
					request.Input.ReferenceExternalTransactionID = bet.Input.ExternalTransactionID
					wantStatus, wantCode := wager.Rejected, "REFERENCE_MISMATCH"

					switch mismatch {
					case "round":
						request.Input.RoundID = "another-round"
					case "partial amount":
						request.Input.Money = amount(t, "10.00")
						wantCode = "REFERENCE_AMOUNT_MISMATCH"
					case "excess amount":
						request.Input.Money = amount(t, "30.00")
						wantCode = "REFERENCE_AMOUNT_MISMATCH"
					case "provider scope":
						// The lookup must not use another provider's matching external ID.
						bet = command(t, originalWallet, wager.Bet, "20.00")
						bet.Input.ProviderID = "provider-b"
						if _, err := f.stores[0].Submit(t.Context(), bet); err != nil {
							t.Fatal(err)
						}

						request.Input.ReferenceExternalTransactionID = bet.Input.ExternalTransactionID
						wantStatus, wantCode = wager.PendingReference, ""
					}

					result, err := f.stores[2].Submit(t.Context(), request)
					if err != nil {
						t.Fatal(err)
					}

					if result.Status != wantStatus || result.FailureCode != wantCode {
						t.Fatalf("result = %+v; want %s, %q", result, wantStatus, wantCode)
					}

					assertReversalEffects(t, f, result, "")
					balance, entries := "80.00", int64(2)

					if mismatch == "provider scope" {
						balance, entries = "60.00", 3
					}

					assertBalance(t, f.stores[0], originalWallet.ID, balance, entries)

					if targetWallet.ID != originalWallet.ID {
						assertBalance(t, f.stores[1], targetWallet.ID, "100.00", 1)
					}
				})
			}
		})
	}
}
