// Package queue adapts SQS using the AWS SDK v2 and explicit queue URLs.
package queue

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"jungle/internal/platform/config"
)

type Client struct {
	client   *sqs.Client
	requests string
	events   string
	cfg      config.Config
}

func New(cfg config.Config) *Client { return &Client{cfg: cfg} }

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

	requests, err := c.client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(c.cfg.RequestsQueue)})
	if err != nil {
		return err
	}

	events, err := c.client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(c.cfg.EventsQueue)})
	if err != nil {
		return err
	}

	c.requests = aws.ToString(requests.QueueUrl)
	c.events = aws.ToString(events.QueueUrl)

	return nil
}

func (c *Client) Ready(ctx context.Context) error {
	_, err := c.client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: aws.String(c.requests), AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn}})

	return err
}

func (c *Client) Receive(ctx context.Context) ([]types.Message, error) {
	out, err := c.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:                    aws.String(c.requests),
		MaxNumberOfMessages:         1,
		WaitTimeSeconds:             10,
		VisibilityTimeout:           60,
		MessageAttributeNames:       []string{"AccessToken"},
		MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameApproximateReceiveCount},
	})
	if err != nil {
		return nil, err
	}

	return out.Messages, nil
}

func (c *Client) Delete(ctx context.Context, receipt string) error {
	_, err := c.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(c.requests),
		ReceiptHandle: aws.String(receipt)})

	return err
}

func (c *Client) Release(ctx context.Context, receipt string, seconds int32) error {
	_, err := c.client.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl:          aws.String(c.requests),
		ReceiptHandle:     aws.String(receipt),
		VisibilityTimeout: seconds,
	})

	return err
}

func (c *Client) Publish(ctx context.Context, eventID, aggregateID string, payload []byte) error {
	_, err := c.client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(c.events),
		MessageBody:            aws.String(string(payload)),
		MessageGroupId:         aws.String(aggregateID),
		MessageDeduplicationId: aws.String(eventID),
	})

	return err
}
