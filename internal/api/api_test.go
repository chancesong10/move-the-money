package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chancesong10/move-the-money/internal/bank"
	"github.com/chancesong10/move-the-money/internal/testdb"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	pool, err := testdb.Setup(context.Background(), "api")
	if err != nil {
		fmt.Fprintln(os.Stderr, "test database setup failed:", err)
		os.Exit(1)
	}
	testPool = pool
	code := m.Run()
	pool.Close()
	os.Exit(code)
}

func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	testdb.Reset(t, testPool)
	srv := httptest.NewServer(New(bank.New(testPool), slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(srv.Close)
	return srv
}

type response struct {
	status int
	header http.Header
	body   []byte
}

// send makes a request; key, if non-empty, goes in the Idempotency-Key header.
// It reports failures with t.Errorf (not Fatal) so it is safe in goroutines.
func send(t *testing.T, srv *httptest.Server, method, path, body, key string) response {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Errorf("build request: %v", err)
		return response{}
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Errorf("%s %s: %v", method, path, err)
		return response{}
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return response{res.StatusCode, res.Header, b}
}

func decode[T any](t *testing.T, r response) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(r.body, &v); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
	return v
}

func wantStatus(t *testing.T, r response, want int) {
	t.Helper()
	if r.status != want {
		t.Fatalf("status = %d, want %d; body: %s", r.status, want, r.body)
	}
}

func openAccount(t *testing.T, srv *httptest.Server, opening string) int64 {
	t.Helper()
	r := send(t, srv, "POST", "/accounts", `{"opening_balance": "`+opening+`"}`, "")
	wantStatus(t, r, http.StatusCreated)
	return decode[accountResponse](t, r).ID
}

func balance(t *testing.T, srv *httptest.Server, id int64) string {
	t.Helper()
	r := send(t, srv, "GET", fmt.Sprintf("/accounts/%d", id), "", "")
	wantStatus(t, r, http.StatusOK)
	return decode[map[string]any](t, r)["balance"].(string)
}

func transferBody(from, to int64, amount string) string {
	return fmt.Sprintf(`{"from_account_id": %d, "to_account_id": %d, "amount": "%s"}`, from, to, amount)
}

// --- tests ---

func TestEndToEnd(t *testing.T) {
	srv := newServer(t)
	alice := openAccount(t, srv, "100.00")
	bob := openAccount(t, srv, "0")

	r := send(t, srv, "POST", "/transfers", transferBody(alice, bob, "25.29"), "pay-1")
	wantStatus(t, r, http.StatusCreated)
	tr := decode[map[string]any](t, r)
	if tr["amount"] != "25.29" || tr["status"] != "completed" {
		t.Errorf("transfer = %s", r.body)
	}

	if got := balance(t, srv, alice); got != "74.71" {
		t.Errorf("alice = %s, want 74.71", got)
	}
	if got := balance(t, srv, bob); got != "25.29" {
		t.Errorf("bob = %s, want 25.29", got)
	}

	r = send(t, srv, "GET", fmt.Sprintf("/accounts/%d/transactions", alice), "", "")
	wantStatus(t, r, http.StatusOK)
	// Amounts decoded as plain strings: money.Amount deliberately refuses
	// negative input (nobody may *send* a negative amount), so it can't read
	// back the debits it writes. This also checks they're JSON strings.
	hist := decode[struct {
		Transactions []struct {
			Type                  string `json:"type"`
			Amount                string `json:"amount"`
			CounterpartyAccountID *int64 `json:"counterparty_account_id"`
		} `json:"transactions"`
	}](t, r)
	if len(hist.Transactions) != 2 {
		t.Fatalf("history = %s, want 2 entries", r.body)
	}
	open, out := hist.Transactions[0], hist.Transactions[1]
	if open.Type != "opening_balance" || open.Amount != "100.00" || open.CounterpartyAccountID != nil {
		t.Errorf("opening entry = %+v", open)
	}
	if out.Type != "transfer_out" || out.Amount != "-25.29" || out.CounterpartyAccountID == nil || *out.CounterpartyAccountID != bob {
		t.Errorf("transfer entry = %+v, want transfer_out of -25.29 to bob", out)
	}
}

func TestDoubleSubmitOverHTTP(t *testing.T) {
	srv := newServer(t)
	alice := openAccount(t, srv, "100.00")
	bob := openAccount(t, srv, "0")
	body := transferBody(alice, bob, "40.00")

	first := send(t, srv, "POST", "/transfers", body, "double-click")
	wantStatus(t, first, http.StatusCreated)
	second := send(t, srv, "POST", "/transfers", body, "double-click")
	wantStatus(t, second, http.StatusOK)

	if second.header.Get("Idempotent-Replayed") != "true" {
		t.Error("replay is missing the Idempotent-Replayed header")
	}
	if first.header.Get("Idempotent-Replayed") != "" {
		t.Error("first submission should not be marked as replayed")
	}
	if a, b := decode[transferResponse](t, first).ID, decode[transferResponse](t, second).ID; a != b {
		t.Errorf("replay returned transfer %d, want %d", b, a)
	}
	if got := balance(t, srv, alice); got != "60.00" {
		t.Errorf("alice = %s, want 60.00 (debited once)", got)
	}
}

