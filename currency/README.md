# currency

Exact money amounts, and the decimals used to compute them.

```go
import "gitlab.internal/tuan.quang.tran/aioz-common/currency"
```

Two types, with different jobs:

- **`Amount`** - a quantity of money, held as an integer count of base units.
  `USD` and `AIOZ` are the instantiations.
- **`Decimal`** - a rate or a ratio: a price per TB, a share of revenue.

Compute in `Decimal`, store an `Amount`. Rates carry many significant digits and
get multiplied by figures that overflow `int64`, so the arithmetic happens in
decimal and the result converts to an amount once, at the boundary. Rounding
then happens in one place and never accumulates.

## Money is never a float, and never a scaled decimal column

```go
price   := currency.NewDecimalFromString("0.00000208")  // USD per GB-hour
byteHrs := currency.NewDecimalFromInt(gbHours)

owed := currency.FromDecimal[currency.USDUnit](price.Mul(byteHrs).Shopspring())
```

An `Amount` holds an integer count of base units at 18 decimals - `attousd`,
`attoaioz` - and the column is `NUMERIC(60,0)`. Base units of large amounts
exceed `BIGINT`, and a scaled `NUMERIC` would reintroduce the rounding the type
exists to avoid.

## Currencies cannot be mixed

The currency is part of the type, so this does not compile:

```go
var dollars currency.USD
var tokens  currency.AIOZ

dollars.Add(tokens)
// cannot use tokens (variable of struct type currency.AIOZ)
// as currency.Amount[currency.USDUnit] value in argument to dollars.Add
```

That is the whole reason for the type parameter. A single `Amount` type with a
currency *field* would have to catch this at runtime, and a mismatched-currency
addition is exactly the bug that reaches production - because the test used one
currency throughout.

The type parameter is a phantom: `USDUnit` and `AIOZUnit` are empty structs that
exist only to name a denomination and a decimal count. They cost nothing at
runtime, and one set of methods serves every currency.

## Usage

```go
zero    := currency.USD{}                                  // usable unconstructed
dollars := currency.FromUnits[currency.USDUnit](5)         // $5.00
exact   := currency.FromString[currency.USDUnit](fromDB)   // base-unit string
scaled  := currency.FromDecimal[currency.AIOZUnit](d)      // rounds once, here

total := currency.Sum(a, b, c)
owed  := total.Sub(paid).ClampNonNegative()
```

`ClampNonNegative` exists because ledger balances are non-negative by
construction: it makes a subtraction that would go below zero explicit, rather
than producing a negative payable that flows onward.

**The zero value works.** A `USD` from a struct literal, a `NULL` column or a
JSON `null` all behave as zero rather than panicking on a nil `Int` - every
method routes through `BaseUnits`, which normalizes it.

**Compare with `Equal`, not `==`.** The zero value and an explicit zero hold
different `Int`s, so `==` would distinguish amounts that are numerically equal.

## Adding a currency

Implement `Unit` with an empty struct and alias the instantiation:

```go
type EURUnit struct{}

func (EURUnit) Denom() string   { return "attoeur" }
func (EURUnit) Decimals() int32 { return 18 }

type EUR = currency.Amount[EURUnit]
```

Every method, and all the SQL and JSON handling, comes with it.

## Storage and encoding

`Value` writes the base-unit count as a numeric string; `Scan` accepts `string`,
`[]byte`, `int64` and `nil`. `GormDataType` declares `NUMERIC(60,0)` for an
amount and `NUMERIC(40,20)` for a `Decimal`.

**JSON is a string, not a number.** 18 decimals of base units exceeds what a
`float64` holds exactly, and most JSON parsers decode a bare number into one, so
a numeric encoding would silently lose the last digits on a round trip. An empty
string and `null` both decode to zero.

## API

**Amount** - `FromBaseUnits`, `FromDecimal`, `FromUnits`, `FromString`, `Zero`;
`BaseUnits`, `Decimal`, `BigInt`, `Denom`, `Decimals`; `Add`, `Sub`, `Neg`,
`Abs`, `Cmp`, `Equal`, `IsZero`, `IsNegative`, `IsPositive`,
`ClampNonNegative`; `Max`, `Min`, `Sum`; `String` (base units), `Units`
(display).

**Decimal** - `NewDecimal`, `NewDecimalFromInt`, `NewDecimalFromFloat`,
`NewDecimalFromString`, `NewDecimalFromBigInt`; `Add`, `Sub`, `Mul`, `Div`,
`DivRound`, `Neg`, `Abs`; `Round`, `RoundBank`, `Truncate`, `Floor`, `Ceil`;
`Cmp`, `Equal`, `LessThan`, `GreaterThan`, `Sign`; `String`, `StringFixed`,
`IntPart`, `Exponent`, `Float64`; `Shopspring` for anything not wrapped.
