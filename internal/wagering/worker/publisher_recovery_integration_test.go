//go:build integration

package worker

import (
	"bytes"
	"context"
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
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"jungle/internal/platform/config"
	"jungle/internal/platform/queue"
	"jungle/internal/platform/telemetry"
	"jungle/internal/wagering/application"
	"jungle/internal/wagering/domain/money"
	"jungle/internal/wagering/domain/wager"
	"jungle/internal/wagering/postgres"
)

// TestPublisherRecoveryProcess runs the production publisher in a separate process.
func TestPublisherRecoveryProcess(t *testing.T) {
	mode := os.Getenv("JUNGLE_RECOVERY_CHILD")
	if mode == "" {
		t.Skip("subprocess entrypoint for TestPublisherRecoversAfterInterruption")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	// Only the test binary shortens the lease; production configuration remains validated.
	cfg.OutboxLeaseDuration = 3 * time.Second
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}

	defer pool.Close()

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	store := postgres.New(pool, logger)
	client := queue.New(cfg)
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}

	metrics := &telemetry.Metrics{}
	workers := New(store, client, application.New(store), nil, logger, metrics, cfg)
	done := make(chan struct{})

	go func() {
		defer close(done)

		workers.publish(ctx)
	}()

	// Join the publisher even when assertions fail; pool ownership stays in this process.
	defer func() {
		cancel()
		<-done
	}()

	if mode == "crash" {
		<-done
		t.Fatal("publisher escaped the send failpoint")
	}

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		var published bool

		err := pool.QueryRow(
			ctx,
			`SELECT published_at IS NOT NULL FROM outbox WHERE event_id=$1`,
			os.Getenv("JUNGLE_RECOVERY_EVENT"),
		).Scan(&published)
		if err != nil {
			t.Fatal(err)
		}

		if published {
			cancel()
			<-done

			if metrics.Published.Load() != 1 {
				t.Fatalf("successful publishes = %d; want 1", metrics.Published.Load())
			}

			if metrics.Retries.Load() != 0 {
				t.Fatal("recovered publisher encountered a publication failure")
			}

			return
		}

		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("publisher did not complete the abandoned event")
		}
	}
}

type publicationAttempt struct {
	MessageBody            string
	MessageGroupID         string `json:"MessageGroupId"`
	MessageDeduplicationID string `json:"MessageDeduplicationId"`
	accepted               bool
}

