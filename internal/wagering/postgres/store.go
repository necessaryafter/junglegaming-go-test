// Package postgres implements explicit, per-wallet SQL units of work.
package postgres

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"jungle/internal/platform/telemetry"
	"jungle/internal/wagering/application"
	"jungle/internal/wagering/domain/event"
	"jungle/internal/wagering/domain/money"
	"jungle/internal/wagering/domain/wager"
	"jungle/internal/wagering/domain/wallet"
)

type Store struct {
	pool    *pgxpool.Pool
	logger  *slog.Logger
	metrics *telemetry.Metrics
}

// New uses a private metrics registry when metrics is nil.
func New(
	pool *pgxpool.Pool,
	logger *slog.Logger,
	metrics *telemetry.Metrics,
) *Store {
	if metrics == nil {
		metrics = &telemetry.Metrics{}
	}

	return &Store{pool: pool, logger: logger, metrics: metrics}
}

func newID() (string, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}

	return id.String(), nil
}

// rollback uses a bounded cleanup context because the request may be canceled.
func (s *Store) rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		s.logger.Error("rollback failed", "error", err)
	}
}

func classify(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return application.ErrNotFound
	}

	var pgErr *pgconn.PgError

	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return fmt.Errorf("%w: %w", application.ErrConflict, err)
	}

	return err
}

func scanWallet(row pgx.Row) (wallet.Snapshot, error) {
	var state wallet.Snapshot
	var currency money.Currency
	var units int64

	if err := row.Scan(
		&state.ID,
		&state.PlayerID,
		&currency,
		&units,
		&state.Version,
		&state.CreatedAt,
		&state.UpdatedAt,
	); err != nil {
		return wallet.Snapshot{}, classify(err)
	}

	balance, err := money.FromMinorUnits(units, currency)
	if err != nil {
		return wallet.Snapshot{}, err
	}

	state.Balance = balance
	value, err := wallet.Restore(state)
	if err != nil {
		return wallet.Snapshot{}, err
	}

	return value.Snapshot(), nil
}

const walletColumns = "id::text, player_id::text, currency, balance, version, created_at, updated_at"

func (s *Store) Wallet(ctx context.Context, id string) (wallet.Snapshot, error) {
	return scanWallet(s.pool.QueryRow(ctx, "SELECT "+walletColumns+" FROM wallets WHERE id=$1", id))
}

