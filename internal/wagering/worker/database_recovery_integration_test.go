//go:build integration

package worker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"jungle/internal/platform/auth"
	"jungle/internal/wagering/domain/wager"
	"jungle/internal/wagering/postgres"
)

func injectLedgerFailure(t *testing.T, fixture recoveryFixture) {
	t.Helper()

	// Sequences survive rollback, providing evidence that every attempt reached the write boundary.
	const injectFailureQuery = `
    CREATE SEQUENCE ledger_fault_attempts;

    CREATE FUNCTION fail_ledger_write() RETURNS TRIGGER LANGUAGE plpgsql AS $$
    BEGIN
        PERFORM NEXTVAL('ledger_fault_attempts');
        RAISE EXCEPTION 'injected storage failure after ledger insert'
            USING ERRCODE = '58000';
    END;
    $$;

    CREATE TRIGGER injected_ledger_failure
        AFTER INSERT ON wallet_ledger
        FOR EACH ROW EXECUTE FUNCTION fail_ledger_write();
 `

	if _, err := fixture.pool.Exec(fixture.ctx, injectFailureQuery); err != nil {
		t.Fatal(err)
	}
}

func assertLedgerFailures(t *testing.T, fixture recoveryFixture, want int) {
	t.Helper()

	const failureCountQuery = `
    SELECT
        last_value,
        is_called
    FROM ledger_fault_attempts
 `

	var count int
	var called bool

	err := fixture.pool.QueryRow(fixture.ctx, failureCountQuery).Scan(
		&count,
		&called,
	)
	if err != nil {
		t.Fatal(err)
	}

	if !called {
		t.Fatal("message did not reach the injected ledger failure")
	}

	if count != want {
		t.Fatalf("ledger failures = %d; want %d", count, want)
	}
}

func recoverDeadLetter(
	t *testing.T,
	fixture recoveryFixture,
	dlqURL string,
	message types.Message,
	envelope requested,
	store *postgres.Store,
	verifier *auth.Verifier,
) {
	t.Helper()

	const removeFailureQuery = `
    DROP TRIGGER injected_ledger_failure ON wallet_ledger;
    DROP FUNCTION fail_ledger_write();
 `

	if _, err := fixture.pool.Exec(fixture.ctx, removeFailureQuery); err != nil {
		t.Fatal(err)
	}

	// Reuse the original credential and bytes; only the database fault and transport deduplication ID change.
	attribute := message.MessageAttributes["AccessToken"]
	principal, err := verifier.Verify(fixture.ctx, aws.ToString(attribute.StringValue))
	if err != nil {
		t.Fatal(err)
	}

	if principal.ProviderID != envelope.Data.ProviderID {
		t.Fatal("DLQ credential no longer authorizes the original provider")
	}

	restored := sendDeadLetterCopy(t, fixture, message, envelope.Data.WalletID, "database-restored")
	process, output := startRecoveryProcess(
		t,
		fixture.ctx,
		"TestConsumerRecoveryProcess",
		"reprocess",
		fixture.endpoint,
	)
	if err := process.Wait(); err != nil {
		t.Fatalf("DLQ reprocessing failed: %v; %s", err, output)
	}

	snapshot := assertReprocessedOperation(t, fixture, store, envelope, aws.ToString(message.Body))

	// Remove the DLQ copy only after verifying the durable financial commit.
	_, err = fixture.broker.DeleteMessage(fixture.ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(dlqURL),
		ReceiptHandle: message.ReceiptHandle,
	})
	if err != nil {
		t.Fatal(err)
	}

	duplicate := sendDeadLetterCopy(t, fixture, message, envelope.Data.WalletID, "duplicate-after-recovery")
	if duplicate == restored {
		t.Fatal("broker deduplication prevented a new delivery of the duplicate")
	}

	replay, replayOutput := startRecoveryProcess(
		t,
		fixture.ctx,
		"TestConsumerRecoveryProcess",
		"replay",
		fixture.endpoint,
	)
	if err := replay.Wait(); err != nil {
		t.Fatalf("recovered operation replay failed: %v; %s", err, replayOutput)
	}

	after := assertReprocessedOperation(t, fixture, store, envelope, aws.ToString(message.Body))
	if !bytes.Equal(snapshot, after) {
		t.Fatal("duplicate after recovery changed the committed financial snapshot")
	}

	assertLedgerFailures(t, fixture, redriveReceiveLimit)

	for _, address := range []string{fixture.requestsURL, dlqURL} {
		remaining, err := fixture.broker.ReceiveMessage(fixture.ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(address),
			MaxNumberOfMessages: 1,
			WaitTimeSeconds:     3,
		})
		if err != nil {
			t.Fatal(err)
		}

		if len(remaining.Messages) != 0 {
			t.Fatal("processed message remained in source queue or DLQ")
		}
	}
}

