package money_test

import (
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"

	"jungle/internal/wagering/domain/money"
)

func TestNew(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantAmount string
		wantUnits  int64
	}{
		{name: "zero", input: "0.00", wantAmount: "0.00"},
		{name: "whole amount", input: "25.00", wantAmount: "25.00", wantUnits: 2500},
		{name: "one cent", input: "0.01", wantAmount: "0.01", wantUnits: 1},
		{name: "leading zeroes", input: "00025.10", wantAmount: "25.10", wantUnits: 2510},
		{name: "maximum", input: "92233720368547758.07", wantAmount: "92233720368547758.07", wantUnits: math.MaxInt64},
		{name: "zero at length limit", input: strings.Repeat("0", 17) + ".00", wantAmount: "0.00"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value, err := money.New(tt.input, money.BRL)
			if err != nil {
				t.Fatalf("New(%q): %v", tt.input, err)
			}

			assertMoney(t, value, tt.wantUnits, money.BRL)

			amount, err := value.Amount()
			if err != nil {
				t.Fatalf("Amount(): %v", err)
			}

			if amount != tt.wantAmount {
				t.Fatalf("Amount() = %q, want %q", amount, tt.wantAmount)
			}
		})
	}
}

func TestNewRejectsInvalidAmounts(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr error
	}{
		{name: "empty", input: "", wantErr: money.ErrInvalidAmount},
		{name: "not a number", input: "NaN", wantErr: money.ErrInvalidAmount},
		{name: "infinity", input: "Infinity", wantErr: money.ErrInvalidAmount},
		{name: "scientific notation", input: "1e2", wantErr: money.ErrInvalidAmount},
		{name: "exponent after decimal", input: "1.00e0", wantErr: money.ErrInvalidAmount},
		{name: "missing fraction", input: "1", wantErr: money.ErrInvalidAmount},
		{name: "short fraction", input: "1.0", wantErr: money.ErrInvalidAmount},
		{name: "excess precision", input: "1.000", wantErr: money.ErrInvalidAmount},
		{name: "missing integer", input: ".00", wantErr: money.ErrInvalidAmount},
		{name: "plus sign", input: "+1.00", wantErr: money.ErrInvalidAmount},
		{name: "leading whitespace", input: " 1.00", wantErr: money.ErrInvalidAmount},
		{name: "trailing whitespace", input: "1.00 ", wantErr: money.ErrInvalidAmount},
		{name: "comma separator", input: "1,00", wantErr: money.ErrInvalidAmount},
		{name: "nondecimal digit", input: "a.00", wantErr: money.ErrInvalidAmount},
		{name: "unicode digit", input: "１.00", wantErr: money.ErrInvalidAmount},
		{name: "extra separator", input: "1..00", wantErr: money.ErrInvalidAmount},
		{name: "negative", input: "-1.00", wantErr: money.ErrNegativeAmount},
		{name: "negative zero", input: "-0.00", wantErr: money.ErrNegativeAmount},
		{name: "one cent above maximum", input: "92233720368547758.08", wantErr: money.ErrOverflow},
		{name: "numeric overflow", input: "99999999999999999.00", wantErr: money.ErrOverflow},
		{name: "one byte above limit", input: strings.Repeat("0", 18) + ".00", wantErr: money.ErrAmountTooLong},
		{name: "large leading zero input", input: strings.Repeat("0", 100_000) + "1.00", wantErr: money.ErrAmountTooLong},
		{name: "oversized number", input: "9999999999999999999999.00", wantErr: money.ErrAmountTooLong},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := money.New(tt.input, money.BRL)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("New() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestConstructorsRejectUnsupportedCurrencies(t *testing.T) {
	for _, currency := range []money.Currency{"", "brl", "ABC", "JPY"} {
		t.Run("currency="+string(currency), func(t *testing.T) {
			_, err := money.New("1.00", currency)
			if !errors.Is(err, money.ErrInvalidCurrency) {
				t.Fatalf("New() error = %v, want ErrInvalidCurrency", err)
			}

			_, err = money.Zero(currency)
			if !errors.Is(err, money.ErrInvalidCurrency) {
				t.Fatalf("Zero() error = %v, want ErrInvalidCurrency", err)
			}

			_, err = money.FromMinorUnits(0, currency)
			if !errors.Is(err, money.ErrInvalidCurrency) {
				t.Fatalf("FromMinorUnits() error = %v, want ErrInvalidCurrency", err)
			}
		})
	}
}

func TestZero(t *testing.T) {
	for _, currency := range []money.Currency{money.BRL, money.USD, money.EUR} {
		t.Run(string(currency), func(t *testing.T) {
			value, err := money.Zero(currency)
			if err != nil {
				t.Fatalf("Zero(): %v", err)
			}

			assertMoney(t, value, 0, currency)
		})
	}
}

func TestArithmetic(t *testing.T) {
	tests := []struct {
		name      string
		operation func(money.Money, money.Money) (money.Money, error)
		left      int64
		right     int64
		want      int64
		wantErr   error
	}{
		{name: "add", operation: money.Money.Add, left: 2500, right: 10, want: 2510},
		{name: "add negative", operation: money.Money.Add, left: -25, right: 10, want: -15},
		{name: "add reaches maximum", operation: money.Money.Add, left: math.MaxInt64 - 1, right: 1, want: math.MaxInt64},
		{name: "add opposite limits", operation: money.Money.Add, left: math.MaxInt64, right: math.MinInt64, want: -1},
		{name: "add positive overflow", operation: money.Money.Add, left: math.MaxInt64, right: 1, wantErr: money.ErrOverflow},
		{name: "add negative overflow", operation: money.Money.Add, left: math.MinInt64, right: -1, wantErr: money.ErrOverflow},
		{name: "subtract", operation: money.Money.Subtract, left: 2500, right: 10, want: 2490},
		{name: "subtract produces negative", operation: money.Money.Subtract, left: 10, right: 25, want: -15},
		{name: "subtract reaches minimum", operation: money.Money.Subtract, left: math.MinInt64 + 1, right: 1, want: math.MinInt64},
		{name: "subtract minimum from itself", operation: money.Money.Subtract, left: math.MinInt64, right: math.MinInt64, want: 0},
		{name: "subtract positive overflow", operation: money.Money.Subtract, left: math.MaxInt64, right: -1, wantErr: money.ErrOverflow},
		{name: "subtract negative overflow", operation: money.Money.Subtract, left: math.MinInt64, right: 1, wantErr: money.ErrOverflow},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			left := mustMoney(t, tt.left, money.BRL)
			right := mustMoney(t, tt.right, money.BRL)

			result, err := tt.operation(left, right)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("operation error = %v, want %v", err, tt.wantErr)
			}

			assertMoney(t, left, tt.left, money.BRL)
			assertMoney(t, right, tt.right, money.BRL)

			if err != nil {
				return
			}

			assertMoney(t, result, tt.want, money.BRL)
		})
	}
}

