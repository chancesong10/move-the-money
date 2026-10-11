-- Bound idempotency keys so a client can't store arbitrarily large ones.
-- The application checks this too; this is the database-level backstop.
ALTER TABLE transfers
    ADD CONSTRAINT transfers_idempotency_key_length
    CHECK (char_length(idempotency_key) BETWEEN 1 AND 255);
