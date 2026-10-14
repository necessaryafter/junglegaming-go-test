// Package app composes the business and platform modules into one process.
package app

import (
	"log/slog"
	"time"

	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"

	"jungle/internal/platform/auth"
	"jungle/internal/platform/config"
	"jungle/internal/platform/database"
	"jungle/internal/platform/httpserver"
	"jungle/internal/platform/queue"
	"jungle/internal/platform/telemetry"
	"jungle/internal/wagering"
)

// Options assembles modules in startup order. Fx stops hooks in reverse order,
// draining HTTP and workers before closing their database connections.
func Options() fx.Option {
	return fx.Options(
		fx.StartTimeout(45*time.Second),
		fx.StopTimeout(30*time.Second),
		fx.WithLogger(func(logger *slog.Logger) fxevent.Logger {
			return &fxevent.SlogLogger{Logger: logger}
		}),
		config.Module,
		telemetry.Module,
		database.Module,
		auth.Module,
		queue.Module,
		wagering.Module,
		httpserver.Module,
	)
}
