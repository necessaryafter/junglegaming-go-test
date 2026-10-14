//go:build integration

package worker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"

	"jungle/internal/platform/auth"
	"jungle/internal/platform/config"
	"jungle/internal/platform/queue"
	"jungle/internal/platform/telemetry"
	"jungle/internal/wagering/application"
	"jungle/internal/wagering/domain/money"
	"jungle/internal/wagering/domain/wager"
	"jungle/internal/wagering/postgres"
)

const redriveReceiveLimit = 5

type redrivePolicy struct {
	DeadLetterTargetARN string      `json:"deadLetterTargetArn"`
	MaxReceiveCount     json.Number `json:"maxReceiveCount"`
}

type retryObservation struct {
	operation    string
	messageID    string
	receipt      string
	receiveCount string
	delay        int32
	err          error
}

type redriveScenario struct {
	failLedgerWrites bool
	unknownField     bool
	credential       func(*testing.T, string) string
	wantProvider     string // Empty means authentication must fail before authorization.
	reason           string
}

func TestInvalidMessageReachesDLQ(t *testing.T) {
	testMessageRedrive(t, redriveScenario{
		unknownField: true,
		credential:   recoveryToken,
		wantProvider: "provider-a",
		reason:       "invalid_request",
	})
}

func TestDatabaseFailureRedriveAndRecovery(t *testing.T) {
	testMessageRedrive(t, redriveScenario{
		failLedgerWrites: true,
		credential:       recoveryToken,
		wantProvider:     "provider-a",
		reason:           "unavailable",
	})
}

func TestUnauthorizedMessageReachesDLQ(t *testing.T) {
	tests := []struct {
		name         string
		credential   func(*testing.T, string) string
		wantProvider string
	}{
		{name: "missing credential"},
		{name: "invalid signature", credential: tamperedRecoveryToken},
		{
			name: "another provider",
			credential: func(t *testing.T, issuer string) string {
				return recoveryProviderToken(t, issuer, "provider-b")
			},
			wantProvider: "provider-b",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testMessageRedrive(t, redriveScenario{
				credential:   tt.credential,
				wantProvider: tt.wantProvider,
				reason:       "unauthorized",
			})
		})
	}
}

func tamperedRecoveryToken(t *testing.T, issuer string) string {
	t.Helper()

	parts := strings.Split(recoveryToken(t, issuer), ".")
	if len(parts) != 3 {
		t.Fatal("IdP did not return a compact JWT")
	}

	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}

	if len(signature) == 0 {
		t.Fatal("IdP returned an unsigned token")
	}

	signature[0] ^= 1
	parts[2] = base64.RawURLEncoding.EncodeToString(signature)

	return strings.Join(parts, ".")
}

