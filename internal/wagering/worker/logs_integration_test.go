//go:build integration

package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"

	"jungle/internal/platform/config"
	"jungle/internal/platform/queue"
	"jungle/internal/platform/telemetry"
	"jungle/internal/testsupport/sqsidentity"
	"jungle/internal/wagering/application"
	"jungle/internal/wagering/domain/money"
	"jungle/internal/wagering/domain/wager"
	"jungle/internal/wagering/postgres"
)

func TestRealConsumerAndPublisherLogs(t *testing.T) {
	tests := []struct {
		name   string
		kind   wager.Kind
		amount string
		status wager.Status
	}{
		{name: "processed and replayed", kind: wager.Bet, amount: "25.00", status: wager.Processed},
		{name: "business rejected", kind: wager.Bet, amount: "125.00", status: wager.Rejected},
		{name: "reference pending", kind: wager.Refund, amount: "25.00", status: wager.PendingReference},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := setupRecovery(t)
			cfg, err := config.Load()
			if err != nil {
				t.Fatal(err)
			}

			cfg.SQSEndpoint = fixture.endpoint
			var output bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&output, nil))
			metrics := &telemetry.Metrics{}
			store := postgres.New(fixture.pool, logger, metrics)
			client := queue.New(cfg, metrics)
			if err := client.Start(fixture.ctx); err != nil {
				t.Fatal(err)
			}

			playerID, err := uuid.NewRandom()
			if err != nil {
				t.Fatal(err)
			}

			initial, err := money.New("100.00", money.BRL)
			if err != nil {
				t.Fatal(err)
			}

			state, err := store.Open(fixture.ctx, playerID.String(), initial)
			if err != nil {
				t.Fatal(err)
			}

			value, err := money.New(tt.amount, money.BRL)
			if err != nil {
				t.Fatal(err)
			}

			envelope := requested{MessageID: "logs-message", Type: "WagerTransactionRequested", OccurredAt: time.Now().UTC()}
			envelope.Data.Input = wager.Input{
				ProviderID: "provider-a", ExternalTransactionID: "logs-external",
				WalletID: state.ID, PlayerID: state.PlayerID, RoundID: "round", GameID: "game",
				Kind: tt.kind, Money: value,
			}
			envelope.Data.IdempotencyKey = "logs-idempotency-key"

			if tt.kind == wager.Refund {
				envelope.Data.ReferenceExternalTransactionID = "missing-reference"
			}

			body, err := json.Marshal(envelope)
			if err != nil {
				t.Fatal(err)
			}

			credential := sqsidentity.Signature(t, "provider-a", string(body))
			consumer := NewConsumer(client, application.New(store), sqsidentity.Verifier(t), logger, metrics)
			publisher := NewPublisher(store, client, logger, metrics, cfg)

			for _, deduplicationID := range []string{"logs-first", "logs-replay"} {
				_, err := fixture.broker.SendMessage(fixture.ctx, &sqs.SendMessageInput{
					QueueUrl: aws.String(fixture.requestsURL), MessageBody: aws.String(string(body)),
					MessageGroupId: aws.String(state.ID), MessageDeduplicationId: aws.String(deduplicationID),
					MessageAttributes: map[string]types.MessageAttributeValue{
						"MessageSignature": {DataType: aws.String("String"), StringValue: aws.String(credential)},
					},
				})
				if err != nil {
					t.Fatal(err)
				}

				messages, err := client.Receive(fixture.ctx)
				if err != nil {
					t.Fatal(err)
				}

				if len(messages) != 1 {
					t.Fatalf("received %d messages, want one", len(messages))
				}

				consumer.handle(fixture.ctx, messages[0])
			}

			committed := 0
			deleted := 0
			transactionID := ""

			for _, record := range logRecords(t, output.Bytes()) {
				if record["operation"] != "sqs_consume" {
					continue
				}

				for key, want := range map[string]any{
					"messageId": "logs-message", "correlationId": "logs-message",
					"walletId": state.ID, "providerId": "provider-a",
				} {
					if record[key] != want {
						t.Errorf("%s = %v, want %v", key, record[key], want)
					}
				}

				if record["brokerMessageId"] == nil {
					t.Fatal("broker message identity missing")
				}

				id, ok := record["transactionId"].(string)
				if !ok {
					t.Fatal("committed transaction identity missing")
				}

				if transactionID == "" {
					transactionID = id
				}

				if id != transactionID {
					t.Fatal("replay log changed the transaction identity")
				}

				switch record["msg"] {
				case "SQS transaction committed":
					if record["status"] != string(tt.status) {
						t.Fatalf("logged status = %v, want %s", record["status"], tt.status)
					}

					if record["idempotentReplay"] != (committed == 1) {
						t.Fatal("log did not distinguish first commit from replay")
					}

					committed++
				case "SQS message deleted":
					deleted++
				}
			}

			if committed != 2 {
				t.Fatalf("commit logs = %d, want two deliveries", committed)
			}

			if deleted != 2 {
				t.Fatalf("delete logs = %d, want two acknowledgements", deleted)
			}

			runCtx, stop := context.WithCancel(fixture.ctx)
			done := make(chan struct{})

			go func() {
				defer close(done)

				publisher.run(runCtx)
			}()

			t.Cleanup(func() {
				stop()
				<-done
			})

			for {
				stats, err := store.Statistics(fixture.ctx)
				if err != nil {
					t.Fatal(err)
				}

				if stats.OutboxPending == 0 {
					break
				}

				if !pause(fixture.ctx, 20*time.Millisecond) {
					t.Fatal("publisher did not drain committed events")
				}
			}

			stop()
			<-done
			published := 0

			for _, record := range logRecords(t, output.Bytes()) {
				if record["msg"] != "outbox publication accepted" {
					continue
				}

				if record["transactionId"] != transactionID {
					continue
				}

				published++

				if record["eventId"] == nil {
					t.Fatal("publication log omitted event identity")
				}

				if record["correlationId"] != "logs-message" {
					t.Fatal("publication lost the consumer correlation identity")
				}

				if record["walletId"] != state.ID {
					t.Fatal("publication lost the wallet identity")
				}
			}

			if published == 0 {
				t.Fatal("no publication was linked to the consumed transaction")
			}

			for _, forbidden := range []string{credential, string(body), state.PlayerID, `"` + tt.amount + `"`, `"100.00"`, "MessageSignature", "idempotencyKey"} {
				if strings.Contains(output.String(), forbidden) {
					t.Fatal("logs exposed credentials, personal data or financial payload")
				}
			}
		})
	}
}
