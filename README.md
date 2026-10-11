# Move the Money

A small Go + PostgreSQL service where accounts hold a balance and money moves
between them, behind a thin JSON HTTP API. The scope is deliberately small;
the focus is on the four rules below holding under concurrency, retries and
failure, and on tests that prove it against a real database.

How I built it, and where the AI tools I used were wrong, is in
[Build log.md](Build%20log.md).

## Quick start

Prerequisites: **Go 1.25+** and **Docker**.

```bash
docker compose up -d       # Postgres 16 on localhost:5432
go run ./cmd/server        # API on localhost:8080 (Ctrl+C to stop)
```

Then, in a second terminal (Git Bash on Windows):

```bash
bash scripts/demo.sh       # walks through the API step by step, then runs the concurrency test
```

**Migrations gotcha.** Postgres applies `migrations/` only when its data volume
is first created, while the tests rebuild their schema from `migrations/` on
every run. So after adding a migration, the tests pass but the running server
won't have the change until you either recreate the database (deletes dev data):

```bash
docker compose down -v && docker compose up -d
```

or apply the new file by hand:

```bash
# Git Bash / macOS / Linux
docker compose exec -T postgres psql -U money -d money < migrations/004_ledger_append_only.sql
# PowerShell
Get-Content migrations\004_ledger_append_only.sql | docker compose exec -T postgres psql -U money -d money
```

| Variable | Default | Used by |
|---|---|---|
| `DATABASE_URL` | `postgres://money:money@localhost:5432/money` | server |
| `ADDR` | `:8080` | server |
| `TEST_DATABASE_URL` | `postgres://money:money@localhost:5432/money_test` | tests (name must end in `_test`) |

## API

Amounts are decimal strings with at most two places (`"25.29"`). Requests may
also send a bare JSON number (`25.29`); it is parsed as text, never as a float.
Responses always use strings. Unknown JSON fields are rejected.

### `POST /accounts`

```bash
curl -X POST localhost:8080/accounts -d '{"opening_balance": "100.00"}'
```
```json
{"id":1,"balance":"100.00","created_at":"2026-10-11T13:38:07.69752-07:00"}
```

### `GET /accounts/{id}`

```json
{"id":1,"balance":"74.71","created_at":"2026-10-11T13:38:07.69752-07:00"}
```

### `GET /accounts/{id}/transactions`

