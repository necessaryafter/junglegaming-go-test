package worker

import (
	"context"
	"time"

	"jungle/internal/wagering/postgres"
)

// References schedules durable attempts to resolve pending references.
type References struct {
	store *postgres.Store
}

func NewReferences(store *postgres.Store) *References {
	return &References{store: store}
}

func (w *References) run(ctx context.Context) {
	for ctx.Err() == nil {
		opCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := w.store.RetryReference(opCtx)
		cancel()

		// RetryReference logs outcomes with the selected transaction metadata.
		// Other errors are retried on the next poll; cancellation ends this worker.
		if err != nil && ctx.Err() != nil {
			return
		}

		if !pause(ctx, 250*time.Millisecond) {
			return
		}
	}
}
