package currency_test

import (
	"encoding/json"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/currency"
)

// TestZeroValueIsUsable: an Amount arrives from a struct literal, a nil column
// and a decoded JSON null. All three must behave as zero rather than panicking
// on a nil Int.
func TestZeroValueIsUsable(t *testing.T) {
	var amount currency.USD

	require.True(t, amount.IsZero())
	require.False(t, amount.IsNegative())
	require.Equal(t, "0", amount.String())
	require.Equal(t, int64(0), amount.BaseUnits().Int64())
	require.True(t, amount.Add(currency.FromUnits[currency.USDUnit](1)).
		Equal(currency.FromUnits[currency.USDUnit](1)))
}

// TestBaseUnitsRoundTrip: the string form is what goes into the database and
// onto the wire, so it has to come back unchanged - including at magnitudes
// that overflow int64, which is why the column is NUMERIC(60,0).
func TestBaseUnitsRoundTrip(t *testing.T) {
	// 12345.678901234567890123 USD in attousd: 23 digits, well past int64.
	const raw = "12345678901234567890123"

	amount, err := currency.FromString[currency.USDUnit](raw)
	require.NoError(t, err)
	require.Equal(t, raw, amount.String())

	back, err := currency.FromString[currency.USDUnit](amount.String())
	require.NoError(t, err)
	require.True(t, amount.Equal(back))
}

// TestFromDecimalRoundsOnce is the property the whole design is for: converting
// a display amount to base units rounds at the 18th place and nowhere else.
func TestFromDecimalRoundsOnce(t *testing.T) {
	d := decimal.RequireFromString("1.5")
	amount := currency.FromDecimal[currency.USDUnit](d)

	require.Equal(t, "1500000000000000000", amount.String())
	require.True(t, amount.Decimal().Equal(d))
}

// TestEmptyStringIsZero: a NULL column and an absent field are common and are
// not errors.
func TestEmptyStringIsZero(t *testing.T) {
	amount, err := currency.FromString[currency.USDUnit]("")
	require.NoError(t, err)
	require.True(t, amount.IsZero())
}

func TestFromStringRejectsGarbage(t *testing.T) {
	_, err := currency.FromString[currency.USDUnit]("not-a-number")
	require.Error(t, err)
	require.Contains(t, err.Error(), "attousd")
}

// TestJSONIsAStringNotANumber: 18 decimals of base units exceeds what a float64
// holds exactly, and most JSON parsers decode a bare number into one. Emitting a
// string is what stops a round trip losing the last digits.
func TestJSONIsAStringNotANumber(t *testing.T) {
	amount, err := currency.FromString[currency.AIOZUnit](
		"12345678901234567890123",
	)
	require.NoError(t, err)

	encoded, err := json.Marshal(amount)
	require.NoError(t, err)
	require.Equal(t, `"12345678901234567890123"`, string(encoded))

	var back currency.AIOZ
	require.NoError(t, json.Unmarshal(encoded, &back))
	require.True(t, amount.Equal(back))
}

func TestJSONNullIsZero(t *testing.T) {
	var amount currency.USD
	require.NoError(t, json.Unmarshal([]byte("null"), &amount))
	require.True(t, amount.IsZero())
}

// TestScanAcceptsWhatDriversReturn: a NUMERIC column comes back as []byte from
// one driver and string from another, and a nil for a NULL.
func TestScanAcceptsWhatDriversReturn(t *testing.T) {
	for _, value := range []any{
		"1500000000000000000",
		[]byte("1500000000000000000"),
	} {
		var amount currency.USD
		require.NoError(t, amount.Scan(value))
		require.Equal(t, "1500000000000000000", amount.String())
	}

	var amount currency.USD
	require.NoError(t, amount.Scan(nil))
	require.True(t, amount.IsZero())

	require.Error(t, amount.Scan(struct{}{}))
}

// TestValueIsExactInteger: the column is NUMERIC(60,0), so what is written must
// be an integer string rather than a float or a scaled decimal.
func TestValueIsExactInteger(t *testing.T) {
	amount := currency.FromUnits[currency.USDUnit](3)
	value, err := amount.Value()
	require.NoError(t, err)
	require.Equal(t, "3000000000000000000", value)
}

func TestClampNonNegative(t *testing.T) {
	negative := currency.FromUnits[currency.USDUnit](-5)
	require.True(t, negative.IsNegative())
	require.True(t, negative.ClampNonNegative().IsZero())

	positive := currency.FromUnits[currency.USDUnit](5)
	require.True(t, positive.ClampNonNegative().Equal(positive))
}

func TestSumMaxMin(t *testing.T) {
	one := currency.FromUnits[currency.AIOZUnit](1)
	two := currency.FromUnits[currency.AIOZUnit](2)

	require.True(
		t,
		currency.Sum(one, two).Equal(currency.FromUnits[currency.AIOZUnit](3)),
	)
	require.True(t, currency.Sum[currency.AIOZUnit]().IsZero())
	require.True(t, currency.Max(one, two).Equal(two))
	require.True(t, currency.Min(one, two).Equal(one))
}

// TestDenomsAreDistinct: the two currencies carry different denominations, which
// is what makes an error message name the right one.
func TestDenomsAreDistinct(t *testing.T) {
	require.Equal(t, "attousd", currency.USD{}.Denom())
	require.Equal(t, "attoaioz", currency.AIOZ{}.Denom())
	require.Equal(t, int32(18), currency.USD{}.Decimals())
}

// TestCurrenciesDoNotMix documents the compile-time guarantee. The following
// does not build, which is the point of the type parameter:
//
//	var dollars currency.USD
//	var tokens currency.AIOZ
//	dollars.Add(tokens)  // cannot use tokens as Amount[USDUnit]
//
// A single Amount type with a currency field would have to catch that at
// runtime instead, and a mismatched-currency addition is exactly the bug that
// reaches production because the test used one currency throughout.
func TestCurrenciesDoNotMix(t *testing.T) {
	require.NotEqual(t, currency.USD{}.Denom(), currency.AIOZ{}.Denom())
}
