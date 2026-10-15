package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cloudwatchtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

const collectionInterval = 15 * time.Second

// StartMonitoring is separate from Start so explicit test and CLI clients can
// resolve queues without owning a background task. StopMonitoring joins it.
func (c *Client) StartMonitoring() {
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	c.done = make(chan struct{})

	go func() {
		defer close(c.done)

		ticker := time.NewTicker(collectionInterval)
		defer ticker.Stop()

		for {
			c.RefreshMetrics(ctx)

			select {
			case <-ticker.C:
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (c *Client) StopMonitoring(ctx context.Context) error {
	if c.cancel == nil {
		return nil
	}

	c.cancel()

	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RefreshMetrics reads attributes and CloudWatch without receiving messages or
// changing visibility. DLQs are discovered from the actual redrive policies.
func (c *Client) RefreshMetrics(ctx context.Context) {
	for _, source := range []struct {
		role string
		url  string
	}{
		{role: "requests", url: c.requests},
		{role: "events", url: c.events},
	} {
		attributes, err := c.collectQueue(ctx, source.role, source.url)
		if err != nil {
			c.metrics.SetDLQ(source.role, math.NaN())
			c.metrics.SetQueue(source.role+"_dlq", 0, 0, 0, err)
			c.metrics.SetQueueAge(source.role+"_dlq", 0, time.Time{}, err)

			continue
		}

		policy := attributes[string(types.QueueAttributeNameRedrivePolicy)]
		if policy == "" {
			c.metrics.SetDLQ(source.role, 0)
			missing := errors.New("queue: redrive policy is not configured")
			c.metrics.SetQueue(source.role+"_dlq", 0, 0, 0, missing)
			c.metrics.SetQueueAge(source.role+"_dlq", 0, time.Time{}, missing)

			continue
		}

		c.metrics.SetDLQ(source.role, 1)
		dlqURL, err := c.resolveDLQ(ctx, policy)
		if err != nil {
			c.metrics.SetQueue(source.role+"_dlq", 0, 0, 0, err)
			c.metrics.SetQueueAge(source.role+"_dlq", 0, time.Time{}, err)

			continue
		}

		if _, err := c.collectQueue(ctx, source.role+"_dlq", dlqURL); err != nil {
			continue
		}
	}
}

func (c *Client) collectQueue(
	ctx context.Context,
	role string,
	queueURL string,
) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	out, err := c.attributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(queueURL),
		AttributeNames: []types.QueueAttributeName{
			types.QueueAttributeNameApproximateNumberOfMessages,
			types.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
			types.QueueAttributeNameApproximateNumberOfMessagesDelayed,
			types.QueueAttributeNameRedrivePolicy,
		},
	})
	if err != nil {
		c.metrics.SetQueue(role, 0, 0, 0, err)
		c.metrics.SetQueueAge(role, 0, time.Time{}, err)

		return nil, err
	}

	visible, inflight, delayed, err := queueCounts(out.Attributes)
	c.metrics.SetQueue(role, visible, inflight, delayed, err)
	c.collectAge(ctx, role, queueURL)

	return out.Attributes, err
}

func queueCounts(attributes map[string]string) (int64, int64, int64, error) {
	values := [3]int64{}

	for i, name := range []types.QueueAttributeName{
		types.QueueAttributeNameApproximateNumberOfMessages,
		types.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
		types.QueueAttributeNameApproximateNumberOfMessagesDelayed,
	} {
		value, err := strconv.ParseInt(attributes[string(name)], 10, 64)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("parse %s: %w", name, err)
		}

		if value < 0 {
			return 0, 0, 0, fmt.Errorf("queue: negative %s", name)
		}

		values[i] = value
	}

	return values[0], values[1], values[2], nil
}

func (c *Client) resolveDLQ(ctx context.Context, policy string) (string, error) {
	var redrive struct {
		Target string `json:"deadLetterTargetArn"`
	}

	if err := json.Unmarshal([]byte(policy), &redrive); err != nil {
		return "", fmt.Errorf("decode redrive policy: %w", err)
	}

	parts := strings.SplitN(redrive.Target, ":", 6)
	if len(parts) != 6 {
		return "", errors.New("queue: invalid redrive target ARN")
	}

	if parts[2] != "sqs" {
		return "", errors.New("queue: redrive target is not SQS")
	}

	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	out, err := c.resolve(ctx, &sqs.GetQueueUrlInput{
		QueueName: aws.String(parts[5]), QueueOwnerAWSAccountId: aws.String(parts[4]),
	})
	if err != nil {
		return "", err
	}

	return aws.ToString(out.QueueUrl), nil
}

func (c *Client) collectAge(ctx context.Context, role string, queueURL string) {
	name := queueURL[strings.LastIndex(queueURL, "/")+1:]
	now := time.Now().UTC()
	start := now.Add(-5 * time.Minute)
	out, err := c.ages.GetMetricStatistics(ctx, &cloudwatch.GetMetricStatisticsInput{
		Namespace:  aws.String("AWS/SQS"),
		MetricName: aws.String("ApproximateAgeOfOldestMessage"),
		Dimensions: []cloudwatchtypes.Dimension{{Name: aws.String("QueueName"), Value: aws.String(name)}},
		StartTime:  &start,
		EndTime:    &now,
		Period:     aws.Int32(60),
		Statistics: []cloudwatchtypes.Statistic{cloudwatchtypes.StatisticMaximum},
	})
	if err != nil {
		c.metrics.SetQueueAge(role, 0, time.Time{}, err)

		return
	}

	age, timestamp := latestAge(out.Datapoints, start, now)
	c.metrics.SetQueueAge(role, age, timestamp, nil)
}

func latestAge(
	points []cloudwatchtypes.Datapoint,
	start time.Time,
	end time.Time,
) (float64, time.Time) {
	var timestamp time.Time
	var age float64

	for _, point := range points {
		if point.Timestamp == nil {
			continue
		}

		if point.Maximum == nil {
			continue
		}

		if point.Timestamp.Before(start) || point.Timestamp.After(end) {
			continue
		}

		if *point.Maximum < 0 || math.IsNaN(*point.Maximum) || math.IsInf(*point.Maximum, 0) {
			continue
		}

		if point.Timestamp.After(timestamp) {
			timestamp = *point.Timestamp
			age = *point.Maximum
		}
	}

	return age, timestamp
}
