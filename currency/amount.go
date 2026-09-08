// Package currency represents money as a whole number of tiny units.
//
// The rule this package exists to enforce is that money is never a float.
// 0.1 + 0.2 is not 0.3 in binary floating point, and a ledger that adds a few
// million such amounts drifts by an amount nobody can account for afterwards.
// The fix is the one banks have always used: count the smallest unit and never
// divide it. Cents, rather than dollars.
//
// Here the smallest unit is much smaller than a cent. An Amount is an integer
// count of base units at 18 decimal places - "attousd" for dollars, "attoaioz"
// for the AIOZ token, following the convention the token itself uses on chain.
// A dollar is 10^18 attousd. That is far more precision than any price needs,
// which is the point: intermediate results never need rounding, so rounding
// happens once, where a number is finally converted, instead of accumulating a
// little at every step.
//
// Two types divide the work:
//
//   - Amount is a quantity of money that someone owes, holds or was paid. USD
//     and AIOZ are the two instantiations.
//   - Decimal is a rate or a ratio - a price per TB, a share of revenue. It is
//     not money; it is what money gets computed from.
//
// The workflow is to compute in Decimal and store an Amount:
//
//	price  := currency.NewDecimalFromString("0.00000208")  // USD per GB-hour
//	usage  := currency.NewDecimalFromInt(gbHours)
//	owed   := currency.FromDecimal[currency.USDUnit](price.Mul(usage).Shopspring())
//
//	balance = balance.Add(owed)
//	fmt.Println(owed.Units())    // "3.120000" - for a human
//	fmt.Println(owed.String())   // "3120000000000000000" - for the database
//
// The currency is part of the type, so adding dollars to tokens does not
// compile. See Amount for why that is worth a type parameter.
package currency

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"math/big"

	sdkmath "cosmossdk.io/math"
	"github.com/shopspring/decimal"
	"github.com/zeebo/errs/v2"
)

// Error tags every failure this package produces.
var Error = errs.Tag("currency")

// Unit describes one currency: the name of its base denomination and how many
// decimal places separate that denomination from the display unit.
//
// An implementation is an empty struct used only as a type parameter, so it
// costs nothing at runtime and exists purely to keep two currencies apart at
// compile time.
type Unit interface {
	Denom() string
	Decimals() int32
}

// AIOZUnit is the AIOZ Network token, carried as attoaioz.
type AIOZUnit struct{}

// Denom returns the base denomination name.
func (AIOZUnit) Denom() string { return "attoaioz" }

// Decimals returns the number of decimal places in one AIOZ.
func (AIOZUnit) Decimals() int32 { return 18 }

// USDUnit is United States dollars, carried as attousd.
type USDUnit struct{}

// Denom returns the base denomination name.
func (USDUnit) Denom() string { return "attousd" }

// Decimals returns the number of decimal places in one dollar.
func (USDUnit) Decimals() int32 { return 18 }

// Amount is an exact quantity of the currency U, held as an integer count of
// U's base units.
//
// The currency is part of the type, so adding dollars to tokens does not
// compile. That is the whole reason for the type parameter: a single Amount
// type carrying a currency field would have to fail at runtime instead, and a
// mismatched-currency addition is exactly the kind of bug that reaches
// production because the test used one currency throughout.
//
// The zero value is zero of that currency and is usable without construction.
type Amount[U Unit] struct {
	// amount is the count of base units. A nil Int means zero, so the zero
	// value works; BaseUnits is what normalizes that, and every method goes
	// through it rather than touching the field.
	amount sdkmath.Int
}

// AIOZ is an exact amount of the AIOZ Network token.
type AIOZ = Amount[AIOZUnit]

// USD is an exact amount of United States dollars.
type USD = Amount[USDUnit]

// Compile-time interface checks, on one instantiation - the methods are shared,
// so satisfying them for one currency satisfies them for all.
var (
	_ driver.Valuer    = USD{}
	_ sql.Scanner      = &USD{}
	_ json.Marshaler   = USD{}
	_ json.Unmarshaler = &USD{}
)

