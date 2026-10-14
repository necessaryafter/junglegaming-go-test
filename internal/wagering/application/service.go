// Package application owns authorization and the transport-independent API.
package application

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"

	"jungle/internal/platform/identity"
	"jungle/internal/wagering/domain/money"
	"jungle/internal/wagering/domain/wager"
	"jungle/internal/wagering/domain/wallet"
)

var (
	ErrInvalid   = errors.New("application: invalid request")
	ErrForbidden = errors.New("application: forbidden")
	ErrConflict  = errors.New("application: idempotency or uniqueness conflict")
	ErrNotFound  = errors.New("application: resource not found")
)

// Principal is the authenticated identity accepted by the module's use cases.
type Principal = identity.Principal

type Command struct {
	Input         wager.Input
	Key           string
	CorrelationID string
	MessageID     string
	MessageHash   string
}

type Result struct {
	TransactionID    string       `json:"transactionId"`
	Status           wager.Status `json:"status"`
	Balance          money.Money  `json:"balance"`
	FailureCode      string       `json:"failureCode,omitempty"`
	IdempotentReplay bool         `json:"idempotentReplay"`
}

type LedgerPage struct {
	Entries    []wallet.EntrySnapshot `json:"entries"`
	NextCursor string                 `json:"nextCursor,omitempty"`
}

type Reconciliation struct {
	WalletID          string      `json:"walletId"`
	StoredBalance     money.Money `json:"storedBalance"`
	CalculatedBalance money.Money `json:"calculatedBalance"`
	Difference        money.Money `json:"difference"`
	Consistent        bool        `json:"consistent"`
	CheckedEntries    int64       `json:"checkedEntries"`
}

// Repository commits the complete financial operation atomically. HTTP and SQS
// share Submit, including inbox completion when Command carries a message ID.
type Repository interface {
	Open(context.Context, string, money.Money) (wallet.Snapshot, error)
	Wallet(context.Context, string) (wallet.Snapshot, error)
	Submit(context.Context, Command) (Result, error)
	Transaction(context.Context, string, string, string) (wager.Snapshot, error)
	Ledger(context.Context, string, string, int) (LedgerPage, error)
	Reconcile(context.Context, string) (Reconciliation, error)
}

type Service struct {
	repository Repository
}

func New(repository Repository) *Service { return &Service{repository: repository} }

func validID(value string) bool {
	id, err := uuid.Parse(value)

	return err == nil && id != uuid.Nil && id.String() == value
}

func (s *Service) Open(
	ctx context.Context,
	principal Principal,
	playerID string,
	initial money.Money,
) (wallet.Snapshot, error) {
	if !principal.Internal {
		return wallet.Snapshot{}, ErrForbidden
	}

	if !validID(playerID) {
		return wallet.Snapshot{}, ErrInvalid
	}

	if err := initial.ValidateNonNegative(); err != nil {
		return wallet.Snapshot{}, errors.Join(ErrInvalid, err)
	}

	return s.repository.Open(ctx, playerID, initial)
}

func (s *Service) Wallet(ctx context.Context, principal Principal, id string) (wallet.Snapshot, error) {
	if !principal.Internal {
		return wallet.Snapshot{}, ErrForbidden
	}

	if !validID(id) {
		return wallet.Snapshot{}, ErrInvalid
	}

	return s.repository.Wallet(ctx, id)
}

func (s *Service) Submit(ctx context.Context, principal Principal, command Command) (Result, error) {
	if principal.ProviderID == "" || principal.ProviderID != command.Input.ProviderID {
		return Result{}, ErrForbidden
	}

	if !validID(command.Input.PlayerID) || !validID(command.Input.WalletID) {
		return Result{}, ErrInvalid
	}

	if strings.TrimSpace(command.Key) == "" || len(command.Key) > 200 {
		return Result{}, ErrInvalid
	}

	if len(command.CorrelationID) > 200 || len(command.MessageID) > 200 {
		return Result{}, ErrInvalid
	}

	if err := command.Input.Validate(); err != nil {
		return Result{}, errors.Join(ErrInvalid, err)
	}

	if command.CorrelationID == "" {
		command.CorrelationID = command.Input.ExternalTransactionID
	}

	return s.repository.Submit(ctx, command)
}

func (s *Service) Transaction(
	ctx context.Context,
	principal Principal,
	id string,
	providerID string,
	externalID string,
) (wager.Snapshot, error) {
	if principal.ProviderID == "" || (providerID != "" && providerID != principal.ProviderID) {
		return wager.Snapshot{}, ErrForbidden
	}

	if id != "" {
		if !validID(id) {
			return wager.Snapshot{}, ErrInvalid
		}
	} else {
		if externalID == "" || len(externalID) > 200 {
			return wager.Snapshot{}, ErrInvalid
		}
	}

	return s.repository.Transaction(ctx, id, principal.ProviderID, externalID)
}

func (s *Service) Ledger(
	ctx context.Context,
	principal Principal,
	id string,
	cursor string,
	limit int,
) (LedgerPage, error) {
	if !principal.Internal {
		return LedgerPage{}, ErrForbidden
	}

	if !validID(id) {
		return LedgerPage{}, ErrInvalid
	}

	if limit < 1 || limit > 100 {
		return LedgerPage{}, ErrInvalid
	}

	if len(cursor) > 500 {
		return LedgerPage{}, ErrInvalid
	}

	return s.repository.Ledger(ctx, id, cursor, limit)
}

func (s *Service) Reconcile(ctx context.Context, principal Principal, id string) (Reconciliation, error) {
	if !principal.Internal {
		return Reconciliation{}, ErrForbidden
	}

	if !validID(id) {
		return Reconciliation{}, ErrInvalid
	}

	return s.repository.Reconcile(ctx, id)
}
