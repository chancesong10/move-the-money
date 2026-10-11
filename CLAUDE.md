# Project: Move the Money


A small Go service where accounts hold a balance and money moves between them.
Correctness matters far more than features. Keep the scope small.


## Required features
- Open an account with a starting balance
- Transfer an amount from one account to another
- Get an account's current balance
- Get an account's transaction history
- Interface: a thin HTTP API (JSON). No UI, no auth, no deployment.


## The four rules (never break these)
1. A balance can never go negative, including under concurrent requests.
2. A transfer is all or nothing. Money never leaves one account without arriving in the other.
3. The same transfer submitted twice (same idempotency key) is applied only once.
4. Amounts are exact. No cent can ever go missing.


## Stack
- Go, PostgreSQL 16 (local, via docker-compose), pgx as the driver.
- Connection string comes from the `DATABASE_URL` environment variable.
- Schema lives in `migrations/` as plain SQL.


## Money handling
- Store all amounts as integer cents (`BIGINT` in Postgres, `int64` in Go).
- NEVER use float32/float64 for money, anywhere, including JSON parsing.
- Reject amounts that are zero, negative, or have more than 2 decimal places.


## Transfer design (all inside ONE database transaction)
1. Insert the transfer row with its idempotency key first (UNIQUE constraint).
   If the key already exists, return the original result instead of re-applying.
   Do NOT "check if the key exists, then insert" in separate steps; that is a race.
   If the same key arrives with a different amount/from/to, return an error.
2. Lock both accounts with `SELECT ... FOR UPDATE`, always in a consistent order
   (lowest account ID first) to avoid deadlocks.
3. Check the sender's balance, then debit and credit.
4. Insert two ledger entries (debit + credit) and commit.
   Any error rolls back the whole transaction.
- Also keep `CHECK (balance_cents >= 0)` on accounts as a database-level safety net.
- Reject transfers from an account to itself.


## Testing rules
- Tests run against a REAL Postgres database. Never mock the database:
  mocks cannot test locking or transactions.
- Each test starts from a clean database (truncate tables before each test).
- Concurrency tests must start goroutines at the same moment using a barrier
  (e.g. all goroutines wait on a shared channel that is then closed),
  so the transfers genuinely compete. Use `go test -race`.
- Every concurrency test should FAIL if the row lock is removed.
- Assert the total money across all accounts never changes (conservation).


## How to work with me
- Explain non-obvious decisions briefly, especially anything about concurrency.
- Make small, focused changes. Ask before large refactors or adding dependencies.
- Prefer the standard library unless a dependency is clearly worth it.
- Do NOT write or edit BUILD_LOG.md. I write that myself.


## Commits
- Small commits with honest, descriptive messages.
- NEVER add "Co-Authored-By" trailers or "Generated with Claude Code" lines
  to commit messages or pull requests.
