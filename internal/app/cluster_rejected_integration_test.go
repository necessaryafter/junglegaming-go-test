//go:build integration

package app_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"jungle/internal/testsupport/sqsidentity"
	"jungle/internal/wagering/domain/wager"
	"jungle/internal/wagering/domain/wallet"
)

type rejectedClusterMessage struct {
	name       string
	wallet     wallet.Snapshot
	brokerID   string
	body       string
	credential string
	snapshot   []byte
	count      int
}

func TestThreeProcessesRejectedMessagesRestart(t *testing.T) {
	fixture := setupCluster(t)
	dlqURL := configureClusterDLQ(t, fixture)
	fault := newClusterFault(t, "observe")
	endpoints := fault.proxies(t, fixture)
	nodes := fixture.startThree(t, endpoints)
	cases := []rejectedClusterMessage{
		{name: "unknown field"},
		{name: "missing credential"},
		{name: "invalid signature"},
		{name: "another provider"},
	}
	byBrokerID := make(map[string]int)
	byBody := make(map[string]int)

	for i := range cases {
		current := &cases[i]
		current.wallet = fixture.open(t, nodes[i%3])
		input := clusterInput(t, current.wallet, wager.Bet, "25.00")
		envelope := map[string]any{
			"messageId":  current.name,
			"type":       "WagerTransactionRequested",
			"occurredAt": time.Now().UTC(),
			"data": struct {
				wager.Input
				IdempotencyKey string `json:"idempotencyKey"`
			}{Input: input, IdempotencyKey: input.ExternalTransactionID},
		}

		if current.name == "unknown field" {
			envelope["unsupported"] = true
		}

		encoded, err := json.Marshal(envelope)
		if err != nil {
			t.Fatal(err)
		}

		current.body = string(encoded)
		if current.name != "missing credential" {
			provider := "provider-a"
			if current.name == "another provider" {
				provider = "provider-b"
			}

			current.credential = sqsidentity.Signature(t, provider, current.body)
		}

		if current.name == "invalid signature" {
			parts := strings.Split(current.credential, ".")
			signature, err := base64.RawURLEncoding.DecodeString(parts[2])
			if err != nil {
				t.Fatal(err)
			}

			signature[0] ^= 1
			parts[2] = base64.RawURLEncoding.EncodeToString(signature)
			current.credential = strings.Join(parts, ".")
		}

		current.snapshot = clusterFinancialSnapshot(t, fixture, current.wallet.ID)
		message := types.Message{Body: aws.String(current.body)}

		if current.credential != "" {
			message.MessageAttributes = map[string]types.MessageAttributeValue{
				"MessageSignature": {DataType: aws.String("String"), StringValue: aws.String(current.credential)},
			}
		}

		current.brokerID = sendClusterMessage(
			t,
			fixture,
			message,
			current.wallet.ID,
			strings.ReplaceAll(current.name, " ", "-"),
		)
		byBrokerID[current.brokerID] = i
		byBody[current.body] = i
	}

	// Wait until every message has reached an original consumer, then replace the whole group.
	observed := 0

	for observed < len(cases) {
		delivery := awaitClusterDelivery(t, fixture.ctx, fault.received)
		index, exists := byBrokerID[delivery.messageID]

		if !exists {
			t.Fatal("cluster received an unrelated message")
		}

		count, err := strconv.Atoi(delivery.receiveCount)
		if err != nil {
			t.Fatal(err)
		}

		if count != cases[index].count+1 {
			t.Fatalf("%s receive count = %d; want %d", cases[index].name, count, cases[index].count+1)
		}

		if cases[index].count == 0 {
			observed++
		}

		cases[index].count = count
	}

	nodes = fixture.restart(t, nodes, endpoints)

	for {
		complete := true

		for _, current := range cases {
			if current.count < 5 {
				complete = false
			}
		}

		if complete {
			break
		}

		delivery := awaitClusterDelivery(t, fixture.ctx, fault.received)
		index, exists := byBrokerID[delivery.messageID]

		if !exists {
			t.Fatal("restart received an unrelated message")
		}

		count, err := strconv.Atoi(delivery.receiveCount)
		if err != nil {
			t.Fatal(err)
		}

		// Killing a process can consume a broker receipt before its response reaches the observer.
		// The counter must survive restart; gaps are valid for abandoned in-flight receives.
		if count < 2 || count > 5 {
			t.Fatalf(
				"%s restarted receive count = %d; want preserved count within 2..5",
				cases[index].name,
				count,
			)
		}

		cases[index].count = max(cases[index].count, count)
	}

	deadLetters := make(map[string]bool)

	for len(deadLetters) < len(cases) {
		received, err := fixture.broker.ReceiveMessage(fixture.ctx, &sqs.ReceiveMessageInput{
			QueueUrl:              aws.String(dlqURL),
			MaxNumberOfMessages:   1,
			WaitTimeSeconds:       10,
			MessageAttributeNames: []string{"MessageSignature"},
		})
		if err != nil {
			t.Fatal(err)
		}

		for _, message := range received.Messages {
			index, exists := byBody[aws.ToString(message.Body)]

			if !exists {
				t.Fatal("DLQ changed the original rejected message")
			}

			current := cases[index]
			attribute, present := message.MessageAttributes["MessageSignature"]

			if present != (current.credential != "") {
				t.Fatalf("DLQ changed credential presence for %s", current.name)
			}

			if aws.ToString(attribute.StringValue) != current.credential {
				t.Fatalf("DLQ changed credential for %s", current.name)
			}

			deadLetters[current.name] = true
		}
	}

	select {
	case <-fault.deleted:
		t.Fatal("consumer deleted a rejected message")
	default:
	}

	for _, current := range cases {
		if !bytes.Equal(current.snapshot, clusterFinancialSnapshot(t, fixture, current.wallet.ID)) {
			t.Fatalf("restart or rejected delivery changed %s financial state", current.name)
		}

		fixture.assertWallet(t, nodes, current.wallet, "100.00", 1)
	}

	var inbox int

	if err := fixture.pool.QueryRow(fixture.ctx, "SELECT COUNT(*) FROM inbox").Scan(&inbox); err != nil {
		t.Fatal(err)
	}

	if inbox != 0 {
		t.Fatal("rejected messages were marked as completed in the inbox")
	}

	for _, node := range nodes {
		if err := node.stop(); err != nil {
			t.Fatal(err)
		}
	}
}