// Zero is the zero amount of a currency. Useful where the zero value cannot be
// written directly, such as a return in a generic helper.
func Zero[U Unit]() Amount[U] { return Amount[U]{} }

// FromBaseUnits builds an amount from a raw base-unit count.
func FromBaseUnits[U Unit](
	i sdkmath.Int,
) Amount[U] {
	return Amount[U]{amount: i}
}

// FromDecimal converts a display-unit amount - dollars, or whole AIOZ - to base
// units, rounding half away from zero at the last base-unit place. Rates and
// usage figures carry more decimal places than that only in intermediate
// products, so this is where the single rounding step happens.
func FromDecimal[U Unit](d decimal.Decimal) Amount[U] {
	var u U
	scaled := d.Mul(decimal.New(1, u.Decimals())).Round(0)
	return Amount[U]{amount: sdkmath.NewIntFromBigInt(scaled.BigInt())}
}

// FromUnits builds an amount from a whole number of display units.
func FromUnits[U Unit](units int64) Amount[U] {
	return FromDecimal[U](decimal.NewFromInt(units))
}

// FromString parses a base-unit integer string - the form String emits and the
// form stored in the database. An empty string is zero, so a NULL column and an
// absent JSON field need no special case at the call site.
func FromString[U Unit](s string) (Amount[U], error) {
	if s == "" {
		return Amount[U]{}, nil
	}
	amount, ok := sdkmath.NewIntFromString(s)
	if !ok {
		var u U
		return Amount[U]{}, Error.Errorf(
			"invalid base-unit string for %s: %q",
			u.Denom(),
			s,
		)
	}
	return Amount[U]{amount: amount}, nil
}

// Parse reads a display-unit string - "3.12", "0.00000208" - the form a human
// writes in a config file or a CSV. It rounds to base units once, here.
//
// This is the counterpart of Units, as FromString is the counterpart of String.
// The two parsers exist because the two forms are for different readers: the
// base-unit integer round-trips exactly and is what goes in the database, while
// the display form is what a person types.
func Parse[U Unit](s string) (Amount[U], error) {
	if s == "" {
		return Amount[U]{}, nil
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		var u U
		return Amount[U]{}, Error.Errorf("invalid %s amount %q: %v", u.Denom(), s, err)
	}
	return FromDecimal[U](d), nil
}

// MustParse is Parse for a string that is known good at author time - a
// constant default, a fixture - where an error has nowhere to go. It panics
// rather than returning one.
func MustParse[U Unit](s string) Amount[U] {
	amount, err := Parse[U](s)
	if err != nil {
		panic(err)
	}
	return amount
}

// Denom returns the base denomination name, such as "attoaioz".
func (a Amount[U]) Denom() string {
	var u U
	return u.Denom()
}

// Decimals returns how many decimal places separate the base denomination from
// the display unit.
func (a Amount[U]) Decimals() int32 {
	var u U
	return u.Decimals()
}

// BaseUnits returns the raw base-unit count. A zero-valued Amount reports zero
// rather than a nil Int, so callers never have to check.
func (a Amount[U]) BaseUnits() sdkmath.Int {
	if a.amount.IsNil() {
		return sdkmath.ZeroInt()
	}
	return a.amount
}

// Decimal returns the amount in display units.
func (a Amount[U]) Decimal() decimal.Decimal {
	return decimal.NewFromBigInt(a.BaseUnits().BigInt(), -a.Decimals())
}

// BigInt returns the base-unit count as a big.Int.
func (a Amount[U]) BigInt() *big.Int { return a.BaseUnits().BigInt() }

// IsZero reports whether the amount is zero.
func (a Amount[U]) IsZero() bool { return a.BaseUnits().IsZero() }

// IsNegative reports whether the amount is below zero.
func (a Amount[U]) IsNegative() bool { return a.BaseUnits().IsNegative() }

// IsPositive reports whether the amount is above zero.
func (a Amount[U]) IsPositive() bool { return a.BaseUnits().IsPositive() }

