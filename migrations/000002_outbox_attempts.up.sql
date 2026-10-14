-- Existing attempts count claims, not necessarily failed publications.
ALTER TABLE outbox RENAME COLUMN attempts TO processing_attempts;
ALTER TABLE outbox ADD COLUMN publication_failures INTEGER NOT NULL DEFAULT 0
    CHECK (publication_failures >= 0);
ALTER TABLE outbox ADD CONSTRAINT outbox_processing_attempts_nonnegative
    CHECK (processing_attempts >= 0);
