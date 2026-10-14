package app_test

import (
	"testing"

	"go.uber.org/fx"

	"jungle/internal/app"
)

func TestDependencyGraph(t *testing.T) {
	if err := fx.ValidateApp(app.Options(), fx.NopLogger); err != nil {
		t.Fatalf("invalid Fx dependency graph: %v", err)
	}
}
