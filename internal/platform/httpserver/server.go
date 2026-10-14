package httpserver

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"go.uber.org/fx"

	"jungle/internal/platform/config"
)

type Routes interface {
	Register(*http.ServeMux)
}

type RouteSet struct {
	fx.Out

	Routes Routes `group:"http_routes"`
}

type parameters struct {
	fx.In

	Config   config.Config
	Logger   *slog.Logger
	Shutdown fx.Shutdowner
	Routes   []Routes `group:"http_routes"`
}

type Server struct {
	server   *http.Server
	logger   *slog.Logger
	shutdown fx.Shutdowner
	listener net.Listener
	done     chan struct{}
}

func newServer(params parameters) *Server {
	mux := http.NewServeMux()

	for _, routes := range params.Routes {
		routes.Register(mux)
	}

	return &Server{
		server: &http.Server{
			Addr: params.Config.HTTPAddress, Handler: mux,
			ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
			WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second,
		},
		logger: params.Logger, shutdown: params.Shutdown, done: make(chan struct{}),
	}
}

func (s *Server) start(context.Context) error {
	listener, err := net.Listen("tcp", s.server.Addr)
	if err != nil {
		return err
	}

	s.listener = listener

	go func() {
		defer close(s.done)

		if err := s.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Error("HTTP server stopped unexpectedly")

			if err := s.shutdown.Shutdown(fx.ExitCode(1)); err != nil {
				s.logger.Error("shutdown request failed")
			}
		}
	}()

	return nil
}

func (s *Server) stop(ctx context.Context) error {
	if err := s.server.Shutdown(ctx); err != nil {
		closeErr := s.server.Close()

		return errors.Join(err, closeErr)
	}

	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
