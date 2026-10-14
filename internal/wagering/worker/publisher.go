package worker

import (
	"context"
	"errors"
	"time"

	"jungle/internal/wagering/application"
)

func (w *Workers) publish(ctx context.Context) {
	for ctx.Err() == nil {
		opCtx, cancel := context.WithTimeout(ctx, w.cfg.OperationTimeout)
		delivery, err := w.store.ClaimEvent(opCtx, w.cfg.OutboxLeaseDuration)
		cancel()

		if err != nil {
			if !errors.Is(err, application.ErrNotFound) && ctx.Err() == nil {
				w.logger.Error("outbox claim failed")
			}

			if !pause(ctx, time.Second) {
				return
			}

			continue
		}

		opCtx, cancel = context.WithTimeout(ctx, w.cfg.OperationTimeout)
		err = w.queue.Publish(opCtx, delivery.ID, delivery.AggregateID, delivery.Payload)
		cancel()

		if err != nil {
			w.metrics.Retries.Add(1)
		} else {
			w.metrics.Published.Add(1)
		}

		// Even on shutdown, release or acknowledge our lease within a deadline.
		cleanup, finish := context.WithTimeout(context.Background(), 3*time.Second)
		if finishErr := w.store.FinishEvent(cleanup, delivery, err == nil); finishErr != nil {
			w.logger.Error("outbox completion failed", "eventId", delivery.ID)
		}

		finish()
	}
}
