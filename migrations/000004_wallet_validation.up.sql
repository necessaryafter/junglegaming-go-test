-- Preserve all financial checks and support their existing lookup predicates.
CREATE INDEX transaction_wallet ON wager_transactions(wallet_id);
CREATE INDEX ledger_transaction ON wallet_ledger(transaction_id);
