package currency

import (
	"database/sql/driver"
	"encoding/json"

	"github.com/shopspring/decimal"
)

// Decimal is an arbitrary-precision decimal with database and JSON handling
// attached.
//
// It is the type for a rate or a ratio - a price per TB, a share of revenue -
// where Amount is the type for a quantity of money. Rates carry many
// significant digits and are multiplied by figures that overflow int64, so the
// arithmetic happens here and the result is converted to an Amount once, at
// the boundary.
type Decimal decimal.Decimal

// Common values.
var (
	DecimalZero = Decimal(decimal.Zero)
	DecimalOne  = Decimal(decimal.NewFromInt(1))
)

// NewDecimal wraps a shopspring decimal.
func NewDecimal(d decimal.Decimal) Decimal { return Decimal(d) }

// NewDecimalFromInt builds a Decimal from an integer.
func NewDecimalFromInt(value int64) Decimal { return Decimal(decimal.NewFromInt(value)) }

// NewDecimalFromFloat builds a Decimal from a float. Use it for a value that
// was already a float; never to hold money.
func NewDecimalFromFloat(value float64) Decimal { return Decimal(decimal.NewFromFloat(value)) }

// NewDecimalFromString parses a decimal string. This is how a rate should
// arrive from configuration: exact, and identical whether it came from a
// default or a config file.
func NewDecimalFromString(value string) (Decimal, error) {
	d, err := decimal.NewFromString(value)
	if err != nil {
		return DecimalZero, Error.Wrap(err)
	}
	return Decimal(d), nil
}

// MustDecimal is NewDecimalFromString for a rate that is known good at author
// time - a constant default in a config package, a fixture. Rates are written
// as string constants precisely so they are exact; this is how one becomes a
// Decimal without an error return nobody can act on.
func MustDecimal(value string) Decimal {
	d, err := NewDecimalFromString(value)
	if err != nil {
		panic(err)
	}
	return d
}

// NewDecimalFromBigInt builds a Decimal from a coefficient and an exponent,
// giving coefficient * 10^exponent.
func NewDecimalFromBigInt(coefficient int64, exponent int32) Decimal {
	return Decimal(decimal.New(coefficient, exponent))
}

// Shopspring returns the underlying decimal, for an operation this wrapper does
// not expose.
func (d Decimal) Shopspring() decimal.Decimal { return decimal.Decimal(d) }

// Arithmetic.

func (d Decimal) Add(other Decimal) Decimal { return Decimal(d.Shopspring().Add(other.Shopspring())) }
func (d Decimal) Sub(other Decimal) Decimal { return Decimal(d.Shopspring().Sub(other.Shopspring())) }
func (d Decimal) Mul(other Decimal) Decimal { return Decimal(d.Shopspring().Mul(other.Shopspring())) }
func (d Decimal) Div(other Decimal) Decimal { return Decimal(d.Shopspring().Div(other.Shopspring())) }
func (d Decimal) Neg() Decimal              { return Decimal(d.Shopspring().Neg()) }
func (d Decimal) Abs() Decimal              { return Decimal(d.Shopspring().Abs()) }

// DivRound divides and rounds to precision decimal places.
func (d Decimal) DivRound(other Decimal, precision int32) Decimal {
	return Decimal(d.Shopspring().DivRound(other.Shopspring(), precision))
}

// Rounding.

func (d Decimal) Round(places int32) Decimal     { return Decimal(d.Shopspring().Round(places)) }
func (d Decimal) RoundBank(places int32) Decimal { return Decimal(d.Shopspring().RoundBank(places)) }
func (d Decimal) Truncate(precision int32) Decimal {
	return Decimal(d.Shopspring().Truncate(precision))
}
func (d Decimal) Floor() Decimal { return Decimal(d.Shopspring().Floor()) }
func (d Decimal) Ceil() Decimal  { return Decimal(d.Shopspring().Ceil()) }

// Comparison.

func (d Decimal) Cmp(other Decimal) int          { return d.Shopspring().Cmp(other.Shopspring()) }
func (d Decimal) Equal(other Decimal) bool       { return d.Cmp(other) == 0 }
func (d Decimal) LessThan(other Decimal) bool    { return d.Cmp(other) < 0 }
func (d Decimal) GreaterThan(other Decimal) bool { return d.Cmp(other) > 0 }
func (d Decimal) Sign() int                      { return d.Shopspring().Sign() }
func (d Decimal) IsZero() bool                   { return d.Shopspring().IsZero() }
func (d Decimal) IsPositive() bool               { return d.Shopspring().IsPositive() }
func (d Decimal) IsNegative() bool               { return d.Shopspring().IsNegative() }

// Rendering.

func (d Decimal) String() string                  { return d.Shopspring().String() }
func (d Decimal) StringFixed(places int32) string { return d.Shopspring().StringFixed(places) }
func (d Decimal) IntPart() int64                  { return d.Shopspring().IntPart() }
func (d Decimal) Exponent() int32                 { return d.Shopspring().Exponent() }

// Float64 returns the float representation and whether it is exact. Read the
// second return value: a rate that does not fit a float is the case this type
// exists for.
func (d Decimal) Float64() (float64, bool) { return d.Shopspring().Float64() }

// MarshalJSON emits the decimal as a JSON string.
func (d Decimal) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// UnmarshalJSON accepts a JSON string or number.
func (d *Decimal) UnmarshalJSON(data []byte) error {
	var dec decimal.Decimal
	if err := dec.UnmarshalJSON(data); err != nil {
		return Error.Wrap(err)
	}
	*d = Decimal(dec)
	return nil
}

// Value implements driver.Valuer.
func (d Decimal) Value() (driver.Value, error) { return d.Shopspring().Value() }

// Scan implements sql.Scanner.
func (d *Decimal) Scan(value any) error {
	if value == nil {
		*d = DecimalZero
		return nil
	}
	var dec decimal.Decimal
	if err := dec.Scan(value); err != nil {
		return Error.Wrap(err)
	}
	*d = Decimal(dec)
	return nil
}

// GormDataType stores a rate at full precision.
func (Decimal) GormDataType() string { return "NUMERIC(40,20)" }
