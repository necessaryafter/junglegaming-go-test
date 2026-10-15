//go:build integration

package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

type clusterDelivery struct {
	node         int
	eventID      string
	body         string
	group        string
	messageID    string
	receipt      string
	receiveCount string
	accepted     bool
}

type clusterFault struct {
	stage       string
	selected    atomic.Bool
	interrupted chan clusterDelivery
	sent        chan clusterDelivery
	deleted     chan clusterDelivery
	received    chan clusterDelivery
	receives    [3]chan struct{}
	release     chan struct{}
	releaseOnce sync.Once
}

func newClusterFault(t *testing.T, stage string) *clusterFault {
	t.Helper()

	fault := &clusterFault{
		stage:       stage,
		interrupted: make(chan clusterDelivery, 16),
		sent:        make(chan clusterDelivery, 64),
		deleted:     make(chan clusterDelivery, 16),
		received:    make(chan clusterDelivery, 32),
		release:     make(chan struct{}),
	}

	for i := range fault.receives {
		fault.receives[i] = make(chan struct{})

		if stage != "consumer commit" || i == 0 {
			close(fault.receives[i])
		}
	}

	t.Cleanup(fault.unblock)

	return fault
}

func (fault *clusterFault) proxies(t *testing.T, fixture *clusterFixture) [3]string {
	t.Helper()

	var endpoints [3]string

	for i := range endpoints {
		endpoints[i] = fault.proxy(t, fixture.endpoint, i)
	}

	return endpoints
}

func (fault *clusterFault) proxy(t *testing.T, endpoint string, node int) string {
	t.Helper()

	target, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}

	var mutex sync.Mutex
	receipts := make(map[string]clusterDelivery)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		operation := r.Header.Get("X-Amz-Target")

		if strings.HasSuffix(operation, ".ReceiveMessage") {
			select {
			case <-fault.receives[node]:
			case <-r.Context().Done():
				return
			case <-fault.release:
				return
			}
		}

		attempt := clusterDelivery{node: node}
		selected := false

		switch {
		case strings.HasSuffix(operation, ".SendMessage"), strings.HasSuffix(operation, ".DeleteMessage"):
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "read SQS request", http.StatusBadRequest)

				return
			}

			if err := r.Body.Close(); err != nil {
				http.Error(w, "close SQS request", http.StatusBadRequest)

				return
			}

			var request struct {
				MessageBody            string
				MessageDeduplicationID string `json:"MessageDeduplicationId"`
				MessageGroupID         string `json:"MessageGroupId"`
				ReceiptHandle          string
			}

			if err := json.Unmarshal(body, &request); err != nil {
				http.Error(w, "decode SQS request", http.StatusBadRequest)

				return
			}

			r.Body = io.NopCloser(bytes.NewReader(body))

			if strings.HasSuffix(operation, ".SendMessage") {
				attempt.eventID, attempt.body, attempt.group = request.MessageDeduplicationID, request.MessageBody, request.MessageGroupID
				selected = (fault.stage == "before send" || fault.stage == "after send") && fault.selected.CompareAndSwap(false, true)
			} else {
				mutex.Lock()
				attempt = receipts[request.ReceiptHandle]
				mutex.Unlock()

				selected = fault.stage == "consumer commit" && node == 0 && fault.selected.CompareAndSwap(false, true)
			}
		}

		if selected && fault.stage != "after send" {
			fault.interrupted <- attempt
			fault.hold(r)

			return
		}

		proxy := httputil.NewSingleHostReverseProxy(target)
		proxy.ModifyResponse = func(response *http.Response) error {
			if response.StatusCode != http.StatusOK {
				return nil
			}

			switch {
			case strings.HasSuffix(operation, ".ReceiveMessage"):
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
					return err
				}

				mutex.Lock()
				defer mutex.Unlock()

				for _, message := range received.Messages {
					receipt := aws.ToString(message.ReceiptHandle)
					delivery := clusterDelivery{
						node:         node,
						messageID:    aws.ToString(message.MessageId),
						receipt:      receipt,
						receiveCount: message.Attributes["ApproximateReceiveCount"],
					}
					receipts[receipt] = delivery
					fault.received <- delivery
				}

			case strings.HasSuffix(operation, ".SendMessage"):
				attempt.accepted = true

				if selected {
					fault.interrupted <- attempt
					fault.hold(r)

					return context.Canceled
				}

				fault.sent <- attempt

			case strings.HasSuffix(operation, ".DeleteMessage"):
				attempt.accepted = true
				fault.deleted <- attempt
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
	t.Cleanup(func() {
		fault.unblock()
		server.Close()
	})

	return server.URL
}

func (fault *clusterFault) hold(r *http.Request) {
	select {
	case <-r.Context().Done():
	case <-fault.release:
	}
}

func awaitClusterDelivery(
	t *testing.T,
	ctx context.Context,
	deliveries <-chan clusterDelivery,
) clusterDelivery {
	t.Helper()

	select {
	case delivery := <-deliveries:
		return delivery
	case <-ctx.Done():
		t.Fatal("timed out waiting for cluster delivery")

		return clusterDelivery{}
	}
}

func (fault *clusterFault) unblock() {
	fault.releaseOnce.Do(func() { close(fault.release) })
}
