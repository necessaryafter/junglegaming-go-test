//go:build integration

package worker

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"jungle/internal/testsupport/sqsidentity"
	"jungle/migrations"
)

type recoveryFixture struct {
	ctx         context.Context
	pool        *pgxpool.Pool
	broker      *sqs.Client
	requestsURL string
	eventsURL   string
	endpoint    string
}

func setupRecovery(t *testing.T) recoveryFixture {
	t.Helper()

	return setupRecoveryWithin(t, 60*time.Second)
}

func setupRecoveryWithin(t *testing.T, timeout time.Duration) recoveryFixture {
	t.Helper()

	databaseURL := os.Getenv("TEST_DATABASE_URL")
	issuer := os.Getenv("TEST_OIDC_ISSUER")
	endpoint := os.Getenv("TEST_SQS_ENDPOINT")
	if databaseURL == "" || issuer == "" || endpoint == "" {
		t.Skip("TEST_DATABASE_URL, TEST_OIDC_ISSUER and TEST_SQS_ENDPOINT are required")
	}

	sqsidentity.Configure(t)

	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	t.Cleanup(cancel)

	suffix, err := uuid.NewRandom()
	if err != nil {
		t.Fatal(err)
	}

	schema := "worker_recovery_" + strings.ReplaceAll(suffix.String(), "-", "")
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(admin.Close)

	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()

		if _, err := admin.Exec(cleanup, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop recovery schema: %v", err)
		}
	})

	address, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}

	params := address.Query()
	params.Set("search_path", schema)
	address.RawQuery = params.Encode()
	pool, err := pgxpool.New(ctx, address.String())
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(pool.Close)

	connection, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}

	err = migrations.Apply(ctx, connection.Conn(), "up")
	connection.Release()
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("DATABASE_URL", address.String())
	t.Setenv("OIDC_ISSUER", issuer)
	t.Setenv("OIDC_JWKS_URL", issuer+"/protocol/openid-connect/certs")
	t.Setenv("OIDC_AUDIENCE", "jungle-api")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("SQS_REQUESTS_QUEUE", "recovery-requests-"+suffix.String()+".fifo")
	t.Setenv("SQS_EVENTS_QUEUE", "recovery-events-"+suffix.String()+".fifo")

	broker := recoveryBroker(t, endpoint)
	var requestsURL, eventsURL string

	for _, name := range []string{os.Getenv("SQS_REQUESTS_QUEUE"), os.Getenv("SQS_EVENTS_QUEUE")} {
		created, err := broker.CreateQueue(ctx, &sqs.CreateQueueInput{
			QueueName:  aws.String(name),
			Attributes: map[string]string{"FifoQueue": "true"},
		})
		if err != nil {
			t.Fatal(err)
		}

		queueURL := aws.ToString(created.QueueUrl)
		if requestsURL == "" {
			requestsURL = queueURL
		} else {
			eventsURL = queueURL
		}

		t.Cleanup(func() {
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()

			if _, err := broker.DeleteQueue(cleanup, &sqs.DeleteQueueInput{QueueUrl: aws.String(queueURL)}); err != nil {
				t.Errorf("delete recovery queue: %v", err)
			}
		})
	}

	return recoveryFixture{
		ctx:         ctx,
		pool:        pool,
		broker:      broker,
		requestsURL: requestsURL,
		eventsURL:   eventsURL,
		endpoint:    endpoint,
	}
}

func recoveryBroker(t *testing.T, endpoint string) *sqs.Client {
	t.Helper()

	cfg, err := awsconfig.LoadDefaultConfig(t.Context(), awsconfig.WithRegion("us-east-1"))
	if err != nil {
		t.Fatal(err)
	}

	return sqs.NewFromConfig(cfg, func(options *sqs.Options) {
		options.BaseEndpoint = aws.String(endpoint)
	})
}

func startRecoveryProcess(
	t *testing.T,
	ctx context.Context,
	testName string,
	mode string,
	endpoint string,
) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	command := exec.CommandContext(ctx, executable, "-test.run=^"+testName+"$", "-test.v")
	command.Env = append(os.Environ(), "JUNGLE_RECOVERY_CHILD="+mode, "SQS_ENDPOINT="+endpoint)
	output := &bytes.Buffer{}
	command.Stdout = output
	command.Stderr = output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		// CommandContext owns cancellation; Kill also covers assertion failures before the deadline.
		if command.ProcessState == nil {
			if err := command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				t.Errorf("stop recovery subprocess: %v", err)
			}

			if err := command.Wait(); err != nil {
				var exitError *exec.ExitError
				if !errors.As(err, &exitError) {
					t.Errorf("reap recovery subprocess: %v", err)
				}
			}
		}
	})

	return command, output
}