Oldest first. Only money that actually moved appears here (rejected transfers don't).

```json
{"account_id":1,"transactions":[
  {"id":1,"type":"opening_balance","amount":"100.00","transfer_id":null,"counterparty_account_id":null,"created_at":"..."},
  {"id":2,"type":"transfer_out","amount":"-25.29","transfer_id":1,"counterparty_account_id":2,"created_at":"..."}
]}
```

### `POST /transfers`

The `Idempotency-Key` header is required (1–255 characters).

```bash
curl -X POST localhost:8080/transfers \
  -H "Idempotency-Key: pay-bob-001" \
  -d '{"from_account_id": 1, "to_account_id": 2, "amount": "25.29"}'
```
```json
{"id":1,"idempotency_key":"pay-bob-001","from_account_id":1,"to_account_id":2,"amount":"25.29","status":"completed","created_at":"..."}
```

Insufficient funds is recorded as a rejected transfer and returned with a 422:

```json
{"error":"insufficient_funds","message":"insufficient funds","transfer":{"id":3,...,"status":"rejected","failure_reason":"insufficient_funds"}}
```

### Status codes

| Status | When |
|---|---|
| `201` | Account opened / transfer completed |
| `200` | Read succeeded, or **replay** of a completed transfer (with `Idempotent-Replayed: true`) |
| `400` | Malformed JSON, unknown or missing fields, invalid amount, missing/oversized key, same account |
| `404` | Account doesn't exist |
| `409` | Idempotency key already used for a *different* transfer (the IETF Idempotency-Key draft suggests 422 here; I used 409 to keep it distinct from insufficient funds) |
| `422` | Insufficient funds (a replay of the rejection also gets `Idempotent-Replayed: true`), or the receiver's balance would exceed the maximum |
| `500` | Unexpected error (logged server-side; the client only sees `internal_error`) |

Errors look like `{"error": "<code>", "message": "<text>"}`.

## Running the tests

Tests run against a **real Postgres** (no mocks: mocks can't test locks or
transactions). With `docker compose up -d` running:

```bash
go test ./...
```

Each test package gets its own database (`money_bank_test`, `money_api_test`),
created automatically and rebuilt from `migrations/` on every run; each test
starts from empty tables. The harness refuses any database whose name doesn't
end in `_test`, so it can't wipe the dev database.

**Race detector.** On macOS/Linux: `go test -race ./...`. On Windows it needs a
C compiler; without one, run the tests in a Go container (PowerShell, Docker
Desktop):

```powershell
docker run --rm -v "${PWD}:/src" -v gomodcache:/go/pkg/mod -w /src `
  -e TEST_DATABASE_URL=postgres://money:money@host.docker.internal:5432/money_test `
  golang:1.25 go test -race -count=1 ./...
```

## The four rules: how each is enforced and proven

| Rule | Enforced by | Proven by (`internal/bank/transfer_test.go` unless noted) |
|---|---|---|
| **1. Balance never negative**, including concurrent requests | Both accounts locked (`FOR NO KEY UPDATE`) before the balance is checked, in the same transaction that changes it; `CHECK (balance_cents >= 0)` as a database backstop | `TestConcurrentTransfersCannotOverdraw`: $100, 20 goroutines × $10 released by a barrier, 5 rounds; exactly 10 succeed and **every** failure must be a clean insufficient-funds error (so the `CHECK` can't mask a missing lock). Also `TestConcurrentRandomTransfersConserveMoney`, `TestConcurrentOpposingTransfersDoNotDeadlock`, and `TestConcurrentTransfersOverHTTP` (`internal/api`) |
| **2. All or nothing** | One database transaction per transfer; any error rolls back | `TestTransferIsAllOrNothing`: the credit overflows *after* the debit has run; the sender's balance must be unchanged |
| **3. Applied once** | `UNIQUE` idempotency key, claimed first with `INSERT ... ON CONFLICT DO NOTHING` in the same transaction | `TestDuplicateSubmissionIsAppliedOnce`, `TestConcurrentDuplicateSubmissionsAreAppliedOnce` (10 at once), `TestReusedKeyWithDifferentDetailsIsRejected`, `TestRetryOfRejectedTransferStaysRejected`, `TestConcurrentDuplicatesOfRejectedTransferAllSeeRejection` |
| **4. Exact amounts** | Integer cents everywhere (`BIGINT` / `int64`); amounts parsed from text, never via float | After every test: balance = sum of ledger entries, and total money unchanged. `internal/money` tests (`0.30000000000000004` rejected, overflow edges); random transfers use amounts a float gets wrong (0.29, 1.15, 4.35); `TestNoFloatsInProductionCode` fails if `float32`/`float64` appears in non-test code |

**The tests have been seen to fail.** I removed the row lock and re-ran the
suite: all three bank concurrency tests fail. Removing the lock *and* the
`CHECK` lets 20 of 20 transfers succeed against $100 (balance goes negative).
Switching to `FOR UPDATE` produces deadlocks. Removing `ON CONFLICT` fails the
idempotency tests. To try it: in `internal/bank/transfer.go`, delete
` FOR NO KEY UPDATE` from the `SELECT balance_cents` query and run
`go test -count=1 -run TestConcurrentTransfersCannotOverdraw -v ./internal/bank/`.

## Design decisions

- **Postgres over SQLite or in-memory.** SQLite allows one writer at a time for
  the whole database, so a concurrency test would pass without proving
  anything about my locking. Postgres has real row-level locks and
  transactions, and the CHECK constraint gives a guarantee independent of the
  Go code. Cost: Docker is required to run anything.
- **Integer cents.** Floats can't represent most decimal fractions (`0.29`
  becomes `0.28999…`, and `int64(0.29*100)` is 28). This works because the
  scope is one currency and no division; see "not built".
- **Row locks, not `SERIALIZABLE`.** Transactions run at Postgres's default
  `READ COMMITTED` with explicit locks. `SERIALIZABLE` would also be correct
  but fails conflicting transactions, so every caller needs a retry loop;
  explicit locks are easier to reason about and to test.
- **`FOR NO KEY UPDATE`, not `FOR UPDATE`.** Inserting the transfer row makes
  Postgres take `FOR KEY SHARE` on both accounts for the foreign-key check, and
  `FOR UPDATE` conflicts with it: concurrent transfers deadlocked. `NO KEY
  UPDATE` still serialises transfers on the same account. Locks are taken
  lowest account ID first so A→B and B→A can't deadlock either.
- **Transfers table + ledger.** `transfers` records every attempt and its
  outcome (and holds the idempotency key); `ledger_entries` records money that
  actually moved, with signed amounts. It's redundant on purpose: tests check
  they agree, and history comes from the ledger so rejections never show up
  as money movement. A database trigger rejects any `UPDATE` or `DELETE` on
  the ledger, so history can't be rewritten.
- **Rejected transfers are recorded.** Insufficient funds commits a `rejected`
  row instead of rolling back, so a retry with the same key gets the original
  rejection. Otherwise a stale retry could succeed after money arrives and move
  money the user was told had failed. Consequence: a rejected key is spent, and
  a genuine new attempt needs a new key. Invalid requests (bad amount, unknown
  account) aren't recorded. One gap that leaves: a transfer to an account ID
  that doesn't exist yet gets a 404, but if that ID is opened later, a retry
  with the same key would succeed and pay the new account.
- **Idempotency key in a header** (`Idempotency-Key`), the common convention,
  keeping the body about the transfer itself.
- **Strict input.** Unknown fields are rejected and required fields must be
  present, so a typo like `opening_balnce` is a 400 rather than a silent $0
  account. Bodies are capped at 1 MB.

## Project layout

```
cmd/server/          HTTP server entry point
internal/api/        HTTP handlers (translation only, no money rules) + tests
internal/bank/       accounts, transfers, locking, idempotency + tests
internal/money/      exact decimal-string <-> cents conversion + tests
internal/testdb/     real-Postgres test harness
migrations/          schema, plain SQL
scripts/demo.sh      step-by-step API demo
```

## What I chose not to build 
(adding most of these would be a quick change but testing them for correctness takes longer and I decided for the current scope of the assigment, as well as the 4 hour limit, I would leave these for future implementations.)

- **Authentication / authorisation**: anyone who can reach the API can move anyone's money.
- **Multiple currencies**: amounts assume two decimal places.
- **Deposits, withdrawals, closing accounts**: money only enters at account opening.
- **Interest, fees, splitting**: anything that divides money needs explicit rounding rules.
- **Pagination** on history: it returns every entry.
- **Idempotency key expiry or per-client scoping**: keys are global and kept forever.
- **A migration tool**: Postgres applies `migrations/` on first start only.
- **Deployment, metrics, tracing.**

## What I'd do next

1. **A migration runner** that applies new files on server start (tracking applied versions in a table), removing the migrations gotcha above.
2. **Record every rejected attempt the same way**: balance overflow and unknown accounts currently aren't recorded, which leaves the retry gap described above.
3. **Scope idempotency keys per client and expire them**, which needs authentication first.
4. **Paginate history** (`?after=<entry id>&limit=50`, using the existing `(account_id, id)` index).
5. **Authentication**, with accounts owned by users.
