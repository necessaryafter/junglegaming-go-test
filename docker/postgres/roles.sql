-- Run as the database administrator before migrations. Settings are supplied by
-- bootstrap.psql (or by the isolated PostgreSQL integration fixture).
DO $$
DECLARE
    migration_role TEXT := current_setting('jungle.migration_role');
    runtime_role TEXT := current_setting('jungle.runtime_role');
    target_schema TEXT := current_schema();
    object_name TEXT;
BEGIN
    PERFORM pg_advisory_xact_lock(751932012);
    IF migration_role = runtime_role OR migration_role = current_user OR runtime_role = current_user THEN
        RAISE EXCEPTION 'administrator, migration and runtime roles must be distinct';
    END IF;
    IF current_setting('jungle.migration_password') = '' OR current_setting('jungle.runtime_password') = '' THEN
        RAISE EXCEPTION 'migration and runtime passwords must not be empty';
    END IF;

    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = migration_role) THEN
        EXECUTE format('CREATE ROLE %I', migration_role);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = runtime_role) THEN
        EXECUTE format('CREATE ROLE %I', runtime_role);
    END IF;

    EXECUTE format(
        'ALTER ROLE %I LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS PASSWORD %L',
        migration_role, current_setting('jungle.migration_password')
    );
    EXECUTE format(
        'ALTER ROLE %I LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS PASSWORD %L',
        runtime_role, current_setting('jungle.runtime_password')
    );

    IF EXISTS (
        SELECT 1
        FROM pg_auth_members
        WHERE member IN (
            SELECT oid FROM pg_roles WHERE rolname IN (migration_role, runtime_role)
        )
    ) THEN
        RAISE EXCEPTION 'migration and runtime roles must not belong to other roles';
    END IF;
    IF EXISTS (
        SELECT 1 FROM pg_database
        WHERE datname = current_database()
          AND datdba = (SELECT oid FROM pg_roles WHERE rolname = runtime_role)
    ) THEN
        RAISE EXCEPTION 'runtime role must not own the database';
    END IF;

    EXECUTE format('ALTER ROLE %I RESET ALL', runtime_role);
    EXECUTE format('ALTER ROLE %I IN DATABASE %I RESET ALL', runtime_role, current_database());
    EXECUTE format('REVOKE SET, ALTER SYSTEM ON PARAMETER session_replication_role FROM %I', runtime_role);
    IF has_parameter_privilege(runtime_role, 'session_replication_role', 'SET, ALTER SYSTEM') THEN
        RAISE EXCEPTION 'runtime inherits unsafe session_replication_role privileges';
    END IF;
    EXECUTE format('REVOKE ALL ON DATABASE %I FROM PUBLIC', current_database());
    EXECUTE format('GRANT CONNECT ON DATABASE %I TO %I, %I', current_database(), migration_role, runtime_role);
    EXECUTE format('REVOKE ALL ON DATABASE %I FROM %I', current_database(), runtime_role);
    EXECUTE format('GRANT CONNECT ON DATABASE %I TO %I', current_database(), runtime_role);
    EXECUTE format('REVOKE ALL ON SCHEMA %I FROM PUBLIC, %I', target_schema, runtime_role);
    EXECUTE format('ALTER SCHEMA %I OWNER TO %I', target_schema, migration_role);

    -- Upgrade the original deployment without reassigning unrelated objects or
    -- ownership of the administrator's database.
    FOREACH object_name IN ARRAY ARRAY[
        'wallets', 'wager_transactions', 'wallet_ledger', 'inbox', 'outbox', 'schema_migrations'
    ] LOOP
        IF to_regclass(format('%I.%I', target_schema, object_name)) IS NOT NULL THEN
            EXECUTE format('ALTER TABLE %I.%I OWNER TO %I', target_schema, object_name, migration_role);
        END IF;
    END LOOP;

    FOREACH object_name IN ARRAY ARRAY[
        'reject_financial_mutation', 'check_wallet_ledger', 'protect_transaction'
    ] LOOP
        IF to_regprocedure(format('%I.%I()', target_schema, object_name)) IS NOT NULL THEN
            EXECUTE format('ALTER FUNCTION %I.%I() OWNER TO %I', target_schema, object_name, migration_role);
        END IF;
    END LOOP;
END;
$$;
