# currency

Money as a whole number of tiny units.

```go
import "gitlab.internal/tuan.quang.tran/aioz-common/currency"
```

## What this is, and the rule behind it

**Money is never a float.** `0.1 + 0.2` is not `0.3` in binary floating point -
no base-2 fraction lands on those values - and a ledger that adds a few million
such amounts drifts by a sum nobody can reconstruct afterwards. Worse, the drift
is invisible in a test with three rows and obvious in production with three
million.

The fix is the one banks have always used: **count the smallest unit and never
divide it.** Cents, not dollars.

Here the smallest unit is much smaller than a cent. An `Amount` is an integer
count of base units at 18 decimal places - `attousd` for dollars, `attoaioz` for
the AIOZ token, following the convention the token itself uses on chain. One
dollar is 10^18 attousd.

Eighteen places is far more precision than any price needs, and that is the
point: intermediate results never need rounding, so rounding happens **once**,
where a number is finally converted, instead of a little at every step.

```go
tenth := currency.MustParse[currency.USDUnit]("0.1")
fifth := currency.MustParse[currency.USDUnit]("0.2")

tenth.Add(fifth).Equal(currency.MustParse[currency.USDUnit]("0.3"))  // true
```

That example is a runnable test in `example_test.go`, so the claim is checked
rather than asserted.

## The two types

- **`Amount`** - a quantity of money someone owes, holds or was paid. `USD` and
  `AIOZ` are the two instantiations.
- **`Decimal`** - a rate or a ratio: a price per TB, a share of revenue. It is
  not money; it is what money gets computed *from*.

Compute in `Decimal`, store an `Amount`. Rates carry many significant digits and
get multiplied by figures that overflow `int64`, so the arithmetic happens in
decimal and the result converts to an amount once, at the boundary. Rounding
then happens in one place and never accumulates.

```go
price := currency.MustDecimal("0.00000208")        // USD per GB-hour, a constant
usage := currency.NewDecimalFromInt(gbHours)

owed := currency.FromDecimal[currency.USDUnit](price.Mul(usage).Shopspring())
```

`MustDecimal` is for a rate that is known good at author time - a constant
default, a fixture - where an error return has nowhere useful to go.

## Two string forms, for two readers

| | writes | reads | for |
| --- | --- | --- | --- |
| base units | `String()` | `FromString` | the database, the wire, a CSV - exact, round-trips losslessly |
| display units | `Units()` | `Parse` / `MustParse` | a config file, a person - `"3.12"` rather than `"3120000000000000000"` |

`Parse` rounds to base units once, on the way in. `FromString` does not round at
all, because a base-unit string is already exact. An empty string is zero in
both, so a `NULL` column and an absent field need no special case.

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
zero    := currency.USD{}                              // usable unconstructed
dollars := currency.FromUnits[currency.USDUnit](5)     // $5.00
price   := currency.MustParse[currency.USDUnit]("0.1") // from a config value
scaled  := currency.FromDecimal[currency.AIOZUnit](d)  // rounds once, here

exact, err := currency.FromString[currency.USDUnit](fromDB)  // base-unit string

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

**Amount** - `FromBaseUnits`, `FromDecimal`, `FromUnits`, `FromString`, `Parse`,
`MustParse`, `Zero`;
`BaseUnits`, `Decimal`, `BigInt`, `Denom`, `Decimals`; `Add`, `Sub`, `Neg`,
`Abs`, `Cmp`, `Equal`, `IsZero`, `IsNegative`, `IsPositive`,
`ClampNonNegative`; `Max`, `Min`, `Sum`; `String` (base units), `Units`
(display).

**Decimal** - `NewDecimal`, `NewDecimalFromInt`, `NewDecimalFromFloat`,
`NewDecimalFromString`, `MustDecimal`, `NewDecimalFromBigInt`; `Add`, `Sub`, `Mul`, `Div`,
`DivRound`, `Neg`, `Abs`; `Round`, `RoundBank`, `Truncate`, `Floor`, `Ceil`;
`Cmp`, `Equal`, `LessThan`, `GreaterThan`, `Sign`; `String`, `StringFixed`,
`IntPart`, `Exponent`, `Float64`; `Shopspring` for anything not wrapped.
