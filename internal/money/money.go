// Package money converts between decimal amounts ("12.34") and integer cents.
// It never uses floating point: parsing is done on the text itself.
package money

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

var ErrInvalidAmount = errors.New("invalid amount")

// ParseCents parses a non-negative decimal amount with at most two decimal
// places ("12", "12.3", "12.34") into cents. Signs, exponents, whitespace and
// a third decimal place are all rejected rather than rounded.
//
// Zero is accepted here (an account may open with 0); callers that need a
// strictly positive amount, like transfers, must check that themselves.
func ParseCents(s string) (int64, error) {
	whole, frac, hasDot := strings.Cut(s, ".")
	if !allDigits(whole) {
		return 0, fmt.Errorf("%w: %q", ErrInvalidAmount, s)
	}
	if hasDot && (!allDigits(frac) || len(frac) > 2) {
		return 0, fmt.Errorf("%w: %q (at most 2 decimal places)", ErrInvalidAmount, s)
	}
	for len(frac) < 2 {
		frac += "0"
	}

	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q (too large)", ErrInvalidAmount, s)
	}
	f, _ := strconv.ParseInt(frac, 10, 64) // two digits, cannot fail
	if w > (math.MaxInt64-f)/100 {
		return 0, fmt.Errorf("%w: %q (too large)", ErrInvalidAmount, s)
	}
	return w*100 + f, nil
}

// FormatCents renders cents as a decimal string with exactly two places.
// Negative values (ledger debits) get a leading "-".
func FormatCents(c int64) string {
	sign := ""
	u := uint64(c)
	if c < 0 {
		sign = "-"
		u = -u // two's complement negation; correct even for math.MinInt64
	}
	return fmt.Sprintf("%s%d.%02d", sign, u/100, u%100)
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Amount is an amount in cents that decodes from JSON without going through
// float64. It accepts a JSON string ("12.34") or a bare JSON number (12.34);
// either way the raw text is handed to ParseCents.
type Amount int64

func (a *Amount) UnmarshalJSON(b []byte) error {
	s := string(b)
	if s == "null" {
		return fmt.Errorf("%w: null", ErrInvalidAmount)
	}
	if strings.HasPrefix(s, `"`) {
		if err := json.Unmarshal(b, &s); err != nil {
			return fmt.Errorf("%w: %s", ErrInvalidAmount, b)
		}
	}
	c, err := ParseCents(s)
	if err != nil {
		return err
	}
	*a = Amount(c)
	return nil
}

// MarshalJSON always emits a string so clients never parse money as a float.
func (a Amount) MarshalJSON() ([]byte, error) {
	return json.Marshal(FormatCents(int64(a)))
}
