-- Read-only probes of the three financial validations in check_wallet_ledger().
-- Supply a synthetic wallet from a load report: psql ... -v wallet_id=... -f ...
\set ON_ERROR_STOP on
BEGIN READ ONLY;
SET LOCAL statement_timeout = '15s';
SET LOCAL search_path = public;

EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)
SELECT
    COALESCE(SUM(CASE l.direction WHEN 'CREDIT' THEN l.amount::numeric ELSE -l.amount::numeric END), 0),
    COUNT(*) FILTER (WHERE t.kind <> 'OPENING')
FROM wallet_ledger l
JOIN wager_transactions t ON t.id = l.transaction_id
WHERE l.wallet_id = :'wallet_id'::uuid;

EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)
SELECT EXISTS (
    SELECT 1
    FROM wallet_ledger l
    JOIN wager_transactions t ON t.id = l.transaction_id
    WHERE l.wallet_id = :'wallet_id'::uuid
      AND (
          t.status <> 'PROCESSED'
          OR t.kind = 'LOSS'
          OR t.amount <> l.amount
          OR (l.direction = 'CREDIT' AND t.kind = 'BET')
          OR (l.direction = 'DEBIT' AND t.kind IN ('OPENING', 'WIN', 'REFUND'))
      )
);

EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)
SELECT EXISTS (
    SELECT 1
    FROM wager_transactions t
    LEFT JOIN wallet_ledger l ON l.transaction_id = t.id
    WHERE t.wallet_id = :'wallet_id'::uuid
      AND t.status = 'PROCESSED'
      AND t.kind <> 'LOSS'
      AND l.id IS NULL
);

ROLLBACK;
