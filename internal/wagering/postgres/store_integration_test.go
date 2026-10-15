//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"jungle/internal/wagering/application"
	"jungle/internal/wagering/domain/money"
	"jungle/internal/wagering/domain/wager"
	"jungle/internal/wagering/domain/wallet"
	"jungle/internal/wagering/postgres"
	"jungle/migrations"
)

type fixture struct {
	stores []*postgres.Store
	pool   *pgxpool.Pool
}

func setup(t *testing.T) fixture {
	t.Helper()

	address := os.Getenv("TEST_DATABASE_URL")
	if address == "" {
		t.Skip("TEST_DATABASE_URL is required for real PostgreSQL integration tests")
	}

	ctx := t.Context()
	admin, err := pgxpool.New(ctx, address)
	if err != nil {
		t.Fatal(err)
	}

	id, err := uuid.NewRandom()
	if err != nil {
		admin.Close()
		t.Fatal(err)
	}

	schema := "test_" + id.String()[:8]
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}

	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		defer admin.Close()

		if _, err := admin.Exec(cleanup, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop test schema: %v", err)
		}
	})

	result := fixture{}

	// Each Store owns an independent pool; no shared application state or locks.
	for range 3 {
		cfg, err := pgxpool.ParseConfig(address)
		if err != nil {
			t.Fatal(err)
		}

		cfg.ConnConfig.RuntimeParams["search_path"] = schema
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}

		t.Cleanup(pool.Close)
		result.stores = append(result.stores, postgres.New(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)), nil))
		result.pool = pool
	}

	connection, err := result.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}

	defer connection.Release()

	if err := migrations.Apply(ctx, connection.Conn(), "up"); err != nil {
		t.Fatal(err)
	}

	return result
}

func id(t *testing.T) string {
	t.Helper()

	value, err := uuid.NewRandom()
	if err != nil {
		t.Fatal(err)
	}

	return value.String()
}

func amount(t *testing.T, value string) money.Money {
	t.Helper()

	result, err := money.New(value, money.BRL)
	if err != nil {
		t.Fatal(err)
	}

	return result
}

func open(t *testing.T, store *postgres.Store) wallet.Snapshot {
	t.Helper()

	state, err := store.Open(t.Context(), id(t), amount(t, "100.00"))
	if err != nil {
		t.Fatal(err)
	}

	return state
}

func command(t *testing.T, state wallet.Snapshot, kind wager.Kind, value string) application.Command {
	t.Helper()

	externalID := id(t)

	return application.Command{Key: "provider-a:" + externalID, CorrelationID: externalID, Input: wager.Input{
		ProviderID: "provider-a", ExternalTransactionID: externalID, WalletID: state.ID,
		PlayerID: state.PlayerID, RoundID: "round", GameID: "game", Kind: kind, Money: amount(t, value),
	}}
}

func assertBalance(t *testing.T, store *postgres.Store, walletID, want string, entries int64) {
	t.Helper()

	result, err := store.Reconcile(t.Context(), walletID)
	if err != nil {
		t.Fatal(err)
	}

	got, err := result.StoredBalance.Amount()
	if err != nil {
		t.Fatal(err)
	}

	if !result.Consistent || got != want || result.CheckedEntries != entries {
		t.Fatalf("reconciliation = %+v; want %s, %d entries", result, want, entries)
	}
}