func sendDeadLetterCopy(
	t *testing.T,
	fixture recoveryFixture,
	message types.Message,
	walletID string,
	deduplicationID string,
) string {
	t.Helper()

	sent, err := fixture.broker.SendMessage(fixture.ctx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(fixture.requestsURL),
		MessageBody:            message.Body,
		MessageAttributes:      message.MessageAttributes,
		MessageGroupId:         aws.String(walletID),
		MessageDeduplicationId: aws.String(deduplicationID),
	})
	if err != nil {
		t.Fatal(err)
	}

	return aws.ToString(sent.MessageId)
}

func assertReprocessedOperation(
	t *testing.T,
	fixture recoveryFixture,
	store *postgres.Store,
	envelope requested,
	body string,
) []byte {
	t.Helper()

	input := envelope.Data.Input
	transaction, err := store.Transaction(fixture.ctx, "", input.ProviderID, input.ExternalTransactionID)
	if err != nil {
		t.Fatal(err)
	}

	if transaction.Status != wager.Processed {
		t.Fatalf("reprocessed status = %s; want PROCESSED", transaction.Status)
	}

	resultBalance, err := transaction.Balance.Amount()
	if err != nil {
		t.Fatal(err)
	}

	if resultBalance != "75.00" {
		t.Fatalf("reprocessed result balance = %s; want 75.00", resultBalance)
	}

	reconciliation, err := store.Reconcile(fixture.ctx, input.WalletID)
	if err != nil {
		t.Fatal(err)
	}

	if !reconciliation.Consistent {
		t.Fatal("reprocessed wallet diverged from the ledger")
	}

	if reconciliation.CheckedEntries != 2 {
		t.Fatalf("reprocessed ledger entries = %d; want 2", reconciliation.CheckedEntries)
	}

	wallet, err := store.Wallet(fixture.ctx, input.WalletID)
	if err != nil {
		t.Fatal(err)
	}

	if wallet.Version != 2 || wallet.Balance != transaction.Balance {
		t.Fatalf("reprocessed wallet = %+v; want version 2 and original result balance", wallet)
	}

	const operationEffectsQuery = `
    SELECT
        (
            SELECT COUNT(*)
            FROM wager_transactions
            WHERE wallet_id = $1
        ),
        (
            SELECT COUNT(*)
            FROM wallet_ledger
            WHERE transaction_id = $2
              AND direction = 'DEBIT'
              AND amount = 2500
        ),
        (
            SELECT COUNT(*)
            FROM inbox
        ),
        (
            SELECT payload_hash
            FROM inbox
            WHERE consumer_name = 'wager'
              AND message_id = $3
              AND completed_at IS NOT NULL
        ),
        COUNT(*) FILTER (WHERE event_type = 'WagerTransactionProcessed'),
        COUNT(*) FILTER (WHERE event_type = 'WalletBalanceChanged')
    FROM outbox
    WHERE payload->'data'->>'transactionId' = $2::text
 `

	var transactions, debits, inbox, processedEvents, balanceEvents int
	var messageHash string

	err = fixture.pool.QueryRow(
		fixture.ctx,
		operationEffectsQuery,
		input.WalletID,
		transaction.ID,
		envelope.MessageID,
	).Scan(
		&transactions,
		&debits,
		&inbox,
		&messageHash,
		&processedEvents,
		&balanceEvents,
	)
	if err != nil {
		t.Fatal(err)
	}

	if transactions != 2 {
		t.Fatalf("transactions = %d; want opening and one bet", transactions)
	}

	if debits != 1 {
		t.Fatalf("reprocessed debits = %d; want 1", debits)
	}

	sum := sha256.Sum256([]byte(body))
	if inbox != 1 || messageHash != hex.EncodeToString(sum[:]) {
		t.Fatal("reprocessing did not commit exactly one matching inbox entry")
	}

	if processedEvents != 1 || balanceEvents != 1 {
		t.Fatalf("financial events = %d processed, %d balance changed; want 1 each", processedEvents, balanceEvents)
	}

	return publicationFinancialSnapshot(t, fixture.pool, input.WalletID)
}
