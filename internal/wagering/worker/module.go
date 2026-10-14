package worker

import (
	"context"

	"go.uber.org/fx"
)

var Module = fx.Module("workers", fx.Provide(New), fx.Invoke(register))

func register(lifecycle fx.Lifecycle, workers *Workers) {
	lifecycle.Append(fx.Hook{
		OnStart: func(context.Context) error {
			workers.Start()

			return nil
		},
		OnStop: workers.Stop,
	})
}
