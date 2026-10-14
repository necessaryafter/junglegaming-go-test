package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5"

	"jungle/migrations"
)

func main() {
	if err := run(); err != nil {
		slog.Error("migration failed", "error", err)
		os.Exit(1)
	}
}

func run() (err error) {
	if len(os.Args) != 2 || os.Getenv("DATABASE_URL") == "" {
		return errors.New("usage: DATABASE_URL=... migrate up|down")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	connection, err := pgx.Connect(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}

	defer func() { err = errors.Join(err, connection.Close(ctx)) }()

	return migrations.Apply(ctx, connection, os.Args[1])
}
