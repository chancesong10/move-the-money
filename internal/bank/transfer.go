package bank

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/chancesong10/move-the-money/internal/money"
)

var (
	ErrInsufficientFunds   = errors.New("insufficient funds")
	ErrSameAccount         = errors.New("cannot transfer to the same account")
	ErrInvalidKey          = errors.New("idempotency key must be 1-255 characters")
	ErrIdempotencyConflict = errors.New("idempotency key already used for a different transfer")
	ErrBalanceOverflow     = errors.New("receiver's balance would exceed the maximum")
)

const maxKeyLength = 255 // matches the CHECK in migrations/002

// Transfer statuses and failure reasons, as stored in the transfers table.
const (
	StatusCompleted = "completed"
	StatusRejected  = "rejected"

	reasonInsufficientFunds = "insufficient_funds"
)

type TransferRequest struct {
	IdempotencyKey string
	FromAccountID  int64
	ToAccountID    int64
	AmountCents    int64
}

type Transfer struct {
	ID             int64
	IdempotencyKey string
	FromAccountID  int64
	ToAccountID    int64
	AmountCents    int64
	Status         string // StatusCompleted or StatusRejected
	FailureReason  string // empty unless rejected
	CreatedAt      time.Time
}

// Transfer moves money between two accounts in a single database transaction:
// either all of the money movement happens, or none of it does.
//
// If the sender can't cover the amount, the attempt is still recorded, as
// rejected, and Transfer returns that record together with
// ErrInsufficientFunds. Callers must check the error before treating the
// transfer as completed.
//
// If the idempotency key was already used for an identical request, nothing
// new happens: the original outcome is returned with replayed = true. That
// includes a rejection, so a stale retry can't succeed later just because
// money has since arrived. If the key was used for a different request,
// ErrIdempotencyConflict is returned.
//
// Invalid requests (bad amount, bad key, same account, unknown account) are
// not recorded: they never became a transfer attempt.
func (b *Bank) Transfer(ctx context.Context, req TransferRequest) (t Transfer, replayed bool, err error) {
	if err := validateTransfer(req); err != nil {
		return Transfer{}, false, err
	}

	err = pgx.BeginFunc(ctx, b.pool, func(tx pgx.Tx) error {
		// Step 1: claim the idempotency key by inserting the transfer row.
		// The UNIQUE constraint makes this atomic: if two requests with the
		// same key race, Postgres makes the second INSERT wait until the first
		// transaction finishes, then either skips it (first committed) or lets
		// it through (first rolled back). There is no check-then-insert gap.
		//
		// The row starts as 'completed' and is flipped to 'rejected' below if
		// the balance check fails. Nobody outside this transaction can see the
		// row until it commits, so nobody ever sees the wrong status.
		t = Transfer{
			IdempotencyKey: req.IdempotencyKey,
			FromAccountID:  req.FromAccountID,
			ToAccountID:    req.ToAccountID,
			AmountCents:    req.AmountCents,
			Status:         StatusCompleted,
		}
		err := tx.QueryRow(ctx,
			`INSERT INTO transfers (idempotency_key, from_account_id, to_account_id, amount_cents, status)
			 VALUES ($1, $2, $3, $4, $5)
			 ON CONFLICT (idempotency_key) DO NOTHING
			 RETURNING id, created_at`,
			req.IdempotencyKey, req.FromAccountID, req.ToAccountID, req.AmountCents, StatusCompleted,
		).Scan(&t.ID, &t.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			// Key already used: replay the original instead of moving money again.
			existing, err := transferByKey(ctx, tx, req.IdempotencyKey)
			if err != nil {
				return err
			}
			if existing.FromAccountID != req.FromAccountID ||
				existing.ToAccountID != req.ToAccountID ||
				existing.AmountCents != req.AmountCents {
				return ErrIdempotencyConflict
			}
			t, replayed = existing, true
			return nil
		}
		if isForeignKeyViolation(err) {
			return ErrAccountNotFound
		}
		if err != nil {
			return err
		}

		// Step 2: lock both accounts, lowest ID first. Every transfer takes
		// locks in the same order, so two transfers can never each hold one
		// lock while waiting for the other (A->B and B->A would deadlock).
		//
		// FOR NO KEY UPDATE, not FOR UPDATE: the INSERT above already took a
		// FOR KEY SHARE lock on both accounts (that's how Postgres checks the
		// foreign keys). FOR UPDATE conflicts with other transactions' KEY
		// SHARE locks, so two concurrent transfers deadlock on each other.
		// NO KEY UPDATE still blocks other transfers on these rows, which is
		// what rule 1 needs, but doesn't conflict with the FK check.
		first, second := req.FromAccountID, req.ToAccountID
		if first > second {
			first, second = second, first
		}
		balances := make(map[int64]int64, 2)
		for _, id := range []int64{first, second} {
			var balance int64
			err := tx.QueryRow(ctx,
				`SELECT balance_cents FROM accounts WHERE id = $1 FOR NO KEY UPDATE`, id,
			).Scan(&balance)
			if err != nil {
				return err
			}
			balances[id] = balance
		}

		// Step 3: check the balance. Safe because we hold the lock: nobody else
		// can change this balance until we commit or roll back.
		//
		// On failure, record the rejection and COMMIT (return nil) rather than
		// rolling back, so the key now permanently maps to this outcome. No
		// money has moved yet, so there is nothing to undo.
		if balances[req.FromAccountID] < req.AmountCents {
			t.Status, t.FailureReason = StatusRejected, reasonInsufficientFunds
			_, err := tx.Exec(ctx,
				`UPDATE transfers SET status = $1, failure_reason = $2 WHERE id = $3`,
				t.Status, t.FailureReason, t.ID,
			)
			return err
		}

		// Step 4: move the money and record it. If any statement fails (e.g.
		// the receiver's balance would overflow BIGINT), the whole transaction
		// rolls back, including the debit and the transfer row.
		if _, err := tx.Exec(ctx,
			`UPDATE accounts SET balance_cents = balance_cents - $1 WHERE id = $2`,
			req.AmountCents, req.FromAccountID,
		); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE accounts SET balance_cents = balance_cents + $1 WHERE id = $2`,
			req.AmountCents, req.ToAccountID,
		); err != nil {
			// Unlike insufficient funds, this isn't recorded as a rejection:
			// the debit above has already run, so the whole transaction must
			// roll back. Only reachable near $92 quadrillion.
			if isOutOfRange(err) {
				return ErrBalanceOverflow
			}
			return err
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO ledger_entries (account_id, transfer_id, amount_cents)
			 VALUES ($1, $3, $4), ($2, $3, $5)`,
			req.FromAccountID, req.ToAccountID, t.ID, -req.AmountCents, req.AmountCents,
		)
		return err
	})
	if err != nil {
		return Transfer{}, false, fmt.Errorf("transfer: %w", err)
	}
	if t.Status == StatusRejected {
		return t, replayed, rejectionError(t.FailureReason)
	}
	return t, replayed, nil
}

