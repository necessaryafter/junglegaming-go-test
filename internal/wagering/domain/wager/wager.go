// Package wager defines external operations and their durable state machine.
package wager

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"jungle/internal/wagering/domain/money"
	"jungle/internal/wagering/domain/wallet"
)

type Kind string

type Status string

const (
	Opening          Kind   = "OPENING"
	Bet              Kind   = "BET"
	Win              Kind   = "WIN"
	Loss             Kind   = "LOSS"
	Refund           Kind   = "REFUND"
	Rollback         Kind   = "ROLLBACK"
	Pending          Status = "PENDING"
	PendingReference Status = "PENDING_REFERENCE"
	Processed        Status = "PROCESSED"
	Rejected         Status = "REJECTED"
	Failed           Status = "FAILED"
)

var (
	ErrInvalid  = errors.New("wager: invalid input or state")
	ErrTerminal = errors.New("wager: terminal transaction")
)

// Input contains business fields only. Idempotency keys belong to the command.
type Input struct {
	ProviderID                     string      `json:"providerId"`
	ExternalTransactionID          string      `json:"externalTransactionId"`
	PlayerID                       string      `json:"playerId"`
	WalletID                       string      `json:"walletId"`
	RoundID                        string      `json:"roundId"`
	GameID                         string      `json:"gameId"`
	Kind                           Kind        `json:"kind"`
	Money                          money.Money `json:"money"`
	ReferenceExternalTransactionID string      `json:"referenceExternalTransactionId,omitempty"`
}

func (in Input) Validate() error {
	for _, value := range []string{in.ProviderID, in.ExternalTransactionID, in.PlayerID, in.WalletID, in.RoundID, in.GameID} {
		if strings.TrimSpace(value) == "" || len(value) > 200 {
			return ErrInvalid
		}
	}

	if len(in.ReferenceExternalTransactionID) > 200 || in.ReferenceExternalTransactionID == in.ExternalTransactionID {
		return ErrInvalid
	}

	if err := in.Money.ValidateNonNegative(); err != nil {
		return err
	}

	units, err := in.Money.MinorUnits()
	if err != nil {
		return err
	}

	switch in.Kind {
	case Loss:
		if units != 0 || in.ReferenceExternalTransactionID != "" {
			return ErrInvalid
		}

	case Bet, Win, Refund, Rollback:
		if units == 0 {
			return ErrInvalid
		}

	default:
		return ErrInvalid
	}

	if in.Kind == Refund || in.Kind == Rollback {
		if strings.TrimSpace(in.ReferenceExternalTransactionID) == "" {
			return ErrInvalid
		}
	}

	if in.Kind == Bet && in.ReferenceExternalTransactionID != "" {
		return ErrInvalid
	}

	return nil
}

// Hash uses SHA-256 over sorted-key JSON of normalized business fields.
// No transport metadata or idempotency key participates in this hash.
func (in Input) Hash() (string, error) {
	if err := in.Validate(); err != nil {
		return "", err
	}

	amount, err := in.Money.Amount()
	if err != nil {
		return "", err
	}

	canonical := map[string]any{
		"providerId": in.ProviderID, "externalTransactionId": in.ExternalTransactionID,
		"playerId": in.PlayerID, "walletId": in.WalletID, "roundId": in.RoundID,
		"gameId": in.GameID, "kind": in.Kind,
		"money":                          map[string]string{"amount": amount, "currency": string(in.Money.Currency())},
		"referenceExternalTransactionId": in.ReferenceExternalTransactionID,
	}

	data, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:]), nil
}

