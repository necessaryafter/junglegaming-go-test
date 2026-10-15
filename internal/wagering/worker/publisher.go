package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"jungle/internal/platform/config"
	"jungle/internal/platform/queue"
	"jungle/internal/platform/telemetry"
	"jungle/internal/wagering/application"
	"jungle/internal/wagering/postgres"
)

// Publisher leases committed outbox events and publishes their immutable snapshots.
type Publisher struct {
	store            *postgres.Store
	queue            *queue.Client
	logger           *slog.Logger
	metrics          *telemetry.Metrics
	operationTimeout time.Duration
	leaseDuration    time.Duration
}

func NewPublisher(
	store *postgres.Store,
	queue *queue.Client,
	logger *slog.Logger,
	metrics *telemetry.Metrics,
	cfg config.Config,
) *Publisher {
	return &Publisher{
		store:            store,
		queue:            queue,
		logger:           logger,
		metrics:          metrics,
		operationTimeout: cfg.OperationTimeout,
		leaseDuration:    cfg.OutboxLeaseDuration,
	}
}

func (w *Publisher) run(ctx context.Context) {
	for ctx.Err() == nil {
		opCtx, cancel := context.WithTimeout(ctx, w.operationTimeout)
		delivery, err := w.store.ClaimEvent(opCtx, w.leaseDuration)
		cancel()

		if err != nil {
			if !errors.Is(err, application.ErrNotFound) && ctx.Err() == nil {
				w.logger.Error(
					"outbox claim failed",
					"operation",
					"outbox_claim",
					"reason",
					failureReason(err),
				)
			}

			if !pause(ctx, time.Second) {
				return
			}

			continue
		}

		logger := w.eventLogger(delivery)
		opCtx, cancel = context.WithTimeout(ctx, w.operationTimeout)
		err = w.queue.Publish(opCtx, delivery.ID, delivery.AggregateID, delivery.Payload)
		cancel()

		if err != nil {
			w.metrics.Retries.Add(1)
			logger.Warn("outbox publication failed", "reason", failureReason(err))
		} else {
			w.metrics.Published.Add(1)
			logger.Info("outbox publication accepted")
		}

		// Even on shutdown, release or acknowledge our lease within a deadline.
		cleanup, finish := context.WithTimeout(context.Background(), 3*time.Second)
		if finishErr := w.store.FinishEvent(cleanup, delivery, err == nil); finishErr != nil {
			logger.Error("outbox completion failed", "reason", failureReason(finishErr))
		} else {
			// A stale lease is a no-op in FinishEvent; this records the call,
			// not ownership or a new durable publication.
			logger.Info("outbox completion call succeeded", "publicationAccepted", err == nil)
		}

		finish()
	}
}
