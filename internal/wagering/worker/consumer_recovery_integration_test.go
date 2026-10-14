//go:build integration

package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"jungle/internal/platform/auth"
	"jungle/internal/platform/config"
	"jungle/internal/platform/queue"
	"jungle/internal/platform/telemetry"
	"jungle/internal/wagering/application"
	"jungle/internal/wagering/domain/money"
	"jungle/internal/wagering/domain/wager"
	"jungle/internal/wagering/postgres"
)

// TestConsumerRecoveryProcess runs only as a separate process started by the recovery test.
func TestConsumerRecoveryProcess(t *testing.T) {
	mode := os.Getenv("JUNGLE_RECOVERY_CHILD")
	if mode == "" {
		t.Skip("subprocess entrypoint for TestConsumerRecoversAfterCommitBeforeDelete")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}

	defer pool.Close()

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	store := postgres.New(pool, logger)
	verifier := auth.New(cfg)
	client := queue.New(cfg)
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}

	broker := recoveryBroker(t, cfg.SQSEndpoint)
	address, err := broker.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(cfg.RequestsQueue)})
	if err != nil {
		t.Fatal(err)
	}

	// A short broker visibility lease makes real expiry reproducible without a minute-long test.
	received, err := broker.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:                    address.QueueUrl,
		MaxNumberOfMessages:         1,
		WaitTimeSeconds:             10,
		VisibilityTimeout:           2,
		MessageAttributeNames:       []string{"AccessToken"},
		MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameApproximateReceiveCount},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(received.Messages) != 1 {
		t.Fatalf("received %d messages; want 1", len(received.Messages))
	}

	wantCount := "1"

	if mode == "recover" {
		wantCount = "2"
	}

	message := received.Messages[0]
	if count := message.Attributes["ApproximateReceiveCount"]; count != wantCount {
		t.Fatalf("receive count = %q; want %q", count, wantCount)
	}

	metrics := &telemetry.Metrics{}
	workers := New(store, client, application.New(store), verifier, logger, metrics, cfg)
	workers.handle(ctx, message)

	if mode == "crash" {
		t.Fatal("consumer escaped the delete failpoint")
	}

	if metrics.Duplicates.Load() != 1 {
		t.Fatalf("recovered consumer saw %d replays; want 1", metrics.Duplicates.Load())
	}

	if metrics.Retries.Load() != 0 {
		t.Fatal("recovered consumer retained the message for retry")
	}
}

func TestConsumerRecoversAfterCommitBeforeDelete(t *testing.T) {
	fixture := setupRecovery(t)
	ctx := fixture.ctx
	pool := fixture.pool
	broker := fixture.broker
	requestsURL := fixture.requestsURL
	endpoint := fixture.endpoint

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	store := postgres.New(pool, logger)
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

	value, err := money.New("25.00", money.BRL)
	if err != nil {
		t.Fatal(err)
	}

	input := wager.Input{
		ProviderID:            "provider-a",
		ExternalTransactionID: "crash-bet",
		PlayerID:              state.PlayerID,
		WalletID:              state.ID,
		RoundID:               "round",
		GameID:                "game",
		Kind:                  wager.Bet,
		Money:                 value,
	}
	envelope := requested{MessageID: "crash-message", Type: "WagerTransactionRequested", OccurredAt: time.Now().UTC()}
	envelope.Data.Input = input
	envelope.Data.IdempotencyKey = "crash-key"
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}

	accessToken := recoveryToken(t, os.Getenv("OIDC_ISSUER"))
	_, err = broker.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(requestsURL),
		MessageBody:            aws.String(string(body)),
		MessageGroupId:         aws.String(state.ID),
		MessageDeduplicationId: aws.String("single-send"),
		MessageAttributes: map[string]types.MessageAttributeValue{
			"AccessToken": {DataType: aws.String("String"), StringValue: aws.String(accessToken)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	target, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}

	deleteReached := make(chan struct{}, 1)
	releaseProxy := make(chan struct{})
	proxy := httputil.NewSingleHostReverseProxy(target)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.Header.Get("X-Amz-Target"), ".DeleteMessage") {
			select {
			case deleteReached <- struct{}{}:
			default:
			}

			// Do not forward deletion: the parent kills the consumer at this precise boundary.
			select {
			case <-r.Context().Done():
			case <-releaseProxy:
			}

			return
		}

		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(releaseProxy) })

	crashed, crashOutput := startRecoveryProcess(t, ctx, "TestConsumerRecoveryProcess", "crash", server.URL)

	select {
	case <-deleteReached:
	case <-ctx.Done():
		t.Fatal("consumer did not reach delete after commit")
	}

	before := assertCommittedRecovery(t, pool, store, state.ID, body)
	if err := crashed.Process.Kill(); err != nil {
		t.Fatal(err)
	}

	err = crashed.Wait()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		t.Fatalf("crashed consumer did not exit from forced termination: %v; %s", err, crashOutput)
	}

	if exitError.ExitCode() != -1 {
		t.Fatalf("consumer exited normally instead of being killed: %s", crashOutput)
	}

	// A fresh process, new SQL connections and real SQS expiry must reproduce the committed result.
	recovered, recoveryOutput := startRecoveryProcess(t, ctx, "TestConsumerRecoveryProcess", "recover", endpoint)
	if err := recovered.Wait(); err != nil {
		t.Fatalf("recovered consumer failed: %v; %s", err, recoveryOutput)
	}

	after := assertCommittedRecovery(t, pool, store, state.ID, body)
	if !bytes.Equal(before, after) {
		t.Fatal("redelivery changed the financial or outbox snapshot")
	}

	remaining, err := broker.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(requestsURL),
		MaxNumberOfMessages: 1,
		WaitTimeSeconds:     3,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(remaining.Messages) != 0 {
		t.Fatal("recovered consumer did not remove the committed message")
	}
}

