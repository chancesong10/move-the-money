// Package api exposes the bank over a small JSON HTTP API. It only translates
// between HTTP and the bank package; every money rule lives in bank.
package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/chancesong10/move-the-money/internal/bank"
	"github.com/chancesong10/move-the-money/internal/money"
)

const maxBodyBytes = 1 << 20 // 1 MB

type Server struct {
	bank *bank.Bank
	log  *slog.Logger
}

func New(b *bank.Bank, logger *slog.Logger) http.Handler {
	s := &Server{bank: b, log: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /accounts", s.openAccount)
	mux.HandleFunc("GET /accounts/{id}", s.getAccount)
	mux.HandleFunc("GET /accounts/{id}/transactions", s.getTransactions)
	mux.HandleFunc("POST /transfers", s.transfer)
	return mux
}

// --- request and response shapes ---
// Money is always money.Amount: parsed from text on the way in, written as a
// string on the way out, never a float. Pointers mark required fields, so a
// missing field is an error rather than silently zero.

type openAccountRequest struct {
	OpeningBalance *money.Amount `json:"opening_balance"`
}

type accountResponse struct {
	ID        int64        `json:"id"`
	Balance   money.Amount `json:"balance"`
	CreatedAt time.Time    `json:"created_at"`
}

type transferRequest struct {
	FromAccountID *int64        `json:"from_account_id"`
	ToAccountID   *int64        `json:"to_account_id"`
	Amount        *money.Amount `json:"amount"`
}

type transferResponse struct {
	ID             int64        `json:"id"`
	IdempotencyKey string       `json:"idempotency_key"`
	FromAccountID  int64        `json:"from_account_id"`
	ToAccountID    int64        `json:"to_account_id"`
	Amount         money.Amount `json:"amount"`
	Status         string       `json:"status"`
	FailureReason  string       `json:"failure_reason,omitempty"`
	CreatedAt      time.Time    `json:"created_at"`
}

type transactionResponse struct {
	ID                    int64        `json:"id"`
	Type                  string       `json:"type"` // opening_balance, transfer_in, transfer_out
	Amount                money.Amount `json:"amount"`
	TransferID            *int64       `json:"transfer_id"`
	CounterpartyAccountID *int64       `json:"counterparty_account_id"`
	CreatedAt             time.Time    `json:"created_at"`
}

type transactionsResponse struct {
	AccountID    int64                 `json:"account_id"`
	Transactions []transactionResponse `json:"transactions"`
}

type errorResponse struct {
	Error    string            `json:"error"` // stable, machine-readable code
	Message  string            `json:"message"`
	Transfer *transferResponse `json:"transfer,omitempty"` // set for recorded rejections
}

// --- handlers ---

func (s *Server) openAccount(w http.ResponseWriter, r *http.Request) {
	var req openAccountRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid_request", Message: err.Error()})
		return
	}
	if req.OpeningBalance == nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid_request", Message: "opening_balance is required"})
		return
	}

	acc, err := s.bank.OpenAccount(r.Context(), int64(*req.OpeningBalance))
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toAccountResponse(acc))
}

func (s *Server) getAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	acc, err := s.bank.GetAccount(r.Context(), id)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAccountResponse(acc))
}

func (s *Server) getTransactions(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	entries, err := s.bank.History(r.Context(), id)
	if err != nil {
		s.writeError(w, err)
		return
	}
	resp := transactionsResponse{AccountID: id, Transactions: make([]transactionResponse, 0, len(entries))}
	for _, e := range entries {
		typ := "transfer_in"
		switch {
		case e.TransferID == nil:
			typ = "opening_balance"
		case e.AmountCents < 0:
			typ = "transfer_out"
		}
		resp.Transactions = append(resp.Transactions, transactionResponse{
			ID:                    e.ID,
			Type:                  typ,
			Amount:                money.Amount(e.AmountCents),
			TransferID:            e.TransferID,
			CounterpartyAccountID: e.CounterpartyAccountID,
			CreatedAt:             e.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// transfer responds with:
//
//	201 the transfer completed
//	200 replay of a completed transfer (Idempotent-Replayed: true)
//	422 rejected for insufficient funds, with the recorded transfer in the
//	    body; a replay of that rejection gets the same 422 plus the header
func (s *Server) transfer(w http.ResponseWriter, r *http.Request) {
	var req transferRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid_request", Message: err.Error()})
		return
	}
	if req.FromAccountID == nil || req.ToAccountID == nil || req.Amount == nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{
			Error: "invalid_request", Message: "from_account_id, to_account_id and amount are required",
		})
		return
	}

	t, replayed, err := s.bank.Transfer(r.Context(), bank.TransferRequest{
		// A missing header reads as "", which the bank rejects as an invalid
		// key. Treating it as a real key would make every keyless request
		// collide with the first one.
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
		FromAccountID:  *req.FromAccountID,
		ToAccountID:    *req.ToAccountID,
		AmountCents:    int64(*req.Amount),
	})
	if replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	if err != nil {
		if errors.Is(err, bank.ErrInsufficientFunds) {
			tr := toTransferResponse(t)
			writeJSON(w, http.StatusUnprocessableEntity, errorResponse{
				Error: "insufficient_funds", Message: "insufficient funds", Transfer: &tr,
			})
			return
		}
		s.writeError(w, err)
		return
	}

	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, toTransferResponse(t))
}

// --- helpers ---

// writeError maps bank errors to HTTP statuses. Anything unrecognised is a
// 500: logged in full, but only a generic message goes to the client.
func (s *Server) writeError(w http.ResponseWriter, err error) {
	mappings := []struct {
		target error
		status int
		code   string
	}{
		{money.ErrInvalidAmount, http.StatusBadRequest, "invalid_amount"},
		{bank.ErrInvalidKey, http.StatusBadRequest, "invalid_idempotency_key"},
		{bank.ErrSameAccount, http.StatusBadRequest, "same_account"},
		{bank.ErrAccountNotFound, http.StatusNotFound, "account_not_found"},
		{bank.ErrIdempotencyConflict, http.StatusConflict, "idempotency_key_reused"},
		{bank.ErrBalanceOverflow, http.StatusUnprocessableEntity, "balance_limit_exceeded"},
	}
	for _, m := range mappings {
		if errors.Is(err, m.target) {
			writeJSON(w, m.status, errorResponse{Error: m.code, Message: m.target.Error()})
			return
		}
	}
	s.log.Error("internal error", "err", err)
	writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal_error", Message: "internal error"})
}

// decodeJSON reads exactly one JSON object, rejecting unknown fields (so a
// typo like "opening_balnce" fails loudly instead of defaulting to zero),
// oversized bodies, and trailing data.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain a single JSON object")
	}
	return nil
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid_account_id", Message: "account id must be a positive integer"})
		return 0, false
	}
	return id, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) // headers are sent; nothing useful to do on error
}

func toAccountResponse(a bank.Account) accountResponse {
	return accountResponse{ID: a.ID, Balance: money.Amount(a.BalanceCents), CreatedAt: a.CreatedAt}
}

func toTransferResponse(t bank.Transfer) transferResponse {
	return transferResponse{
		ID:             t.ID,
		IdempotencyKey: t.IdempotencyKey,
		FromAccountID:  t.FromAccountID,
		ToAccountID:    t.ToAccountID,
		Amount:         money.Amount(t.AmountCents),
		Status:         t.Status,
		FailureReason:  t.FailureReason,
		CreatedAt:      t.CreatedAt,
	}
}
