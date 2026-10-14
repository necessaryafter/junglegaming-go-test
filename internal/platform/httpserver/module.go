package httpserver

import "go.uber.org/fx"

var Module = fx.Module("httpserver", fx.Provide(newServer), fx.Invoke(register))

func register(lifecycle fx.Lifecycle, server *Server) {
	lifecycle.Append(fx.Hook{OnStart: server.start, OnStop: server.stop})
}
