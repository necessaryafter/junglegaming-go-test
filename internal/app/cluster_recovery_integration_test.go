//go:build integration

package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/jackc/pgx/v5"

	"jungle/internal/wagering/application"
	"jungle/internal/wagering/domain/wager"
)

func TestThreeProcessesConcurrencyAndRestart(t *testing.T) {
	fixture := setupCluster(t)
	endpoints := [3]string{fixture.endpoint, fixture.endpoint, fixture.endpoint}
	nodes := fixture.startThree(t, endpoints)
	state := fixture.open(t, nodes[0])
	bet := clusterInput(t, state, wager.Bet, "80.00")
	results := make(chan application.Result, 50)
	failures := make(chan error, 50)
	start := make(chan struct{})
	var group sync.WaitGroup

	for i := range 50 {
		group.Add(1)

		go func() {
			defer group.Done()
			<-start

			result, status, err := fixture.submit(nodes[i%3], bet)
			if err != nil {
				failures <- err

				return
			}

			if status != http.StatusOK {
				failures <- fmt.Errorf("unexpected HTTP status %d", status)

				return
			}

			results <- result
		}()
	}

	close(start)
	group.Wait()
	close(results)
	close(failures)

	for err := range failures {
		t.Error(err)
	}

	replays := 0
	transactionID := ""

	for result := range results {
		if result.Status != wager.Processed {
			t.Fatalf("duplicate result status = %s", result.Status)
		}

		if transactionID != "" && transactionID != result.TransactionID {
			t.Fatal("duplicate requests created different transactions")
		}

		transactionID = result.TransactionID

		if result.IdempotentReplay {
			replays++
		}
	}

	if replays != 49 {
		t.Fatalf("replays = %d; want 49", replays)
	}

	fixture.assertWallet(t, nodes, state, "20.00", 2)
	contested := fixture.open(t, nodes[0])
	bets := [2]wager.Input{
		clusterInput(t, contested, wager.Bet, "80.00"),
		clusterInput(t, contested, wager.Bet, "80.00"),
	}
	outcomes := make([]application.Result, 2)
	statuses := make([]int, 2)
	errorsFound := make([]error, 2)
	start = make(chan struct{})

	for i := range bets {
		group.Add(1)

		go func() {
			defer group.Done()
			<-start

			outcomes[i], statuses[i], errorsFound[i] = fixture.submit(nodes[i], bets[i])
		}()
	}

	close(start)
	group.Wait()
	processed, rejected := 0, 0

	for i, result := range outcomes {
		if errorsFound[i] != nil {
			t.Fatal(errorsFound[i])
		}

		switch statuses[i] {
		case http.StatusOK:
			processed++
		case http.StatusUnprocessableEntity:
			rejected++

			if result.FailureCode != "INSUFFICIENT_BALANCE" {
				t.Fatalf("rejected bet code = %s", result.FailureCode)
			}

		default:
			t.Fatalf("bet status = %d", statuses[i])
		}
	}

	if processed != 1 || rejected != 1 {
		t.Fatalf("processed/rejected = %d/%d; want 1/1", processed, rejected)
	}

	fixture.assertWallet(t, nodes, contested, "20.00", 2)
	assertIndependentClusterWallets(t, fixture, nodes)
	nodes = fixture.restart(t, nodes, endpoints)
	replay, status, err := fixture.submit(nodes[2], bet)
	if err != nil {
		t.Fatal(err)
	}

	if status != http.StatusOK {
		t.Fatalf("duplicate after restart HTTP status = %d", status)
	}

	if !replay.IdempotentReplay || replay.TransactionID != transactionID {
		t.Fatalf("duplicate after restart = %+v", replay)
	}

	for i, bet := range bets {
		replay, status, err := fixture.submit(nodes[2], bet)
		if err != nil {
			t.Fatal(err)
		}

		original := outcomes[i]
		original.IdempotentReplay = true

		if status != statuses[i] || replay != original {
			t.Fatalf("contested replay after restart = %+v, status %d; want %+v, %d", replay, status, original, statuses[i])
		}
	}

	fixture.assertWallet(t, nodes, state, "20.00", 2)
	fixture.assertWallet(t, nodes, contested, "20.00", 2)

	for _, node := range nodes {
		if err := node.stop(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestThreeProcessesConsumerRecovery(t *testing.T) {
	fixture := setupCluster(t)
	fault := newClusterFault(t, "consumer commit")
	endpoints := fault.proxies(t, fixture)
	nodes := fixture.startThree(t, endpoints)
	state := fixture.open(t, nodes[0])
	bet := clusterInput(t, state, wager.Bet, "25.00")
	body, err := json.Marshal(map[string]any{
		"messageId":  "cluster-consumer-message",
		"type":       "WagerTransactionRequested",
		"occurredAt": state.CreatedAt,
		"data": struct {
			wager.Input
			IdempotencyKey string `json:"idempotencyKey"`
		}{Input: bet, IdempotencyKey: bet.ExternalTransactionID},
	})
	if err != nil {
		t.Fatal(err)
	}

	sent, err := fixture.broker.SendMessage(fixture.ctx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(fixture.requestsURL),
		MessageBody:            aws.String(string(body)),
		MessageGroupId:         aws.String(state.ID),
		MessageDeduplicationId: aws.String("cluster-single-send"),
		MessageAttributes: map[string]types.MessageAttributeValue{
			"AccessToken": {DataType: aws.String("String"), StringValue: aws.String(fixture.providerToken)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	interrupted := awaitClusterDelivery(t, fixture.ctx, fault.interrupted)
	if interrupted.node != 0 || interrupted.receiveCount != "1" {
		t.Fatalf("initial consumer = %d, receipt count %s", interrupted.node, interrupted.receiveCount)
	}

	if interrupted.messageID != aws.ToString(sent.MessageId) {
		t.Fatal("consumer interrupted another broker message")
	}

	before := clusterFinancialSnapshot(t, fixture, state.ID)
	if err := nodes[0].kill(); err != nil {
		t.Fatal(err)
	}

	replay, status, err := fixture.submit(nodes[1], bet)
	if err != nil {
		t.Fatal(err)
	}

	if status != http.StatusOK || !replay.IdempotentReplay {
		t.Fatalf("HTTP replay after SQS commit = %+v, status %d", replay, status)
	}

	close(fault.receives[1])
	close(fault.receives[2])
	recovered := awaitClusterDelivery(t, fixture.ctx, fault.deleted)
	if recovered.node == 0 || recovered.receiveCount != "2" {
		t.Fatalf("recovered consumer = %d, receipt count %s", recovered.node, recovered.receiveCount)
	}

	if recovered.messageID != interrupted.messageID || recovered.receipt == interrupted.receipt {
		t.Fatal("recovery did not receive a fresh receipt for the same SQS message")
	}

	if !bytes.Equal(before, clusterFinancialSnapshot(t, fixture, state.ID)) {
		t.Fatal("HTTP/SQS recovery changed the committed financial snapshot")
	}

	const inboxQuery = `
    SELECT COUNT(*)
    FROM inbox
    WHERE consumer_name = 'wager'
      AND message_id = 'cluster-consumer-message'
      AND completed_at IS NOT NULL
 `

	var inbox int
	if err := fixture.pool.QueryRow(fixture.ctx, inboxQuery).Scan(&inbox); err != nil {
		t.Fatal(err)
	}

	if inbox != 1 {
		t.Fatalf("committed inbox entries = %d; want 1", inbox)
	}

	nodes[0] = fixture.start(t, 0, endpoints[0])
	fixture.assertWallet(t, nodes, state, "75.00", 2)

	for _, node := range nodes {
		if err := node.stop(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestThreeProcessesPublisherRecovery(t *testing.T) {
	for _, stage := range []string{"before send", "after send"} {
		t.Run(stage, func(t *testing.T) {
			fixture := setupCluster(t)
			fault := newClusterFault(t, stage)
			endpoints := fault.proxies(t, fixture)
			nodes := fixture.startThree(t, endpoints)
			state := fixture.open(t, nodes[0])
			interrupted := awaitClusterDelivery(t, fixture.ctx, fault.interrupted)

			if interrupted.accepted != (stage == "after send") {
				t.Fatal("publisher did not stop at the requested boundary")
			}

			before := clusterFinancialSnapshot(t, fixture, state.ID)
			const abandonedEventQuery = `
        SELECT
            payload,
            lease_until,
            published_at IS NOT NULL AS published,
            processing_attempts
        FROM outbox
        WHERE event_id = $1
   `

			var payload []byte
			var leaseUntil time.Time
			var published bool
			var attempts int

			err := fixture.pool.QueryRow(fixture.ctx, abandonedEventQuery, interrupted.eventID).Scan(
				&payload,
				&leaseUntil,
				&published,
				&attempts,
			)
			if err != nil {
				t.Fatal(err)
			}

			if published || attempts != 1 {
				t.Fatal("interrupted event was not an abandoned first claim")
			}

			if interrupted.body != string(payload) || interrupted.group != state.ID {
				t.Fatal("publisher sent a different event snapshot or wallet group")
			}

			if err := nodes[interrupted.node].kill(); err != nil {
				t.Fatal(err)
			}

			var recovered clusterDelivery

			for recovered.eventID != interrupted.eventID {
				recovered = awaitClusterDelivery(t, fixture.ctx, fault.sent)
			}

			if recovered.node == interrupted.node || !recovered.accepted {
				t.Fatal("abandoned event was not accepted from another live process")
			}

			if recovered.body != interrupted.body || recovered.group != interrupted.group {
				t.Fatal("replacement publisher changed the event identity or payload")
			}

			const completedEventQuery = `
        SELECT
            published_at IS NOT NULL AS published,
            COALESCE(published_at >= $2, FALSE) AS lease_expired,
            lease_id IS NULL AND lease_until IS NULL AS released,
            processing_attempts,
            publication_failures
        FROM outbox
        WHERE event_id = $1
   `

			var expired, released bool
			var failures int

			awaitCluster(t, fixture.ctx, func() bool {
				err := fixture.pool.QueryRow(fixture.ctx, completedEventQuery, interrupted.eventID, leaseUntil).Scan(
					&published,
					&expired,
					&released,
					&attempts,
					&failures,
				)
				if err != nil {
					t.Fatal(err)
				}

				return published
			})

			if !expired || !released {
				t.Fatalf("recovered lease: expired=%t, released=%t", expired, released)
			}

			if attempts != 2 || failures != 0 {
				t.Fatalf("recovered counters: attempts=%d, failures=%d", attempts, failures)
			}

			if !bytes.Equal(before, clusterFinancialSnapshot(t, fixture, state.ID)) {
				t.Fatal("publisher recovery changed financial state or immutable events")
			}

			nodes[interrupted.node] = fixture.start(t, interrupted.node, endpoints[interrupted.node])
			fixture.assertWallet(t, nodes, state, "100.00", 1)

			for _, node := range nodes {
				if err := node.stop(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestThreeProcessesPendingReferenceRestart(t *testing.T) {
	for _, kind := range []wager.Kind{wager.Refund, wager.Rollback} {
		t.Run(string(kind), func(t *testing.T) {
			fixture := setupCluster(t)
			endpoints := [3]string{fixture.endpoint, fixture.endpoint, fixture.endpoint}
			nodes := fixture.startThree(t, endpoints)
			state := fixture.open(t, nodes[0])
			bet := clusterInput(t, state, wager.Bet, "20.00")
			refund := clusterInput(t, state, kind, "20.00")
			refund.ReferenceExternalTransactionID = bet.ExternalTransactionID
			pending, status, err := fixture.submit(nodes[0], refund)
			if err != nil {
				t.Fatal(err)
			}

			if status != http.StatusAccepted || pending.Status != wager.PendingReference {
				t.Fatalf("pending refund = %+v, status %d", pending, status)
			}

			nodes = fixture.restart(t, nodes, endpoints)
			processedBet, status, err := fixture.submit(nodes[2], bet)
			if err != nil {
				t.Fatal(err)
			}

			if status != http.StatusOK || processedBet.Status != wager.Processed {
				t.Fatalf("late bet = %+v, status %d", processedBet, status)
			}

			const referenceStateQuery = `
    SELECT
        status,
        reference_id::text
    FROM wager_transactions
    WHERE id = $1
 `

			var finalStatus wager.Status
			var referenceID string

			awaitCluster(t, fixture.ctx, func() bool {
				var resolved *string

				if err := fixture.pool.QueryRow(fixture.ctx, referenceStateQuery, pending.TransactionID).Scan(&finalStatus, &resolved); err != nil {
					t.Fatal(err)
				}

				if resolved != nil {
					referenceID = *resolved
				}

				return finalStatus == wager.Processed
			})

			if referenceID != processedBet.TransactionID {
				t.Fatal("restarted reference worker resolved another transaction")
			}

			fixture.assertWallet(t, nodes, state, "100.00", 3)
			snapshot := clusterFinancialSnapshot(t, fixture, state.ID)
			nodes = fixture.restart(t, nodes, endpoints)
			replay, status, err := fixture.submit(nodes[1], refund)
			if err != nil {
				t.Fatal(err)
			}

			if status != http.StatusOK {
				t.Fatalf("resolved reversal replay HTTP status = %d", status)
			}

			if !replay.IdempotentReplay || replay.TransactionID != pending.TransactionID {
				t.Fatalf("resolved reversal replay = %+v", replay)
			}

			if !bytes.Equal(snapshot, clusterFinancialSnapshot(t, fixture, state.ID)) {
				t.Fatal("restart or replay reapplied the resolved refund")
			}

			fixture.assertWallet(t, nodes, state, "100.00", 3)

			for _, node := range nodes {
				if err := node.stop(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func assertIndependentClusterWallets(
	t *testing.T,
	fixture *clusterFixture,
	nodes []*clusterNode,
) {
	t.Helper()

	first := fixture.open(t, nodes[0])
	second := fixture.open(t, nodes[1])
	firstBet := clusterInput(t, first, wager.Bet, "80.00")
	secondBet := clusterInput(t, second, wager.Bet, "80.00")
	lock, err := fixture.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}

	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := lock.Rollback(cleanup); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("release held wallet: %v", err)
		}
	}()

	if _, err := lock.Exec(fixture.ctx, "SELECT id FROM wallets WHERE id=$1 FOR UPDATE", first.ID); err != nil {
		t.Fatal(err)
	}

	completed := make(chan clusterSubmitOutcome, 1)

	go func() {
		result, status, err := fixture.submit(nodes[0], firstBet)
		completed <- clusterSubmitOutcome{result: result, status: status, err: err}
	}()

	const waitingWalletQuery = `
    SELECT COUNT(*)
    FROM pg_stat_activity
    WHERE application_name = $1
      AND wait_event_type = 'Lock'
 `

	awaitCluster(t, fixture.ctx, func() bool {
		var waiting int

		if err := fixture.pool.QueryRow(fixture.ctx, waitingWalletQuery, fixture.prefix+"_node_0").Scan(&waiting); err != nil {
			t.Fatal(err)
		}

		return waiting > 0
	})

	result, status, err := fixture.submit(nodes[1], secondBet)
	if err != nil {
		t.Fatal(err)
	}

	if status != http.StatusOK || result.Status != wager.Processed {
		t.Fatal("independent wallet did not progress while another wallet was locked")
	}

	select {
	case <-completed:
		t.Fatal("locked wallet completed before its lock was released")
	default:
	}

	if err := lock.Commit(fixture.ctx); err != nil {
		t.Fatal(err)
	}

	outcome := <-completed
	if outcome.err != nil {
		t.Fatal(outcome.err)
	}

	if outcome.status != http.StatusOK || outcome.result.Status != wager.Processed {
		t.Fatal("wallet did not resume after releasing its lock")
	}

	fixture.assertWallet(t, nodes, first, "20.00", 2)
	fixture.assertWallet(t, nodes, second, "20.00", 2)
}

type clusterSubmitOutcome struct {
	result application.Result
	status int
	err    error
}

func clusterFinancialSnapshot(
	t *testing.T,
	fixture *clusterFixture,
	walletID string,
) []byte {
	t.Helper()

	const snapshotQuery = `
    SELECT JSONB_BUILD_OBJECT(
        'wallet', (
            SELECT TO_JSONB(w)
            FROM wallets AS w
            WHERE id = $1
        ),
        'transactions', (
            SELECT JSONB_AGG(TO_JSONB(t) ORDER BY id)
            FROM wager_transactions AS t
            WHERE wallet_id = $1
        ),
        'ledger', (
            SELECT JSONB_AGG(TO_JSONB(l) ORDER BY id)
            FROM wallet_ledger AS l
            WHERE wallet_id = $1
        ),
        'inbox', (
            SELECT JSONB_AGG(TO_JSONB(i) ORDER BY consumer_name, message_id)
            FROM inbox AS i
        ),
        'events', (
            SELECT JSONB_AGG(JSONB_BUILD_OBJECT(
                'id', event_id,
                'type', event_type,
                'payload', payload,
                'occurredAt', occurred_at
            ) ORDER BY event_id)
            FROM outbox
            WHERE aggregate_id = $1
        )
    )
 `

	var snapshot []byte

	if err := fixture.pool.QueryRow(fixture.ctx, snapshotQuery, walletID).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}

	return snapshot
}
