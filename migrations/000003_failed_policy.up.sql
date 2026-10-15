-- Permanent failure requires a confirmed cause, never merely exhausted retries.
ALTER TABLE wager_transactions
    ADD CONSTRAINT transaction_failed_policy CHECK (
        (status = 'FAILED') = (failure_code = 'INFRASTRUCTURE_DATA_LOSS')
    );
