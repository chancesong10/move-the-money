-- Record rejected transfers (e.g. insufficient funds) instead of rolling them
-- back. Retrying the same idempotency key then returns the original outcome,
-- rejection included, rather than re-evaluating it later when the balance may
-- have changed. It also leaves an audit trail of failed attempts.
--
-- Rejected transfers write no ledger entries: the ledger is money that moved,
-- this table is every attempt and its outcome.
ALTER TABLE transfers
    ADD COLUMN status TEXT NOT NULL DEFAULT 'completed'
        CHECK (status IN ('completed', 'rejected')),
    ADD COLUMN failure_reason TEXT,
    ADD CONSTRAINT transfers_failure_reason_matches_status
        CHECK ((status = 'rejected') = (failure_reason IS NOT NULL));

-- The default only exists to backfill rows that predate this migration.
-- From here on, every insert must say what happened.
ALTER TABLE transfers ALTER COLUMN status DROP DEFAULT;
