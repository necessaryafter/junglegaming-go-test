// Command publish signs and sends an SQS request using the producer's AWS credentials.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"jungle/internal/platform/messageauth"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := run(ctx, os.Args[1:], os.Stdin); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, input io.Reader) error {
	flags := flag.NewFlagSet("publish", flag.ContinueOnError)
	keyFile := flags.String("key-file", os.Getenv("SQS_SIGNING_KEY_FILE"), "PKCS#8 Ed25519 private key file")
	keyID := flags.String("key-id", os.Getenv("SQS_SIGNING_KEY_ID"), "registered signing key ID")
	queueName := flags.String("queue", "wager-transactions.fifo", "destination queue")
	deduplicationID := flags.String("dedup-id", "", "FIFO deduplication ID; defaults to envelope messageId")
	if err := flags.Parse(args); err != nil {
		return err
	}

	body, err := io.ReadAll(io.LimitReader(input, 262145))
	if err != nil {
		return fmt.Errorf("read request: %w", err)
	}

	if len(body) > 262144 {
		return fmt.Errorf("request exceeds size limit")
	}

	var envelope struct {
		MessageID string `json:"messageId"`
		Data      struct {
			WalletID string `json:"walletId"`
		} `json:"data"`
	}

	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("decode request envelope: %w", err)
	}

	if envelope.MessageID == "" {
		return fmt.Errorf("request messageId is required")
	}

	if envelope.Data.WalletID == "" {
		return fmt.Errorf("request walletId is required")
	}

	if *deduplicationID == "" {
		*deduplicationID = envelope.MessageID
	}

	key, err := messageauth.ReadPrivateKey(*keyFile)
	if err != nil {
		return err
	}

	signature, err := messageauth.Sign(body, *keyID, key)
	if err != nil {
		return err
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("load AWS configuration: %w", err)
	}

	client := sqs.NewFromConfig(cfg, func(options *sqs.Options) {
		if endpoint := os.Getenv("SQS_ENDPOINT"); endpoint != "" {
			options.BaseEndpoint = aws.String(endpoint)
		}
	})
	queue, err := client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: queueName})
	if err != nil {
		return fmt.Errorf("resolve request queue: %w", err)
	}

	_, err = client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl: queue.QueueUrl, MessageBody: aws.String(string(body)),
		MessageGroupId: aws.String(envelope.Data.WalletID), MessageDeduplicationId: deduplicationID,
		MessageAttributes: map[string]types.MessageAttributeValue{
			messageauth.Attribute: {DataType: aws.String("String"), StringValue: aws.String(signature)},
		},
	})
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}

	return nil
}
