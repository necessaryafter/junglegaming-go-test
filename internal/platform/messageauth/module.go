package messageauth

import "go.uber.org/fx"

var Module = fx.Module("messageauth", fx.Provide(New))