func TestNegate(t *testing.T) {
	tests := []struct {
		name    string
		units   int64
		want    int64
		wantErr error
	}{
		{name: "zero"},
		{name: "positive", units: 100, want: -100},
		{name: "negative", units: -1, want: 1},
		{name: "maximum", units: math.MaxInt64, want: -math.MaxInt64},
		{name: "minimum overflow", units: math.MinInt64, wantErr: money.ErrOverflow},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value := mustMoney(t, tt.units, money.BRL)

			result, err := value.Negate()
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Negate() error = %v, want %v", err, tt.wantErr)
			}

			assertMoney(t, value, tt.units, money.BRL)

			if err != nil {
				return
			}

			assertMoney(t, result, tt.want, money.BRL)
		})
	}
}

func TestAmount(t *testing.T) {
	tests := []struct {
		name  string
		units int64
		want  string
	}{
		{name: "minimum", units: math.MinInt64, want: "-92233720368547758.08"},
		{name: "negative cent", units: -1, want: "-0.01"},
		{name: "negative whole", units: -100, want: "-1.00"},
		{name: "zero", want: "0.00"},
		{name: "maximum", units: math.MaxInt64, want: "92233720368547758.07"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value := mustMoney(t, tt.units, money.BRL)

			amount, err := value.Amount()
			if err != nil {
				t.Fatalf("Amount(): %v", err)
			}

			if amount != tt.want {
				t.Fatalf("Amount() = %q, want %q", amount, tt.want)
			}
		})
	}
}

