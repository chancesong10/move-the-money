// Package bank holds the accounts and the rules for moving money between them.
// All amounts are integer cents.
package bank

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chancesong10/move-the-money/internal/money"
)

var ErrAccountNotFound = errors.New("account not found")

type Account struct {
	ID           int64
	BalanceCents int64
	CreatedAt    time.Time
}

// Entry is one line of an account's history. Negative = money out, positive = in.
type Entry struct {
	ID                    int64
	TransferID            *int64 // nil for the opening-balance entry
	CounterpartyAccountID *int64 // the other account in the transfer; nil for opening balance
	AmountCents           int64
	CreatedAt             time.Time
}

type Bank struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Bank {
	return &Bank{pool: pool}
}

// OpenAccount creates an account and records its opening balance in the
// ledger, in one transaction, so balance = SUM(ledger entries) holds from the
// start. A zero opening balance writes no entry (the sum of nothing is 0).
func (b *Bank) OpenAccount(ctx context.Context, openingCents int64) (Account, error) {
	if openingCents < 0 {
		return Account{}, fmt.Errorf("%w: opening balance cannot be negative", money.ErrInvalidAmount)
	}

	var acc Account
	err := pgx.BeginFunc(ctx, b.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`INSERT INTO accounts (balance_cents) VALUES ($1)
			 RETURNING id, balance_cents, created_at`,
			openingCents,
		).Scan(&acc.ID, &acc.BalanceCents, &acc.CreatedAt)
		if err != nil {
			return err
		}
		if openingCents == 0 {
			return nil
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO ledger_entries (account_id, amount_cents) VALUES ($1, $2)`,
			acc.ID, openingCents,
		)
		return err
	})
	if err != nil {
		return Account{}, fmt.Errorf("open account: %w", err)
	}
	return acc, nil
}

func (b *Bank) GetAccount(ctx context.Context, id int64) (Account, error) {
	var acc Account
	err := b.pool.QueryRow(ctx,
		`SELECT id, balance_cents, created_at FROM accounts WHERE id = $1`, id,
	).Scan(&acc.ID, &acc.BalanceCents, &acc.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrAccountNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("get account: %w", err)
	}
	return acc, nil
}

// History returns an account's ledger entries, oldest first.
// No pagination: fine at this scale, the first thing to add for real use.
func (b *Bank) History(ctx context.Context, accountID int64) ([]Entry, error) {
	// Distinguish "no such account" from "account with no entries yet".
	if _, err := b.GetAccount(ctx, accountID); err != nil {
		return nil, err
	}

	// The counterparty is whichever side of the transfer isn't this account.
	// Opening-balance entries have no transfer, so the join yields NULL.
	rows, err := b.pool.Query(ctx,
		`SELECT e.id, e.transfer_id,
		        CASE WHEN t.from_account_id = e.account_id THEN t.to_account_id
		             ELSE t.from_account_id END,
		        e.amount_cents, e.created_at
		 FROM ledger_entries e
		 LEFT JOIN transfers t ON t.id = e.transfer_id
		 WHERE e.account_id = $1
		 ORDER BY e.id`,
		accountID,
	)
	if err != nil {
		return nil, fmt.Errorf("history: %w", err)
	}
	entries, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Entry, error) {
		var e Entry
		err := row.Scan(&e.ID, &e.TransferID, &e.CounterpartyAccountID, &e.AmountCents, &e.CreatedAt)
		return e, err
	})
	if err != nil {
		return nil, fmt.Errorf("history: %w", err)
	}
	return entries, nil
}
