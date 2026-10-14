package telemetry

import (
	"log/slog"
	"os"

	"go.uber.org/fx"
)

var Module = fx.Module("telemetry", fx.Provide(newLogger, newMetrics))

func newLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, nil))
}

func newMetrics() *Metrics {
	return &Metrics{}
}
