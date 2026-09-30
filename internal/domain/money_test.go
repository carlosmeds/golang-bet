package domain

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
)

func TestParseMoneyValid(t *testing.T) {
	cases := map[string]int64{
		"0": 0, "0.00": 0, "0.0": 0, "10": 1000, "10.5": 1050, "10.50": 1050, "0.01": 1, "007.10": 710,
		"92233720368547758.07": math.MaxInt64, "92233720368547757.99": math.MaxInt64 - 8,
	}
	for in, want := range cases {
		m, err := ParseMoney(in, "BRL")
		if err != nil {
			t.Errorf("ParseMoney(%q): %v", in, err)
			continue
		}
		if m.Minor() != want {
			t.Errorf("ParseMoney(%q) = %d, want %d", in, m.Minor(), want)
		}
	}
}

func TestParseMoneyInvalid(t *testing.T) {
	cases := map[string]string{
		"": CodeInvalidAmount, "-1": CodeInvalidAmount, "-0": CodeInvalidAmount, "+1": CodeInvalidAmount,
		"1e3": CodeInvalidAmount, "1E3": CodeInvalidAmount, "NaN": CodeInvalidAmount, "Infinity": CodeInvalidAmount,
		"inf": CodeInvalidAmount, ".5": CodeInvalidAmount, "5.": CodeInvalidAmount, "1.234": CodeInvalidAmount,
		"1.001": CodeInvalidAmount, "1,00": CodeInvalidAmount, " 1": CodeInvalidAmount, "1 ": CodeInvalidAmount,
		"1_0": CodeInvalidAmount, "١٢": CodeInvalidAmount, "1.2.3": CodeInvalidAmount, "0x10": CodeInvalidAmount,
		".": CodeInvalidAmount, "--1": CodeInvalidAmount,
		"92233720368547758.08": CodeAmountOverflow, "92233720368547759": CodeAmountOverflow,
		"99999999999999999999999": CodeAmountOverflow, "18446744073709551616": CodeAmountOverflow,
	}
	for in, code := range cases {
		_, err := ParseMoney(in, "BRL")
		if err == nil {
			t.Errorf("ParseMoney(%q) accepted", in)
			continue
		}
		var de *Error
		if !errors.As(err, &de) || de.Code != code {
			t.Errorf("ParseMoney(%q) error = %v, want code %s", in, err, code)
		}
	}
}

func TestCurrency(t *testing.T) {
	if c, err := ParseCurrency("brl"); err != nil || c != "BRL" {
		t.Fatalf("ParseCurrency(brl) = %q, %v", c, err)
	}
	for _, bad := range []string{"", "BR", "BRLL", "B1L", "BR ", " BR", "é€£"} {
		if _, err := ParseCurrency(bad); !errors.Is(err, ErrInvalidCurrency) {
			t.Errorf("ParseCurrency(%q) = %v, want ErrInvalidCurrency", bad, err)
		}
	}
	if _, err := ParseMoney("1", "brl"); !errors.Is(err, ErrInvalidCurrency) {
		t.Errorf("non-canonical currency accepted by ParseMoney: %v", err)
	}
	if _, err := NewMoney(1, "eur"); !errors.Is(err, ErrInvalidCurrency) {
		t.Errorf("non-canonical currency accepted by NewMoney: %v", err)
	}
}

func TestMoneyAmountFormat(t *testing.T) {
	cases := []struct {
		minor int64
		want  string
	}{
		{0, "0.00"}, {1, "0.01"}, {100, "1.00"}, {1050, "10.50"}, {-5, "-0.05"}, {-12345, "-123.45"},
		{math.MaxInt64, "92233720368547758.07"}, {math.MinInt64, "-92233720368547758.08"},
	}
	for _, c := range cases {
		m, _ := NewMoney(c.minor, "USD")
		if got := m.Amount(); got != c.want {
			t.Errorf("Amount(%d) = %s, want %s", c.minor, got, c.want)
		}
	}
	m, _ := NewMoney(1050, "USD")
	if m.String() != "10.50 USD" {
		t.Errorf("String = %s", m)
	}
}

func TestMoneyRoundTripProperty(t *testing.T) {
	for _, minor := range []int64{0, 1, 9, 10, 99, 100, 101, 12345678901, math.MaxInt64, math.MaxInt64 - 1} {
		m, _ := NewMoney(minor, "BRL")
		back, err := ParseMoney(m.Amount(), "BRL")
		if err != nil || back != m {
			t.Errorf("round trip of %d: %v, %v", minor, back, err)
		}
	}
}

func TestMoneyArithmetic(t *testing.T) {
	a, b := brl(t, "10.00"), brl(t, "2.50")
	sum, err := a.Add(b)
	if err != nil || sum.Amount() != "12.50" {
		t.Fatalf("Add = %v, %v", sum, err)
	}
	diff, err := b.Sub(a)
	if err != nil || diff.Amount() != "-7.50" || !diff.IsNegative() {
		t.Fatalf("signed Sub = %v, %v", diff, err)
	}
	neg, err := a.Neg()
	if err != nil || neg.Amount() != "-10.00" {
		t.Fatalf("Neg = %v, %v", neg, err)
	}
	if c, _ := a.Cmp(b); c != 1 {
		t.Errorf("Cmp = %d", c)
	}
	if c, _ := b.Cmp(a); c != -1 {
		t.Errorf("Cmp = %d", c)
	}
	if c, _ := a.Cmp(a); c != 0 {
		t.Errorf("Cmp = %d", c)
	}
	// Immutability: operands unchanged.
	if a.Amount() != "10.00" || b.Amount() != "2.50" {
		t.Errorf("operands mutated: %s %s", a, b)
	}
}

