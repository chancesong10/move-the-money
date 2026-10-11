package bank

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chancesong10/move-the-money/internal/money"
	"github.com/chancesong10/move-the-money/internal/testdb"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	pool, err := testdb.Setup(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "test database setup failed:", err)
		os.Exit(1)
	}
	testPool = pool
	code := m.Run()
	pool.Close()
	os.Exit(code)
}

// newBank gives each test an empty database.
func newBank(t *testing.T) *Bank {
	t.Helper()
	testdb.Reset(t, testPool)
	return New(testPool)
}

// assertLedgerMatchesBalances checks the core invariant: every account's
// stored balance equals the sum of its ledger entries.
func assertLedgerMatchesBalances(t *testing.T) {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `
		SELECT a.id, a.balance_cents, COALESCE(SUM(e.amount_cents), 0)::BIGINT
		FROM accounts a LEFT JOIN ledger_entries e ON e.account_id = a.id
		GROUP BY a.id ORDER BY a.id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, balance, ledgerSum int64
		if err := rows.Scan(&id, &balance, &ledgerSum); err != nil {
			t.Fatal(err)
		}
		if balance != ledgerSum {
			t.Errorf("account %d: balance %d != ledger sum %d", id, balance, ledgerSum)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenAccount(t *testing.T) {
	b := newBank(t)
	ctx := context.Background()

	acc, err := b.OpenAccount(ctx, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	if acc.BalanceCents != 10_000 {
		t.Errorf("returned balance = %d, want 10000", acc.BalanceCents)
	}

	got, err := b.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.BalanceCents != 10_000 {
		t.Errorf("stored balance = %d, want 10000", got.BalanceCents)
	}

	history, err := b.History(ctx, acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].AmountCents != 10_000 || history[0].TransferID != nil {
		t.Errorf("history = %+v, want one opening entry of 10000", history)
	}
	assertLedgerMatchesBalances(t)
}

func TestOpenAccountWithZeroBalance(t *testing.T) {
	b := newBank(t)
	ctx := context.Background()

	acc, err := b.OpenAccount(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	history, err := b.History(ctx, acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 0 {
		t.Errorf("history = %+v, want empty", history)
	}
	assertLedgerMatchesBalances(t)
}

func TestOpenAccountRejectsNegativeBalance(t *testing.T) {
	b := newBank(t)

	_, err := b.OpenAccount(context.Background(), -1)
	if !errors.Is(err, money.ErrInvalidAmount) {
		t.Fatalf("err = %v, want ErrInvalidAmount", err)
	}
	var n int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM accounts`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d accounts created, want 0", n)
	}
}

func TestUnknownAccount(t *testing.T) {
	b := newBank(t)
	ctx := context.Background()

	if _, err := b.GetAccount(ctx, 999); !errors.Is(err, ErrAccountNotFound) {
		t.Errorf("GetAccount err = %v, want ErrAccountNotFound", err)
	}
	if _, err := b.History(ctx, 999); !errors.Is(err, ErrAccountNotFound) {
		t.Errorf("History err = %v, want ErrAccountNotFound", err)
	}
}
