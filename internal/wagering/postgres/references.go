package postgres

import (
	"context"
	"errors"
	"time"

	"jungle/internal/wagering/application"
	"jungle/internal/wagering/domain/wager"
)

// RetryReference locks the wallet before the transaction, matching Submit's lock
// order. SKIP LOCKED keeps independent wallets moving across multiple workers.
func (s *Store) RetryReference(ctx context.Context) (err error) {
	start := time.Now()
	observing := false
	status := "ERROR"
	failureCode := ""
	logger := s.logger.With("operation", "reference_retry")
	defer func() {
		s.observeConflict(err)

		if observing {
			s.metrics.ObserveProcessing("reference", status, false, time.Since(start))
		}

		if errors.Is(err, application.ErrNotFound) {
			return
		}

		if errors.Is(err, context.Canceled) {
			return
		}

		if err != nil {
			logger.Error("reference retry failed", "reason", "unavailable")

			return
		}

		if observing {
			logger.Info("reference retry committed", "status", status, "failureCode", failureCode)
		}
	}()

	var id, walletID string

	const pendingReferenceQuery = `
    SELECT
        id::text,
        wallet_id::text
    FROM wager_transactions
    WHERE status = 'PENDING_REFERENCE'
      AND next_attempt_at <= NOW()
    ORDER BY next_attempt_at, id
    LIMIT 1`

	err = s.pool.QueryRow(ctx, pendingReferenceQuery).Scan(
		&id,
		&walletID,
	)
	if err != nil {
		return classify(err)
	}

	logger = logger.With("transactionId", id, "walletId", walletID)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}

	defer s.rollback(tx)

	const lockWalletQuery = "SELECT " + walletColumns + `
    FROM wallets
    WHERE id = $1
    FOR UPDATE SKIP LOCKED`

	state, err := scanWallet(tx.QueryRow(ctx, lockWalletQuery, walletID))
	if err != nil {
		return err
	}

	const currentReferenceQuery = "SELECT " + transactionColumns + `
    FROM wager_transactions
    WHERE id = $1
      AND status = 'PENDING_REFERENCE'
      AND next_attempt_at <= NOW()
    FOR UPDATE`

	current, err := scanTransaction(tx.QueryRow(ctx, currentReferenceQuery, id))
	if err != nil {
		return err
	}

	logger = logger.With(
		"providerId", current.Input.ProviderID,
		"correlationId", current.Input.ExternalTransactionID,
	)
	transaction, err := wager.Restore(current)
	if err != nil {
		return err
	}

	expired := time.Since(current.CreatedAt) >= 24*time.Hour
	observing = true
	finished, err := s.process(ctx, tx, transaction, state, current.Input.ExternalTransactionID, "", expired)
	if err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}

	status = string(finished.Status)
	failureCode = finished.FailureCode

	return nil
}