func recoveryToken(t *testing.T, issuer string) string {
	t.Helper()

	return recoveryProviderToken(t, issuer, "provider-a")
}

func recoveryProviderToken(
	t *testing.T,
	issuer string,
	clientID string,
) string {
	t.Helper()

	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.PostForm(issuer+"/protocol/openid-connect/token", url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {clientID},
		"client_secret": {clientID + "-local-secret"},
	})
	if err != nil {
		t.Fatal(err)
	}

	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close token response: %v", err)
		}
	}()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("token endpoint status = %d", response.StatusCode)
	}

	var token struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(response.Body).Decode(&token); err != nil {
		t.Fatal(err)
	}

	if token.AccessToken == "" {
		t.Fatal("token endpoint returned no token")
	}

	return token.AccessToken
}

func assertCommittedRecovery(
	t *testing.T,
	pool *pgxpool.Pool,
	store *postgres.Store,
	walletID string,
	body []byte,
) []byte {
	t.Helper()

	reconciliation, err := store.Reconcile(t.Context(), walletID)
	if err != nil {
		t.Fatal(err)
	}

	if !reconciliation.Consistent {
		t.Fatal("wallet and ledger diverged")
	}

	balance, err := reconciliation.StoredBalance.Amount()
	if err != nil {
		t.Fatal(err)
	}

	if balance != "75.00" || reconciliation.CheckedEntries != 2 {
		t.Fatalf("reconciliation = %+v; want 75.00 and two ledger entries", reconciliation)
	}

	transaction, err := store.Transaction(t.Context(), "", "provider-a", "crash-bet")
	if err != nil {
		t.Fatal(err)
	}

	if transaction.Status != wager.Processed {
		t.Fatalf("transaction status = %s; want PROCESSED", transaction.Status)
	}

	var inboxCount int
	var messageHash string

	err = pool.QueryRow(t.Context(), `SELECT count(*),COALESCE(min(payload_hash),'') FROM inbox
  WHERE consumer_name='wager' AND message_id='crash-message' AND completed_at IS NOT NULL`).Scan(
		&inboxCount,
		&messageHash,
	)
	if err != nil {
		t.Fatal(err)
	}

	sum := sha256.Sum256(body)
	if inboxCount != 1 || messageHash != hex.EncodeToString(sum[:]) {
		t.Fatal("committed inbox does not contain exactly one matching message hash")
	}

	var financialEvents, processedEvents, balanceEvents int

	err = pool.QueryRow(t.Context(), `SELECT count(*),
        count(*) FILTER (WHERE event_type='WagerTransactionProcessed'),
        count(*) FILTER (WHERE event_type='WalletBalanceChanged')
        FROM outbox WHERE payload->'data'->>'transactionId'=$1`, transaction.ID).Scan(
		&financialEvents,
		&processedEvents,
		&balanceEvents,
	)
	if err != nil {
		t.Fatal(err)
	}

	if financialEvents != 2 {
		t.Fatalf("financial events = %d; want processed and balance changed", financialEvents)
	}

	if processedEvents != 1 || balanceEvents != 1 {
		t.Fatalf("events = %d processed, %d balance changed; want 1 each", processedEvents, balanceEvents)
	}

	current, err := store.Wallet(t.Context(), walletID)
	if err != nil {
		t.Fatal(err)
	}

	if current.Version != 2 {
		t.Fatalf("wallet version = %d; want 2", current.Version)
	}

	var snapshot []byte

	err = pool.QueryRow(t.Context(), `SELECT jsonb_build_object(
        'wallet', (SELECT to_jsonb(w) FROM wallets AS w WHERE id=$1),
        'transactions', (SELECT jsonb_agg(to_jsonb(t) ORDER BY id)
            FROM wager_transactions AS t WHERE wallet_id=$1),
        'ledger', (SELECT jsonb_agg(to_jsonb(l) ORDER BY id)
            FROM wallet_ledger AS l WHERE wallet_id=$1),
        'inbox', (SELECT jsonb_agg(to_jsonb(i) ORDER BY consumer_name,message_id) FROM inbox AS i),
        'outbox', (SELECT jsonb_agg(to_jsonb(o) ORDER BY event_id)
            FROM outbox AS o WHERE aggregate_id=$1)
    )`, walletID).Scan(&snapshot)
	if err != nil {
		t.Fatal(err)
	}

	return snapshot
}
