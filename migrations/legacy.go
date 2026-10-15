package migrations

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// adoptLegacyHistory converts only the original runner's history table. Schema
// changes and the replacement of version rows are atomic; dirty histories from
// golang-migrate are never repaired or forced by this compatibility bridge.
func adoptLegacyHistory(ctx context.Context, connection *pgx.Conn) error {
	const query = `
    DO $$
    DECLARE
        latest INTEGER;
        applied INTEGER;
        first INTEGER;
    BEGIN
        PERFORM pg_advisory_xact_lock(751932012);
        IF to_regclass('schema_migrations') IS NULL THEN
            RETURN;
        END IF;
        IF EXISTS (
            SELECT 1
            FROM pg_attribute
            WHERE attrelid = to_regclass('schema_migrations')
              AND attname = 'dirty'
              AND NOT attisdropped
        ) THEN
            RETURN;
        END IF;

        LOCK TABLE schema_migrations IN ACCESS EXCLUSIVE MODE;
        SELECT COALESCE(MAX(version), 0), COUNT(*), COALESCE(MIN(version), 1)
        INTO latest, applied, first
        FROM schema_migrations;
        IF first < 1 OR latest <> applied THEN
            RAISE EXCEPTION 'inconsistent legacy migration history';
        END IF;

        ALTER TABLE schema_migrations ALTER COLUMN version TYPE BIGINT;
        ALTER TABLE schema_migrations ADD COLUMN dirty BOOLEAN NOT NULL DEFAULT false;
        ALTER TABLE schema_migrations ALTER COLUMN dirty DROP DEFAULT;
        DELETE FROM schema_migrations WHERE version <> latest;
    END;
    $$`

	_, err := connection.Exec(ctx, query)

	return err
}
