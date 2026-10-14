// Package money provides exact, immutable monetary values with a fixed scale of
// two decimal places. Supported currencies are BRL, USD and EUR (ISO 4217).
// All arithmetic uses int64 minor units; no floating-point conversions occur.
package money

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
)

// Currency identifies one of the supported currencies. Its zero value is invalid.
type Currency string

const (
	BRL Currency = "BRL"
	USD Currency = "USD"
	EUR Currency = "EUR"
)

// 17 integer digits, one decimal point and two fractional digits.
const maxAmountLength = 20

var (
	// ErrInvalidCurrency indicates a currency outside the supported set.
	ErrInvalidCurrency = errors.New("money: unsupported currency")

	// ErrInvalidAmount indicates an invalid decimal or JSON representation.
	ErrInvalidAmount = errors.New("money: amount must be a decimal string with two fractional digits")

	// ErrAmountTooLong indicates an external amount exceeding the input limit.
	ErrAmountTooLong = errors.New("money: amount exceeds 20 bytes")

	// ErrNegativeAmount indicates a negative value in a nonnegative context.
	ErrNegativeAmount = errors.New("money: amount must not be negative in this context")

	// ErrInvalidMoney indicates an uninitialized or invalid Money value.
	ErrInvalidMoney = errors.New("money: uninitialized or invalid value")

	// ErrCurrencyMismatch indicates an operation between different currencies.
	ErrCurrencyMismatch = errors.New("money: currencies do not match")

	// ErrOverflow indicates a value or arithmetic result outside the int64 range.
	ErrOverflow = errors.New("money: int64 overflow")
)

// Money is a value object. Copying it is safe; arithmetic returns new values.
// The zero Go value is invalid: use Zero to create a monetary zero.
// Its range is -92233720368547758.08 through 92233720368547758.07.
type Money struct {
	minorUnits int64
	currency   Currency
}

// New parses an external, nonnegative amount. The grammar is [0-9]+\.[0-9]{2}.
// At most 20 bytes are accepted, including leading zeroes and the decimal point.
// Leading zeroes are normalized in JSON output. Whitespace, signs,
// scientific notation and other scales are rejected, without rounding.
func New(amount string, currency Currency) (Money, error) {
	if !validCurrency(currency) {
		return Money{}, ErrInvalidCurrency
	}

	if len(amount) > maxAmountLength {
		return Money{}, ErrAmountTooLong
	}

	if len(amount) > 0 && amount[0] == '-' {
		return Money{}, ErrNegativeAmount
	}

	if len(amount) < 4 || amount[len(amount)-3] != '.' {
		return Money{}, ErrInvalidAmount
	}

	decimalPoint := len(amount) - 3

	for i := 0; i < len(amount); i++ {
		if i == decimalPoint {
			continue
		}

		if amount[i] < '0' || amount[i] > '9' {
			return Money{}, ErrInvalidAmount
		}
	}

	units, err := strconv.ParseInt(amount[:decimalPoint]+amount[decimalPoint+1:], 10, 64)
	if err != nil {
		return Money{}, fmt.Errorf("%w: %w", ErrOverflow, err)
	}

	return FromMinorUnits(units, currency)
}

// FromMinorUnits constructs or rehydrates an exact value, including negative
// internal differences. External financial inputs must use New instead.
func FromMinorUnits(units int64, currency Currency) (Money, error) {
	if !validCurrency(currency) {
		return Money{}, ErrInvalidCurrency
	}

	return Money{minorUnits: units, currency: currency}, nil
}

// Zero constructs a monetary zero in a supported currency.
func Zero(currency Currency) (Money, error) {
	return FromMinorUnits(0, currency)
}

// Validate rejects uninitialized values. Negative internal values are valid.
func (m Money) Validate() error {
	if !validCurrency(m.currency) {
		return ErrInvalidMoney
	}

	return nil
}

// ValidateNonNegative validates a balance or an external financial amount,
// including values obtained through arithmetic or rehydration. General Money
// arithmetic permits negative internal differences, as required by the domain.
func (m Money) ValidateNonNegative() error {
	if err := m.Validate(); err != nil {
		return err
	}

	if m.minorUnits < 0 {
		return ErrNegativeAmount
	}

	return nil
}

// Currency returns the currency code, or an empty code for the zero Go value.
func (m Money) Currency() Currency { return m.currency }

// MinorUnits returns the exact representation for BIGINT persistence.
func (m Money) MinorUnits() (int64, error) {
	if err := m.Validate(); err != nil {
		return 0, err
	}

	return m.minorUnits, nil
}

