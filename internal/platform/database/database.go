// Package database owns PostgreSQL connections, without business persistence.
package database

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"

	"jungle/internal/platform/config"
)

var Module = fx.Module("database", fx.Provide(New), fx.Invoke(register))

// New creates a lazy pool; the module checks connectivity during startup.
func New(cfg config.Config) (*pgxpool.Pool, error) {
	return pgxpool.New(context.Background(), cfg.DatabaseURL)
}

func register(lifecycle fx.Lifecycle, pool *pgxpool.Pool) {
	lifecycle.Append(fx.Hook{
		OnStart: pool.Ping,
		OnStop: func(context.Context) error {
			pool.Close()

			return nil
		},
	})
}
