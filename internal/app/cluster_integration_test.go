//go:build integration

package app_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"

	"jungle/internal/app"
	"jungle/internal/wagering/application"
	"jungle/internal/wagering/domain/money"
	"jungle/internal/wagering/domain/wager"
	"jungle/internal/wagering/domain/wallet"
	"jungle/internal/wagering/httpapi"
	"jungle/migrations"
)

// TestClusterProcess is a test-only entrypoint for a complete, independent Fx application.
func TestClusterProcess(t *testing.T) {
	readyPath := os.Getenv("JUNGLE_CLUSTER_READY")
	if readyPath == "" {
		t.Skip("entrypoint for the three-process integration tests")
	}

	var api *httpapi.API
	process := fx.New(
		app.Options(),
		fx.NopLogger,
		fx.Decorate(func(*slog.Logger) *slog.Logger {
			return slog.New(slog.NewJSONHandler(os.Stdout, nil))
		}),
		fx.Populate(&api),
	)
	startup, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()

	if err := process.Start(startup); err != nil {
		t.Fatal(err)
	}

	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()

		if err := process.Stop(cleanup); err != nil {
			t.Errorf("stop cluster application: %v", err)
		}
	}()

	// The test listener exposes the production API handler without reserving fixed host ports.
	server := httptest.NewServer(api.Handler())
	defer server.Close()

	ready, err := json.Marshal(clusterReady{URL: server.URL, PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(readyPath+".tmp", ready, 0600); err != nil {
		t.Fatal(err)
	}

	if err := os.Rename(readyPath+".tmp", readyPath); err != nil {
		t.Fatal(err)
	}

	commands := bufio.NewScanner(os.Stdin)
	if !commands.Scan() {
		t.Fatalf("missing shutdown command: %v", commands.Err())
	}

	if commands.Text() != "stop" {
		t.Fatal("unexpected cluster command")
	}
}

type clusterReady struct {
	URL string
	PID int
}

type clusterNode struct {
	ready   clusterReady
	command *exec.Cmd
	input   io.WriteCloser
	output  *bytes.Buffer
	reaped  bool
}

func (n *clusterNode) stop() error {
	if n.reaped {
		return nil
	}

	if _, err := io.WriteString(n.input, "stop\n"); err != nil {
		return err
	}

	if err := n.input.Close(); err != nil {
		return err
	}

	err := n.command.Wait()
	n.reaped = true
	if err != nil {
		return fmt.Errorf("stop application: %w; %s", err, n.output)
	}

	return nil
}

func (n *clusterNode) kill() error {
	if n.reaped {
		return nil
	}

	if err := n.command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}

	err := n.command.Wait()
	n.reaped = true

	// Wait owns the pipe and may already have closed it.
	if closeErr := n.input.Close(); closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
		return closeErr
	}

	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		return fmt.Errorf("application was not killed: %w; %s", err, n.output)
	}

	if exitError.ExitCode() != -1 {
		return fmt.Errorf("application exited before forced termination: %s", n.output)
	}

	return nil
}

type clusterFixture struct {
	ctx           context.Context
	pool          *pgxpool.Pool
	broker        *sqs.Client
	databaseURL   string
	endpoint      string
	issuer        string
	requestsURL   string
	eventsURL     string
	prefix        string
	internalToken string
	providerToken string
	client        *http.Client
}

