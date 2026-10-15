-- Run as the schema owner after every migration. Future objects receive no
-- runtime access until this explicit allowlist is updated.
DO $$
DECLARE
    runtime_role TEXT := current_setting('jungle.runtime_role');
    target_schema TEXT := current_schema();
BEGIN
    PERFORM pg_advisory_xact_lock(751932012);
    IF runtime_role = current_user THEN
        RAISE EXCEPTION 'runtime must not own the schema';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_roles
        WHERE rolname = runtime_role
          AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole
          AND NOT rolreplication AND NOT rolbypassrls
    ) THEN
        RAISE EXCEPTION 'runtime role has unsafe attributes or does not exist';
    END IF;
    IF EXISTS (
        SELECT 1 FROM pg_auth_members
        WHERE member = (SELECT oid FROM pg_roles WHERE rolname = runtime_role)
    ) THEN
        RAISE EXCEPTION 'runtime role must not belong to other roles';
    END IF;
    IF has_parameter_privilege(runtime_role, 'session_replication_role', 'SET, ALTER SYSTEM') THEN
        RAISE EXCEPTION 'runtime has unsafe session_replication_role privileges';
    END IF;
    IF EXISTS (
        SELECT 1 FROM pg_class
        WHERE relnamespace = (SELECT oid FROM pg_namespace WHERE nspname = target_schema)
          AND relowner = (SELECT oid FROM pg_roles WHERE rolname = runtime_role)
    ) THEN
        RAISE EXCEPTION 'runtime role must not own schema objects';
    END IF;

    EXECUTE format('REVOKE ALL ON SCHEMA %I FROM PUBLIC, %I', target_schema, runtime_role);
    EXECUTE format('GRANT USAGE ON SCHEMA %I TO %I', target_schema, runtime_role);
    EXECUTE format('REVOKE ALL ON ALL TABLES IN SCHEMA %I FROM PUBLIC, %I', target_schema, runtime_role);
    EXECUTE format('REVOKE ALL ON ALL FUNCTIONS IN SCHEMA %I FROM PUBLIC, %I', target_schema, runtime_role);
    ALTER DEFAULT PRIVILEGES REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC;

    EXECUTE format(
        'GRANT SELECT, INSERT, UPDATE ON %I.wallets, %I.wager_transactions, %I.inbox, %I.outbox TO %I',
        target_schema, target_schema, target_schema, target_schema, runtime_role
    );
    EXECUTE format('GRANT SELECT, INSERT ON %I.wallet_ledger TO %I', target_schema, runtime_role);
    EXECUTE format('GRANT SELECT ON %I.schema_migrations TO %I', target_schema, runtime_role);
    EXECUTE format(
        'GRANT EXECUTE ON FUNCTION %I.reject_financial_mutation(), %I.check_wallet_ledger(), %I.protect_transaction() TO %I',
        target_schema, target_schema, target_schema, runtime_role
    );
END;
$$;
