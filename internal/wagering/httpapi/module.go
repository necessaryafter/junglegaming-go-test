package httpapi

import (
	"go.uber.org/fx"

	"jungle/internal/platform/httpserver"
)

var Module = fx.Module("http", fx.Provide(New, routes))

func routes(api *API) httpserver.RouteSet {
	return httpserver.RouteSet{Routes: api}
}
