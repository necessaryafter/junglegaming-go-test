package auth

import "go.uber.org/fx"

var Module = fx.Module("auth", fx.Provide(New), fx.Invoke(register))

func register(lifecycle fx.Lifecycle, verifier *Verifier) {
	lifecycle.Append(fx.Hook{OnStart: verifier.Ready})
}
