// Package event defines immutable integration snapshots with typed payloads.
package event

import (
	"encoding/json"
	"errors"
	"time"

	"jungle/internal/wagering/domain/money"
	"jungle/internal/wagering/domain/wager"
	"jungle/internal/wagering/domain/wallet"
)

// Envelope fixes metadata at construction; Data contains a concrete event type.
type Envelope[T any] struct {
	EventID       string    `json:"eventId"`
	EventType     string    `json:"eventType"`
	AggregateID   string    `json:"aggregateId"`
	CorrelationID string    `json:"correlationId"`
	CausationID   string    `json:"causationId,omitempty"`
	OccurredAt    time.Time `json:"occurredAt"`
	Version       int       `json:"version"`
	Data          T         `json:"data"`
}

type TransactionProcessed struct {
	TransactionID         string     `json:"transactionId"`
	WalletID              string     `json:"walletId"`
	Kind                  wager.Kind `json:"kind"`
	ProviderID            string     `json:"providerId,omitempty"`
	ExternalTransactionID string     `json:"externalTransactionId,omitempty"`
}

type TransactionRejected struct {
	TransactionID string `json:"transactionId"`
	WalletID      string `json:"walletId"`
	FailureCode   string `json:"failureCode"`
}

type TransactionPendingReference struct {
	TransactionID                  string `json:"transactionId"`
	WalletID                       string `json:"walletId"`
	ReferenceExternalTransactionID string `json:"referenceExternalTransactionId"`
}

type BalanceChanged struct {
	WalletID      string           `json:"walletId"`
	TransactionID string           `json:"transactionId"`
	Direction     wallet.Direction `json:"direction"`
	Money         money.Money      `json:"money"`
	BalanceBefore money.Money      `json:"balanceBefore"`
	BalanceAfter  money.Money      `json:"balanceAfter"`
	WalletVersion int64            `json:"walletVersion"`
}

// Record contains encoded bytes for persistence. Workers publish this exact
// snapshot; retries must retain the ID and payload.
type Record struct {
	ID          string
	AggregateID string
	Type        string
	Payload     json.RawMessage
	OccurredAt  time.Time
}

func Processed(
	id string,
	correlationID string,
	causationID string,
	data TransactionProcessed,
	now time.Time,
) (Record, error) {
	if data.TransactionID == "" {
		return Record{}, wager.ErrInvalid
	}

	switch data.Kind {
	case wager.Opening:
		if data.ProviderID != "" || data.ExternalTransactionID != "" {
			return Record{}, wager.ErrInvalid
		}
	case wager.Bet, wager.Win, wager.Loss, wager.Refund, wager.Rollback:
		if data.ProviderID == "" || data.ExternalTransactionID == "" {
			return Record{}, wager.ErrInvalid
		}
	default:
		return Record{}, wager.ErrInvalid
	}

	return encode(id, "WagerTransactionProcessed", data.WalletID, correlationID, causationID, data, now)
}

func Rejected(
	id string,
	correlationID string,
	causationID string,
	data TransactionRejected,
	now time.Time,
) (Record, error) {
	if data.TransactionID == "" || data.FailureCode == "" {
		return Record{}, wager.ErrInvalid
	}

	return encode(id, "WagerTransactionRejected", data.WalletID, correlationID, causationID, data, now)
}

func PendingReference(
	id string,
	correlationID string,
	causationID string,
	data TransactionPendingReference,
	now time.Time,
) (Record, error) {
	if data.TransactionID == "" || data.ReferenceExternalTransactionID == "" {
		return Record{}, wager.ErrInvalid
	}

	return encode(id, "WagerTransactionPendingReference", data.WalletID, correlationID, causationID, data, now)
}

func Changed(
	id string,
	correlationID string,
	causationID string,
	data BalanceChanged,
	now time.Time,
) (Record, error) {
	_, err := wallet.NewEntry(wallet.EntrySnapshot{
		ID: id, WalletID: data.WalletID, TransactionID: data.TransactionID,
		Direction: data.Direction, Money: data.Money, BalanceBefore: data.BalanceBefore,
		BalanceAfter: data.BalanceAfter, CreatedAt: now,
	})
	if err != nil {
		return Record{}, err
	}

	if data.WalletVersion < 1 {
		return Record{}, wallet.ErrInvalid
	}

	return encode(id, "WalletBalanceChanged", data.WalletID, correlationID, causationID, data, now)
}

func encode[T any](
	id string,
	eventType string,
	aggregateID string,
	correlationID string,
	causationID string,
	data T,
	now time.Time,
) (Record, error) {
	if id == "" || aggregateID == "" || correlationID == "" {
		return Record{}, errors.New("event: missing metadata")
	}

	if now.IsZero() {
		return Record{}, errors.New("event: missing metadata")
	}

	payload, err := json.Marshal(Envelope[T]{
		EventID: id, EventType: eventType, AggregateID: aggregateID,
		CorrelationID: correlationID, CausationID: causationID,
		OccurredAt: now.UTC(), Version: 1, Data: data,
	})
	if err != nil {
		return Record{}, err
	}

	return Record{ID: id, AggregateID: aggregateID, Type: eventType, Payload: payload, OccurredAt: now.UTC()}, nil
}