func TestPublisherRecoversAfterInterruption(t *testing.T) {
	for _, stage := range []string{"before send", "after send before confirmation"} {
		t.Run(stage, func(t *testing.T) {
			fixture := setupRecovery(t)
			ctx := fixture.ctx
			logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
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

			value, err := money.New("25.00", money.BRL)
			if err != nil {
				t.Fatal(err)
			}

			result, err := store.Submit(ctx, application.Command{
				Key:           "publisher-bet-key",
				CorrelationID: "publisher-bet",
				Input: wager.Input{
					ProviderID:            "provider-a",
					ExternalTransactionID: "publisher-bet",
					PlayerID:              state.PlayerID,
					WalletID:              state.ID,
					RoundID:               "round",
					GameID:                "game",
					Kind:                  wager.Bet,
					Money:                 value,
				},
			})
			if err != nil {
				t.Fatal(err)
			}

			if result.Status != wager.Processed {
				t.Fatalf("bet status = %s; want PROCESSED", result.Status)
			}

			var eventID string
			var payload []byte

			err = fixture.pool.QueryRow(ctx, `SELECT event_id::text,payload FROM outbox
    WHERE event_type='WalletBalanceChanged' AND payload->'data'->>'transactionId'=$1`, result.TransactionID).Scan(
				&eventID,
				&payload,
			)
			if err != nil {
				t.Fatal(err)
			}

			// Keep other genuine events pending, so both publishers dispute this exact event.

			if _, err := fixture.pool.Exec(ctx, `UPDATE outbox SET next_attempt_at=now()+interval '1 day'
    WHERE event_id<>$1`, eventID); err != nil {
				t.Fatal(err)
			}

			t.Setenv("JUNGLE_RECOVERY_EVENT", eventID)
			before := publicationFinancialSnapshot(t, fixture.pool, state.ID)
			crashProxy, interruptedAttempts := publicationProxy(t, fixture.endpoint, stage)
			crashed, crashOutput := startRecoveryProcess(t, ctx, "TestPublisherRecoveryProcess", "crash", crashProxy)
			interrupted := awaitPublicationAttempt(t, ctx, interruptedAttempts)
			assertPublicationAttempt(t, interrupted, eventID, state.ID, payload)

			if interrupted.accepted != (stage == "after send before confirmation") {
				t.Fatal("publisher reached the wrong interruption boundary")
			}

			var abandonedLease string
			var published bool
			var leaseUntil time.Time
			var claims, failures int

			err = fixture.pool.QueryRow(ctx, `SELECT lease_id::text,lease_until,published_at IS NOT NULL,
    processing_attempts,publication_failures FROM outbox WHERE event_id=$1`, eventID).Scan(
				&abandonedLease,
				&leaseUntil,
				&published,
				&claims,
				&failures,
			)
			if err != nil {
				t.Fatal(err)
			}

			if abandonedLease == "" || published {
				t.Fatal("event was confirmed before the interrupted send returned")
			}

			if claims != 1 || failures != 0 {
				t.Fatalf("abandoned claims/failures = %d/%d; want 1/0", claims, failures)
			}

			if err := crashed.Process.Kill(); err != nil {
				t.Fatal(err)
			}

			err = crashed.Wait()
			var exitError *exec.ExitError
			if !errors.As(err, &exitError) {
				t.Fatalf("publisher was not terminated: %v; %s", err, crashOutput)
			}

			if exitError.ExitCode() != -1 {
				t.Fatalf("publisher exited normally instead of being killed: %s", crashOutput)
			}

			// The second process must wait for the actual SQL lease to expire, then send the same snapshot.
			recoveryProxy, recoveredAttempts := publicationProxy(t, fixture.endpoint, "observe")
			recovered, recoveryOutput := startRecoveryProcess(t, ctx, "TestPublisherRecoveryProcess", "recover", recoveryProxy)
			if err := recovered.Wait(); err != nil {
				t.Fatalf("recovered publisher failed: %v; %s", err, recoveryOutput)
			}

			completed := awaitPublicationAttempt(t, ctx, recoveredAttempts)
			assertPublicationAttempt(t, completed, eventID, state.ID, payload)

			if !completed.accepted {
				t.Fatal("SQS did not accept the recovered publication")
			}

			var leaseReleased bool
			var publishedAt time.Time
			var persistedPayload []byte

			err = fixture.pool.QueryRow(ctx, `SELECT published_at IS NOT NULL,published_at,
    lease_id IS NULL AND lease_until IS NULL,processing_attempts,publication_failures,payload
    FROM outbox WHERE event_id=$1`, eventID).Scan(
				&published,
				&publishedAt,
				&leaseReleased,
				&claims,
				&failures,
				&persistedPayload,
			)
			if err != nil {
				t.Fatal(err)
			}

			if !published || !leaseReleased {
				t.Fatal("recovered publisher did not confirm and release its lease")
			}

			if publishedAt.Before(leaseUntil) {
				t.Fatal("replacement publisher completed before the abandoned lease expired")
			}

			if claims != 2 || failures != 0 {
				t.Fatalf("recovered claims/failures = %d/%d; want 2/0", claims, failures)
			}

			if !bytes.Equal(payload, persistedPayload) {
				t.Fatal("recovery changed the outbox payload")
			}

			after := publicationFinancialSnapshot(t, fixture.pool, state.ID)
			if !bytes.Equal(before, after) {
				t.Fatal("publication recovery changed financial state")
			}

			reconciliation, err := store.Reconcile(ctx, state.ID)
			if err != nil {
				t.Fatal(err)
			}

			if !reconciliation.Consistent || reconciliation.CheckedEntries != 2 {
				t.Fatalf("publication recovery reconciliation = %+v", reconciliation)
			}

			delivered, err := fixture.broker.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
				QueueUrl:            aws.String(fixture.eventsURL),
				MaxNumberOfMessages: 1,
				WaitTimeSeconds:     3,
			})
			if err != nil {
				t.Fatal(err)
			}

			if len(delivered.Messages) != 1 {
				t.Fatalf("delivered events = %d; want 1", len(delivered.Messages))
			}

			if aws.ToString(delivered.Messages[0].Body) != string(payload) {
				t.Fatal("SQS delivered a different event snapshot")
			}
		})
	}
}

