// Package worker owns the background processing of the wagering module.
package worker

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"jungle/internal/platform/auth"
	"jungle/internal/platform/config"
	"jungle/internal/platform/queue"
	"jungle/internal/platform/telemetry"
	"jungle/internal/wagering/application"
	"jungle/internal/wagering/postgres"
)

type Workers struct {
	cfg     config.Config
	store   *postgres.Store
	queue   *queue.Client
	service *application.Service
	auth    *auth.Verifier
	logger  *slog.Logger
	metrics *telemetry.Metrics
	cancel  context.CancelFunc
	done    chan struct{}
}

func New(
	store *postgres.Store,
	queue *queue.Client,
	service *application.Service,
	verifier *auth.Verifier,
	logger *slog.Logger,
	metrics *telemetry.Metrics,
	cfg config.Config,
) *Workers {
	return &Workers{cfg: cfg, store: store, queue: queue, service: service, auth: verifier, logger: logger, metrics: metrics}
}

func (w *Workers) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	w.done = make(chan struct{})
	var group sync.WaitGroup

	for _, run := range []func(context.Context){w.consume, w.publish, w.references} {
		group.Add(1)

		go func() {
			defer group.Done()

			run(ctx)
		}()
	}

	go func() {
		group.Wait()
		close(w.done)
	}()
}

func (w *Workers) Stop(ctx context.Context) error {
	w.cancel()

	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func pause(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