func setupCluster(t *testing.T) *clusterFixture {
	t.Helper()

	databaseURL := os.Getenv("TEST_DATABASE_URL")
	issuer := os.Getenv("TEST_OIDC_ISSUER")
	endpoint := os.Getenv("TEST_SQS_ENDPOINT")
	if databaseURL == "" || issuer == "" || endpoint == "" {
		t.Skip("TEST_DATABASE_URL, TEST_OIDC_ISSUER and TEST_SQS_ENDPOINT are required")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	t.Cleanup(cancel)

	randomID, err := uuid.NewRandom()
	if err != nil {
		t.Fatal(err)
	}

	prefix := "cluster_" + strings.ReplaceAll(randomID.String(), "-", "")
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(admin.Close)

	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+prefix); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()

		if _, err := admin.Exec(cleanup, "DROP SCHEMA "+prefix+" CASCADE"); err != nil {
			t.Errorf("drop cluster schema: %v", err)
		}
	})

	address, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}

	params := address.Query()
	params.Set("search_path", prefix)
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

	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion("us-east-1"))
	if err != nil {
		t.Fatal(err)
	}

	fixture := &clusterFixture{
		ctx:           ctx,
		pool:          pool,
		broker:        sqs.NewFromConfig(cfg, func(options *sqs.Options) { options.BaseEndpoint = aws.String(endpoint) }),
		databaseURL:   address.String(),
		endpoint:      endpoint,
		issuer:        issuer,
		prefix:        prefix,
		internalToken: token(t, issuer, "jungle-internal"),
		providerToken: token(t, issuer, "provider-a"),
		client:        &http.Client{Timeout: 15 * time.Second},
	}

	for _, name := range []string{prefix + "-requests.fifo", prefix + "-events.fifo"} {
		created, err := fixture.broker.CreateQueue(ctx, &sqs.CreateQueueInput{
			QueueName:  aws.String(name),
			Attributes: map[string]string{"FifoQueue": "true"},
		})
		if err != nil {
			t.Fatal(err)
		}

		queueURL := aws.ToString(created.QueueUrl)
		if fixture.requestsURL == "" {
			fixture.requestsURL = queueURL
		} else {
			fixture.eventsURL = queueURL
		}

		t.Cleanup(func() {
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()

			if _, err := fixture.broker.DeleteQueue(cleanup, &sqs.DeleteQueueInput{QueueUrl: aws.String(queueURL)}); err != nil {
				t.Errorf("delete cluster queue: %v", err)
			}
		})
	}

	return fixture
}

func (f *clusterFixture) start(t *testing.T, index int, endpoint string) *clusterNode {
	t.Helper()

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	readyPath := filepath.Join(t.TempDir(), "ready.json")
	address, err := url.Parse(f.databaseURL)
	if err != nil {
		t.Fatal(err)
	}

	params := address.Query()
	params.Set("application_name", fmt.Sprintf("%s_node_%d", f.prefix, index))
	address.RawQuery = params.Encode()
	command := exec.CommandContext(f.ctx, executable, "-test.run=^TestClusterProcess$", "-test.v")
	command.Env = append(os.Environ(),
		"JUNGLE_CLUSTER_READY="+readyPath,
		"DATABASE_URL="+address.String(),
		"HTTP_ADDRESS=127.0.0.1:0",
		"OIDC_ISSUER="+f.issuer,
		"OIDC_JWKS_URL="+f.issuer+"/protocol/openid-connect/certs",
		"OIDC_AUDIENCE=jungle-api",
		"SQS_ENDPOINT="+endpoint,
		"SQS_REQUESTS_QUEUE="+f.prefix+"-requests.fifo",
		"SQS_EVENTS_QUEUE="+f.prefix+"-events.fifo",
		"OUTBOX_LEASE_DURATION=20s",
	)
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}

	node := &clusterNode{command: command, input: input, output: &bytes.Buffer{}}
	command.Stdout = node.output
	command.Stderr = node.output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if !node.reaped {
			if err := node.kill(); err != nil {
				t.Errorf("clean cluster process: %v", err)
			}
		}
	})

	awaitCluster(t, f.ctx, func() bool {
		data, err := os.ReadFile(readyPath)
		if errors.Is(err, os.ErrNotExist) {
			return false
		}

		if err != nil {
			t.Fatal(err)
		}

		if err := json.Unmarshal(data, &node.ready); err != nil {
			t.Fatal(err)
		}

		return true
	})

	return node
}

func awaitCluster(t *testing.T, ctx context.Context, ready func() bool) {
	t.Helper()

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for !ready() {
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("timed out waiting for cluster progress")
		}
	}
}

func (f *clusterFixture) startThree(t *testing.T, endpoints [3]string) []*clusterNode {
	t.Helper()

	nodes := make([]*clusterNode, 3)
	pids := make(map[int]bool)

	for i := range nodes {
		nodes[i] = f.start(t, i, endpoints[i])
		if pids[nodes[i].ready.PID] {
			t.Fatal("applications share a process ID")
		}

		pids[nodes[i].ready.PID] = true
		request, err := http.NewRequestWithContext(f.ctx, http.MethodGet, nodes[i].ready.URL+"/health/ready", nil)
		if err != nil {
			t.Fatal(err)
		}

		response, err := f.client.Do(request)
		if err != nil {
			t.Fatal(err)
		}

		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}

		if response.StatusCode != http.StatusOK {
			t.Fatal("cluster application is not ready")
		}
	}

	const connectionsQuery = `
    SELECT COUNT(DISTINCT application_name)
    FROM pg_stat_activity
    WHERE application_name = ANY($1)
 `
	names := []string{f.prefix + "_node_0", f.prefix + "_node_1", f.prefix + "_node_2"}
	var pools int

	if err := f.pool.QueryRow(f.ctx, connectionsQuery, names).Scan(&pools); err != nil {
		t.Fatal(err)
	}

	if pools != 3 {
		t.Fatalf("independent SQL application pools = %d; want 3", pools)
	}

	return nodes
}

