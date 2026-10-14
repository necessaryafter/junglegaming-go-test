package queue

import "go.uber.org/fx"

var Module = fx.Module("queue", fx.Provide(New), fx.Invoke(register))

func register(lifecycle fx.Lifecycle, client *Client) {
	lifecycle.Append(fx.Hook{OnStart: client.Start})
}
