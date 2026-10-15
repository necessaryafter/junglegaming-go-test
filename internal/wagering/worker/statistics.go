package worker

import (
	"context"
	"time"

	"jungle/internal/platform/telemetry"
	"jungle/internal/wagering/postgres"
)

// StatisticsCollector refreshes the financial backlog gauges in the background.
type StatisticsCollector struct {
	store   *postgres.Store
	metrics *telemetry.Metrics
}

func NewStatisticsCollector(
	store *postgres.Store,
	metrics *telemetry.Metrics,
) *StatisticsCollector {
	return &StatisticsCollector{store: store, metrics: metrics}
}

func (w *StatisticsCollector) run(ctx context.Context) {
	for ctx.Err() == nil {
		opCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		stats, err := w.store.Statistics(opCtx)
		cancel()

		w.metrics.SetDatabase(stats.OutboxPending, stats.OutboxOldestSeconds, stats.PendingReferences, err)

		if !pause(ctx, 15*time.Second) {
			return
		}
	}
}
