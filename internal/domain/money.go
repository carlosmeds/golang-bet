package domain

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
)

// Currency is an ISO 4217 alphabetic code in canonical uppercase form.
type Currency string

// ParseCurrency validates and normalizes a three-letter ISO 4217 code. ASCII
// letters are upper-cased; anything else (whitespace, digits, other lengths)
// is rejected. Membership in the ISO 4217 registry is not checked: the domain
// validates shape and leaves the list of accepted currencies to configuration.
func ParseCurrency(s string) (Currency, error) {
	if len(s) != 3 {
		return "", newInvalid(CodeInvalidCurrency, "currency must be a 3-letter ISO 4217 code")
	}
	var b [3]byte
	for i := 0; i < 3; i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
		case c >= 'a' && c <= 'z':
			c -= 'a' - 'A'
		default:
			return "", newInvalid(CodeInvalidCurrency, "currency must be a 3-letter ISO 4217 code")
		}
		b[i] = c
	}
	return Currency(b[:]), nil
}

// Valid reports whether c is in canonical form.
func (c Currency) Valid() bool {
	got, err := ParseCurrency(string(c))
	return err == nil && got == c
}

func (c Currency) String() string { return string(c) }

// moneyScale is the fixed number of decimal places (amounts are int64 cents).
const moneyScale = 2

// Money is an immutable amount in minor units (cents) with a currency. The
// zero value is uninitialized and every operation rejects it (REQ-012). Money
// never uses floating point. Values may be negative internally (differences,
// signed ledger sums); external input is validated non-negative by ParseMoney
// and Money.UnmarshalJSON.
type Money struct {
	minor    int64
	currency Currency
}

// NewMoney builds a Money from minor units. The amount may be negative.
func NewMoney(minor int64, currency Currency) (Money, error) {
	if !currency.Valid() {
		return Money{}, newInvalid(CodeInvalidCurrency, "invalid currency %q", string(currency))
	}
	return Money{minor: minor, currency: currency}, nil
}

// Zero returns a zero amount in currency.
func Zero(currency Currency) (Money, error) { return NewMoney(0, currency) }

// ParseMoney parses an external decimal string. It accepts digits with an
// optional fractional part of one or two digits ("10", "10.5", "10.50"); it
// rejects empty input, signs (so negatives), exponents, NaN/Infinity,
// separators, whitespace, more than two decimals and values beyond int64
// cents. Nothing is ever rounded.
func ParseMoney(amount string, currency Currency) (Money, error) {
	if !currency.Valid() {
		return Money{}, newInvalid(CodeInvalidCurrency, "invalid currency %q", string(currency))
	}
	minor, err := parseCents(amount)
	if err != nil {
		return Money{}, err
	}
	return Money{minor: minor, currency: currency}, nil
}

func parseCents(s string) (int64, error) {
	if s == "" {
		return 0, newInvalid(CodeInvalidAmount, "amount is empty")
	}
	dot := -1
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c == '.' && dot < 0:
			dot = i
		default:
			return 0, newInvalid(CodeInvalidAmount, "amount %q must be a non-negative decimal without sign, exponent or separators", s)
		}
	}
	whole, frac := s, ""
	if dot >= 0 {
		whole, frac = s[:dot], s[dot+1:]
	}
	if whole == "" {
		return 0, newInvalid(CodeInvalidAmount, "amount %q has no integer part", s)
	}
	if dot >= 0 && (frac == "" || len(frac) > moneyScale) {
		return 0, newInvalid(CodeInvalidAmount, "amount %q must have one or two decimal places", s)
	}
	var w uint64
	for i := 0; i < len(whole); i++ {
		d := uint64(whole[i] - '0')
		if w > (math.MaxUint64-d)/10 {
			return 0, newInvalid(CodeAmountOverflow, "amount %q exceeds the supported range", s)
		}
		w = w*10 + d
	}
	var f uint64
	for i := 0; i < len(frac); i++ {
		f = f*10 + uint64(frac[i]-'0')
	}
	if len(frac) == 1 {
		f *= 10
	}
	if w > (math.MaxInt64-f)/100 {
		return 0, newInvalid(CodeAmountOverflow, "amount %q exceeds the supported range", s)
	}
	return int64(w*100 + f), nil
}

// IsInitialized reports whether m was built by a constructor.
func (m Money) IsInitialized() bool { return m.currency != "" }

// Minor returns the amount in minor units (cents).
func (m Money) Minor() int64 { return m.minor }

// Currency returns the currency code.
func (m Money) Currency() Currency { return m.currency }

// IsZero reports whether the amount is zero (and m is initialized).
func (m Money) IsZero() bool { return m.IsInitialized() && m.minor == 0 }

