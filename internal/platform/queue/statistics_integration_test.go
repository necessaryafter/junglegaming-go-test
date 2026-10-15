//go:build integration

package queue_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cloudwatchtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"

	"jungle/internal/platform/config"
	"jungle/internal/platform/queue"
	"jungle/internal/platform/telemetry"
)

func TestRealQueueMetricsDoNotConsumeDLQ(t *testing.T) {
	endpoint := os.Getenv("TEST_SQS_ENDPOINT")
	if endpoint == "" {
		t.Skip("TEST_SQS_ENDPOINT is required for real SQS and CloudWatch integration")
	}

	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion("us-east-1"))
	if err != nil {
		t.Fatal(err)
	}

	broker := sqs.NewFromConfig(
		cfg,
		func(options *sqs.Options) { options.BaseEndpoint = aws.String(endpoint) },
	)
	cloud := cloudwatch.NewFromConfig(
		cfg,
		func(options *cloudwatch.Options) { options.BaseEndpoint = aws.String(endpoint) },
	)
	id, err := uuid.NewRandom()
	if err != nil {
		t.Fatal(err)
	}

	prefix := "metrics-" + id.String()
	urls := make(map[string]string)

	for _, role := range []string{"requests", "events", "requests_dlq", "events_dlq"} {
		out, err := broker.CreateQueue(ctx, &sqs.CreateQueueInput{
			QueueName:  aws.String(prefix + "-" + role + ".fifo"),
			Attributes: map[string]string{"FifoQueue": "true"},
		})
		if err != nil {
			t.Fatal(err)
		}

		url := aws.ToString(out.QueueUrl)
		urls[role] = url

		t.Cleanup(func() {
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()

			if _, err := broker.DeleteQueue(cleanup, &sqs.DeleteQueueInput{QueueUrl: aws.String(url)}); err != nil {
				t.Errorf("delete metrics queue: %v", err)
			}
		})
	}

	for _, role := range []string{"requests", "events"} {
		attributes, err := broker.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
			QueueUrl:       aws.String(urls[role+"_dlq"]),
			AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn},
		})
		if err != nil {
			t.Fatal(err)
		}

		policy, err := json.Marshal(map[string]string{
			"deadLetterTargetArn": attributes.Attributes["QueueArn"], "maxReceiveCount": "5",
		})
		if err != nil {
			t.Fatal(err)
		}

		_, err = broker.SetQueueAttributes(ctx, &sqs.SetQueueAttributesInput{
			QueueUrl:   aws.String(urls[role]),
			Attributes: map[string]string{"RedrivePolicy": string(policy)},
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	metrics := &telemetry.Metrics{}
	client := queue.New(config.Config{
		SQSRegion: "us-east-1", SQSEndpoint: endpoint,
		RequestsQueue: prefix + "-requests.fifo", EventsQueue: prefix + "-events.fifo",
	}, metrics)
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}

	for _, role := range []string{"requests", "requests_dlq"} {
		_, err := broker.SendMessage(ctx, &sqs.SendMessageInput{
			QueueUrl: aws.String(urls[role]), MessageBody: aws.String("metrics fixture"),
			MessageGroupId: aws.String("metrics"), MessageDeduplicationId: aws.String(role),
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	messages, err := client.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(messages) != 1 {
		t.Fatalf("received %d messages, want 1", len(messages))
	}

	if err := client.Release(ctx, aws.ToString(messages[0].ReceiptHandle), 0); err != nil {
		t.Fatal(err)
	}

	messages, err = client.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(messages) != 1 {
		t.Fatalf("redelivered %d messages, want 1", len(messages))
	}

	if err := client.Delete(ctx, aws.ToString(messages[0].ReceiptHandle)); err != nil {
		t.Fatal(err)
	}

	if err := client.Publish(
		ctx,
		"metric-event",
		"metrics",
		[]byte(`{"eventId":"metric-event"}`),
	); err != nil {
		t.Fatal(err)
	}

	// LocalStack 4 does not generate ApproximateAgeOfOldestMessage itself.
	// Supply a controlled CloudWatch provider sample to exercise the real API;
	// production collection never publishes or fabricates this metric.
	sampleTime := time.Now().UTC().Truncate(time.Minute)
	_, err = cloud.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
		Namespace: aws.String("AWS/SQS"),
		MetricData: []cloudwatchtypes.MetricDatum{{
			MetricName: aws.String("ApproximateAgeOfOldestMessage"),
			Dimensions: []cloudwatchtypes.Dimension{{
				Name: aws.String("QueueName"), Value: aws.String(prefix + "-requests_dlq.fifo"),
			}},
			Timestamp: &sampleTime, Value: aws.Float64(42), Unit: cloudwatchtypes.StandardUnitSeconds,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	for range 2 {
		client.RefreshMetrics(ctx)
	}

	recorder := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	body := recorder.Body.String()

	for _, want := range []string{
		`jungle_sqs_queue_messages{queue="requests_dlq",state="visible"} 1`,
		`jungle_sqs_queue_messages{queue="requests_dlq",state="inflight"} 0`,
		`jungle_sqs_queue_messages{queue="events",state="visible"} 1`,
		`jungle_sqs_oldest_message_age_available{queue="requests_dlq"} 1`,
		`jungle_sqs_messages_total{action="received"} 2`,
		`jungle_sqs_messages_total{action="redelivered"} 1`,
		`jungle_sqs_messages_total{action="deleted"} 1`,
		`jungle_sqs_messages_total{action="published"} 1`,
		`jungle_sqs_dlq_configured{queue="requests"} 1`,
		`jungle_sqs_operations_total{operation="visibility",result="success"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing metric %s", want)
		}
	}

	if strings.Contains(body, prefix) {
		t.Fatal("queue names leaked into labels")
	}

	// The collector must not receive messages just to inspect their timestamp.
	out, err := broker.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl: aws.String(urls["requests_dlq"]), MaxNumberOfMessages: 1,
		MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameApproximateReceiveCount},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(out.Messages) != 1 {
		t.Fatal("metrics collection consumed or hid the DLQ fixture")
	}

	if got := out.Messages[0].Attributes["ApproximateReceiveCount"]; got != "1" {
		t.Fatalf("collector changed the DLQ receive count: %s", got)
	}

	client.StartMonitoring()
	stop, finish := context.WithTimeout(t.Context(), 5*time.Second)
	defer finish()

	if err := client.StopMonitoring(stop); err != nil {
		t.Fatal(fmt.Errorf("stop metric collector: %w", err))
	}
}
