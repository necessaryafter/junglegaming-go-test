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
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"jungle/internal/platform/auth"
	"jungle/internal/platform/messageauth"
	"jungle/internal/platform/queue"
	"jungle/internal/platform/telemetry"
	"jungle/internal/wagering/application"
	"jungle/internal/wagering/domain/wager"
)

// Consumer handles authenticated SQS deliveries through the shared use case.
type Consumer struct {
	queue   *queue.Client
	service *application.Service
	auth    *messageauth.Verifier
	logger  *slog.Logger
	metrics *telemetry.Metrics
}

func NewConsumer(
	queue *queue.Client,
	service *application.Service,
	verifier *messageauth.Verifier,
	logger *slog.Logger,
	metrics *telemetry.Metrics,
) *Consumer {
	return &Consumer{
		queue:   queue,
		service: service,
		auth:    verifier,
		logger:  logger,
		metrics: metrics,
	}
}

type requested struct {
	MessageID  string    `json:"messageId"`
	Type       string    `json:"type"`
	OccurredAt time.Time `json:"occurredAt"`
	Data       struct {
		wager.Input
		IdempotencyKey string `json:"idempotencyKey"`
	} `json:"data"`
}

func decode(body string) (requested, error) {
	if len(body) > 262144 {
		return requested{}, application.ErrInvalid
	}

	var envelope requested
	decoder := json.NewDecoder(bytes.NewBufferString(body))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&envelope); err != nil {
		return requested{}, application.ErrInvalid
	}

	if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return requested{}, application.ErrInvalid
	}

	if envelope.MessageID == "" {
		return requested{}, application.ErrInvalid
	}

	if envelope.OccurredAt.IsZero() {
		return requested{}, application.ErrInvalid
	}

	if envelope.Type != "WagerTransactionRequested" {
		return requested{}, application.ErrInvalid
	}

	return envelope, nil
}

func (w *Consumer) run(ctx context.Context) {
	for ctx.Err() == nil {
		messages, err := w.queue.Receive(ctx)
		if err != nil {
			if ctx.Err() == nil {
				w.logger.Error("SQS receive failed", "operation", "sqs_receive", "reason", failureReason(err))
			}

			if !pause(ctx, time.Second) {
				return
			}

			continue
		}

		for _, message := range messages {
			w.handle(ctx, message)
		}
	}
}

func (w *Consumer) handle(ctx context.Context, message types.Message) {
	opCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	envelope, err := decode(aws.ToString(message.Body))
	logger := w.messageLogger(aws.ToString(message.MessageId), envelope)
	var result application.Result

	if err == nil {
		attribute := message.MessageAttributes[messageauth.Attribute]
		var principal application.Principal

		principal, err = w.auth.Verify(aws.ToString(message.Body), aws.ToString(attribute.StringValue))
		if err == nil {
			logger = withIdentifier(logger, "providerId", principal.ProviderID)
			sum := sha256.Sum256([]byte(aws.ToString(message.Body)))

			start := time.Now()

			result, err = w.service.Submit(opCtx, principal, application.Command{
				Input:         envelope.Data.Input,
				Key:           envelope.Data.IdempotencyKey,
				CorrelationID: envelope.MessageID,
				MessageID:     envelope.MessageID,
				MessageHash:   hex.EncodeToString(sum[:]),
			})
			if err != nil {
				w.metrics.ObserveProcessing("sqs", "ERROR", false, time.Since(start))
			} else {
				w.metrics.ObserveProcessing(
					"sqs",
					string(result.Status),
					result.IdempotentReplay,
					time.Since(start),
				)
			}
		}
	}

	cleanup, finish := context.WithTimeout(context.Background(), 3*time.Second)
	defer finish()

	if err == nil {
		logger = withIdentifier(logger, "transactionId", result.TransactionID)
		logger.Info(
			"SQS transaction committed",
			"status", result.Status,
			"idempotentReplay", result.IdempotentReplay,
			"failureCode", result.FailureCode,
		)

		if err := w.queue.Delete(cleanup, aws.ToString(message.ReceiptHandle)); err != nil {
			logger.Error("SQS delete failed", "reason", failureReason(err))

			return
		}

		logger.Info("SQS message deleted")

		return
	}

	w.metrics.Retries.Add(1)
	w.metrics.RecordSQSRetry(failureReason(err))

	if errors.Is(err, application.ErrConflict) {
		w.metrics.RecordConflict("sqs")
	}

	logger.Warn("SQS message retained for retry or redrive", "reason", failureReason(err))

	delay := int32(0)

	if ctx.Err() == nil {
		count, parseErr := strconv.Atoi(message.Attributes["ApproximateReceiveCount"])
		if parseErr != nil || count < 1 {
			count = 1
		}

		delay = int32(1 << min(count, 8))
	}

	if err := w.queue.Release(cleanup, aws.ToString(message.ReceiptHandle), delay); err != nil {
		logger.Error("SQS visibility update failed", "reason", failureReason(err))
	}
}

func failureReason(err error) string {
	switch {
	case errors.Is(err, auth.ErrUnauthorized), errors.Is(err, messageauth.ErrUnauthorized), errors.Is(err, application.ErrForbidden):
		return "unauthorized"
	case errors.Is(err, application.ErrInvalid):
		return "invalid_request"
	case errors.Is(err, application.ErrConflict):
		return "conflict"
	default:
		return "unavailable"
	}
}