// publicationProxy blocks either side of SendMessage; successful calls always reach real SQS.
func publicationProxy(
	t *testing.T,
	endpoint string,
	stage string,
) (string, <-chan publicationAttempt) {
	t.Helper()

	target, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}

	attempts := make(chan publicationAttempt, 8)
	release := make(chan struct{})
	passthrough := httputil.NewSingleHostReverseProxy(target)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.Header.Get("X-Amz-Target"), ".SendMessage") {
			passthrough.ServeHTTP(w, r)

			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read send request", http.StatusBadRequest)

			return
		}

		if err := r.Body.Close(); err != nil {
			http.Error(w, "close send request", http.StatusBadRequest)

			return
		}

		var attempt publicationAttempt
		if err := json.Unmarshal(body, &attempt); err != nil {
			http.Error(w, "decode send request", http.StatusBadRequest)

			return
		}

		r.Body = io.NopCloser(bytes.NewReader(body))

		if stage == "before send" {
			attempts <- attempt

			select {
			case <-r.Context().Done():
			case <-release:
			}

			return
		}

		proxy := httputil.NewSingleHostReverseProxy(target)
		proxy.ModifyResponse = func(response *http.Response) error {
			if response.StatusCode != http.StatusOK {
				return nil
			}

			attempt.accepted = true
			attempts <- attempt

			if stage == "after send before confirmation" {
				select {
				case <-r.Context().Done():
				case <-release:
				}

				return context.Canceled
			}

			return nil
		}
		proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				return
			}

			http.Error(w, "SQS forwarding failed", http.StatusBadGateway)
		}
		proxy.ServeHTTP(w, r)
	}))

	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })

	return server.URL, attempts
}

func awaitPublicationAttempt(
	t *testing.T,
	ctx context.Context,
	attempts <-chan publicationAttempt,
) publicationAttempt {
	t.Helper()

	select {
	case attempt := <-attempts:
		return attempt
	case <-ctx.Done():
		t.Fatal("publisher did not reach the send boundary")

		return publicationAttempt{}
	}
}

func assertPublicationAttempt(
	t *testing.T,
	attempt publicationAttempt,
	eventID string,
	walletID string,
	payload []byte,
) {
	t.Helper()

	if attempt.MessageDeduplicationID != eventID {
		t.Fatalf("deduplication ID = %s; want %s", attempt.MessageDeduplicationID, eventID)
	}

	if attempt.MessageGroupID != walletID {
		t.Fatalf("message group = %s; want %s", attempt.MessageGroupID, walletID)
	}

	if attempt.MessageBody != string(payload) {
		t.Fatal("publisher changed the event snapshot")
	}
}

func publicationFinancialSnapshot(
	t *testing.T,
	pool *pgxpool.Pool,
	walletID string,
) []byte {
	t.Helper()

	var snapshot []byte

	err := pool.QueryRow(t.Context(), `SELECT jsonb_build_object(
  'wallet', (SELECT to_jsonb(w) FROM wallets AS w WHERE id=$1),
  'transactions', (SELECT jsonb_agg(to_jsonb(t) ORDER BY id)
   FROM wager_transactions AS t WHERE wallet_id=$1),
  'ledger', (SELECT jsonb_agg(to_jsonb(l) ORDER BY id)
   FROM wallet_ledger AS l WHERE wallet_id=$1),
  'inbox', (SELECT jsonb_agg(to_jsonb(i) ORDER BY consumer_name,message_id) FROM inbox AS i),
  'events', (SELECT jsonb_agg(jsonb_build_object(
   'id',event_id,'aggregateId',aggregate_id,'type',event_type,'payload',payload,'occurredAt',occurred_at
   ) ORDER BY event_id) FROM outbox WHERE aggregate_id=$1)
 )`, walletID).Scan(&snapshot)
	if err != nil {
		t.Fatal(err)
	}

	return snapshot
}
