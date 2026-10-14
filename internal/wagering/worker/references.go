package worker

import (
	"context"
	"errors"
	"time"

	"jungle/internal/wagering/application"
)

func (w *Workers) references(ctx context.Context) {
	for ctx.Err() == nil {
		opCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := w.store.RetryReference(opCtx)
		cancel()

		if err != nil && !errors.Is(err, application.ErrNotFound) && ctx.Err() == nil {
			w.logger.Error("reference retry failed")
		}

		if !pause(ctx, 250*time.Millisecond) {
			return
		}
	}
}