func TestCompare(t *testing.T) {
	tests := []struct {
		name  string
		left  int64
		right int64
		want  int
	}{
		{name: "less", left: -1, want: -1},
		{name: "equal"},
		{name: "greater", left: 1, want: 1},
		{name: "minimum to maximum", left: math.MinInt64, right: math.MaxInt64, want: -1},
		{name: "maximum to minimum", left: math.MaxInt64, right: math.MinInt64, want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			left := mustMoney(t, tt.left, money.BRL)
			right := mustMoney(t, tt.right, money.BRL)

			got, err := left.Compare(right)
			if err != nil {
				t.Fatalf("Compare(): %v", err)
			}

			if got != tt.want {
				t.Fatalf("Compare() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestBinaryOperationsRejectInvalidValues(t *testing.T) {
	valid := mustMoney(t, 1, money.BRL)
	tests := []struct {
		name    string
		left    money.Money
		right   money.Money
		wantErr error
	}{
		{name: "invalid left", right: valid, wantErr: money.ErrInvalidMoney},
		{name: "invalid right", left: valid, wantErr: money.ErrInvalidMoney},
		{name: "both invalid", wantErr: money.ErrInvalidMoney},
		{name: "different currencies", left: valid, right: mustMoney(t, 1, money.USD), wantErr: money.ErrCurrencyMismatch},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.left.Add(tt.right)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Add() error = %v, want %v", err, tt.wantErr)
			}

			_, err = tt.left.Subtract(tt.right)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Subtract() error = %v, want %v", err, tt.wantErr)
			}

			_, err = tt.left.Compare(tt.right)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Compare() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateNonNegative(t *testing.T) {
	tests := []struct {
		name    string
		value   money.Money
		wantErr error
	}{
		{name: "invalid", wantErr: money.ErrInvalidMoney},
		{name: "negative", value: mustMoney(t, -1, money.BRL), wantErr: money.ErrNegativeAmount},
		{name: "zero", value: mustMoney(t, 0, money.BRL)},
		{name: "positive", value: mustMoney(t, 1, money.BRL)},
		{name: "maximum", value: mustMoney(t, math.MaxInt64, money.BRL)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.value.ValidateNonNegative()
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ValidateNonNegative() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateNonNegativeAfterArithmetic(t *testing.T) {
	value := mustMoney(t, 100, money.BRL)
	tests := []struct {
		name      string
		operation func() (money.Money, error)
	}{
		{name: "add", operation: func() (money.Money, error) { return value.Add(mustMoney(t, -200, money.BRL)) }},
		{name: "subtract", operation: func() (money.Money, error) { return value.Subtract(mustMoney(t, 200, money.BRL)) }},
		{name: "negate", operation: value.Negate},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := tt.operation()
			if err != nil {
				t.Fatalf("operation: %v", err)
			}

			if err := result.ValidateNonNegative(); !errors.Is(err, money.ErrNegativeAmount) {
				t.Fatalf("ValidateNonNegative() error = %v, want ErrNegativeAmount", err)
			}
		})
	}
}

func TestSubtractNonNegative(t *testing.T) {
	tests := []struct {
		name    string
		balance money.Money
		debit   money.Money
		want    int64
		wantErr error
	}{
		{name: "debit", balance: mustMoney(t, 10000, money.BRL), debit: mustMoney(t, 8000, money.BRL), want: 2000},
		{name: "exact balance", balance: mustMoney(t, 100, money.BRL), debit: mustMoney(t, 100, money.BRL)},
		{name: "insufficient balance", balance: mustMoney(t, 2000, money.BRL), debit: mustMoney(t, 8000, money.BRL), wantErr: money.ErrNegativeAmount},
		{name: "negative balance", balance: mustMoney(t, -1, money.BRL), debit: mustMoney(t, 1, money.BRL), wantErr: money.ErrNegativeAmount},
		{name: "negative debit", balance: mustMoney(t, 1, money.BRL), debit: mustMoney(t, -1, money.BRL), wantErr: money.ErrNegativeAmount},
		{name: "currency mismatch", balance: mustMoney(t, 1, money.BRL), debit: mustMoney(t, 1, money.USD), wantErr: money.ErrCurrencyMismatch},
		{name: "invalid balance", debit: mustMoney(t, 1, money.BRL), wantErr: money.ErrInvalidMoney},
		{name: "invalid debit", balance: mustMoney(t, 1, money.BRL), wantErr: money.ErrInvalidMoney},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := tt.balance

			result, err := tt.balance.SubtractNonNegative(tt.debit)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("SubtractNonNegative() error = %v, want %v", err, tt.wantErr)
			}

			if tt.balance != before {
				t.Fatal("SubtractNonNegative() changed the original balance")
			}

			if err != nil {
				return
			}

			assertMoney(t, result, tt.want, money.BRL)
		})
	}
}

func TestZeroValueRejected(t *testing.T) {
	var value money.Money

	if err := value.Validate(); !errors.Is(err, money.ErrInvalidMoney) {
		t.Fatalf("Validate() error = %v, want ErrInvalidMoney", err)
	}

	_, err := value.Amount()
	if !errors.Is(err, money.ErrInvalidMoney) {
		t.Fatalf("Amount() error = %v, want ErrInvalidMoney", err)
	}

	_, err = value.MinorUnits()
	if !errors.Is(err, money.ErrInvalidMoney) {
		t.Fatalf("MinorUnits() error = %v, want ErrInvalidMoney", err)
	}

	_, err = value.Negate()
	if !errors.Is(err, money.ErrInvalidMoney) {
		t.Fatalf("Negate() error = %v, want ErrInvalidMoney", err)
	}

	_, err = json.Marshal(value)
	if !errors.Is(err, money.ErrInvalidMoney) {
		t.Fatalf("json.Marshal() error = %v, want ErrInvalidMoney", err)
	}
}

func TestJSONRoundTrip(t *testing.T) {
	var value money.Money

	err := json.Unmarshal([]byte(`{"amount":"00025.00","currency":"BRL"}`), &value)
	if err != nil {
		t.Fatalf("json.Unmarshal(): %v", err)
	}

	assertMoney(t, value, 2500, money.BRL)

	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal(): %v", err)
	}

	want := `{"amount":"25.00","currency":"BRL"}`
	if string(data) != want {
		t.Fatalf("json.Marshal() = %s, want %s", data, want)
	}

	var decoded money.Money

	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode canonical JSON: %v", err)
	}

	assertMoney(t, decoded, 2500, money.BRL)
}