func testMessageRedrive(t *testing.T, scenario redriveScenario) {
	t.Helper()

	// Five unmodified retry delays add up to 62 seconds before the broker moves the message.
	fixture := setupRecoveryWithin(t, 2*time.Minute)
	ctx := fixture.ctx
	assertProvisionedRedrive(t, fixture)
	dlqURL := configureTestRedrive(t, fixture)
	proxyURL, observations, deletes := observeRetries(t, fixture.endpoint)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	cfg.SQSEndpoint = proxyURL
	client := queue.New(cfg)
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	store := postgres.New(fixture.pool, logger)
	initial, err := money.New("100.00", money.BRL)
	if err != nil {
		t.Fatal(err)
	}

	playerID, err := uuid.NewRandom()
	if err != nil {
		t.Fatal(err)
	}

	state, err := store.Open(ctx, playerID.String(), initial)
	if err != nil {
		t.Fatal(err)
	}

	amount, err := money.New("25.00", money.BRL)
	if err != nil {
		t.Fatal(err)
	}

	envelope := requested{MessageID: "redrive-envelope", Type: "WagerTransactionRequested", OccurredAt: time.Now().UTC()}
	envelope.Data.Input = wager.Input{
		ProviderID:            "provider-a",
		ExternalTransactionID: "redrive-bet",
		PlayerID:              state.PlayerID,
		WalletID:              state.ID,
		RoundID:               "round",
		GameID:                "game",
		Kind:                  wager.Bet,
		Money:                 amount,
	}
	envelope.Data.IdempotencyKey = "redrive-key"
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}

	if scenario.unknownField {
		payload["unsupported"] = json.RawMessage(`true`)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}

	accessToken := ""
	if scenario.credential != nil {
		accessToken = scenario.credential(t, os.Getenv("OIDC_ISSUER"))
	}

	verifier := auth.New(cfg)
	principal, credentialErr := verifier.Verify(ctx, accessToken)

	if scenario.wantProvider == "" {
		if !errors.Is(credentialErr, auth.ErrUnauthorized) {
			t.Fatalf("credential verification = %v; want unauthorized", credentialErr)
		}
	} else {
		if credentialErr != nil {
			t.Fatal(credentialErr)
		}

		if principal.ProviderID != scenario.wantProvider {
			t.Fatalf("credential provider = %s; want %s", principal.ProviderID, scenario.wantProvider)
		}
	}

	var attributes map[string]types.MessageAttributeValue

	if accessToken != "" {
		attributes = map[string]types.MessageAttributeValue{
			"AccessToken": {DataType: aws.String("String"), StringValue: aws.String(accessToken)},
		}
	}

	before := publicationFinancialSnapshot(t, fixture.pool, state.ID)

	if scenario.failLedgerWrites {
		injectLedgerFailure(t, fixture)
	}

	sent, err := fixture.broker.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(fixture.requestsURL),
		MessageBody:            aws.String(string(body)),
		MessageGroupId:         aws.String(state.ID),
		MessageDeduplicationId: aws.String("single-redrive-send"),
		MessageAttributes:      attributes,
	})
	if err != nil {
		t.Fatal(err)
	}

	metrics := &telemetry.Metrics{}
	workers := New(store, client, application.New(store), verifier, logger, metrics, cfg)
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)

		workers.consume(runCtx)
	}()

	// Join before inspecting logs or deleting queues, including assertion failures.
	defer func() {
		stop()
		<-done
	}()

	for count := 1; count <= redriveReceiveLimit; count++ {
		received := nextRetryObservation(t, ctx, observations)
		if received.operation != "receive" {
			t.Fatalf("observation = %q; want receive", received.operation)
		}

		if received.messageID != aws.ToString(sent.MessageId) {
			t.Fatal("consumer received a different broker message")
		}

		if received.receiveCount != strconv.Itoa(count) {
			t.Fatalf("receive count = %q; want %d", received.receiveCount, count)
		}

		released := nextRetryObservation(t, ctx, observations)
		if released.operation != "release" {
			t.Fatalf("observation = %q; want release", released.operation)
		}

		if released.receipt != received.receipt {
			t.Fatal("consumer updated visibility for a different receipt")
		}

		wantDelay := int32(1 << count)
		if released.delay != wantDelay {
			t.Fatalf("retry delay = %d; want %d", released.delay, wantDelay)
		}

		if scenario.failLedgerWrites {
			assertLedgerFailures(t, fixture, count)
		}

		current := publicationFinancialSnapshot(t, fixture.pool, state.ID)
		if !bytes.Equal(before, current) {
			t.Fatalf("rejected delivery %d changed financial state", count)
		}
	}

	var deadLetter types.Message

	for deadLetter.Body == nil {
		received, err := fixture.broker.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:              aws.String(dlqURL),
			MaxNumberOfMessages:   1,
			WaitTimeSeconds:       10,
			MessageAttributeNames: []string{"AccessToken"},
		})
		if err != nil {
			t.Fatal(err)
		}

		if len(received.Messages) > 0 {
			deadLetter = received.Messages[0]
		}
	}

	stop()
	<-done

	if aws.ToString(deadLetter.Body) != string(body) {
		t.Fatal("DLQ changed the original message body")
	}

	attribute, exists := deadLetter.MessageAttributes["AccessToken"]
	if exists != (accessToken != "") {
		t.Fatal("DLQ changed whether the credential attribute is present")
	}

	if aws.ToString(attribute.StringValue) != accessToken {
		t.Fatal("DLQ did not preserve the original credential attribute")
	}

	if deletes.Load() != 0 {
		t.Fatal("consumer deleted a rejected message instead of allowing redrive")
	}

	if metrics.Retries.Load() != redriveReceiveLimit {
		t.Fatalf("retries = %d; want %d", metrics.Retries.Load(), redriveReceiveLimit)
	}

	if metrics.Duplicates.Load() != 0 {
		t.Fatal("rejected message was treated as a business replay")
	}

	select {
	case extra := <-observations:
		t.Fatalf("unexpected activity after receive limit: %s", extra.operation)
	default:
	}

	if !bytes.Equal(before, publicationFinancialSnapshot(t, fixture.pool, state.ID)) {
		t.Fatal("redrive changed financial state")
	}

	var inboxCount int
	if err := fixture.pool.QueryRow(ctx, "SELECT count(*) FROM inbox").Scan(&inboxCount); err != nil {
		t.Fatal(err)
	}

	if inboxCount != 0 {
		t.Fatal("rejected message was persisted as a completed inbox entry")
	}

	if accessToken != "" && bytes.Contains(logs.Bytes(), []byte(accessToken)) {
		t.Fatal("consumer logged a credential")
	}

	if bytes.Contains(logs.Bytes(), body) {
		t.Fatal("consumer logged the message payload")
	}

	reasons := 0
	decoder := json.NewDecoder(&logs)

	for {
		var record struct {
			Reason string `json:"reason"`
		}
		err := decoder.Decode(&record)
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			t.Fatal(err)
		}

		if record.Reason == scenario.reason {
			reasons++
		}
	}

	if reasons != redriveReceiveLimit {
		t.Fatalf("%s logs = %d; want %d", scenario.reason, reasons, redriveReceiveLimit)
	}

	remaining, err := fixture.broker.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(fixture.requestsURL),
		MaxNumberOfMessages: 1,
		WaitTimeSeconds:     1,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(remaining.Messages) != 0 {
		t.Fatal("dead-lettered message remained in the source queue")
	}

	if scenario.failLedgerWrites {
		recoverDeadLetter(t, fixture, dlqURL, deadLetter, envelope, store, verifier)
	}
}

