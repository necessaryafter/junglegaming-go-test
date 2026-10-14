package postgres

import (
	"context"
	"time"

	"jungle/internal/wagering/domain/wager"
)

// RetryReference locks the wallet before the transaction, matching Submit's lock
// order. SKIP LOCKED keeps independent wallets moving across multiple workers.
func (s *Store) RetryReference(ctx context.Context) error {
	var id, walletID string

	err := s.pool.QueryRow(ctx, `SELECT id::text,wallet_id::text FROM wager_transactions
		WHERE status='PENDING_REFERENCE' AND next_attempt_at<=now() ORDER BY next_attempt_at,id LIMIT 1`).Scan(&id, &walletID)
	if err != nil {
		return classify(err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}

	defer s.rollback(tx)

	state, err := scanWallet(tx.QueryRow(ctx, "SELECT "+walletColumns+" FROM wallets WHERE id=$1 FOR UPDATE SKIP LOCKED", walletID))
	if err != nil {
		return err
	}

	current, err := scanTransaction(tx.QueryRow(ctx, "SELECT "+transactionColumns+` FROM wager_transactions
		WHERE id=$1 AND status='PENDING_REFERENCE' AND next_attempt_at<=now() FOR UPDATE`, id))
	if err != nil {
		return err
	}

	transaction, err := wager.Restore(current)
	if err != nil {
		return err
	}

	expired := time.Since(current.CreatedAt) >= 24*time.Hour
	if _, err := s.process(ctx, tx, transaction, state, current.Input.ExternalTransactionID, "", expired); err != nil {
		return err
	}

	return tx.Commit(ctx)
}
