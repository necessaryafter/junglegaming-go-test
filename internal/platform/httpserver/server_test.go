package httpserver

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"go.uber.org/fx"

	"jungle/internal/platform/config"
)

type testRoutes struct {
	pattern string
	name    string
}

func (routes testRoutes) Register(mux *http.ServeMux) {
	mux.HandleFunc(routes.pattern, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Module", routes.name)
		w.WriteHeader(http.StatusNoContent)
	})
}

func TestModuleServesContributionsAndStopsListener(t *testing.T) {
	var server *Server

	process := fx.New(Module, fx.NopLogger,
		fx.Supply(config.Config{HTTPAddress: "127.0.0.1:0"}, slog.New(slog.NewTextHandler(io.Discard, nil))),
		fx.Provide(
			func() RouteSet { return RouteSet{Routes: testRoutes{pattern: "GET /first", name: "first"}} },
			func() RouteSet { return RouteSet{Routes: testRoutes{pattern: "GET /second", name: "second"}} },
		),
		fx.Populate(&server),
	)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	if err := process.Start(ctx); err != nil {
		t.Fatalf("start HTTP module: %v", err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := process.Stop(ctx); err != nil {
			t.Errorf("stop HTTP module: %v", err)
		}
	})

	address := "http://" + server.listener.Addr().String()
	client := &http.Client{Timeout: time.Second}

	for _, name := range []string{"first", "second"} {
		response, err := client.Get(address + "/" + name)
		if err != nil {
			t.Fatalf("request %s: %v", name, err)
		}

		if err := response.Body.Close(); err != nil {
			t.Fatalf("close response: %v", err)
		}

		if response.StatusCode != http.StatusNoContent || response.Header.Get("X-Module") != name {
			t.Fatalf("route %s: status=%d, module=%q", name, response.StatusCode, response.Header.Get("X-Module"))
		}
	}

	if err := process.Stop(ctx); err != nil {
		t.Fatalf("stop HTTP module: %v", err)
	}

	response, err := client.Get(address + "/first")
	if err == nil {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close unexpected response: %v", err)
		}

		t.Fatal("HTTP listener still accepts requests after shutdown")
	}
}

func TestModuleFailsWhenAddressIsOccupied(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := listener.Close(); err != nil {
			t.Errorf("close occupied listener: %v", err)
		}
	}()

	process := fx.New(Module, fx.NopLogger,
		fx.Supply(config.Config{HTTPAddress: listener.Addr().String()}, slog.New(slog.NewTextHandler(io.Discard, nil))),
	)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	if err := process.Start(ctx); err == nil {
		t.Fatal("HTTP module started on an occupied address")
	}
}
