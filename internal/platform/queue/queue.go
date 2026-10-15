// Package queue adapts SQS using the AWS SDK v2 and explicit queue URLs.
package queue

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"jungle/internal/platform/config"
	"jungle/internal/platform/messageauth"
	"jungle/internal/platform/telemetry"
)

type Client struct {
	client   *sqs.Client
	ages     *cloudwatch.Client
	metrics  *telemetry.Metrics
	cancel   context.CancelFunc
	done     chan struct{}
	requests string
	events   string
	cfg      config.Config
}

// New uses a private metrics registry when metrics is nil.
func New(cfg config.Config, metrics *telemetry.Metrics) *Client {
	if metrics == nil {
		metrics = &telemetry.Metrics{}
	}

	return &Client{cfg: cfg, metrics: metrics}
}

// Start resolves provisioned queues; startup fails if dependencies are absent.
func (c *Client) Start(ctx context.Context) error {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(c.cfg.SQSRegion))
	if err != nil {
		return err
	}

	c.client = sqs.NewFromConfig(cfg, func(options *sqs.Options) {
		if c.cfg.SQSEndpoint != "" {
			options.BaseEndpoint = aws.String(c.cfg.SQSEndpoint)
		}
	})

	requests, err := c.resolve(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(c.cfg.RequestsQueue)})
	if err != nil {
		return err
	}

	events, err := c.resolve(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(c.cfg.EventsQueue)})
	if err != nil {
		return err
	}

	c.ages = cloudwatch.NewFromConfig(cfg, func(options *cloudwatch.Options) {
		if c.cfg.SQSEndpoint != "" {
			options.BaseEndpoint = aws.String(c.cfg.SQSEndpoint)
		}
	})

	c.requests = aws.ToString(requests.QueueUrl)
	c.events = aws.ToString(events.QueueUrl)

	return nil
}

func (c *Client) Ready(ctx context.Context) error {
	_, err := c.attributes(
		ctx,
		&sqs.GetQueueAttributesInput{QueueUrl: aws.String(c.requests), AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn}},
	)

	return err
}

func (c *Client) Receive(ctx context.Context) (messages []types.Message, err error) {
	start := time.Now()
	defer func() { c.metrics.ObserveSQS("receive", operationResult(err), time.Since(start)) }()

	out, err := c.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:                    aws.String(c.requests),
		MaxNumberOfMessages:         1,
		WaitTimeSeconds:             10,
		VisibilityTimeout:           60,
		MessageAttributeNames:       []string{messageauth.Attribute},
		MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameApproximateReceiveCount},
	})
	if err != nil {
		return nil, err
	}

	c.metrics.AddSQSMessages("received", len(out.Messages))

	if len(out.Messages) == 0 {
		c.metrics.AddSQSMessages("empty_receives", 1)
	}

	for _, message := range out.Messages {
		count, parseErr := strconv.Atoi(message.Attributes["ApproximateReceiveCount"])
		if parseErr == nil && count > 1 {
			c.metrics.AddSQSMessages("redelivered", 1)
		}
	}

	return out.Messages, nil
}

func (c *Client) Delete(ctx context.Context, receipt string) (err error) {
	start := time.Now()
	defer func() { c.metrics.ObserveSQS("delete", operationResult(err), time.Since(start)) }()

	_, err = c.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(c.requests),
		ReceiptHandle: aws.String(receipt)})

	if err == nil {
		c.metrics.AddSQSMessages("deleted", 1)
	}

	return err
}

func (c *Client) Release(ctx context.Context, receipt string, seconds int32) (err error) {
	start := time.Now()
	defer func() { c.metrics.ObserveSQS("visibility", operationResult(err), time.Since(start)) }()

	_, err = c.client.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl:          aws.String(c.requests),
		ReceiptHandle:     aws.String(receipt),
		VisibilityTimeout: seconds,
	})

	return err
}

func (c *Client) Publish(
	ctx context.Context,
	eventID string,
	aggregateID string,
	payload []byte,
) (err error) {
	start := time.Now()
	defer func() { c.metrics.ObserveSQS("publish", operationResult(err), time.Since(start)) }()

	_, err = c.client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(c.events),
		MessageBody:            aws.String(string(payload)),
		MessageGroupId:         aws.String(aggregateID),
		MessageDeduplicationId: aws.String(eventID),
	})

	if err == nil {
		c.metrics.AddSQSMessages("published", 1)
	}

	return err
}

func operationResult(err error) string {
	if err == nil {
		return "success"
	}

	if errors.Is(err, context.Canceled) {
		return "canceled"
	}

	return "error"
}

func (c *Client) attributes(
	ctx context.Context,
	input *sqs.GetQueueAttributesInput,
) (out *sqs.GetQueueAttributesOutput, err error) {
	start := time.Now()
	defer func() { c.metrics.ObserveSQS("attributes", operationResult(err), time.Since(start)) }()

	return c.client.GetQueueAttributes(ctx, input)
}

func (c *Client) resolve(
	ctx context.Context,
	input *sqs.GetQueueUrlInput,
) (out *sqs.GetQueueUrlOutput, err error) {
	start := time.Now()
	defer func() { c.metrics.ObserveSQS("resolve", operationResult(err), time.Since(start)) }()

	return c.client.GetQueueUrl(ctx, input)
}