// IsPositive reports whether the amount is greater than zero.
func (m Money) IsPositive() bool { return m.IsInitialized() && m.minor > 0 }

// IsNegative reports whether the amount is less than zero.
func (m Money) IsNegative() bool { return m.IsInitialized() && m.minor < 0 }

func (m Money) check() error {
	if !m.IsInitialized() {
		return newInvalid(CodeUninitializedMoney, "money value is not initialized")
	}
	return nil
}

func sameCurrency(a, b Money) error {
	if err := a.check(); err != nil {
		return err
	}
	if err := b.check(); err != nil {
		return err
	}
	if a.currency != b.currency {
		return newInvalid(CodeCurrencyMismatch, "%s and %s cannot be combined", a.currency, b.currency)
	}
	return nil
}

// Add returns m+o, failing on currency mismatch or int64 overflow.
func (m Money) Add(o Money) (Money, error) {
	if err := sameCurrency(m, o); err != nil {
		return Money{}, err
	}
	if (o.minor > 0 && m.minor > math.MaxInt64-o.minor) || (o.minor < 0 && m.minor < math.MinInt64-o.minor) {
		return Money{}, newInvalid(CodeAmountOverflow, "addition overflows")
	}
	return Money{minor: m.minor + o.minor, currency: m.currency}, nil
}

// Sub returns m-o, failing on currency mismatch or int64 overflow. The result
// may be negative.
func (m Money) Sub(o Money) (Money, error) {
	if err := sameCurrency(m, o); err != nil {
		return Money{}, err
	}
	if (o.minor < 0 && m.minor > math.MaxInt64+o.minor) || (o.minor > 0 && m.minor < math.MinInt64+o.minor) {
		return Money{}, newInvalid(CodeAmountOverflow, "subtraction overflows")
	}
	return Money{minor: m.minor - o.minor, currency: m.currency}, nil
}

// Neg returns -m, failing when m is the minimum int64 amount.
func (m Money) Neg() (Money, error) {
	if err := m.check(); err != nil {
		return Money{}, err
	}
	if m.minor == math.MinInt64 {
		return Money{}, newInvalid(CodeAmountOverflow, "negation overflows")
	}
	return Money{minor: -m.minor, currency: m.currency}, nil
}

// Cmp returns -1, 0 or 1 comparing m to o; currencies must match.
func (m Money) Cmp(o Money) (int, error) {
	if err := sameCurrency(m, o); err != nil {
		return 0, err
	}
	switch {
	case m.minor < o.minor:
		return -1, nil
	case m.minor > o.minor:
		return 1, nil
	}
	return 0, nil
}

// Equal reports whether both amount and currency match. Uninitialized values
// are equal only to each other.
func (m Money) Equal(o Money) bool { return m == o }

// Amount returns the decimal string with exactly two decimals ("10.00",
// "-0.05"). It is empty for an uninitialized value.
func (m Money) Amount() string {
	if !m.IsInitialized() {
		return ""
	}
	mag := uint64(m.minor)
	neg := m.minor < 0
	if neg {
		mag = -mag // two's complement magnitude; correct for MinInt64
	}
	whole := strconv.FormatUint(mag/100, 10)
	frac := mag % 100
	buf := make([]byte, 0, len(whole)+4)
	if neg {
		buf = append(buf, '-')
	}
	buf = append(buf, whole...)
	buf = append(buf, '.', byte('0'+frac/10), byte('0'+frac%10))
	return string(buf)
}

// String returns "10.00 BRL".
func (m Money) String() string {
	if !m.IsInitialized() {
		return "<uninitialized money>"
	}
	return m.Amount() + " " + string(m.currency)
}

type moneyJSON struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// MarshalJSON emits {"amount":"10.00","currency":"BRL"}. Uninitialized values
// fail to serialize.
func (m Money) MarshalJSON() ([]byte, error) {
	if err := m.check(); err != nil {
		return nil, err
	}
	return json.Marshal(moneyJSON{Amount: m.Amount(), Currency: string(m.currency)})
}

// UnmarshalJSON parses the external representation: an object with exactly
// the string fields "amount" and "currency". It applies ParseMoney, so
// negative, exponent, excess-scale or non-string amounts are rejected. Signed
// internal differences serialize but are not accepted back by design.
func (m *Money) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var raw moneyJSON
	if err := dec.Decode(&raw); err != nil {
		return newInvalid(CodeInvalidAmount, "malformed money object: %v", err)
	}
	if dec.More() {
		return newInvalid(CodeInvalidAmount, "trailing data after money object")
	}
	cur, err := ParseCurrency(raw.Currency)
	if err != nil {
		return err
	}
	parsed, err := ParseMoney(raw.Amount, cur)
	if err != nil {
		return err
	}
	*m = parsed
	return nil
}