// Amount returns the canonical decimal representation, including its sign.
func (m Money) Amount() (string, error) {
	if err := m.Validate(); err != nil {
		return "", err
	}

	units := uint64(m.minorUnits)
	sign := ""

	if m.minorUnits < 0 {
		// Avoid negating MinInt64 in signed arithmetic.
		units = uint64(-(m.minorUnits + 1)) + 1
		sign = "-"
	}

	return fmt.Sprintf("%s%d.%02d", sign, units/100, units%100), nil
}

// Add returns an exact sum, requiring valid values with matching currencies.
// A negative internal result is allowed; overflow is rejected.
func (m Money) Add(other Money) (Money, error) {
	if err := m.compatible(other); err != nil {
		return Money{}, err
	}

	a, b := m.minorUnits, other.minorUnits

	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return Money{}, ErrOverflow
	}

	return FromMinorUnits(a+b, m.currency)
}

// Subtract returns an exact difference, requiring matching currencies.
// A negative internal result is allowed; overflow is rejected.
func (m Money) Subtract(other Money) (Money, error) {
	if err := m.compatible(other); err != nil {
		return Money{}, err
	}

	a, b := m.minorUnits, other.minorUnits

	if (b > 0 && a < math.MinInt64+b) || (b < 0 && a > math.MaxInt64+b) {
		return Money{}, ErrOverflow
	}

	return FromMinorUnits(a-b, m.currency)
}

// SubtractNonNegative subtracts nonnegative values and rejects a negative
// result. Wallet debits can use this operation, while reconciliation can use
// Subtract to compute signed differences. Neither operand is modified.
func (m Money) SubtractNonNegative(other Money) (Money, error) {
	if err := m.ValidateNonNegative(); err != nil {
		return Money{}, err
	}

	if err := other.ValidateNonNegative(); err != nil {
		return Money{}, err
	}

	result, err := m.Subtract(other)
	if err != nil {
		return Money{}, err
	}

	if err := result.ValidateNonNegative(); err != nil {
		return Money{}, err
	}

	return result, nil
}

// Negate returns the opposite value, rejecting MinInt64 to avoid overflow.
func (m Money) Negate() (Money, error) {
	if err := m.Validate(); err != nil {
		return Money{}, err
	}

	if m.minorUnits == math.MinInt64 {
		return Money{}, ErrOverflow
	}

	return FromMinorUnits(-m.minorUnits, m.currency)
}

// Compare returns -1, 0 or 1 when m is less than, equal to or greater than other.
func (m Money) Compare(other Money) (int, error) {
	if err := m.compatible(other); err != nil {
		return 0, err
	}

	if m.minorUnits < other.minorUnits {
		return -1, nil
	}

	if m.minorUnits > other.minorUnits {
		return 1, nil
	}

	return 0, nil
}

type jsonMoney struct {
	Amount   string   `json:"amount"`
	Currency Currency `json:"currency"`
}

// MarshalJSON also supports negative internal values (e.g. reconciliation).
func (m Money) MarshalJSON() ([]byte, error) {
	amount, err := m.Amount()
	if err != nil {
		return nil, err
	}

	return json.Marshal(jsonMoney{Amount: amount, Currency: m.currency})
}

// UnmarshalJSON applies the external-input rules of New. Negative internal
// values must be rehydrated with FromMinorUnits, rather than this input decoder.
// Unknown fields and trailing JSON values are rejected. On error, the receiver
// is unchanged.
func (m *Money) UnmarshalJSON(data []byte) error {
	if m == nil {
		return ErrInvalidMoney
	}

	var value jsonMoney

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("%w: decode JSON: %w", ErrInvalidAmount, err)
	}

	var trailing json.RawMessage

	err := decoder.Decode(&trailing)
	if err == nil {
		return fmt.Errorf("%w: expected a single JSON object", ErrInvalidAmount)
	}

	if !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: decode trailing JSON: %w", ErrInvalidAmount, err)
	}

	parsed, err := New(value.Amount, value.Currency)
	if err != nil {
		return err
	}

	*m = parsed

	return nil
}

func validCurrency(currency Currency) bool {
	return currency == BRL || currency == USD || currency == EUR
}

func (m Money) compatible(other Money) error {
	if err := m.Validate(); err != nil {
		return err
	}

	if err := other.Validate(); err != nil {
		return err
	}

	if m.currency != other.currency {
		return ErrCurrencyMismatch
	}

	return nil
}
