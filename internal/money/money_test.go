package money

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
)

func TestParseCents(t *testing.T) {
	valid := map[string]int64{
		"0":                    0,
		"0.00":                 0,
		"1":                    100,
		"1.5":                  150,
		"1.05":                 105,
		"0.01":                 1,
		"0.1":                  10,
		"007.50":               750,
		"92233720368547758.07": math.MaxInt64,
	}
	for in, want := range valid {
		got, err := ParseCents(in)
		if err != nil {
			t.Errorf("ParseCents(%q) error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseCents(%q) = %d, want %d", in, got, want)
		}
	}

	invalid := []string{
		"", ".", ".5", "5.", "-1", "+1", "1.234", "1.001", "1e2", "1E2",
		" 1", "1 ", "1,00", "0x10", "abc", "1.2.3", "NaN", "Inf",
		"92233720368547758.08",  // MaxInt64 + 1 cent
		"999999999999999999999", // overflows int64 before scaling
	}
	for _, in := range invalid {
		got, err := ParseCents(in)
		if !errors.Is(err, ErrInvalidAmount) {
			t.Errorf("ParseCents(%q) = %d, %v; want ErrInvalidAmount", in, got, err)
		}
	}
}

func TestFormatCents(t *testing.T) {
	cases := map[int64]string{
		0:             "0.00",
		1:             "0.01",
		10:            "0.10",
		105:           "1.05",
		123456:        "1234.56",
		-1:            "-0.01",
		-150:          "-1.50",
		math.MaxInt64: "92233720368547758.07",
		math.MinInt64: "-92233720368547758.08",
	}
	for in, want := range cases {
		if got := FormatCents(in); got != want {
			t.Errorf("FormatCents(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatParseRoundTrip(t *testing.T) {
	for _, c := range []int64{0, 1, 9, 10, 99, 100, 101, 12345, math.MaxInt64} {
		got, err := ParseCents(FormatCents(c))
		if err != nil || got != c {
			t.Errorf("round trip %d -> %q -> %d, %v", c, FormatCents(c), got, err)
		}
	}
}

func TestAmountJSON(t *testing.T) {
	valid := map[string]Amount{
		`{"a": "12.34"}`: 1234,
		`{"a": 12.34}`:   1234, // bare number: parsed as text, never as float64
		`{"a": "0.1"}`:   10,
		`{"a": 0.1}`:     10,
		`{"a": 5}`:       500,
	}
	for in, want := range valid {
		var v struct{ A Amount }
		if err := json.Unmarshal([]byte(in), &v); err != nil {
			t.Errorf("Unmarshal(%s) error: %v", in, err)
			continue
		}
		if v.A != want {
			t.Errorf("Unmarshal(%s) = %d, want %d", in, v.A, want)
		}
	}

	invalid := []string{
		`{"a": null}`,
		`{"a": 0.30000000000000004}`, // what 0.1 + 0.2 looks like from a JS client
		`{"a": 1e2}`,
		`{"a": -5}`,
		`{"a": "1.234"}`,
		`{"a": true}`,
		`{"a": ""}`,
	}
	for _, in := range invalid {
		var v struct{ A Amount }
		if err := json.Unmarshal([]byte(in), &v); err == nil {
			t.Errorf("Unmarshal(%s) = %d, want error", in, v.A)
		}
	}

	out, err := json.Marshal(struct{ A Amount }{1234})
	if err != nil || string(out) != `{"A":"12.34"}` {
		t.Errorf("Marshal = %s, %v; want {\"A\":\"12.34\"}", out, err)
	}
}
