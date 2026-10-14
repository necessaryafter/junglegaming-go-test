package main

import (
	"go.uber.org/fx"

	"jungle/internal/app"
)

func main() {
	fx.New(app.Options()).Run()
}
