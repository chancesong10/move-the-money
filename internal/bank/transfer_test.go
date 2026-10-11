package bank

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"

	"github.com/chancesong10/move-the-money/internal/money"
)

// --- helpers ---

func mustOpen(t *testing.T, b *Bank, cents int64) Account {
	t.Helper()
	acc, err := b.OpenAccount(context.Background(), cents)
	if err != nil {
		t.Fatal(err)
	}
	return acc
}

func balanceOf(t *testing.T, b *Bank, id int64) int64 {
	t.Helper()
	acc, err := b.GetAccount(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return acc.BalanceCents
}

// totalMoney is the sum of every balance. Transfers only move money, so this
// must never change (conservation). Note: Postgres SUM(bigint) is NUMERIC.
func totalMoney(t *testing.T) int64 {
	t.Helper()
	var total int64
	err := testPool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(balance_cents), 0)::BIGINT FROM accounts`).Scan(&total)
	if err != nil {
		t.Fatal(err)
	}
	return total
}

func countRows(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func countTransfers(t *testing.T, status string) int {
	t.Helper()
	var n int
	err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM transfers WHERE status = $1`, status).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func assertNoNegativeBalances(t *testing.T) {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM accounts WHERE balance_cents < 0`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d accounts have a negative balance", n)
	}
}

type transferResult struct {
	transfer Transfer
	replayed bool
	err      error
}

// runConcurrently starts n goroutines, waits until every one of them is
// parked on the same channel, then closes it so they all start at once.
// Without this barrier, early goroutines can finish before late ones start,
// and the "concurrent" test quietly runs sequentially.
func runConcurrently(n int, fn func(i int) transferResult) []transferResult {
	results := make([]transferResult, n)
	start := make(chan struct{})
	var ready, done sync.WaitGroup
	ready.Add(n)
	for i := range n {
		done.Go(func() {
			ready.Done()
			<-start
			results[i] = fn(i)
		})
	}
	ready.Wait()
	close(start)
	done.Wait()
	return results
}

// --- basic behaviour ---

func TestTransfer(t *testing.T) {
	b := newBank(t)
	ctx := context.Background()
	alice := mustOpen(t, b, 10_000)
	bob := mustOpen(t, b, 5_000)

	tr, replayed, err := b.Transfer(ctx, TransferRequest{"k1", alice.ID, bob.ID, 3_000})
	if err != nil {
		t.Fatal(err)
	}
	if replayed {
		t.Error("first submission reported as replayed")
	}
	if got := balanceOf(t, b, alice.ID); got != 7_000 {
		t.Errorf("alice = %d, want 7000", got)
	}
	if got := balanceOf(t, b, bob.ID); got != 8_000 {
		t.Errorf("bob = %d, want 8000", got)
	}

	history, err := b.History(ctx, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[1].AmountCents != -3_000 ||
		history[1].TransferID == nil || *history[1].TransferID != tr.ID {
		t.Errorf("alice history = %+v, want opening entry then -3000 for transfer %d", history, tr.ID)
	}
	if history[0].CounterpartyAccountID != nil {
		t.Errorf("opening entry counterparty = %v, want nil", *history[0].CounterpartyAccountID)
	}
	if cp := history[1].CounterpartyAccountID; cp == nil || *cp != bob.ID {
		t.Errorf("alice's transfer entry counterparty = %v, want bob (%d)", cp, bob.ID)
	}
	bobHistory, err := b.History(ctx, bob.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cp := bobHistory[1].CounterpartyAccountID; cp == nil || *cp != alice.ID {
		t.Errorf("bob's transfer entry counterparty = %v, want alice (%d)", cp, alice.ID)
	}
	assertLedgerMatchesBalances(t)
}

func TestTransferEntireBalance(t *testing.T) {
	b := newBank(t)
	alice := mustOpen(t, b, 2_500)
	bob := mustOpen(t, b, 0)

	if _, _, err := b.Transfer(context.Background(), TransferRequest{"k1", alice.ID, bob.ID, 2_500}); err != nil {
		t.Fatalf("sending exactly the balance should succeed: %v", err)
	}
	if got := balanceOf(t, b, alice.ID); got != 0 {
		t.Errorf("alice = %d, want 0", got)
	}
}

func TestTransferInsufficientFundsIsRecordedAsRejected(t *testing.T) {
	b := newBank(t)
	ctx := context.Background()
	alice := mustOpen(t, b, 1_000)
	bob := mustOpen(t, b, 0)

	tr, replayed, err := b.Transfer(ctx, TransferRequest{"k1", alice.ID, bob.ID, 1_001})
	if !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("err = %v, want ErrInsufficientFunds", err)
	}
	if replayed || tr.ID == 0 || tr.Status != StatusRejected || tr.FailureReason != "insufficient_funds" {
		t.Errorf("got %+v replayed=%v, want a fresh rejected transfer with reason insufficient_funds", tr, replayed)
	}
	if got := balanceOf(t, b, alice.ID); got != 1_000 {
		t.Errorf("alice = %d, want 1000 (unchanged)", got)
	}
	if n := countTransfers(t, StatusRejected); n != 1 {
		t.Errorf("%d rejected transfer rows, want 1 (the attempt is kept for audit)", n)
	}
	if n := countRows(t, "ledger_entries"); n != 1 {
		t.Errorf("%d ledger entries, want 1 (alice's opening only; rejections move no money)", n)
	}
	assertLedgerMatchesBalances(t)
}

// The stale-retry scenario: a transfer is rejected, money then arrives, and
// a retry of the original request (same key) lands afterwards. The retry must
// get the original rejection back, not quietly move money the user was told
// had failed.
func TestRetryOfRejectedTransferStaysRejected(t *testing.T) {
	b := newBank(t)
	ctx := context.Background()
	alice := mustOpen(t, b, 1_000)
	bob := mustOpen(t, b, 0)
	carol := mustOpen(t, b, 10_000)
	req := TransferRequest{"pay-bob", alice.ID, bob.ID, 5_000}

	first, _, err := b.Transfer(ctx, req)
	if !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("first attempt: err = %v, want ErrInsufficientFunds", err)
	}

	// Money arrives: alice now has enough.
	if _, _, err := b.Transfer(ctx, TransferRequest{"top-up", carol.ID, alice.ID, 9_000}); err != nil {
		t.Fatal(err)
	}

	retry, replayed, err := b.Transfer(ctx, req)
	if !errors.Is(err, ErrInsufficientFunds) || !replayed {
		t.Fatalf("retry: replayed=%v err=%v, want the original rejection replayed", replayed, err)
	}
	if retry.ID != first.ID || retry.Status != StatusRejected {
		t.Errorf("retry returned %+v, want original rejected transfer %d", retry, first.ID)
	}
	if got := balanceOf(t, b, alice.ID); got != 10_000 {
		t.Errorf("alice = %d, want 10000: the stale retry moved money", got)
	}

	// A genuinely new request (new key) is evaluated on its own and succeeds.
	if _, _, err := b.Transfer(ctx, TransferRequest{"pay-bob-again", alice.ID, bob.ID, 5_000}); err != nil {
		t.Errorf("new key after top-up: %v, want success", err)
	}
	assertLedgerMatchesBalances(t)
}

func TestTransferValidation(t *testing.T) {
	b := newBank(t)
	alice := mustOpen(t, b, 10_000)
	bob := mustOpen(t, b, 0)

	cases := []struct {
		name string
		req  TransferRequest
		want error
	}{
		{"zero amount", TransferRequest{"k", alice.ID, bob.ID, 0}, money.ErrInvalidAmount},
		{"negative amount", TransferRequest{"k", alice.ID, bob.ID, -100}, money.ErrInvalidAmount},
		{"same account", TransferRequest{"k", alice.ID, alice.ID, 100}, ErrSameAccount},
		{"empty key", TransferRequest{"", alice.ID, bob.ID, 100}, ErrInvalidKey},
		{"key too long", TransferRequest{strings.Repeat("x", 256), alice.ID, bob.ID, 100}, ErrInvalidKey},
		{"key not valid UTF-8", TransferRequest{"\xff\xfe", alice.ID, bob.ID, 100}, ErrInvalidKey},
		{"key contains NUL", TransferRequest{"a\x00b", alice.ID, bob.ID, 100}, ErrInvalidKey},
		{"unknown receiver", TransferRequest{"k", alice.ID, 999, 100}, ErrAccountNotFound},
		{"unknown sender", TransferRequest{"k", 999, bob.ID, 100}, ErrAccountNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := b.Transfer(context.Background(), tc.req)
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
	if got := balanceOf(t, b, alice.ID); got != 10_000 {
		t.Errorf("alice = %d, want 10000 (no rejected transfer should move money)", got)
	}
	if n := countRows(t, "transfers"); n != 0 {
		t.Errorf("%d transfer rows, want 0", n)
	}
}

// --- rule 2: all or nothing ---

// The receiver's balance is so close to the BIGINT maximum that crediting it
// overflows. By then the sender has already been debited inside the
// transaction, so this proves a mid-transfer failure undoes the debit too.
func TestTransferIsAllOrNothing(t *testing.T) {
	b := newBank(t)
	alice := mustOpen(t, b, 10_000)
	whale := mustOpen(t, b, math.MaxInt64-5)

	_, _, err := b.Transfer(context.Background(), TransferRequest{"k1", alice.ID, whale.ID, 10})
	if !errors.Is(err, ErrBalanceOverflow) {
		t.Fatalf("err = %v, want ErrBalanceOverflow", err)
	}
	if got := balanceOf(t, b, alice.ID); got != 10_000 {
		t.Errorf("alice = %d, want 10000: money left her account without arriving", got)
	}
	if got := balanceOf(t, b, whale.ID); got != math.MaxInt64-5 {
		t.Errorf("whale = %d, want unchanged", got)
	}
	if n := countRows(t, "transfers"); n != 0 {
		t.Errorf("%d transfer rows, want 0", n)
	}
	assertLedgerMatchesBalances(t)
}

// --- rule 3: applied once ---

func TestDuplicateSubmissionIsAppliedOnce(t *testing.T) {
	b := newBank(t)
	ctx := context.Background()
	alice := mustOpen(t, b, 10_000)
	bob := mustOpen(t, b, 0)
	req := TransferRequest{"double-click", alice.ID, bob.ID, 2_500}

	first, replayed1, err := b.Transfer(ctx, req)
	if err != nil || replayed1 {
		t.Fatalf("first: replayed=%v err=%v", replayed1, err)
	}
	second, replayed2, err := b.Transfer(ctx, req)
	if err != nil || !replayed2 {
		t.Fatalf("second: replayed=%v err=%v, want replayed", replayed2, err)
	}
	if second.ID != first.ID {
		t.Errorf("replay returned transfer %d, want original %d", second.ID, first.ID)
	}
	if got := balanceOf(t, b, alice.ID); got != 7_500 {
		t.Errorf("alice = %d, want 7500 (debited once)", got)
	}
	if n := countRows(t, "transfers"); n != 1 {
		t.Errorf("%d transfer rows, want 1", n)
	}
	assertLedgerMatchesBalances(t)
}

func TestReusedKeyWithDifferentDetailsIsRejected(t *testing.T) {
	b := newBank(t)
	ctx := context.Background()
	alice := mustOpen(t, b, 10_000)
	bob := mustOpen(t, b, 0)
	carol := mustOpen(t, b, 0)

	if _, _, err := b.Transfer(ctx, TransferRequest{"k1", alice.ID, bob.ID, 1_000}); err != nil {
		t.Fatal(err)
	}
	for name, req := range map[string]TransferRequest{
		"different amount":   {"k1", alice.ID, bob.ID, 2_000},
		"different receiver": {"k1", alice.ID, carol.ID, 1_000},
	} {
		if _, _, err := b.Transfer(ctx, req); !errors.Is(err, ErrIdempotencyConflict) {
			t.Errorf("%s: err = %v, want ErrIdempotencyConflict", name, err)
		}
	}
	if got := balanceOf(t, b, alice.ID); got != 9_000 {
		t.Errorf("alice = %d, want 9000", got)
	}
}

// --- concurrency ---

// THE key test for rule 1. Alice has $100. Twenty goroutines each try to send
// her $10 to Bob at the same instant. Exactly ten can succeed. If the row
// lock were missing, several would read the same balance, all pass the check,
// and either overdraw her or trip the database CHECK constraint, which shows
// up here as an error other than ErrInsufficientFunds.
func TestConcurrentTransfersCannotOverdraw(t *testing.T) {
	const (
		rounds     = 5 // repeat to make a missed race less likely to slip by
		goroutines = 20
		amount     = 1_000
		opening    = 10_000
	)
	b := newBank(t)
	ctx := context.Background()

	for round := range rounds {
		alice := mustOpen(t, b, opening)
		bob := mustOpen(t, b, 0)
		before := totalMoney(t)

		results := runConcurrently(goroutines, func(i int) transferResult {
			key := fmt.Sprintf("round%d-%d", round, i)
			tr, replayed, err := b.Transfer(ctx, TransferRequest{key, alice.ID, bob.ID, amount})
			return transferResult{tr, replayed, err}
		})

		succeeded := 0
		for _, r := range results {
			switch {
			case r.err == nil:
				succeeded++
			case errors.Is(r.err, ErrInsufficientFunds):
			default:
				t.Errorf("round %d: unexpected error: %v", round, r.err)
			}
		}
		if succeeded != opening/amount {
			t.Errorf("round %d: %d transfers succeeded, want exactly %d", round, succeeded, opening/amount)
		}
		if got := balanceOf(t, b, alice.ID); got != 0 {
			t.Errorf("round %d: alice = %d, want 0", round, got)
		}
		if got := balanceOf(t, b, bob.ID); got != opening {
			t.Errorf("round %d: bob = %d, want %d", round, got, opening)
		}
		if after := totalMoney(t); after != before {
			t.Errorf("round %d: total money changed from %d to %d", round, before, after)
		}
	}
	// Every attempt is on record: 10 completed and 10 rejected per round.
	if n := countTransfers(t, StatusCompleted); n != rounds*opening/amount {
		t.Errorf("%d completed transfers, want %d", n, rounds*opening/amount)
	}
	if n := countTransfers(t, StatusRejected); n != rounds*(goroutines-opening/amount) {
		t.Errorf("%d rejected transfers, want %d", n, rounds*(goroutines-opening/amount))
	}
	assertNoNegativeBalances(t)
	assertLedgerMatchesBalances(t)
}

// A->B and B->A at the same time is the classic deadlock: each transaction
// locks its sender first and then waits for the other's. Locking in ID order
// prevents it, so every one of these transfers must succeed.
func TestConcurrentOpposingTransfersDoNotDeadlock(t *testing.T) {
	const perDirection = 10
	b := newBank(t)
	ctx := context.Background()
	alice := mustOpen(t, b, 10_000)
	bob := mustOpen(t, b, 10_000)

	results := runConcurrently(2*perDirection, func(i int) transferResult {
		from, to := alice.ID, bob.ID
		if i%2 == 1 {
			from, to = bob.ID, alice.ID
		}
		tr, replayed, err := b.Transfer(ctx, TransferRequest{fmt.Sprintf("k%d", i), from, to, 100})
		return transferResult{tr, replayed, err}
	})

	for i, r := range results {
		if r.err != nil {
			t.Errorf("transfer %d failed: %v", i, r.err)
		}
	}
	if a, b := balanceOf(t, b, alice.ID), balanceOf(t, b, bob.ID); a != 10_000 || b != 10_000 {
		t.Errorf("alice = %d, bob = %d, want 10000 each", a, b)
	}
	assertLedgerMatchesBalances(t)
}

// Ten copies of the same request (same idempotency key) arrive at once, like
// a client retrying on a flaky network. Money must move exactly once.
func TestConcurrentDuplicateSubmissionsAreAppliedOnce(t *testing.T) {
	const copies = 10
	b := newBank(t)
	ctx := context.Background()
	alice := mustOpen(t, b, 10_000)
	bob := mustOpen(t, b, 0)
	req := TransferRequest{"retry-storm", alice.ID, bob.ID, 2_500}

	results := runConcurrently(copies, func(int) transferResult {
		tr, replayed, err := b.Transfer(ctx, req)
		return transferResult{tr, replayed, err}
	})

	fresh := 0
	for _, r := range results {
		if r.err != nil {
			t.Fatalf("unexpected error: %v", r.err)
		}
		if !r.replayed {
			fresh++
		}
		if r.transfer.ID != results[0].transfer.ID {
			t.Errorf("got transfer IDs %d and %d, want all the same", r.transfer.ID, results[0].transfer.ID)
		}
	}
	if fresh != 1 {
		t.Errorf("%d submissions applied, want exactly 1", fresh)
	}
	if got := balanceOf(t, b, alice.ID); got != 7_500 {
		t.Errorf("alice = %d, want 7500 (debited once)", got)
	}
	if n := countRows(t, "transfers"); n != 1 {
		t.Errorf("%d transfer rows, want 1", n)
	}
	assertLedgerMatchesBalances(t)
}

// Same as above, but the request is one that gets rejected. Every copy must
// see the same rejection: the waiting copies block on the first one's key,
// then replay its committed 'rejected' outcome.
func TestConcurrentDuplicatesOfRejectedTransferAllSeeRejection(t *testing.T) {
	const copies = 10
	b := newBank(t)
	ctx := context.Background()
	alice := mustOpen(t, b, 1_000)
	bob := mustOpen(t, b, 0)
	req := TransferRequest{"too-much", alice.ID, bob.ID, 5_000}

	results := runConcurrently(copies, func(int) transferResult {
		tr, replayed, err := b.Transfer(ctx, req)
		return transferResult{tr, replayed, err}
	})

	fresh := 0
	for _, r := range results {
		if !errors.Is(r.err, ErrInsufficientFunds) {
			t.Errorf("err = %v, want ErrInsufficientFunds", r.err)
		}
		if !r.replayed {
			fresh++
		}
	}
	if fresh != 1 {
		t.Errorf("%d submissions evaluated, want exactly 1 (the rest replayed)", fresh)
	}
	if n := countRows(t, "transfers"); n != 1 {
		t.Errorf("%d transfer rows, want 1", n)
	}
	if got := balanceOf(t, b, alice.ID); got != 1_000 {
		t.Errorf("alice = %d, want 1000", got)
	}
}

// Rule 4 under load: many random transfers between a few accounts, using
// amounts that a float would get wrong (0.29, 1.15, 4.35...). Some will fail
// for insufficient funds, which is fine. Not one cent may be created or lost.
func TestConcurrentRandomTransfersConserveMoney(t *testing.T) {
	const transfers = 200
	b := newBank(t)
	ctx := context.Background()

	var ids []int64
	for _, opening := range []int64{10_029, 5_115, 435, 1, 33_333} {
		ids = append(ids, mustOpen(t, b, opening).ID)
	}
	before := totalMoney(t)
	amounts := []int64{29, 115, 435, 1, 3_333, 999}
	rng := rand.New(rand.NewPCG(1, 2)) // fixed seed: failures are reproducible

	reqs := make([]TransferRequest, transfers)
	for i := range reqs {
		from := ids[rng.IntN(len(ids))]
		to := ids[rng.IntN(len(ids))]
		for to == from {
			to = ids[rng.IntN(len(ids))]
		}
		reqs[i] = TransferRequest{fmt.Sprintf("rand-%d", i), from, to, amounts[rng.IntN(len(amounts))]}
	}

	results := runConcurrently(transfers, func(i int) transferResult {
		tr, replayed, err := b.Transfer(ctx, reqs[i])
		return transferResult{tr, replayed, err}
	})

	succeeded := 0
	for _, r := range results {
		switch {
		case r.err == nil:
			succeeded++
		case errors.Is(r.err, ErrInsufficientFunds):
		default:
			t.Errorf("unexpected error: %v", r.err)
		}
	}
	if after := totalMoney(t); after != before {
		t.Errorf("total money changed from %d to %d", before, after)
	}
	if n := countTransfers(t, StatusCompleted); n != succeeded {
		t.Errorf("%d completed transfer rows but %d successes", n, succeeded)
	}
	if n := countTransfers(t, StatusRejected); n != transfers-succeeded {
		t.Errorf("%d rejected transfer rows but %d rejections", n, transfers-succeeded)
	}
	if n := countRows(t, "ledger_entries"); n != len(ids)+2*succeeded {
		t.Errorf("%d ledger entries, want %d (one opening each + two per transfer)", n, len(ids)+2*succeeded)
	}
	assertNoNegativeBalances(t)
	assertLedgerMatchesBalances(t)
	t.Logf("%d of %d random transfers succeeded", succeeded, transfers)
}
