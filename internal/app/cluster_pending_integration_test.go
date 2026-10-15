//go:build integration

package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"jungle/internal/testsupport/sqsidentity"
	"jungle/internal/wagering/domain/wager"
)

func TestThreeProcessesReferenceExpirationRestart(t *testing.T) {
	for _, kind := range []wager.Kind{wager.Refund, wager.Rollback} {
		t.Run(string(kind), func(t *testing.T) {
			fixture := setupCluster(t)
			endpoints := [3]string{fixture.endpoint, fixture.endpoint, fixture.endpoint}
			nodes := fixture.startThree(t, endpoints)
			state := fixture.open(t, nodes[0])
			bet := clusterInput(t, state, wager.Bet, "20.00")
			reversal := clusterInput(t, state, kind, "20.00")
			reversal.ReferenceExternalTransactionID = bet.ExternalTransactionID
			encoded, err := json.Marshal(reversal)
			if err != nil {
				t.Fatal(err)
			}

			hash, err := reversal.Hash()
			if err != nil {
				t.Fatal(err)
			}

			transactionID, err := uuid.NewRandom()
			if err != nil {
				t.Fatal(err)
			}

			// Historical input makes TTL testable without changing immutable timestamps or waiting 24 hours.
			const historicalReferenceQuery = `
        INSERT INTO wager_transactions (
            id, wallet_id, player_id, currency, amount, kind, provider_id,
            external_id, idempotency_key, payload_hash, request, status,
            result_balance, created_at, updated_at, next_attempt_at
        ) VALUES (
            $1, $2, $3, 'BRL', 2000, $4, 'provider-a',
            $5, $5, $6, $7, 'PENDING_REFERENCE', 10000,
            NOW() - INTERVAL '25 hours', NOW() - INTERVAL '25 hours',
            NOW() + INTERVAL '1 day'
        )
   `

			_, err = fixture.pool.Exec(
				fixture.ctx,
				historicalReferenceQuery,
				transactionID.String(),
				state.ID,
				state.PlayerID,
				kind,
				reversal.ExternalTransactionID,
				hash,
				encoded,
			)
			if err != nil {
				t.Fatal(err)
			}

			schedule, err := fixture.pool.Begin(fixture.ctx)
			if err != nil {
				t.Fatal(err)
			}

			defer func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()

				if err := schedule.Rollback(cleanup); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
					t.Errorf("release reference schedule: %v", err)
				}
			}()

			const scheduleReferenceQuery = `
        UPDATE wager_transactions
        SET next_attempt_at = NOW()
        WHERE id = $1
   `

			if _, err := schedule.Exec(fixture.ctx, scheduleReferenceQuery, transactionID.String()); err != nil {
				t.Fatal(err)
			}

			// Keep the schedule uncommitted until all three original processes have been replaced.
			nodes = fixture.restart(t, nodes, endpoints)

			if err := schedule.Commit(fixture.ctx); err != nil {
				t.Fatal(err)
			}

			const rejectedReferenceQuery = `
        SELECT
            status,
            failure_code
        FROM wager_transactions
        WHERE id = $1
   `

			var status wager.Status
			var code string

			awaitCluster(t, fixture.ctx, func() bool {
				if err := fixture.pool.QueryRow(
					fixture.ctx,
					rejectedReferenceQuery,
					transactionID.String(),
				).Scan(
					&status,
					&code,
				); err != nil {
					t.Fatal(err)
				}

				return status == wager.Rejected
			})

			if code != "REFERENCE_NOT_FOUND" {
				t.Fatalf("expired reference code = %s", code)
			}

			fixture.assertWallet(t, nodes, state, "100.00", 1)
			result, httpStatus, err := fixture.submit(nodes[2], bet)
			if err != nil {
				t.Fatal(err)
			}

			if httpStatus != http.StatusOK {
				t.Fatal("late reference did not process")
			}

			if result.Status != wager.Processed {
				t.Fatal("late reference did not process")
			}

			snapshot := clusterFinancialSnapshot(t, fixture, state.ID)
			nodes = fixture.restart(t, nodes, endpoints)
			replay, httpStatus, err := fixture.submit(nodes[1], reversal)
			if err != nil {
				t.Fatal(err)
			}

			if httpStatus != http.StatusUnprocessableEntity {
				t.Fatalf("expired replay = %+v, status %d", replay, httpStatus)
			}

			if replay.FailureCode != "REFERENCE_NOT_FOUND" {
				t.Fatalf("expired replay = %+v, status %d", replay, httpStatus)
			}

			if !replay.IdempotentReplay {
				t.Fatal("restart did not preserve the original expired transaction")
			}

			if replay.TransactionID != transactionID.String() {
				t.Fatal("restart did not preserve the original expired transaction")
			}

			originalBalance, err := replay.Balance.Amount()
			if err != nil {
				t.Fatal(err)
			}

			if originalBalance != "100.00" {
				t.Fatalf("expired replay balance = %s; want original 100.00", originalBalance)
			}

			if !bytes.Equal(snapshot, clusterFinancialSnapshot(t, fixture, state.ID)) {
				t.Fatal("late arrival or replay reopened an expired reversal")
			}

			fixture.assertWallet(t, nodes, state, "80.00", 2)

			for _, node := range nodes {
				if err := node.stop(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestThreeProcessesDatabaseRedriveRestart(t *testing.T) {
	fixture := setupCluster(t)
	dlqURL := configureClusterDLQ(t, fixture)
	fault := newClusterFault(t, "observe")
	endpoints := fault.proxies(t, fixture)
	nodes := fixture.startThree(t, endpoints)
	state := fixture.open(t, nodes[0])
	bet := clusterInput(t, state, wager.Bet, "25.00")
	data := struct {
		wager.Input
		IdempotencyKey string `json:"idempotencyKey"`
	}{Input: bet, IdempotencyKey: bet.ExternalTransactionID}
	body, err := json.Marshal(map[string]any{
		"messageId":  "cluster-database-message",
		"type":       "WagerTransactionRequested",
		"occurredAt": time.Now().UTC(),
		"data":       data,
	})
	if err != nil {
		t.Fatal(err)
	}

	const failureQuery = `
    CREATE SEQUENCE cluster_ledger_failures;

    CREATE FUNCTION cluster_fail_ledger() RETURNS TRIGGER LANGUAGE plpgsql AS $$
    BEGIN
        PERFORM NEXTVAL('cluster_ledger_failures');
        RAISE EXCEPTION 'injected cluster storage failure' USING ERRCODE = '58000';
    END;
    $$;

    CREATE TRIGGER cluster_ledger_failure
        AFTER INSERT ON wallet_ledger
        FOR EACH ROW EXECUTE FUNCTION cluster_fail_ledger();
 `

	if _, err := fixture.pool.Exec(fixture.ctx, failureQuery); err != nil {
		t.Fatal(err)
	}

	snapshot := clusterFinancialSnapshot(t, fixture, state.ID)
	message := types.Message{
		Body: aws.String(string(body)),
		MessageAttributes: map[string]types.MessageAttributeValue{
			"MessageSignature": {DataType: aws.String("String"), StringValue: aws.String(sqsidentity.Signature(t, "provider-a", string(body)))},
		},
	}
	originalID := sendClusterMessage(t, fixture, message, state.ID, "cluster-fault")

	for count := 1; count <= 5; count++ {
		received := awaitClusterDelivery(t, fixture.ctx, fault.received)
		t.Logf(
			"storage failure delivery %d, consumer %d, broker count %s",
			count,
			received.node,
			received.receiveCount,
		)

		if received.messageID != originalID {
			t.Fatalf("fault delivery count = %s; want %d on original message", received.receiveCount, count)
		}

		if received.receiveCount != strconv.Itoa(count) {
			t.Fatalf("fault delivery count = %s; want %d on original message", received.receiveCount, count)
		}

		const failureCountQuery = `
        SELECT
            last_value,
            is_called
        FROM cluster_ledger_failures
  `

		awaitCluster(t, fixture.ctx, func() bool {
			var failures int
			var called bool

			if err := fixture.pool.QueryRow(fixture.ctx, failureCountQuery).Scan(
				&failures,
				&called,
			); err != nil {
				t.Fatal(err)
			}

			return called && failures == count
		})

		// Compare committed data after every observed storage failure.
		fixture.assertWallet(t, nodes, state, "100.00", 1)

		if !bytes.Equal(snapshot, clusterFinancialSnapshot(t, fixture, state.ID)) {
			t.Fatal("failed write left partial financial effects")
		}
	}

	var deadLetter types.Message

	for deadLetter.Body == nil {
		messages, err := fixture.broker.ReceiveMessage(fixture.ctx, &sqs.ReceiveMessageInput{
			QueueUrl:              aws.String(dlqURL),
			MaxNumberOfMessages:   1,
			WaitTimeSeconds:       10,
			MessageAttributeNames: []string{"MessageSignature"},
		})
		if err != nil {
			t.Fatal(err)
		}

		if len(messages.Messages) > 0 {
			deadLetter = messages.Messages[0]
		}
	}

	if aws.ToString(deadLetter.Body) != string(body) {
		t.Fatal("DLQ changed the original cluster request")
	}

	t.Log("original message received from the DLQ")

	const restoreQuery = `
    DROP TRIGGER cluster_ledger_failure ON wallet_ledger;
    DROP FUNCTION cluster_fail_ledger();
 `

	if _, err := fixture.pool.Exec(fixture.ctx, restoreQuery); err != nil {
		t.Fatal(err)
	}

	nodes = fixture.restart(t, nodes, endpoints)
	t.Log("cluster restarted after removing the storage fault")
	restoredID := sendClusterMessage(t, fixture, deadLetter, state.ID, "cluster-restored")
	restored := awaitClusterDelivery(t, fixture.ctx, fault.deleted)

	if restored.messageID != restoredID {
		t.Fatalf("restored delivery ID = %s; want %s", restored.messageID, restoredID)
	}

	receivedCount, err := strconv.Atoi(restored.receiveCount)
	if err != nil {
		t.Fatal(err)
	}

	if receivedCount < 1 {
		t.Fatal("restored message has no broker receive count")
	}

	t.Logf("recovered message committed and removed after %d receives", receivedCount)

	fixture.assertWallet(t, nodes, state, "75.00", 2)

	if _, err := fixture.broker.DeleteMessage(fixture.ctx, &sqs.DeleteMessageInput{
		QueueUrl: aws.String(dlqURL), ReceiptHandle: deadLetter.ReceiptHandle,
	}); err != nil {
		t.Fatal(err)
	}

	snapshot = clusterFinancialSnapshot(t, fixture, state.ID)
	duplicateID := sendClusterMessage(t, fixture, deadLetter, state.ID, "cluster-duplicate")
	duplicate := awaitClusterDelivery(t, fixture.ctx, fault.deleted)

	if duplicate.messageID != duplicateID {
		t.Fatal("broker did not deliver the duplicate to the application")
	}

	if duplicateID == restoredID {
		t.Fatal("broker did not deliver the duplicate to the application")
	}

	for _, node := range nodes {
		replay, status, err := fixture.submit(node, bet)
		if err != nil {
			t.Fatal(err)
		}

		if status != http.StatusOK {
			t.Fatal("HTTP replay did not preserve the SQS result")
		}

		if !replay.IdempotentReplay {
			t.Fatal("HTTP replay did not preserve the SQS result")
		}
	}

	nodes = fixture.restart(t, nodes, endpoints)

	if !bytes.Equal(snapshot, clusterFinancialSnapshot(t, fixture, state.ID)) {
		t.Fatal("duplicate or restart changed the recovered operation")
	}

	fixture.assertWallet(t, nodes, state, "75.00", 2)

	for _, node := range nodes {
		if err := node.stop(); err != nil {
			t.Fatal(err)
		}
	}
}

func configureClusterDLQ(t *testing.T, fixture *clusterFixture) string {
	t.Helper()

	created, err := fixture.broker.CreateQueue(fixture.ctx, &sqs.CreateQueueInput{
		QueueName:  aws.String(fixture.prefix + "-dlq.fifo"),
		Attributes: map[string]string{"FifoQueue": "true"},
	})
	if err != nil {
		t.Fatal(err)
	}

	address := aws.ToString(created.QueueUrl)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if _, err := fixture.broker.DeleteQueue(
			cleanup,
			&sqs.DeleteQueueInput{QueueUrl: aws.String(address)},
		); err != nil {
			t.Errorf("delete cluster DLQ: %v", err)
		}
	})

	attributes, err := fixture.broker.GetQueueAttributes(fixture.ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       aws.String(address),
		AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn},
	})
	if err != nil {
		t.Fatal(err)
	}

	policy, err := json.Marshal(map[string]any{
		"deadLetterTargetArn": attributes.Attributes["QueueArn"],
		"maxReceiveCount":     5,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := fixture.broker.SetQueueAttributes(fixture.ctx, &sqs.SetQueueAttributesInput{
		QueueUrl:   aws.String(fixture.requestsURL),
		Attributes: map[string]string{"RedrivePolicy": string(policy)},
	}); err != nil {
		t.Fatal(err)
	}

	return address
}

func sendClusterMessage(
	t *testing.T,
	fixture *clusterFixture,
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