func TestInsufficientFundsOverHTTP(t *testing.T) {
	srv := newServer(t)
	alice := openAccount(t, srv, "10.00")
	bob := openAccount(t, srv, "0")
	body := transferBody(alice, bob, "50.00")

	r := send(t, srv, "POST", "/transfers", body, "too-much")
	wantStatus(t, r, http.StatusUnprocessableEntity)
	e := decode[errorResponse](t, r)
	if e.Error != "insufficient_funds" || e.Transfer == nil || e.Transfer.Status != "rejected" {
		t.Errorf("body = %s, want insufficient_funds with the rejected transfer", r.body)
	}

	// Retrying the same key replays the rejection, even after money arrives.
	carol := openAccount(t, srv, "100.00")
	wantStatus(t, send(t, srv, "POST", "/transfers", transferBody(carol, alice, "90.00"), "top-up"), http.StatusCreated)
	retry := send(t, srv, "POST", "/transfers", body, "too-much")
	wantStatus(t, retry, http.StatusUnprocessableEntity)
	if retry.header.Get("Idempotent-Replayed") != "true" {
		t.Error("replayed rejection is missing the Idempotent-Replayed header")
	}
	if got := balance(t, srv, alice); got != "100.00" {
		t.Errorf("alice = %s, want 100.00 (stale retry must not move money)", got)
	}
}

func TestBadRequests(t *testing.T) {
	srv := newServer(t)
	alice := openAccount(t, srv, "100.00")
	bob := openAccount(t, srv, "0")
	wantStatus(t, send(t, srv, "POST", "/transfers", transferBody(alice, bob, "1.00"), "used"), http.StatusCreated)

	cases := []struct {
		name, method, path, body, key string
		wantStatus                    int
		wantCode                      string
	}{
		{"typo in field name", "POST", "/accounts", `{"opening_balnce": "500.00"}`, "", 400, "invalid_request"},
		{"missing opening balance", "POST", "/accounts", `{}`, "", 400, "invalid_request"},
		{"negative opening balance", "POST", "/accounts", `{"opening_balance": "-5.00"}`, "", 400, "invalid_request"},
		{"three decimal places", "POST", "/accounts", `{"opening_balance": "1.001"}`, "", 400, "invalid_request"},
		{"float artefact", "POST", "/accounts", `{"opening_balance": 0.30000000000000004}`, "", 400, "invalid_request"},
		{"malformed JSON", "POST", "/accounts", `{"opening_balance": `, "", 400, "invalid_request"},
		{"two JSON objects", "POST", "/accounts", `{"opening_balance": "1"}{"opening_balance": "2"}`, "", 400, "invalid_request"},
		{"missing idempotency key", "POST", "/transfers", transferBody(alice, bob, "1.00"), "", 400, "invalid_idempotency_key"},
		{"key too long", "POST", "/transfers", transferBody(alice, bob, "1.00"), strings.Repeat("k", 256), 400, "invalid_idempotency_key"},
		{"missing amount", "POST", "/transfers", fmt.Sprintf(`{"from_account_id": %d, "to_account_id": %d}`, alice, bob), "k1", 400, "invalid_request"},
		{"fractional account id", "POST", "/transfers", `{"from_account_id": 1.5, "to_account_id": 2, "amount": "1"}`, "k2", 400, "invalid_request"},
		{"zero amount", "POST", "/transfers", transferBody(alice, bob, "0.00"), "k3", 400, "invalid_amount"},
		{"same account", "POST", "/transfers", transferBody(alice, alice, "1.00"), "k4", 400, "same_account"},
		{"unknown account", "POST", "/transfers", transferBody(alice, 999, "1.00"), "k5", 404, "account_not_found"},
		{"key reused, different amount", "POST", "/transfers", transferBody(alice, bob, "2.00"), "used", 409, "idempotency_key_reused"},
		{"non-numeric account id", "GET", "/accounts/abc", "", "", 400, "invalid_account_id"},
		{"unknown account balance", "GET", "/accounts/999", "", "", 404, "account_not_found"},
		{"unknown account history", "GET", "/accounts/999/transactions", "", "", 404, "account_not_found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := send(t, srv, tc.method, tc.path, tc.body, tc.key)
			wantStatus(t, r, tc.wantStatus)
			if got := decode[errorResponse](t, r).Error; got != tc.wantCode {
				t.Errorf("error code = %q, want %q; body: %s", got, tc.wantCode, r.body)
			}
		})
	}

	if got := balance(t, srv, alice); got != "99.00" {
		t.Errorf("alice = %s, want 99.00 (no bad request should move money)", got)
	}
}

// Rule 1 end to end: the bank tests prove the locking; this proves the HTTP
// layer doesn't undermine it (no shared state, no mis-mapped errors).
func TestConcurrentTransfersOverHTTP(t *testing.T) {
	const goroutines = 20
	srv := newServer(t)
	alice := openAccount(t, srv, "100.00")
	bob := openAccount(t, srv, "0")

	statuses := make([]int, goroutines)
	start := make(chan struct{})
	var ready, done sync.WaitGroup
	ready.Add(goroutines)
	for i := range goroutines {
		done.Go(func() {
			ready.Done()
			<-start
			r := send(t, srv, "POST", "/transfers", transferBody(alice, bob, "10.00"), fmt.Sprintf("k%d", i))
			statuses[i] = r.status
		})
	}
	ready.Wait()
	close(start)
	done.Wait()

	counts := map[int]int{}
	for _, s := range statuses {
		counts[s]++
	}
	if counts[http.StatusCreated] != 10 || counts[http.StatusUnprocessableEntity] != 10 {
		t.Errorf("status counts = %v, want 10 x 201 and 10 x 422", counts)
	}
	if got := balance(t, srv, alice); got != "0.00" {
		t.Errorf("alice = %s, want 0.00", got)
	}
	if got := balance(t, srv, bob); got != "100.00" {
		t.Errorf("bob = %s, want 100.00", got)
	}
}