func TestPersistentIdempotencyAcrossPools(t *testing.T) {
	f := setup(t)
	state := open(t, f.stores[0])
	bet := command(t, state, wager.Bet, "80.00")
	results := make(chan application.Result, 50)
	errorsFound := make(chan error, 50)
	start := make(chan struct{})
	var group sync.WaitGroup

	for i := range 50 {
		group.Add(1)

		go func() {
			defer group.Done()
			<-start

			result, err := f.stores[i%3].Submit(t.Context(), bet)
			if err != nil {
				errorsFound <- err

				return
			}

			results <- result
		}()
	}

	close(start)
	group.Wait()
	close(results)
	close(errorsFound)

	for err := range errorsFound {
		t.Error(err)
	}

	replays := 0
	count := 0

	for result := range results {
		count++

		if result.Status != wager.Processed {
			t.Fatalf("duplicate result = %+v", result)
		}

		if result.IdempotentReplay {
			replays++
		}
	}

	if count != 50 || replays != 49 {
		t.Fatalf("results/replays = %d/%d, want 50/49", count, replays)
	}

	assertBalance(t, f.stores[0], state.ID, "20.00", 2)

	win := command(t, state, wager.Win, "10.00")
	if _, err := f.stores[1].Submit(t.Context(), win); err != nil {
		t.Fatal(err)
	}

	bet.MessageID = "same-bet-via-sqs"
	bet.MessageHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	replay, err := f.stores[2].Submit(t.Context(), bet)
	if err != nil {
		t.Fatal(err)
	}

	balance, err := replay.Balance.Amount()
	if err != nil {
		t.Fatal(err)
	}

	if !replay.IdempotentReplay || balance != "20.00" {
		t.Fatalf("original result was not preserved: %+v", replay)
	}

	assertBalance(t, f.stores[0], state.ID, "30.00", 3)

	bet.MessageHash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := f.stores[1].Submit(t.Context(), bet); !errors.Is(err, application.ErrConflict) {
		t.Fatalf("inbox hash conflict = %v", err)
	}

	bet.MessageID = ""
	bet.Input.Money = amount(t, "79.00")
	if _, err := f.stores[1].Submit(t.Context(), bet); !errors.Is(err, application.ErrConflict) {
		t.Fatalf("payload conflict = %v", err)
	}

	bet.Input.Money = amount(t, "80.00")
	bet.Key = "different-key"
	if _, err := f.stores[1].Submit(t.Context(), bet); !errors.Is(err, application.ErrConflict) {
		t.Fatalf("external id conflict = %v", err)
	}

	if _, err := f.stores[0].Transaction(t.Context(), replay.TransactionID, "provider-b", ""); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("cross-provider lookup = %v", err)
	}
}

func TestConcurrentBetsAndLedgerProtection(t *testing.T) {
	f := setup(t)
	state := open(t, f.stores[0])
	first := command(t, state, wager.Bet, "80.00")
	second := command(t, state, wager.Bet, "80.00")
	results := make(chan application.Result, 2)
	failures := make(chan error, 2)
	var group sync.WaitGroup

	for i, request := range []application.Command{first, second} {
		group.Add(1)

		go func() {
			defer group.Done()

			result, err := f.stores[i].Submit(t.Context(), request)
			if err != nil {
				failures <- err

				return
			}

			results <- result
		}()
	}

	group.Wait()
	close(results)
	close(failures)

	for err := range failures {
		t.Fatal(err)
	}

	processed, rejected := 0, 0

	for result := range results {
		if result.Status == wager.Processed {
			processed++
		}

		if result.Status == wager.Rejected && result.FailureCode == "INSUFFICIENT_BALANCE" {
			rejected++
		}
	}

	if processed != 1 || rejected != 1 {
		t.Fatalf("processed/rejected = %d/%d", processed, rejected)
	}

	assertBalance(t, f.stores[0], state.ID, "20.00", 2)

	for _, query := range []string{
		"UPDATE wallet_ledger SET amount=1 WHERE wallet_id=$1",
		"DELETE FROM wallet_ledger WHERE wallet_id=$1",
		"UPDATE wallets SET balance=-1 WHERE id=$1",
		"UPDATE wallets SET balance=balance+1 WHERE id=$1",
	} {
		if _, err := f.pool.Exec(t.Context(), query, state.ID); err == nil {
			t.Fatalf("database accepted invariant violation: %s", query)
		}
	}

	page, err := f.stores[0].Ledger(t.Context(), state.ID, "", 1)
	if err != nil {
		t.Fatal(err)
	}

	if len(page.Entries) != 1 || page.NextCursor == "" {
		t.Fatalf("first ledger page = %+v", page)
	}

	next, err := f.stores[0].Ledger(t.Context(), state.ID, page.NextCursor, 1)
	if err != nil {
		t.Fatal(err)
	}

	if len(next.Entries) != 1 {
		t.Fatalf("next ledger page has %d entries, want 1", len(next.Entries))
	}

	if next.Entries[0].ID == page.Entries[0].ID {
		t.Fatal("next ledger page repeated the previous entry")
	}

	if next.NextCursor != "" {
		t.Fatalf("last ledger page has unexpected cursor %q", next.NextCursor)
	}
}

