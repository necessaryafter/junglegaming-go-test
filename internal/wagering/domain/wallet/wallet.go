// Package wallet owns wallet balances and ledger invariants.
package wallet

import (
	"errors"
	"math"
	"time"

	"jungle/internal/wagering/domain/money"
)

var (
	ErrInvalid             = errors.New("wallet: invalid state")
	ErrInsufficientBalance = errors.New("wallet: insufficient balance")
)

type Direction string

const (
	Debit  Direction = "DEBIT"
	Credit Direction = "CREDIT"
)

// Snapshot is a copy of persisted state; modifying it cannot modify a Wallet.
type Snapshot struct {
	ID        string      `json:"id"`
	PlayerID  string      `json:"playerId"`
	Balance   money.Money `json:"balance"`
	Version   int64       `json:"version"`
	CreatedAt time.Time   `json:"createdAt"`
	UpdatedAt time.Time   `json:"updatedAt"`
}

type Wallet struct{ state Snapshot }

func New(id, playerID string, balance money.Money, now time.Time) (Wallet, error) {
	return Restore(Snapshot{ID: id, PlayerID: playerID, Balance: balance, Version: 1, CreatedAt: now.UTC(), UpdatedAt: now.UTC()})
}

// Restore validates persisted state without applying changes or emitting events.
func Restore(state Snapshot) (Wallet, error) {
	if state.ID == "" || state.PlayerID == "" {
		return Wallet{}, ErrInvalid
	}

	if state.Version < 1 {
		return Wallet{}, ErrInvalid
	}

	if state.CreatedAt.IsZero() || state.UpdatedAt.Before(state.CreatedAt) {
		return Wallet{}, ErrInvalid
	}

	if err := state.Balance.ValidateNonNegative(); err != nil {
		return Wallet{}, err
	}

	state.CreatedAt = state.CreatedAt.UTC()
	state.UpdatedAt = state.UpdatedAt.UTC()

	return Wallet{state: state}, nil
}

func (w Wallet) Snapshot() Snapshot { return w.state }

// Move returns a new aggregate. Persistence must commit it with its ledger entry.
// Zero does not change the balance, version or timestamp.
func (w Wallet) Move(direction Direction, amount money.Money, now time.Time) (Wallet, error) {
	if _, err := Restore(w.state); err != nil {
		return Wallet{}, err
	}

	if err := amount.ValidateNonNegative(); err != nil {
		return Wallet{}, err
	}

	if now.IsZero() || now.Before(w.state.UpdatedAt) {
		return Wallet{}, ErrInvalid
	}

	var balance money.Money
	var err error

	switch direction {
	case Debit:
		balance, err = w.state.Balance.SubtractNonNegative(amount)
	case Credit:
		balance, err = w.state.Balance.Add(amount)
	default:
		return Wallet{}, ErrInvalid
	}

	if errors.Is(err, money.ErrNegativeAmount) {
		return Wallet{}, ErrInsufficientBalance
	}

	if err != nil {
		return Wallet{}, err
	}

	comparison, err := balance.Compare(w.state.Balance)
	if err != nil {
		return Wallet{}, err
	}

	if comparison == 0 {
		return w, nil
	}

	if w.state.Version == math.MaxInt64 {
		return Wallet{}, money.ErrOverflow
	}

	state := w.state
	state.Balance = balance
	state.Version++
	state.UpdatedAt = now.UTC()

	return Restore(state)
}

// Entry is immutable; Snapshot returns a detached representation for persistence.
type Entry struct{ state EntrySnapshot }

type EntrySnapshot struct {
	ID            string      `json:"id"`
	WalletID      string      `json:"walletId"`
	TransactionID string      `json:"transactionId"`
	Direction     Direction   `json:"direction"`
	Money         money.Money `json:"money"`
	BalanceBefore money.Money `json:"balanceBefore"`
	BalanceAfter  money.Money `json:"balanceAfter"`
	CreatedAt     time.Time   `json:"createdAt"`
}

func NewEntry(state EntrySnapshot) (Entry, error) {
	if state.ID == "" || state.WalletID == "" || state.TransactionID == "" {
		return Entry{}, ErrInvalid
	}

	if state.CreatedAt.IsZero() {
		return Entry{}, ErrInvalid
	}

	if err := state.Money.ValidateNonNegative(); err != nil {
		return Entry{}, err
	}

	units, err := state.Money.MinorUnits()
	if err != nil {
		return Entry{}, err
	}

	if units == 0 {
		return Entry{}, ErrInvalid
	}

	if err := state.BalanceBefore.ValidateNonNegative(); err != nil {
		return Entry{}, err
	}

	if err := state.BalanceAfter.ValidateNonNegative(); err != nil {
		return Entry{}, err
	}

	var expected money.Money

	switch state.Direction {
	case Debit:
		expected, err = state.BalanceBefore.SubtractNonNegative(state.Money)
	case Credit:
		expected, err = state.BalanceBefore.Add(state.Money)
	default:
		return Entry{}, ErrInvalid
	}

	if err != nil {
		return Entry{}, err
	}

	comparison, err := expected.Compare(state.BalanceAfter)
	if err != nil {
		return Entry{}, err
	}

	if comparison != 0 {
		return Entry{}, ErrInvalid
	}

	state.CreatedAt = state.CreatedAt.UTC()

	return Entry{state: state}, nil
}

func (e Entry) Snapshot() EntrySnapshot { return e.state }
