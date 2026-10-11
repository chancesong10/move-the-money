-- All amounts are integer cents. No floating point anywhere.

CREATE TABLE accounts (
    id            BIGSERIAL   PRIMARY KEY,
    -- Database-level safety net for rule 1. The application checks the
    -- balance first; this catches any code path that forgets to.
    balance_cents BIGINT      NOT NULL CHECK (balance_cents >= 0),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE transfers (
    id              BIGSERIAL   PRIMARY KEY,
    -- UNIQUE is what makes rule 3 race-free: two concurrent inserts with the
    -- same key cannot both succeed, no matter how they interleave.
    idempotency_key TEXT        NOT NULL UNIQUE,
    from_account_id BIGINT      NOT NULL REFERENCES accounts (id),
    to_account_id   BIGINT      NOT NULL REFERENCES accounts (id),
    amount_cents    BIGINT      NOT NULL CHECK (amount_cents > 0),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (from_account_id <> to_account_id)
);

-- Append-only history. Signed amounts: negative = money out, positive = in.
-- Invariant: for every account, balance_cents = SUM(amount_cents) of its entries.
CREATE TABLE ledger_entries (
    id           BIGSERIAL   PRIMARY KEY,
    account_id   BIGINT      NOT NULL REFERENCES accounts (id),
    -- NULL only for the opening-balance entry written when the account is created.
    transfer_id  BIGINT      REFERENCES transfers (id),
    amount_cents BIGINT      NOT NULL CHECK (amount_cents <> 0),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX ledger_entries_account_id_idx ON ledger_entries (account_id, id);