func (f *clusterFixture) restart(
	t *testing.T,
	nodes []*clusterNode,
	endpoints [3]string,
) []*clusterNode {
	t.Helper()

	for _, node := range nodes {
		if err := node.kill(); err != nil {
			t.Fatal(err)
		}
	}

	replacements := f.startThree(t, endpoints)
	for i, node := range replacements {
		if node.ready.PID == nodes[i].ready.PID {
			t.Fatal("restart reused the original process")
		}
	}

	return replacements
}

func (f *clusterFixture) open(t *testing.T, node *clusterNode) wallet.Snapshot {
	t.Helper()

	playerID, err := uuid.NewRandom()
	if err != nil {
		t.Fatal(err)
	}

	body := fmt.Sprintf(`{"playerId":%q,"initialBalance":{"amount":"100.00","currency":"BRL"}}`, playerID.String())
	status, response := request(t, f.client, node.ready.URL+"/wallets", f.internalToken, "", body)
	if status != http.StatusCreated {
		t.Fatalf("open cluster wallet: status %d", status)
	}

	var state wallet.Snapshot
	if err := json.Unmarshal(response, &state); err != nil {
		t.Fatal(err)
	}

	return state
}

func clusterInput(t *testing.T, state wallet.Snapshot, kind wager.Kind, amount string) wager.Input {
	t.Helper()

	value, err := money.New(amount, money.BRL)
	if err != nil {
		t.Fatal(err)
	}

	externalID, err := uuid.NewRandom()
	if err != nil {
		t.Fatal(err)
	}

	return wager.Input{
		ProviderID:            "provider-a",
		ExternalTransactionID: externalID.String(),
		WalletID:              state.ID,
		PlayerID:              state.PlayerID,
		RoundID:               "cluster-round",
		GameID:                "cluster-game",
		Kind:                  kind,
		Money:                 value,
	}
}

func (f *clusterFixture) submit(
	node *clusterNode,
	input wager.Input,
) (result application.Result, status int, err error) {
	body, err := json.Marshal(input)
	if err != nil {
		return application.Result{}, 0, err
	}

	request, err := http.NewRequestWithContext(f.ctx, http.MethodPost, node.ready.URL+"/wagering/transactions", bytes.NewReader(body))
	if err != nil {
		return application.Result{}, 0, err
	}

	request.Header.Set("Authorization", "Bearer "+f.providerToken)
	request.Header.Set("Idempotency-Key", input.ExternalTransactionID)
	request.Header.Set("Content-Type", "application/json")
	response, err := f.client.Do(request)
	if err != nil {
		return application.Result{}, 0, err
	}

	defer func() {
		err = errors.Join(err, response.Body.Close())
	}()
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return application.Result{}, response.StatusCode, err
	}

	return result, response.StatusCode, nil
}

func (f *clusterFixture) assertWallet(
	t *testing.T,
	nodes []*clusterNode,
	state wallet.Snapshot,
	balance string,
	entries int64,
) {
	t.Helper()

	for _, node := range nodes {
		status, body := request(t, f.client, node.ready.URL+"/wallets/"+state.ID+"/reconciliation", f.internalToken, "", "")
		if status != http.StatusOK {
			t.Fatalf("cluster reconciliation status = %d", status)
		}

		var result application.Reconciliation
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatal(err)
		}

		amount, err := result.StoredBalance.Amount()
		if err != nil {
			t.Fatal(err)
		}

		if !result.Consistent || amount != balance {
			t.Fatalf("cluster balance = %s, consistent=%t; want %s", amount, result.Consistent, balance)
		}

		if result.CheckedEntries != entries {
			t.Fatalf("cluster ledger entries = %d; want %d", result.CheckedEntries, entries)
		}
	}
}
