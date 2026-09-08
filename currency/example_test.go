package currency_test

import (
	"fmt"

	"gitlab.internal/tuan.quang.tran/aioz-common/currency"
)

// Compute in Decimal, store an Amount. The rate has more significant digits
// than the result needs; rounding happens once, in FromDecimal.
func Example() {
	price := currency.MustDecimal("0.00000208") // USD per GB-hour
	usage := currency.NewDecimalFromInt(1_500_000)

	owed := currency.FromDecimal[currency.USDUnit](
		price.Mul(usage).Shopspring(),
	)

	fmt.Println(owed.Units())  // for a human
	fmt.Println(owed.String()) // for the database and the wire
	// Output:
	// 3.120000
	// 3120000000000000000
}

// An Amount is a whole number of base units, so money never touches a float.
// The classic float error does not occur.
func ExampleAmount_Add() {
	tenth := currency.MustParse[currency.USDUnit]("0.1")
	fifth := currency.MustParse[currency.USDUnit]("0.2")

	fmt.Println(tenth.Add(fifth).Units())
	fmt.Println(
		tenth.Add(fifth).Equal(currency.MustParse[currency.USDUnit]("0.3")),
	)
	// Output:
	// 0.300000
	// true
}

// The zero value is zero of that currency, so an Amount from a struct literal,
// a NULL column or a JSON null all work without construction.
func ExampleAmount() {
	var balance currency.USD

	fmt.Println(balance.IsZero(), balance.String())

	balance = balance.Add(currency.FromUnits[currency.USDUnit](5))
	fmt.Println(balance.Units(), balance.Denom())
	// Output:
	// true 0
	// 5.000000 attousd
}

// A subtraction that would go below zero is clamped explicitly, rather than
// producing a negative payable that flows onward into a ledger.
func ExampleAmount_ClampNonNegative() {
	balance := currency.FromUnits[currency.AIOZUnit](3)
	charge := currency.FromUnits[currency.AIOZUnit](5)

	fmt.Println(balance.Sub(charge).Units())
	fmt.Println(balance.Sub(charge).ClampNonNegative().Units())
	// Output:
	// -2.000000
	// 0.000000
}
