// Package migrations embeds the versioned SQL shipped with the application.
package migrations

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

//go:embed *.sql
var files embed.FS

// Apply runs all pending migrations with golang-migrate. Down is destructive and
// reverses all versions. Each SQL file is atomic; a failed migration leaves the
// version dirty and requires investigation before another run.
// The caller retains ownership of connection. The runner opens and closes its
// own connection with the same configuration, including the schema search path.
func Apply(ctx context.Context, connection *pgx.Conn, direction string) (err error) {
	if direction != "up" && direction != "down" {
		return errors.New("migrations: expected up or down")
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	if err := adoptLegacyHistory(ctx, connection); err != nil {
		return fmt.Errorf("adopt migration history: %w", err)
	}

	source, err := iofs.New(files, ".")
	if err != nil {
		return fmt.Errorf("open embedded migrations: %w", err)
	}

	defer func() { err = errors.Join(err, source.Close()) }()

	timeout := 60 * time.Second

	if deadline, ok := ctx.Deadline(); ok {
		timeout = min(timeout, time.Until(deadline))
	}

	if timeout <= 0 {
		return context.DeadlineExceeded
	}

	config := connection.Config().Copy()
	config.ConnectTimeout = timeout
	// The driver also performs bookkeeping without a context. Server timeouts
	// bound those statements and advisory-lock waits as well as the SQL files.
	milliseconds := max(int64(1), timeout.Milliseconds())
	config.RuntimeParams["statement_timeout"] = fmt.Sprint(milliseconds)
	config.RuntimeParams["lock_timeout"] = fmt.Sprint(milliseconds)
	db := stdlib.OpenDB(*config)
	defer func() { err = errors.Join(err, db.Close()) }()

	driver, err := migratepgx.WithInstance(db, &migratepgx.Config{StatementTimeout: timeout})
	if err != nil {
		return fmt.Errorf("open migration database: %w", err)
	}

	defer func() { err = errors.Join(err, driver.Close()) }()

	runner, err := migrate.NewWithInstance("iofs", source, "pgx5", driver)
	if err != nil {
		return fmt.Errorf("create migration runner: %w", err)
	}

	runner.LockTimeout = timeout
	stopped := make(chan struct{})
	watcherDone := make(chan struct{})

	go func() {
		defer close(watcherDone)

		select {
		case <-ctx.Done():
			runner.GracefulStop <- true
		case <-stopped:
		}
	}()

	defer func() {
		close(stopped)
		<-watcherDone
	}()

	if direction == "up" {
		err = runner.Up()
	} else {
		err = runner.Down()
	}

	if errors.Is(err, migrate.ErrNoChange) {
		err = nil
	}

	return errors.Join(err, ctx.Err())
}