func (s *Store) Open(
	ctx context.Context,
	playerID string,
	initial money.Money,
) (out wallet.Snapshot, err error) {
	defer func() { s.observeConflict(err) }()

	units, err := initial.MinorUnits()
	if err != nil {
		return wallet.Snapshot{}, err
	}

	id, err := newID()
	if err != nil {
		return wallet.Snapshot{}, err
	}

	now := time.Now().UTC()
	value, err := wallet.New(id, playerID, initial, now)
	if err != nil {
		return wallet.Snapshot{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return wallet.Snapshot{}, err
	}

	defer s.rollback(tx)

	_, err = tx.Exec(ctx, `INSERT INTO wallets(id, player_id, currency, balance, version, created_at, updated_at)
		VALUES($1,$2,$3,$4,1,$5,$5)`, id, playerID, initial.Currency(), units, now)
	if err != nil {
		return wallet.Snapshot{}, classify(err)
	}

	if units > 0 {
		transactionID, err := newID()
		if err != nil {
			return wallet.Snapshot{}, err
		}

		_, err = tx.Exec(ctx, `INSERT INTO wager_transactions
			(id,wallet_id,player_id,currency,amount,kind,status,result_balance,created_at,updated_at)
			VALUES($1,$2,$3,$4,$5,'OPENING','PROCESSED',$5,$6,$6)`, transactionID, id, playerID, initial.Currency(), units, now)
		if err != nil {
			return wallet.Snapshot{}, err
		}

		zero, err := money.Zero(initial.Currency())
		if err != nil {
			return wallet.Snapshot{}, err
		}

		if err := s.movement(
			ctx,
			tx,
			transactionID,
			zero,
			value.Snapshot(),
			initial,
			wallet.Credit,
			transactionID,
			"",
			now,
		); err != nil {
			return wallet.Snapshot{}, err
		}

		eventID, err := newID()
		if err != nil {
			return wallet.Snapshot{}, err
		}

		record, err := event.Processed(eventID, transactionID, "", event.TransactionProcessed{TransactionID: transactionID, WalletID: id, Kind: wager.Opening}, now)
		if err != nil {
			return wallet.Snapshot{}, err
		}

		if err := insertEvent(ctx, tx, record); err != nil {
			return wallet.Snapshot{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return wallet.Snapshot{}, classify(err)
	}

	return value.Snapshot(), nil
}

func insertEvent(ctx context.Context, tx pgx.Tx, record event.Record) error {
	_, err := tx.Exec(ctx, `INSERT INTO outbox(event_id,aggregate_id,event_type,payload,occurred_at)
		VALUES($1,$2,$3,$4,$5)`, record.ID, record.AggregateID, record.Type, record.Payload, record.OccurredAt)

	return err
}

func (s *Store) movement(
	ctx context.Context,
	tx pgx.Tx,
	transactionID string,
	before money.Money,
	after wallet.Snapshot,
	amount money.Money,
	direction wallet.Direction,
	correlationID string,
	causationID string,
	now time.Time,
) error {
	id, err := newID()
	if err != nil {
		return err
	}

	entry, err := wallet.NewEntry(wallet.EntrySnapshot{ID: id, WalletID: after.ID, TransactionID: transactionID,
		Direction: direction, Money: amount, BalanceBefore: before, BalanceAfter: after.Balance, CreatedAt: now})
	if err != nil {
		return err
	}

	state := entry.Snapshot()
	amountUnits, err := state.Money.MinorUnits()
	if err != nil {
		return err
	}

	beforeUnits, err := state.BalanceBefore.MinorUnits()
	if err != nil {
		return err
	}

	afterUnits, err := state.BalanceAfter.MinorUnits()
	if err != nil {
		return err
	}

	_, err = tx.Exec(ctx, `INSERT INTO wallet_ledger(id,wallet_id,transaction_id,currency,direction,amount,balance_before,balance_after,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, state.ID, state.WalletID, state.TransactionID,
		state.Money.Currency(), state.Direction, amountUnits, beforeUnits, afterUnits, state.CreatedAt)
	if err != nil {
		return err
	}

	eventID, err := newID()
	if err != nil {
		return err
	}

	record, err := event.Changed(eventID, correlationID, causationID, event.BalanceChanged{
		WalletID: after.ID, TransactionID: transactionID, Direction: direction, Money: amount,
		BalanceBefore: before, BalanceAfter: after.Balance, WalletVersion: after.Version,
	}, now)
	if err != nil {
		return err
	}

	return insertEvent(ctx, tx, record)
}

const transactionColumns = `id::text, request, idempotency_key, payload_hash, status, failure_code,
	result_balance, currency, COALESCE(reference_id::text,''), created_at, updated_at`

func scanTransaction(row pgx.Row) (wager.Snapshot, error) {
	var state wager.Snapshot
	var request []byte
	var units int64
	var currency money.Currency

	if err := row.Scan(&state.ID, &request, &state.Key, &state.Hash, &state.Status, &state.FailureCode,
		&units, &currency, &state.ReferenceID, &state.CreatedAt, &state.UpdatedAt); err != nil {
		return wager.Snapshot{}, classify(err)
	}

	if err := json.Unmarshal(request, &state.Input); err != nil {
		return wager.Snapshot{}, fmt.Errorf("restore transaction input: %w", err)
	}

	balance, err := money.FromMinorUnits(units, currency)
	if err != nil {
		return wager.Snapshot{}, err
	}

	state.Balance = balance
	value, err := wager.Restore(state)
	if err != nil {
		return wager.Snapshot{}, err
	}

	return value.Snapshot(), nil
}

func (s *Store) Transaction(
	ctx context.Context,
	id string,
	providerID string,
	externalID string,
) (wager.Snapshot, error) {
	return scanTransaction(s.pool.QueryRow(ctx, "SELECT "+transactionColumns+` FROM wager_transactions
		WHERE provider_id=$1 AND (id::text=$2 OR ($2='' AND external_id=$3))`, providerID, id, externalID))
}

func result(state wager.Snapshot, replay bool) application.Result {
	return application.Result{TransactionID: state.ID, Status: state.Status, Balance: state.Balance,
		FailureCode: state.FailureCode, IdempotentReplay: replay}
}

// Submit locks only the target wallet. No accepted PENDING row is committed:
// operations finish or become durable PENDING_REFERENCE in the same transaction.
func (s *Store) Submit(ctx context.Context, command application.Command) (application.Result, error) {
	out, err := s.submit(ctx, command)
	s.observeConflict(err)

	if !errors.Is(err, application.ErrConflict) {
		return out, err
	}

	// A uniqueness race may involve another wallet; start a fresh transaction
	// after rollback to read the winner and verify both persistent identities.
	out, err = s.submit(ctx, command)
	s.observeConflict(err)

	return out, err
}

func (s *Store) submit(ctx context.Context, command application.Command) (application.Result, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return application.Result{}, err
	}

	defer s.rollback(tx)

	hash, err := command.Input.Hash()
	if err != nil {
		return application.Result{}, err
	}

	existing, err := findReplay(ctx, tx, command)
	if err == nil {
		return s.commitReplay(ctx, tx, command, existing, hash)
	}

	if !errors.Is(err, application.ErrNotFound) {
		return application.Result{}, err
	}

	state, err := scanWallet(tx.QueryRow(ctx, "SELECT "+walletColumns+" FROM wallets WHERE id=$1 FOR UPDATE", command.Input.WalletID))
	if err != nil {
		return application.Result{}, err
	}

	// Recheck after waiting for the wallet lock: another process may have
	// committed the same request while this transaction was waiting.
	existing, err = findReplay(ctx, tx, command)
	if err == nil {
		return s.commitReplay(ctx, tx, command, existing, hash)
	}

	if !errors.Is(err, application.ErrNotFound) {
		return application.Result{}, err
	}

	if err := s.inbox(ctx, tx, command); err != nil {
		return application.Result{}, err
	}

	if state.PlayerID != command.Input.PlayerID || state.Balance.Currency() != command.Input.Money.Currency() {
		return application.Result{}, application.ErrInvalid
	}

	id, err := newID()
	if err != nil {
		return application.Result{}, err
	}

	now := time.Now().UTC()
	transaction, err := wager.New(id, command.Key, command.Input, state.Balance, now)
	if err != nil {
		return application.Result{}, err
	}

	request, err := json.Marshal(command.Input)
	if err != nil {
		return application.Result{}, err
	}

	units, err := command.Input.Money.MinorUnits()
	if err != nil {
		return application.Result{}, err
	}

	_, err = tx.Exec(ctx, `INSERT INTO wager_transactions
		(id,wallet_id,player_id,currency,amount,kind,provider_id,external_id,idempotency_key,payload_hash,request,status,result_balance,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'PENDING',$12,$13,$13)`, id,
		state.ID, state.PlayerID, state.Balance.Currency(), units, command.Input.Kind,
		command.Input.ProviderID, command.Input.ExternalTransactionID, command.Key, hash, request,
		mustBalance(state), now)
	if err != nil {
		return application.Result{}, classify(err)
	}

	finished, err := s.process(ctx, tx, transaction, state, command.CorrelationID, command.MessageID, false)
	if err != nil {
		return application.Result{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return application.Result{}, classify(err)
	}

	return result(finished, false), nil
}

func findReplay(ctx context.Context, tx pgx.Tx, command application.Command) (wager.Snapshot, error) {
	return scanTransaction(tx.QueryRow(ctx, "SELECT "+transactionColumns+` FROM wager_transactions
		WHERE provider_id=$1 AND (idempotency_key=$2 OR external_id=$3) LIMIT 1`,
		command.Input.ProviderID, command.Key, command.Input.ExternalTransactionID))
}

func (s *Store) commitReplay(
	ctx context.Context,
	tx pgx.Tx,
	command application.Command,
	existing wager.Snapshot,
	hash string,
) (application.Result, error) {
	if existing.Hash != hash {
		return application.Result{}, application.ErrConflict
	}

	if existing.Key != command.Key {
		return application.Result{}, application.ErrConflict
	}

	if existing.Input.ExternalTransactionID != command.Input.ExternalTransactionID {
		return application.Result{}, application.ErrConflict
	}

	if err := s.inbox(ctx, tx, command); err != nil {
		return application.Result{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return application.Result{}, err
	}

	return result(existing, true), nil
}

// mustBalance is only called with a rehydrated Wallet whose validation succeeded.
func mustBalance(state wallet.Snapshot) int64 {
	units, _ := state.Balance.MinorUnits() // Restore already verified this value.

	return units
}

func (s *Store) inbox(ctx context.Context, tx pgx.Tx, command application.Command) error {
	if command.MessageID == "" {
		return nil
	}

	if len(command.MessageHash) != 64 {
		return application.ErrInvalid
	}

	var hash string

	err := tx.QueryRow(ctx, `INSERT INTO inbox(consumer_name,message_id,payload_hash,completed_at)
		VALUES('wager', $1,$2,now()) ON CONFLICT (consumer_name,message_id)
		DO UPDATE SET payload_hash=inbox.payload_hash RETURNING payload_hash`, command.MessageID, command.MessageHash).Scan(&hash)
	if err != nil {
		return err
	}

	if hash != command.MessageHash {
		return application.ErrConflict
	}

	return nil
}

func (s *Store) process(
	ctx context.Context,
	tx pgx.Tx,
	transaction wager.Transaction,
	state wallet.Snapshot,
	correlationID string,
	causationID string,
	expired bool,
) (wager.Snapshot, error) {
	current := transaction.Snapshot()
	input := current.Input
	var reference *wager.Snapshot

	if input.ReferenceExternalTransactionID != "" {
		found, err := scanTransaction(tx.QueryRow(ctx, "SELECT "+transactionColumns+` FROM wager_transactions
			WHERE provider_id=$1 AND external_id=$2`, input.ProviderID, input.ReferenceExternalTransactionID))
		if err != nil && !errors.Is(err, application.ErrNotFound) {
			return wager.Snapshot{}, err
		}

		if err == nil {
			reference = &found
		}
	}

	direction, failure := input.ReferenceDirection(reference)
	status := wager.Processed
	referenceID := ""

	if reference != nil {
		referenceID = reference.ID
	}

	if failure == "REFERENCE_PENDING" && !expired {
		status = wager.PendingReference
		failure = ""
	} else if failure != "" {
		status = wager.Rejected

		if expired && failure == "REFERENCE_PENDING" {
			failure = "REFERENCE_NOT_FOUND"
		}
	}

	if status == wager.Processed && (input.Kind == wager.Refund || input.Kind == wager.Rollback) {
		var reversed bool

		err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM wager_transactions
			WHERE reference_id=$1 AND status='PROCESSED' AND kind IN ('REFUND','ROLLBACK'))`, referenceID).Scan(&reversed)
		if err != nil {
			return wager.Snapshot{}, err
		}

		if reversed {
			status, failure = wager.Rejected, "REFERENCE_ALREADY_REVERSED"
		}
	}

	now := time.Now().UTC()
	after := state

	if status == wager.Processed && direction != "" {
		aggregate, err := wallet.Restore(state)
		if err != nil {
			return wager.Snapshot{}, err
		}

		moved, err := aggregate.Move(direction, input.Money, now)
		if errors.Is(err, wallet.ErrInsufficientBalance) {
			status, failure = wager.Rejected, "INSUFFICIENT_BALANCE"

			if input.Kind == wager.Rollback {
				failure = "REVERSAL_INSUFFICIENT_BALANCE"
			}
		} else if errors.Is(err, money.ErrOverflow) {
			status, failure = wager.Rejected, "BALANCE_LIMIT_EXCEEDED"
		} else if err != nil {
			return wager.Snapshot{}, err
		} else {
			after = moved.Snapshot()
		}
	}

	finished, err := transaction.Transition(status, after.Balance, failure, referenceID, now)
	if err != nil {
		return wager.Snapshot{}, err
	}

	_, err = tx.Exec(ctx, `UPDATE wager_transactions SET status=$2,failure_code=$3,result_balance=$4,
		reference_id=NULLIF($5,'')::uuid,updated_at=$6::timestamptz,attempts=attempts+1,
		next_attempt_at=$6::timestamptz + make_interval(secs => LEAST(300, power(2, LEAST(attempts,8)))::double precision)
		WHERE id=$1`, current.ID, status, failure, mustBalance(after), referenceID, now)
	if err != nil {
		return wager.Snapshot{}, classify(err)
	}

	if after.Version != state.Version {
		_, err := tx.Exec(ctx, "UPDATE wallets SET balance=$2,version=$3,updated_at=$4 WHERE id=$1", state.ID, mustBalance(after), after.Version, now)
		if err != nil {
			return wager.Snapshot{}, err
		}

		if err := s.movement(
			ctx,
			tx,
			current.ID,
			state.Balance,
			after,
			input.Money,
			direction,
			correlationID,
			causationID,
			now,
		); err != nil {
			return wager.Snapshot{}, err
		}
	}

	if status == wager.PendingReference && current.Status == wager.PendingReference {
		return finished.Snapshot(), nil
	}

	id, err := newID()
	if err != nil {
		return wager.Snapshot{}, err
	}

	var record event.Record

	switch status {
	case wager.Processed:
		record, err = event.Processed(id, correlationID, causationID, event.TransactionProcessed{
			TransactionID: current.ID, WalletID: state.ID, Kind: input.Kind,
			ProviderID: input.ProviderID, ExternalTransactionID: input.ExternalTransactionID,
		}, now)
	case wager.Rejected:
		record, err = event.Rejected(id, correlationID, causationID, event.TransactionRejected{
			TransactionID: current.ID, WalletID: state.ID, FailureCode: failure,
		}, now)
	case wager.PendingReference:
		record, err = event.PendingReference(id, correlationID, causationID, event.TransactionPendingReference{
			TransactionID: current.ID, WalletID: state.ID, ReferenceExternalTransactionID: input.ReferenceExternalTransactionID,
		}, now)
	default:
		return wager.Snapshot{}, wager.ErrInvalid
	}

	if err != nil {
		return wager.Snapshot{}, err
	}

	if err := insertEvent(ctx, tx, record); err != nil {
		return wager.Snapshot{}, err
	}

	return finished.Snapshot(), nil
}

func (s *Store) Ledger(
	ctx context.Context,
	id string,
	cursor string,
	limit int,
) (application.LedgerPage, error) {
	if _, err := s.Wallet(ctx, id); err != nil {
		return application.LedgerPage{}, err
	}

	positionTime := time.Unix(0, 0).UTC()
	positionID := uuid.Nil.String()

	if cursor != "" {
		data, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil {
			return application.LedgerPage{}, application.ErrInvalid
		}

		parts := strings.Split(string(data), "|")
		if len(parts) != 3 || parts[0] != id {
			return application.LedgerPage{}, application.ErrInvalid
		}

		positionTime, err = time.Parse(time.RFC3339Nano, parts[1])
		if err != nil {
			return application.LedgerPage{}, application.ErrInvalid
		}

		if _, err := uuid.Parse(parts[2]); err != nil {
			return application.LedgerPage{}, application.ErrInvalid
		}

		positionID = parts[2]
	}

	rows, err := s.pool.Query(ctx, `SELECT id::text,transaction_id::text,currency,direction,amount,balance_before,balance_after,created_at
		FROM wallet_ledger WHERE wallet_id=$1 AND (created_at,id)>($2,$3::uuid)
		ORDER BY created_at,id LIMIT $4`, id, positionTime, positionID, limit+1)
	if err != nil {
		return application.LedgerPage{}, err
	}

	defer rows.Close()

	page := application.LedgerPage{Entries: make([]wallet.EntrySnapshot, 0, limit)}

	for rows.Next() {
		var state wallet.EntrySnapshot
		var currency money.Currency
		var amount, before, after int64

		state.WalletID = id

		if err := rows.Scan(
			&state.ID,
			&state.TransactionID,
			&currency,
			&state.Direction,
			&amount,
			&before,
			&after,
			&state.CreatedAt,
		); err != nil {
			return application.LedgerPage{}, err
		}

		state.Money, err = money.FromMinorUnits(amount, currency)
		if err != nil {
			return application.LedgerPage{}, err
		}

		state.BalanceBefore, err = money.FromMinorUnits(before, currency)
		if err != nil {
			return application.LedgerPage{}, err
		}

		state.BalanceAfter, err = money.FromMinorUnits(after, currency)
		if err != nil {
			return application.LedgerPage{}, err
		}

		if _, err := wallet.NewEntry(state); err != nil {
			return application.LedgerPage{}, err
		}

		if len(page.Entries) == limit {
			last := page.Entries[len(page.Entries)-1]
			page.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(id + "|" + last.CreatedAt.Format(time.RFC3339Nano) + "|" + last.ID))

			break
		}

		page.Entries = append(page.Entries, state)
	}

	if err := rows.Err(); err != nil {
		return application.LedgerPage{}, err
	}

	return page, nil
}

func (s *Store) Reconcile(ctx context.Context, id string) (application.Reconciliation, error) {
	// One SQL statement gives stored and calculated balances the same snapshot.
	var stored, calculatedText string
	var currency money.Currency
	var count int64

	err := s.pool.QueryRow(ctx, `SELECT w.balance::text,w.currency,
		COALESCE(sum(CASE l.direction WHEN 'CREDIT' THEN l.amount::numeric ELSE -l.amount::numeric END),0)::text,
		count(l.id) FROM wallets w LEFT JOIN wallet_ledger l ON l.wallet_id=w.id WHERE w.id=$1 GROUP BY w.id`, id).Scan(&stored, &currency, &calculatedText, &count)
	if err != nil {
		return application.Reconciliation{}, classify(err)
	}

	storedUnits, err := strconv.ParseInt(stored, 10, 64)
	if err != nil {
		return application.Reconciliation{}, err
	}

	calculatedUnits, err := strconv.ParseInt(calculatedText, 10, 64)
	if err != nil {
		return application.Reconciliation{}, err
	}

	storedBalance, err := money.FromMinorUnits(storedUnits, currency)
	if err != nil {
		return application.Reconciliation{}, err
	}

	calculated, err := money.FromMinorUnits(calculatedUnits, currency)
	if err != nil {
		return application.Reconciliation{}, err
	}

	difference, err := storedBalance.Subtract(calculated)
	if err != nil {
		return application.Reconciliation{}, err
	}

	return application.Reconciliation{WalletID: id, StoredBalance: storedBalance, CalculatedBalance: calculated,
		Difference: difference, Consistent: storedUnits == calculatedUnits, CheckedEntries: count}, nil
}

var _ application.Repository = (*Store)(nil)