func TestPendingReferenceAndOutboxRecovery(t *testing.T) {
	f := setup(t)
	state := open(t, f.stores[0])
	bet := command(t, state, wager.Bet, "20.00")
	refund := command(t, state, wager.Refund, "20.00")
	refund.Input.ReferenceExternalTransactionID = bet.Input.ExternalTransactionID

	pending, err := f.stores[0].Submit(t.Context(), refund)
	if err != nil || pending.Status != wager.PendingReference {
		t.Fatalf("pending = %+v, %v", pending, err)
	}

	if _, err := f.stores[1].Submit(t.Context(), bet); err != nil {
		t.Fatal(err)
	}

	if _, err := f.pool.Exec(t.Context(), "UPDATE wager_transactions SET next_attempt_at=now() WHERE id=$1", pending.TransactionID); err != nil {
		t.Fatal(err)
	}

	if err := f.stores[2].RetryReference(t.Context()); err != nil {
		t.Fatal(err)
	}

	assertBalance(t, f.stores[0], state.ID, "100.00", 3)

	rollback := command(t, state, wager.Rollback, "20.00")
	rollback.Input.ReferenceExternalTransactionID = bet.Input.ExternalTransactionID
	result, err := f.stores[1].Submit(t.Context(), rollback)
	if err != nil || result.FailureCode != "REFERENCE_ALREADY_REVERSED" {
		t.Fatalf("duplicate reversal = %+v, %v", result, err)
	}

	first, err := f.stores[0].ClaimEvent(t.Context(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	second, err := f.stores[1].ClaimEvent(t.Context(), time.Minute)
	if err != nil || second.ID == first.ID {
		t.Fatalf("competing publisher claimed same event: %+v, %v", second, err)
	}

	if _, err := f.pool.Exec(t.Context(), "UPDATE outbox SET lease_until=now()-interval '1 second' WHERE event_id=$1", first.ID); err != nil {
		t.Fatal(err)
	}

	recovered, err := f.stores[2].ClaimEvent(t.Context(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	if recovered.ID != first.ID {
		t.Fatalf("recovered event ID = %s, want %s", recovered.ID, first.ID)
	}

	if string(recovered.Payload) != string(first.Payload) {
		t.Fatal("abandoned event recovery changed the payload")
	}

	if err := f.stores[0].FinishEvent(t.Context(), first, true); err != nil {
		t.Fatal(err)
	}

	var published bool

	if err := f.pool.QueryRow(t.Context(), "SELECT published_at IS NOT NULL FROM outbox WHERE event_id=$1", first.ID).Scan(&published); err != nil {
		t.Fatal(err)
	}

	if published {
		t.Fatal("stale publisher acknowledged another publisher's lease")
	}

	if err := f.stores[2].FinishEvent(t.Context(), recovered, true); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationRoundTrip(t *testing.T) {
	f := setup(t)
	connection, err := f.pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	defer connection.Release()

	for _, direction := range []string{"up", "down", "down", "up"} {
		if err := migrations.Apply(t.Context(), connection.Conn(), direction); err != nil {
			t.Fatalf("migration %s: %v", direction, err)
		}
	}
}