func TestMarshalJSONNegativeInternalValue(t *testing.T) {
	value := mustMoney(t, math.MinInt64, money.BRL)

	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal(): %v", err)
	}

	want := `{"amount":"-92233720368547758.08","currency":"BRL"}`
	if string(data) != want {
		t.Fatalf("json.Marshal() = %s, want %s", data, want)
	}
}

func TestUnmarshalJSONRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr error
	}{
		{name: "null", input: `null`, wantErr: money.ErrInvalidCurrency},
		{name: "empty object", input: `{}`, wantErr: money.ErrInvalidCurrency},
		{name: "numeric amount", input: `{"amount":25.00,"currency":"BRL"}`, wantErr: money.ErrInvalidAmount},
		{name: "null amount", input: `{"amount":null,"currency":"BRL"}`, wantErr: money.ErrInvalidAmount},
		{name: "missing currency", input: `{"amount":"1.00"}`, wantErr: money.ErrInvalidCurrency},
		{name: "negative", input: `{"amount":"-1.00","currency":"BRL"}`, wantErr: money.ErrNegativeAmount},
		{name: "excess precision", input: `{"amount":"1.000","currency":"BRL"}`, wantErr: money.ErrInvalidAmount},
		{name: "array", input: `[]`, wantErr: money.ErrInvalidAmount},
		{name: "string", input: `"1.00"`, wantErr: money.ErrInvalidAmount},
		{name: "incomplete JSON", input: `{"amount":`, wantErr: money.ErrInvalidAmount},
		{name: "unknown field", input: `{"amount":"1.00","currency":"BRL","extra":true}`, wantErr: money.ErrInvalidAmount},
		{name: "misspelled field", input: `{"amount":"1.00","currency":"BRL","curreny":"USD"}`, wantErr: money.ErrInvalidAmount},
		{name: "trailing object", input: `{"amount":"1.00","currency":"BRL"} {}`, wantErr: money.ErrInvalidAmount},
		{name: "trailing malformed JSON", input: `{"amount":"1.00","currency":"BRL"} invalid`, wantErr: money.ErrInvalidAmount},
		{name: "large leading zero input", input: `{"amount":"` + strings.Repeat("0", 100_000) + `.00","currency":"BRL"}`, wantErr: money.ErrAmountTooLong},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value := mustMoney(t, 200, money.BRL)
			before := value

			// Direct calls exercise trailing data as well as malformed JSON;
			// json.Unmarshal rejects both before calling a custom decoder.
			err := value.UnmarshalJSON([]byte(tt.input))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("UnmarshalJSON() error = %v, want %v", err, tt.wantErr)
			}

			if value != before {
				t.Fatal("UnmarshalJSON() changed the receiver on error")
			}
		})
	}
}

