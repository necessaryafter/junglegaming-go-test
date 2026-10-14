package wallet_test

import (
	"errors"
	"math"
	"testing"
	"time"

	"jungle/internal/wagering/domain/money"
	"jungle/internal/wagering/domain/wallet"
)

func amount(t *testing.T, units int64, currency money.Currency) money.Money {
	t.Helper()

	value, err := money.FromMinorUnits(units, currency)
	if err != nil {
		t.Fatal(err)
	}

	return value
}

func TestMove(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		direction wallet.Direction
		amount    money.Money
		want      int64
		version   int64
		wantErr   error
	}{
		{name: "debit", direction: wallet.Debit, amount: amount(t, 8000, money.BRL), want: 2000, version: 2},
		{name: "credit", direction: wallet.Credit, amount: amount(t, 1000, money.BRL), want: 11000, version: 2},
		{name: "zero", direction: wallet.Debit, amount: amount(t, 0, money.BRL), want: 10000, version: 1},
		{name: "insufficient", direction: wallet.Debit, amount: amount(t, 10001, money.BRL), wantErr: wallet.ErrInsufficientBalance},
		{name: "negative", direction: wallet.Credit, amount: amount(t, -1, money.BRL), wantErr: money.ErrNegativeAmount},
		{name: "different currency", direction: wallet.Credit, amount: amount(t, 1, money.USD), wantErr: money.ErrCurrencyMismatch},
		{name: "invalid money", direction: wallet.Credit, wantErr: money.ErrInvalidMoney},
		{name: "invalid direction", direction: "INVALID", amount: amount(t, 1, money.BRL), wantErr: wallet.ErrInvalid},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original, err := wallet.New("wallet", "player", amount(t, 10000, money.BRL), now)
			if err != nil {
				t.Fatal(err)
			}

			before := original.Snapshot()
			moved, err := original.Move(tt.direction, tt.amount, now.Add(time.Second))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Move() error = %v, want %v", err, tt.wantErr)
			}

			if original.Snapshot() != before {
				t.Fatal("Move changed original aggregate")
			}

			if err != nil {
				return
			}

			state := moved.Snapshot()
			units, err := state.Balance.MinorUnits()
			if err != nil {
				t.Fatal(err)
			}

			if units != tt.want || state.Version != tt.version {
				t.Fatalf("balance/version = %d/%d, want %d/%d", units, state.Version, tt.want, tt.version)
			}
		})
	}
}

func TestRestoreAndBoundaries(t *testing.T) {
	now := time.Now().UTC()
	state := wallet.Snapshot{ID: "wallet", PlayerID: "player", Balance: amount(t, math.MaxInt64, money.BRL), Version: 3, CreatedAt: now, UpdatedAt: now}
	value, err := wallet.Restore(state)
	if err != nil || value.Snapshot() != state {
		t.Fatalf("Restore() = %v, %v", value, err)
	}

	if _, err := value.Move(wallet.Credit, amount(t, 1, money.BRL), now); !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("Move() error = %v, want overflow", err)
	}

	state.Version = math.MaxInt64
	value, err = wallet.Restore(state)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := value.Move(wallet.Debit, amount(t, 1, money.BRL), now); !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("version overflow error = %v", err)
	}

	if _, err := wallet.Restore(wallet.Snapshot{}); !errors.Is(err, wallet.ErrInvalid) {
		t.Fatalf("zero state error = %v", err)
	}

	state.Balance = amount(t, -1, money.BRL)
	if _, err := wallet.Restore(state); !errors.Is(err, money.ErrNegativeAmount) {
		t.Fatalf("negative balance error = %v", err)
	}
}

func TestLedgerEntryEquation(t *testing.T) {
	state := wallet.EntrySnapshot{ID: "entry", WalletID: "wallet", TransactionID: "transaction",
		Direction: wallet.Debit, Money: amount(t, 8000, money.BRL), BalanceBefore: amount(t, 10000, money.BRL),
		BalanceAfter: amount(t, 2000, money.BRL), CreatedAt: time.Now().UTC()}

	entry, err := wallet.NewEntry(state)
	if err != nil || entry.Snapshot() != state {
		t.Fatalf("NewEntry() = %v, %v", entry, err)
	}

	state.BalanceAfter = amount(t, 2001, money.BRL)
	if _, err := wallet.NewEntry(state); !errors.Is(err, wallet.ErrInvalid) {
		t.Fatalf("invalid equation error = %v", err)
	}
}