func TestMoneyOverflow(t *testing.T) {
	max, _ := NewMoney(math.MaxInt64, "BRL")
	min, _ := NewMoney(math.MinInt64, "BRL")
	one, _ := NewMoney(1, "BRL")
	negOne, _ := NewMoney(-1, "BRL")
	zero, _ := Zero("BRL")

	checks := []struct {
		name string
		fn   func() (Money, error)
		ok   bool
	}{
		{"max+1", func() (Money, error) { return max.Add(one) }, false},
		{"max+0", func() (Money, error) { return max.Add(zero) }, true},
		{"min+-1", func() (Money, error) { return min.Add(negOne) }, false},
		{"min+1", func() (Money, error) { return min.Add(one) }, true},
		{"min-1", func() (Money, error) { return min.Sub(one) }, false},
		{"max- -1", func() (Money, error) { return max.Sub(negOne) }, false},
		{"max-1", func() (Money, error) { return max.Sub(one) }, true},
		{"0-min", func() (Money, error) { return zero.Sub(min) }, false},
		{"-min", func() (Money, error) { return min.Neg() }, false},
		{"-max", func() (Money, error) { return max.Neg() }, true},
	}
	for _, c := range checks {
		_, err := c.fn()
		if c.ok && err != nil {
			t.Errorf("%s: unexpected error %v", c.name, err)
		}
		if !c.ok && !errors.Is(err, ErrAmountOverflow) {
			t.Errorf("%s: want overflow, got %v", c.name, err)
		}
	}
}

func TestMoneyCurrencyMismatchAndUninitialized(t *testing.T) {
	a, _ := NewMoney(100, "BRL")
	b, _ := NewMoney(100, "USD")
	var z Money
	for name, fn := range map[string]func() error{
		"add": func() error { _, e := a.Add(b); return e },
		"sub": func() error { _, e := a.Sub(b); return e },
		"cmp": func() error { _, e := a.Cmp(b); return e },
	} {
		if err := fn(); !errors.Is(err, ErrCurrencyMismatch) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, fn := range map[string]func() error{
		"add":  func() error { _, e := z.Add(a); return e },
		"add2": func() error { _, e := a.Add(z); return e },
		"sub":  func() error { _, e := z.Sub(z); return e },
		"neg":  func() error { _, e := z.Neg(); return e },
		"cmp":  func() error { _, e := z.Cmp(a); return e },
		"json": func() error { _, e := json.Marshal(z); return e },
	} {
		if err := fn(); err == nil {
			t.Errorf("%s accepted an uninitialized Money", name)
		}
	}
	if z.IsInitialized() || z.IsZero() || z.IsPositive() || z.IsNegative() || z.Amount() != "" {
		t.Error("uninitialized Money must not report a value")
	}
	if !a.Equal(a) || a.Equal(b) {
		t.Error("Equal")
	}
}

func TestMoneyJSON(t *testing.T) {
	m := brl(t, "10.5")
	out, err := json.Marshal(m)
	if err != nil || string(out) != `{"amount":"10.50","currency":"BRL"}` {
		t.Fatalf("Marshal = %s, %v", out, err)
	}
	var back Money
	if err := json.Unmarshal(out, &back); err != nil || back != m {
		t.Fatalf("Unmarshal = %v, %v", back, err)
	}
	if err := json.Unmarshal([]byte(`{"amount":"1","currency":"usd"}`), &back); err != nil || back.Currency() != "USD" {
		t.Errorf("currency not normalized: %v %v", back, err)
	}

	bad := []string{
		`{"amount":10.5,"currency":"BRL"}`, `{"amount":"-1.00","currency":"BRL"}`, `{"amount":"1e2","currency":"BRL"}`,
		`{"amount":"1.001","currency":"BRL"}`, `{"amount":"","currency":"BRL"}`, `{"amount":"NaN","currency":"BRL"}`,
		`{"amount":"1.00"}`, `{"currency":"BRL"}`, `{"amount":"1.00","currency":"BR"}`, `null`, `"10.00"`, `10`,
		`{"amount":"1.00","currency":"BRL","extra":1}`, `{"amount":"1.00","currency":"BRL"} {}`, `[]`,
	}
	for _, in := range bad {
		var m Money
		if err := json.Unmarshal([]byte(in), &m); err == nil {
			t.Errorf("Unmarshal(%s) accepted", in)
		}
	}
	// Signed internal values serialize but are not accepted back.
	neg, _ := NewMoney(-5, "BRL")
	out, err = json.Marshal(neg)
	if err != nil || string(out) != `{"amount":"-0.05","currency":"BRL"}` {
		t.Errorf("negative Marshal = %s, %v", out, err)
	}
}
