#!/usr/bin/env bash
# Walks through the API one step at a time, pausing for Enter between steps.
# Needs the server running (go run ./cmd/server). Run from Git Bash:
#   bash scripts/demo.sh
set -euo pipefail

BASE="${BASE:-http://localhost:8080}"
RUN="$(date +%s)" # makes idempotency keys unique per run, so reruns don't replay old ones

step() {
	echo
	echo "=== $1"
	read -rp "    (Enter to run) " _
}

# Prints the request, then the status line, any replay header, and the body.
call() {
	echo "\$ curl $*"
	curl -s -i "$@" | tr -d '\r' | sed -n '1p;/^Idempotent-Replayed/p;$p'
}

# Opens an account, shows the response, and outputs the new account's ID.
open_account() {
	echo "\$ curl -X POST $BASE/accounts -d '{\"opening_balance\": \"$1\"}'" >&2
	local body
	body=$(curl -s -X POST "$BASE/accounts" -d "{\"opening_balance\": \"$1\"}")
	echo "$body" >&2
	echo "$body" | grep -o '"id":[0-9]*' | cut -d: -f2
}

step "Open Alice with \$100.00 and Bob with \$0"
ALICE=$(open_account "100.00")
BOB=$(open_account "0")
echo "    alice = account $ALICE, bob = account $BOB"

step "Alice sends Bob \$25.29 (key transfer-$RUN)"
call -X POST "$BASE/transfers" -H "Idempotency-Key: transfer-$RUN" \
	-d "{\"from_account_id\": $ALICE, \"to_account_id\": $BOB, \"amount\": \"25.29\"}"

step "Rule 3: the exact same request again (a retry / double-click)"
call -X POST "$BASE/transfers" -H "Idempotency-Key: transfer-$RUN" \
	-d "{\"from_account_id\": $ALICE, \"to_account_id\": $BOB, \"amount\": \"25.29\"}"

step "Same key, different amount: rejected, not applied"
call -X POST "$BASE/transfers" -H "Idempotency-Key: transfer-$RUN" \
	-d "{\"from_account_id\": $ALICE, \"to_account_id\": $BOB, \"amount\": \"99.00\"}"

step "Rule 1: Alice tries to send \$500.00 (she has \$74.71)"
call -X POST "$BASE/transfers" -H "Idempotency-Key: overdraw-$RUN" \
	-d "{\"from_account_id\": $ALICE, \"to_account_id\": $BOB, \"amount\": \"500.00\"}"

step "Rule 4: an amount a float would mangle, and a typo'd field name"
call -X POST "$BASE/accounts" -d '{"opening_balance": 0.30000000000000004}'
call -X POST "$BASE/accounts" -d '{"opening_balnce": "500.00"}'

step "Alice's balance (100.00 - 25.29, applied once)"
call "$BASE/accounts/$ALICE"

step "Alice's transaction history"
call "$BASE/accounts/$ALICE/transactions"

step "Rule 1 under load: 20 simultaneous \$10 transfers from a fresh \$100 account"
# Fires 20 curl processes at once. Not a strict barrier like the Go tests
# (those are the real proof); this is the visual version.
RICH=$(open_account "100.00")
for i in $(seq 1 20); do
	curl -s -o /dev/null -w "%{http_code}\n" -X POST "$BASE/transfers" \
		-H "Idempotency-Key: race-$RUN-$i" \
		-d "{\"from_account_id\": $RICH, \"to_account_id\": $BOB, \"amount\": \"10.00\"}" &
done | sort | uniq -c | sed 's/^/    /'
echo "    (201 = sent, 422 = insufficient funds; expect 10 of each)"
call "$BASE/accounts/$RICH"

step "The real proof: the Go concurrency test (barrier, 20 goroutines, 5 rounds)"
# Unlike the curl loop above, this releases all goroutines at the same instant
# and fails if the row lock in internal/bank/transfer.go is removed.
cd "$(dirname "$0")/.."
echo "\$ go test -count=1 -v -run TestConcurrentTransfersCannotOverdraw ./internal/bank/"
go test -count=1 -v -run TestConcurrentTransfersCannotOverdraw ./internal/bank/