func TestUnmarshalJSONNilReceiver(t *testing.T) {
	var value *money.Money

	err := value.UnmarshalJSON([]byte(`{}`))
	if !errors.Is(err, money.ErrInvalidMoney) {
		t.Fatalf("UnmarshalJSON() error = %v, want ErrInvalidMoney", err)
	}
}

func TestErrorsPreserveCauses(t *testing.T) {
	t.Run("numeric overflow", func(t *testing.T) {
		_, err := money.New("92233720368547758.08", money.BRL)

		var cause *strconv.NumError

		if !errors.Is(err, money.ErrOverflow) || !errors.As(err, &cause) {
			t.Fatalf("New() error = %v, want ErrOverflow and *strconv.NumError", err)
		}
	})

	t.Run("JSON type error", func(t *testing.T) {
		var value money.Money

		err := value.UnmarshalJSON([]byte(`{"amount":25,"currency":"BRL"}`))

		var cause *json.UnmarshalTypeError

		if !errors.Is(err, money.ErrInvalidAmount) || !errors.As(err, &cause) {
			t.Fatalf("UnmarshalJSON() error = %v, want ErrInvalidAmount and *json.UnmarshalTypeError", err)
		}
	})
}

func mustMoney(t *testing.T, units int64, currency money.Currency) money.Money {
	t.Helper()

	value, err := money.FromMinorUnits(units, currency)
	if err != nil {
		t.Fatalf("FromMinorUnits(%d, %q): %v", units, currency, err)
	}

	return value
}

func assertMoney(t *testing.T, value money.Money, wantUnits int64, wantCurrency money.Currency) {
	t.Helper()

	units, err := value.MinorUnits()
	if err != nil {
		t.Fatalf("MinorUnits(): %v", err)
	}

	if units != wantUnits {
		t.Errorf("MinorUnits() = %d, want %d", units, wantUnits)
	}

	if currency := value.Currency(); currency != wantCurrency {
		t.Errorf("Currency() = %q, want %q", currency, wantCurrency)
	}
}