func assertProvisionedRedrive(t *testing.T, fixture recoveryFixture) {
	t.Helper()

	source, err := fixture.broker.GetQueueUrl(fixture.ctx, &sqs.GetQueueUrlInput{
		QueueName: aws.String("wager-transactions.fifo"),
	})
	if err != nil {
		t.Fatal(err)
	}

	destination, err := fixture.broker.GetQueueUrl(fixture.ctx, &sqs.GetQueueUrlInput{
		QueueName: aws.String("wager-transactions-dlq.fifo"),
	})
	if err != nil {
		t.Fatal(err)
	}

	attributes, err := fixture.broker.GetQueueAttributes(fixture.ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       destination.QueueUrl,
		AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn, types.QueueAttributeNameFifoQueue},
	})
	if err != nil {
		t.Fatal(err)
	}

	if attributes.Attributes["FifoQueue"] != "true" {
		t.Fatal("provisioned DLQ is not FIFO")
	}

	assertRedrivePolicy(t, fixture, aws.ToString(source.QueueUrl), attributes.Attributes["QueueArn"])
}

func configureTestRedrive(t *testing.T, fixture recoveryFixture) string {
	t.Helper()

	name := strings.TrimSuffix(os.Getenv("SQS_REQUESTS_QUEUE"), ".fifo") + "-dlq.fifo"
	created, err := fixture.broker.CreateQueue(fixture.ctx, &sqs.CreateQueueInput{
		QueueName:  aws.String(name),
		Attributes: map[string]string{"FifoQueue": "true"},
	})
	if err != nil {
		t.Fatal(err)
	}

	dlqURL := aws.ToString(created.QueueUrl)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if _, err := fixture.broker.DeleteQueue(cleanup, &sqs.DeleteQueueInput{QueueUrl: aws.String(dlqURL)}); err != nil {
			t.Errorf("delete test DLQ: %v", err)
		}
	})

	attributes, err := fixture.broker.GetQueueAttributes(fixture.ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       aws.String(dlqURL),
		AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn},
	})
	if err != nil {
		t.Fatal(err)
	}

	arn := attributes.Attributes["QueueArn"]
	policy, err := json.Marshal(redrivePolicy{DeadLetterTargetARN: arn, MaxReceiveCount: json.Number(strconv.Itoa(redriveReceiveLimit))})
	if err != nil {
		t.Fatal(err)
	}

	_, err = fixture.broker.SetQueueAttributes(fixture.ctx, &sqs.SetQueueAttributesInput{
		QueueUrl: aws.String(fixture.requestsURL),
		Attributes: map[string]string{
			"RedrivePolicy":     string(policy),
			"VisibilityTimeout": "60",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	assertRedrivePolicy(t, fixture, fixture.requestsURL, arn)

	return dlqURL
}

func assertRedrivePolicy(
	t *testing.T,
	fixture recoveryFixture,
	sourceURL string,
	dlqARN string,
) {
	t.Helper()

	attributes, err := fixture.broker.GetQueueAttributes(fixture.ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(sourceURL),
		AttributeNames: []types.QueueAttributeName{
			types.QueueAttributeNameRedrivePolicy,
			types.QueueAttributeNameVisibilityTimeout,
			types.QueueAttributeNameFifoQueue,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	var policy redrivePolicy
	if err := json.Unmarshal([]byte(attributes.Attributes["RedrivePolicy"]), &policy); err != nil {
		t.Fatal(err)
	}

	if policy.DeadLetterTargetARN != dlqARN {
		t.Fatal("redrive points to the wrong DLQ")
	}

	if policy.MaxReceiveCount.String() != strconv.Itoa(redriveReceiveLimit) {
		t.Fatalf("maxReceiveCount = %s; want %d", policy.MaxReceiveCount, redriveReceiveLimit)
	}

	if attributes.Attributes["FifoQueue"] != "true" {
		t.Fatal("request queue is not FIFO")
	}

	if attributes.Attributes["VisibilityTimeout"] != "60" {
		t.Fatal("source queue does not use the provisioned visibility timeout")
	}
}

func nextRetryObservation(
	t *testing.T,
	ctx context.Context,
	observations <-chan retryObservation,
) retryObservation {
	t.Helper()

	select {
	case observation := <-observations:
		if observation.err != nil {
			t.Fatal(observation.err)
		}

		return observation
	case <-ctx.Done():
		t.Fatal("timed out waiting for broker retry activity")

		return retryObservation{}
	}
}

// observeRetries records real broker responses without changing requests, messages, or delays.
func observeRetries(
	t *testing.T,
	endpoint string,
) (string, <-chan retryObservation, *atomic.Uint64) {
	t.Helper()

	target, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}

	observations := make(chan retryObservation, 16)
	deletes := &atomic.Uint64{}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = func(response *http.Response) error {
		operation := response.Request.Header.Get("X-Amz-Target")
		if strings.HasSuffix(operation, ".DeleteMessage") {
			deletes.Add(1)
		}

		if response.StatusCode != http.StatusOK {
			return nil
		}

		if !strings.HasSuffix(operation, ".ReceiveMessage") {
			return nil
		}

		body, err := io.ReadAll(response.Body)
		if err != nil {
			return err
		}

		if err := response.Body.Close(); err != nil {
			return err
		}

		response.Body = io.NopCloser(bytes.NewReader(body))
		var received struct{ Messages []types.Message }
		if err := json.Unmarshal(body, &received); err != nil {
			observations <- retryObservation{err: fmt.Errorf("decode observed receive: %w", err)}

			return err
		}

		for _, message := range received.Messages {
			observations <- retryObservation{
				operation:    "receive",
				messageID:    aws.ToString(message.MessageId),
				receipt:      aws.ToString(message.ReceiptHandle),
				receiveCount: message.Attributes["ApproximateReceiveCount"],
			}
		}

		return nil
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.Header.Get("X-Amz-Target"), ".ChangeMessageVisibility") {
			proxy.ServeHTTP(w, r)

			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read visibility request", http.StatusBadRequest)

			return
		}

		if err := r.Body.Close(); err != nil {
			http.Error(w, "close visibility request", http.StatusBadRequest)

			return
		}

		var visibility struct {
			ReceiptHandle     string
			VisibilityTimeout int32
		}
		if err := json.Unmarshal(body, &visibility); err != nil {
			http.Error(w, "decode visibility request", http.StatusBadRequest)

			return
		}

		r.Body = io.NopCloser(bytes.NewReader(body))
		releaseProxy := httputil.NewSingleHostReverseProxy(target)
		releaseProxy.ModifyResponse = func(response *http.Response) error {
			if response.StatusCode == http.StatusOK {
				observations <- retryObservation{operation: "release", receipt: visibility.ReceiptHandle, delay: visibility.VisibilityTimeout}
			}

			return nil
		}
		releaseProxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)

	return server.URL, observations, deletes
}
