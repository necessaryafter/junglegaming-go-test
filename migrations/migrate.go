// Package migrations embeds the versioned SQL shipped with the application.
package migrations

import (
	"context"
	"embed"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

//go:embed *.sql
var files embed.FS

// Apply applies all pending versions on up and reverses all applied versions on
// down. Down is destructive and must be requested explicitly. Schema changes
// are serialized and committed atomically; application processes never migrate.
func Apply(ctx context.Context, connection *pgx.Conn, direction string) (err error) {
	if direction != "up" && direction != "down" {
		return errors.New("migrations: expected up or down")
	}

	tx, err := connection.Begin(ctx)
	if err != nil {
		return err
	}

	defer func() {
		// The connection is owned by the caller and closed after Apply. Rollback
		// cannot wait indefinitely if the migration context was canceled.
		rollbackErr := tx.Rollback(ctx)
		if rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			err = errors.Join(err, rollbackErr)
		}
	}()

	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(751932012)"); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, "CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY)"); err != nil {
		return err
	}

	versions := []string{"000001_base", "000002_outbox_attempts"}
	var currentVersion, appliedCount, firstVersion int

	const versionQuery = `
    SELECT
        COALESCE(MAX(version), 0),
        COUNT(*),
        COALESCE(MIN(version), 1)
    FROM schema_migrations`

	if err := tx.QueryRow(ctx, versionQuery).Scan(
		&currentVersion,
		&appliedCount,
		&firstVersion,
	); err != nil {
		return err
	}

	if firstVersion < 1 || currentVersion != appliedCount || currentVersion > len(versions) {
		return errors.New("migrations: unsupported or inconsistent version history")
	}

	var pendingVersions []int

	if direction == "up" {
		for version := currentVersion + 1; version <= len(versions); version++ {
			pendingVersions = append(pendingVersions, version)
		}
	} else {
		for version := currentVersion; version > 0; version-- {
			pendingVersions = append(pendingVersions, version)
		}
	}

	for _, version := range pendingVersions {
		sql, err := files.ReadFile(versions[version-1] + "." + direction + ".sql")
		if err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("migration %d %s: %w", version, direction, err)
		}

		statement := "INSERT INTO schema_migrations(version) VALUES($1)"

		if direction == "down" {
			statement = "DELETE FROM schema_migrations WHERE version=$1"
		}

		if _, err := tx.Exec(ctx, statement, version); err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}
