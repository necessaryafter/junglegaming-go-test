package queue

import (
	"context"

	"go.uber.org/fx"
)

var Module = fx.Module("queue", fx.Provide(New), fx.Invoke(register))

func register(lifecycle fx.Lifecycle, client *Client) {
	lifecycle.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			if err := client.Start(ctx); err != nil {
				return err
			}

			client.StartMonitoring()

			return nil
		},
		OnStop: client.StopMonitoring,
	})
}