// ReferenceDirection verifies identity, status, kind and full reversal amount.
// An empty direction denotes LOSS, which never changes a balance.
func (in Input) ReferenceDirection(reference *Snapshot) (wallet.Direction, string) {
	if err := in.Validate(); err != nil {
		return "", "INVALID_OPERATION"
	}

	if in.ReferenceExternalTransactionID != "" {
		if reference == nil {
			return "", "REFERENCE_PENDING"
		}

		if reference.Status == Pending || reference.Status == PendingReference {
			return "", "REFERENCE_PENDING"
		}

		ref := reference.Input
		if reference.Status != Processed {
			return "", "REFERENCE_UNSUCCESSFUL"
		}

		if ref.ExternalTransactionID != in.ReferenceExternalTransactionID {
			return "", "REFERENCE_MISMATCH"
		}

		if ref.ProviderID != in.ProviderID {
			return "", "REFERENCE_MISMATCH"
		}

		if ref.PlayerID != in.PlayerID || ref.WalletID != in.WalletID {
			return "", "REFERENCE_MISMATCH"
		}

		if ref.RoundID != in.RoundID {
			return "", "REFERENCE_MISMATCH"
		}

		if ref.Money.Currency() != in.Money.Currency() {
			return "", "REFERENCE_MISMATCH"
		}

		switch in.Kind {
		case Refund, Win:
			if ref.Kind != Bet {
				return "", "REFERENCE_KIND_INVALID"
			}

		case Rollback:
			switch ref.Kind {
			case Bet, Win, Refund:
			default:
				return "", "REFERENCE_KIND_INVALID"
			}
		}

		if in.Kind == Refund || in.Kind == Rollback {
			comparison, err := in.Money.Compare(ref.Money)
			if err != nil || comparison != 0 {
				return "", "REFERENCE_AMOUNT_MISMATCH"
			}
		}
	}

	switch in.Kind {
	case Bet:
		return wallet.Debit, ""
	case Win, Refund:
		return wallet.Credit, ""
	case Rollback:
		if reference == nil {
			return "", "REFERENCE_PENDING"
		}

		if reference.Input.Kind == Bet {
			return wallet.Credit, ""
		}

		return wallet.Debit, ""
	case Loss:
		return "", ""
	default:
		return "", "INVALID_KIND"
	}
}

type Snapshot struct {
	ID          string      `json:"transactionId"`
	Input       Input       `json:"input"`
	Key         string      `json:"-"`
	Hash        string      `json:"-"`
	Status      Status      `json:"status"`
	FailureCode string      `json:"failureCode,omitempty"`
	Balance     money.Money `json:"balance"`
	ReferenceID string      `json:"referenceId,omitempty"`
	CreatedAt   time.Time   `json:"createdAt"`
	UpdatedAt   time.Time   `json:"updatedAt"`
}

type Transaction struct{ state Snapshot }

func New(id, key string, input Input, balance money.Money, now time.Time) (Transaction, error) {
	hash, err := input.Hash()
	if err != nil {
		return Transaction{}, err
	}

	return Restore(Snapshot{ID: id, Key: key, Input: input, Hash: hash, Balance: balance, Status: Pending, CreatedAt: now.UTC(), UpdatedAt: now.UTC()})
}

func Restore(state Snapshot) (Transaction, error) {
	if state.ID == "" {
		return Transaction{}, ErrInvalid
	}

	if strings.TrimSpace(state.Key) == "" || len(state.Key) > 200 {
		return Transaction{}, ErrInvalid
	}

	if state.CreatedAt.IsZero() || state.UpdatedAt.Before(state.CreatedAt) {
		return Transaction{}, ErrInvalid
	}

	hash, err := state.Input.Hash()
	if err != nil {
		return Transaction{}, err
	}

	if hash != state.Hash {
		return Transaction{}, ErrInvalid
	}

	if state.Balance.Currency() != state.Input.Money.Currency() {
		return Transaction{}, ErrInvalid
	}

	if err := state.Balance.ValidateNonNegative(); err != nil {
		return Transaction{}, err
	}

	if state.Status == PendingReference && state.Input.ReferenceExternalTransactionID == "" {
		return Transaction{}, ErrInvalid
	}

	if state.Status == Processed && state.Input.ReferenceExternalTransactionID != "" {
		if state.ReferenceID == "" {
			return Transaction{}, ErrInvalid
		}
	}

	switch state.Status {
	case Pending, PendingReference, Processed:
		if state.FailureCode != "" {
			return Transaction{}, ErrInvalid
		}

	case Rejected, Failed:
		if state.FailureCode == "" {
			return Transaction{}, ErrInvalid
		}

	default:
		return Transaction{}, ErrInvalid
	}

	state.CreatedAt = state.CreatedAt.UTC()
	state.UpdatedAt = state.UpdatedAt.UTC()

	return Transaction{state: state}, nil
}

func (t Transaction) Snapshot() Snapshot { return t.state }

// Transition permits retries from PENDING_REFERENCE and prevents terminal edits.
func (t Transaction) Transition(
	status Status,
	balance money.Money,
	failureCode string,
	referenceID string,
	now time.Time,
) (Transaction, error) {
	if _, err := Restore(t.state); err != nil {
		return Transaction{}, err
	}

	if t.state.Status != Pending && t.state.Status != PendingReference {
		return Transaction{}, ErrTerminal
	}

	switch status {
	case PendingReference, Processed, Rejected, Failed:
	default:
		return Transaction{}, ErrInvalid
	}

	if now.Before(t.state.UpdatedAt) {
		return Transaction{}, ErrInvalid
	}

	state := t.state
	state.Status = status
	state.Balance = balance
	state.FailureCode = failureCode
	state.ReferenceID = referenceID
	state.UpdatedAt = now.UTC()

	return Restore(state)
}
