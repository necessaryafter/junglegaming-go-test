package wagering

import (
	"go.uber.org/fx"

	"jungle/internal/wagering/application"
	"jungle/internal/wagering/httpapi"
	"jungle/internal/wagering/postgres"
	"jungle/internal/wagering/worker"
)

var Module = fx.Module("wagering",
	postgres.Module,
	fx.Provide(application.New),
	httpapi.Module,
	worker.Module,
)