// Add returns a + other.
func (a Amount[U]) Add(other Amount[U]) Amount[U] {
	return Amount[U]{amount: a.BaseUnits().Add(other.BaseUnits())}
}

// Sub returns a - other.
func (a Amount[U]) Sub(other Amount[U]) Amount[U] {
	return Amount[U]{amount: a.BaseUnits().Sub(other.BaseUnits())}
}

// Neg returns -a.
func (a Amount[U]) Neg() Amount[U] { return Amount[U]{amount: a.BaseUnits().Neg()} }

// Abs returns the absolute value of a.
func (a Amount[U]) Abs() Amount[U] { return Amount[U]{amount: a.BaseUnits().Abs()} }

// Cmp returns -1 if a < other, 0 if equal, 1 if a > other.
func (a Amount[U]) Cmp(other Amount[U]) int {
	return a.BaseUnits().BigInt().Cmp(other.BaseUnits().BigInt())
}

// Equal reports whether two amounts are the same.
//
// Use this rather than ==: the zero value and an explicit zero hold different
// Ints, so == distinguishes amounts that are numerically equal.
func (a Amount[U]) Equal(other Amount[U]) bool { return a.Cmp(other) == 0 }

// ClampNonNegative returns the amount, or zero if it is negative. Ledger
// balances are non-negative by construction; this makes a subtraction that
// would go below zero explicit rather than producing a negative payable.
func (a Amount[U]) ClampNonNegative() Amount[U] {
	if a.IsNegative() {
		return Amount[U]{}
	}
	return a
}

// Max returns whichever amount is larger.
func Max[U Unit](a, b Amount[U]) Amount[U] {
	if a.Cmp(b) >= 0 {
		return a
	}
	return b
}

// Min returns whichever amount is smaller.
func Min[U Unit](a, b Amount[U]) Amount[U] {
	if a.Cmp(b) <= 0 {
		return a
	}
	return b
}

// Sum adds every amount, returning zero for an empty list.
func Sum[U Unit](amounts ...Amount[U]) Amount[U] {
	total := Amount[U]{}
	for _, amount := range amounts {
		total = total.Add(amount)
	}
	return total
}

// String returns the base-unit count. This is the canonical wire and CSV form:
// an exact integer that round-trips through FromString without loss.
func (a Amount[U]) String() string { return a.BaseUnits().String() }

// Units returns a human-readable figure in display units. Display only - it
// truncates at six places.
func (a Amount[U]) Units() string { return a.Decimal().StringFixed(6) }

// GormDataType stores the amount as an exact integer. The column is declared
// NUMERIC(60,0): base units of large amounts exceed BIGINT, and a scaled
// NUMERIC would reintroduce the rounding this type exists to avoid.
func (Amount[U]) GormDataType() string { return "NUMERIC(60,0)" }

// Value implements driver.Valuer, writing the base-unit count as a numeric
// string.
func (a Amount[U]) Value() (driver.Value, error) { return a.BaseUnits().String(), nil }

// Scan implements sql.Scanner.
func (a *Amount[U]) Scan(value any) error {
	var s string
	switch v := value.(type) {
	case nil:
		*a = Amount[U]{}
		return nil
	case string:
		s = v
	case []byte:
		s = string(v)
	case int64:
		*a = Amount[U]{amount: sdkmath.NewInt(v)}
		return nil
	default:
		return Error.Errorf("cannot scan %T into %s amount", value, a.Denom())
	}

	parsed, err := FromString[U](s)
	if err != nil {
		return err
	}
	*a = parsed
	return nil
}

// MarshalJSON emits the base-unit string, so a JSON round trip is lossless. A
// number would not be: 18 decimals of base units exceeds what a float64 holds,
// and most JSON parsers decode a bare number into one.
func (a Amount[U]) MarshalJSON() ([]byte, error) { return json.Marshal(a.String()) }

// UnmarshalJSON accepts the base-unit string form, and null as zero.
func (a *Amount[U]) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*a = Amount[U]{}
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return Error.Wrap(err)
	}
	parsed, err := FromString[U](s)
	if err != nil {
		return err
	}
	*a = parsed
	return nil
}
