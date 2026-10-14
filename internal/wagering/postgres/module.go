package postgres

import (
	"go.uber.org/fx"

	"jungle/internal/wagering/application"
)

var Module = fx.Module("persistence",
	fx.Provide(New, repository),
	fx.Invoke(register),
)

func repository(store *Store) application.Repository {
	return store
}

func register(lifecycle fx.Lifecycle, store *Store) {
	lifecycle.Append(fx.Hook{OnStart: store.Ready})
}