// rejectionError maps a stored failure_reason back to the error the original
// request returned, so a replay fails the same way.
func rejectionError(reason string) error {
	switch reason {
	case reasonInsufficientFunds:
		return fmt.Errorf("transfer: %w", ErrInsufficientFunds)
	default:
		return fmt.Errorf("transfer: rejected (%s)", reason)
	}
}

func validateTransfer(req TransferRequest) error {
	if n := utf8.RuneCountInString(req.IdempotencyKey); n == 0 || n > maxKeyLength {
		return ErrInvalidKey
	}
	if req.AmountCents <= 0 {
		return fmt.Errorf("%w: transfer amount must be positive", money.ErrInvalidAmount)
	}
	if req.FromAccountID == req.ToAccountID {
		return ErrSameAccount
	}
	return nil
}

func transferByKey(ctx context.Context, tx pgx.Tx, key string) (Transfer, error) {
	var t Transfer
	err := tx.QueryRow(ctx,
		`SELECT id, idempotency_key, from_account_id, to_account_id, amount_cents,
		        status, COALESCE(failure_reason, ''), created_at
		 FROM transfers WHERE idempotency_key = $1`, key,
	).Scan(&t.ID, &t.IdempotencyKey, &t.FromAccountID, &t.ToAccountID, &t.AmountCents,
		&t.Status, &t.FailureReason, &t.CreatedAt)
	return t, err
}

func isForeignKeyViolation(err error) bool {
	return hasPgCode(err, "23503")
}

// isOutOfRange reports a BIGINT overflow (numeric_value_out_of_range).
func isOutOfRange(err error) bool {
	return hasPgCode(err, "22003")
}

func hasPgCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}
