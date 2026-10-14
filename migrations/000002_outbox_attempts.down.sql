ALTER TABLE outbox DROP CONSTRAINT outbox_processing_attempts_nonnegative;
ALTER TABLE outbox DROP COLUMN publication_failures;
ALTER TABLE outbox RENAME COLUMN processing_attempts TO attempts;
