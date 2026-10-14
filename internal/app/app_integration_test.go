//go:build integration

package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/fx"

	"jungle/internal/app"
	"jungle/internal/wagering/httpapi"
	"jungle/internal/wagering/postgres"
	"jungle/migrations"
)

func TestRealDependenciesAndLifecycle(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	issuer := os.Getenv("TEST_OIDC_ISSUER")
	endpoint := os.Getenv("TEST_SQS_ENDPOINT")

	if databaseURL == "" || issuer == "" || endpoint == "" {
		t.Skip("TEST_DATABASE_URL, TEST_OIDC_ISSUER and TEST_SQS_ENDPOINT are required")
	}

	id, err := uuid.NewRandom()
	if err != nil {
		t.Fatal(err)
	}

	suffix := id.String()[:8]
	schema := "app_test_" + suffix
	admin, err := pgx.Connect(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if _, err := admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop test schema: %v", err)
		}

		if err := admin.Close(ctx); err != nil {
			t.Errorf("close admin connection: %v", err)
		}
	})

	address, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}

	query := address.Query()
	query.Set("search_path", schema)
	address.RawQuery = query.Encode()
	connection, err := pgx.Connect(t.Context(), address.String())
	if err != nil {
		t.Fatal(err)
	}

	if err := migrations.Apply(t.Context(), connection, "up"); err != nil {
		t.Fatal(err)
	}

	if err := connection.Close(t.Context()); err != nil {
		t.Fatal(err)
	}

	t.Setenv("DATABASE_URL", address.String())
	t.Setenv("HTTP_ADDRESS", "127.0.0.1:0")
	t.Setenv("OIDC_ISSUER", issuer)
	t.Setenv("OIDC_JWKS_URL", issuer+"/protocol/openid-connect/certs")
	t.Setenv("OIDC_AUDIENCE", "jungle-api")
	t.Setenv("SQS_ENDPOINT", endpoint)
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("SQS_REQUESTS_QUEUE", "integration-requests-"+suffix+".fifo")
	t.Setenv("SQS_EVENTS_QUEUE", "integration-events-"+suffix+".fifo")

	awsCfg, err := awsconfig.LoadDefaultConfig(t.Context(), awsconfig.WithRegion("us-east-1"))
	if err != nil {
		t.Fatal(err)
	}

	broker := sqs.NewFromConfig(awsCfg, func(options *sqs.Options) { options.BaseEndpoint = aws.String(endpoint) })
	queueURLs := make([]string, 0, 2)

	for _, name := range []string{os.Getenv("SQS_REQUESTS_QUEUE"), os.Getenv("SQS_EVENTS_QUEUE")} {
		out, err := broker.CreateQueue(t.Context(), &sqs.CreateQueueInput{QueueName: aws.String(name), Attributes: map[string]string{"FifoQueue": "true"}})
		if err != nil {
			t.Fatal(err)
		}

		queueURL := aws.ToString(out.QueueUrl)
		queueURLs = append(queueURLs, queueURL)

		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			if _, err := broker.DeleteQueue(ctx, &sqs.DeleteQueueInput{QueueUrl: aws.String(queueURL)}); err != nil {
				t.Errorf("delete test queue: %v", err)
			}
		})
	}

	var api *httpapi.API
	var store *postgres.Store

	process := fx.New(app.Options(), fx.NopLogger, fx.Populate(&api, &store))
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	if err := process.Start(ctx); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		if err := process.Stop(ctx); err != nil {
			t.Errorf("Fx shutdown: %v", err)
		}
	})

	server := httptest.NewServer(api.Handler())
	defer server.Close()

	internalToken := token(t, issuer, "jungle-internal")
	providerToken := token(t, issuer, "provider-a")
	otherToken := token(t, issuer, "provider-b")
	client := &http.Client{Timeout: 10 * time.Second}
	playerID, err := uuid.NewRandom()
	if err != nil {
		t.Fatal(err)
	}

	payload := fmt.Sprintf(`{"playerId":%q,"initialBalance":{"amount":"100.00","currency":"BRL"}}`, playerID.String())
	if status, _ := request(t, client, server.URL+"/wallets", "", "", payload); status != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d", status)
	}

	if status, _ := request(t, client, server.URL+"/wallets", providerToken, "", payload); status != http.StatusForbidden {
		t.Fatalf("provider wallet creation status = %d", status)
	}

	status, body := request(t, client, server.URL+"/wallets", internalToken, "", payload)
	if status != http.StatusCreated {
		t.Fatalf("wallet creation = %d: %s", status, body)
	}

	var wallet struct {
		ID string `json:"id"`
	}

	if err := json.Unmarshal(body, &wallet); err != nil {
		t.Fatal(err)
	}

	operation := fmt.Sprintf(`{"providerId":"provider-a","externalTransactionId":"bet","playerId":%q,"walletId":%q,"roundId":"round","gameId":"game","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}`, playerID.String(), wallet.ID)
	status, body = request(t, client, server.URL+"/wagering/transactions", providerToken, "bet-key", operation)
	if status != http.StatusOK {
		t.Fatalf("bet = %d: %s", status, body)
	}

	if status, _ := request(t, client, server.URL+"/wagering/transactions", otherToken, "bet-key", operation); status != http.StatusForbidden {
		t.Fatalf("cross-provider replay status = %d", status)
	}

	var data map[string]json.RawMessage
	if err := json.Unmarshal([]byte(operation), &data); err != nil {
		t.Fatal(err)
	}

	data["idempotencyKey"] = json.RawMessage(`"bet-key"`)
	envelope, err := json.Marshal(map[string]any{"messageId": "bet-via-sqs", "type": "WagerTransactionRequested", "occurredAt": time.Now().UTC(), "data": data})
	if err != nil {
		t.Fatal(err)
	}

	_, err = broker.SendMessage(t.Context(), &sqs.SendMessageInput{
		QueueUrl: aws.String(queueURLs[0]), MessageBody: aws.String(string(envelope)),
		MessageGroupId: aws.String(wallet.ID), MessageDeduplicationId: aws.String("first-delivery"),
		MessageAttributes: map[string]types.MessageAttributeValue{"AccessToken": {DataType: aws.String("String"), StringValue: aws.String(providerToken)}},
	})
	if err != nil {
		t.Fatal(err)
	}

	await(t, func() bool {
		// An inbox row proves the SQS path actually received the duplicate.
		var count int

		if err := admin.QueryRow(t.Context(), "SELECT count(*) FROM "+schema+".inbox WHERE message_id='bet-via-sqs'").Scan(&count); err != nil {
			t.Fatal(err)
		}

		return count == 1
	})

	reconciliation, err := store.Reconcile(t.Context(), wallet.ID)
	if err != nil {
		t.Fatal(err)
	}

	if !reconciliation.Consistent {
		t.Fatalf("HTTP/SQS reconciliation diverged: %+v", reconciliation)
	}

	if reconciliation.CheckedEntries != 2 {
		t.Fatalf("HTTP/SQS ledger entries = %d, want 2", reconciliation.CheckedEntries)
	}

	await(t, func() bool {
		messages, err := broker.ReceiveMessage(t.Context(), &sqs.ReceiveMessageInput{QueueUrl: aws.String(queueURLs[1]), MaxNumberOfMessages: 1})
		if err != nil {
			t.Fatal(err)
		}

		return len(messages.Messages) > 0
	})
}

func token(t *testing.T, issuer, clientID string) string {
	t.Helper()

	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {clientID}, "client_secret": {clientID + "-local-secret"}}
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.PostForm(issuer+"/protocol/openid-connect/token", form)
	if err != nil {
		t.Fatal(err)
	}

	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close token response: %v", err)
		}
	}()

	var result struct {
		AccessToken string `json:"access_token"`
	}

	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}

	if response.StatusCode != http.StatusOK || result.AccessToken == "" {
		t.Fatalf("token endpoint status = %d", response.StatusCode)
	}

	return result.AccessToken
}

func request(t *testing.T, client *http.Client, address, token, key, body string) (int, []byte) {
	t.Helper()

	r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, address, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}

	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", key)
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}

	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close API response: %v", err)
		}
	}()

	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}

	return response.StatusCode, data
}

func await(t *testing.T, ready func() bool) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		if ready() {
			return
		}

		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("timed out waiting for durable worker progress")
		}
	}
}
