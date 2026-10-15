package worker

import (
	"context"

	"go.uber.org/fx"
)

var Module = fx.Module("workers",
	fx.Provide(NewConsumer, NewPublisher, NewReferences, NewStatisticsCollector),
	fx.Invoke(register),
)

func register(
	lifecycle fx.Lifecycle,
	consumer *Consumer,
	publisher *Publisher,
	references *References,
	statistics *StatisticsCollector,
) {
	process := &background{tasks: []func(context.Context){
		consumer.run,
		publisher.run,
		references.run,
		statistics.run,
	}}
	lifecycle.Append(fx.Hook{OnStart: process.start, OnStop: process.stop})
}
