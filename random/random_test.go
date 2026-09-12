package random_test

import (
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/random"
)

// chiSquare is Pearson's statistic for counts against a uniform expectation
// over k categories. Each test compares it with the 0.9999 critical value for
// k-1 degrees of freedom: a real bias over these sample sizes lands far above
// that, and an honest source crosses it about once in ten thousand runs.
func chiSquare(counts map[byte]int, k, samples int) float64 {
	expected := float64(samples) / float64(k)
	var sum float64
	for _, observed := range counts {
		d := float64(observed) - expected
		sum += d * d / expected
	}
	// A category that never appeared contributes its whole expectation.
	sum += float64(k-len(counts)) * expected
	return sum
}

func TestBytesLength(t *testing.T) {
	t.Parallel()

	for _, n := range []int{0, 1, 16, 1000} {
		require.Len(t, random.Bytes(n), n)
	}
	require.NotEqual(t, random.Bytes(32), random.Bytes(32))
}

func TestBytesPanicsOnANegativeLength(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() { random.Bytes(-1) })
}

func TestIntNStaysInRange(t *testing.T) {
	t.Parallel()

	for range 100 {
		require.Equal(t, 0, random.IntN(1))

		v := random.IntN(7)
		require.GreaterOrEqual(t, v, 0)
		require.Less(t, v, 7)

		big := random.IntN(math.MaxInt)
		require.GreaterOrEqual(t, big, 0)
	}
}

func TestIntNIsUnbiased(t *testing.T) {
	t.Parallel()

	const samples = 70000
	counts := map[byte]int{}
	for range samples {
		counts[byte(random.IntN(7))]++
	}
	stat := chiSquare(counts, 7, samples)
	require.Less(
		t,
		stat,
		27.9,
		"IntN(7) is not uniform (chi-square %.1f)",
		stat,
	)
}

func TestIntNPanicsOnANonPositiveBound(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() { random.IntN(0) })
	require.Panics(t, func() { random.IntN(-1) })
}

var alphabets = []random.Alphabet{
	random.Decimal,
	random.Base62,
	random.Crockford,
}

func TestStringShape(t *testing.T) {
	t.Parallel()

	for _, alphabet := range alphabets {
		for _, n := range []int{0, 1, 43, 200} {
			s := random.String(n, alphabet)
			require.Len(t, s, n)
			require.Empty(t, strings.Trim(s, string(alphabet)),
				"%q has characters outside %q", s, alphabet)
		}
	}
	require.NotEqual(
		t,
		random.String(43, random.Base62),
		random.String(43, random.Base62),
	)
}

// Sizes that need no rejection (2, 32) check the mapping; sizes that do (10,
// 62, 86, 95) check that the rejected bytes are really dropped.
func TestStringIsUnbiased(t *testing.T) {
	t.Parallel()

	const printable = " !\"#$%&'()*+,-./0123456789:;<=>?@ABCDEFGHIJKLMNOPQRSTUVWXYZ[\\]^_`abcdefghijklmnopqrstuvwxyz{|}~"

	for _, tc := range []struct {
		alphabet  random.Alphabet
		threshold float64
	}{
		{alphabet: "01", threshold: 15.1},
		{alphabet: random.Decimal, threshold: 33.7},
		{alphabet: random.Crockford, threshold: 69.3},
		{alphabet: random.Base62, threshold: 111},
		{alphabet: random.Alphabet(printable[:86]), threshold: 142},
		{alphabet: random.Alphabet(printable), threshold: 154},
	} {
		const samples = 200000

		counts := map[byte]int{}
		for range samples / 100 {
			for _, c := range []byte(random.String(100, tc.alphabet)) {
				counts[c]++
			}
		}
		k := len(tc.alphabet)
		stat := chiSquare(counts, k, samples)
		require.Less(
			t,
			stat,
			tc.threshold,
			"k=%d is not uniform (chi-square %.1f)",
			k,
			stat,
		)
	}
}

func TestStringPanicsOnAnInvalidAlphabet(t *testing.T) {
	t.Parallel()

	for name, alphabet := range map[string]random.Alphabet{
		"empty":            "",
		"one character":    "a",
		"a duplicate":      "abca",
		"non-ASCII":        "aé",
		"a newline":        "a\n",
		"a NUL":            "a\x00",
		"DEL":              "a\x7f",
		"a range, written": "0-9A-Za-z",
		"over 128":         random.Alphabet(strings.Repeat("x", 129)),
	} {
		require.Panics(t, func() { random.String(4, alphabet) }, name)
	}
}

func TestStringPanicsOnANegativeLength(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() { random.String(-1, random.Base62) })
}

// The alphabets are part of the contract: aioz-common/secret encodes API-key
// checksums with Base62, so reordering it breaks every key already issued.
func TestAlphabetsArePinned(t *testing.T) {
	t.Parallel()

	require.Equal(t, random.Alphabet("0123456789"), random.Decimal)
	require.Equal(
		t,
		random.Alphabet(
			"0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz",
		),
		random.Base62,
	)
	require.Equal(
		t,
		random.Alphabet("0123456789ABCDEFGHJKMNPQRSTVWXYZ"),
		random.Crockford,
	)

	require.Len(t, string(random.Decimal), 10)
	require.Len(t, string(random.Base62), 62)
	require.Len(t, string(random.Crockford), 32)
	require.False(t, strings.ContainsAny(string(random.Crockford), "ILOU"))

	for _, alphabet := range alphabets {
		require.NotPanics(t, func() { random.String(1, alphabet) })
	}
}

func TestDigitsShape(t *testing.T) {
	t.Parallel()

	for _, n := range []int{random.MinDigits, 8} {
		code := random.Digits(n)
		require.Len(t, code, n)
		require.Empty(t, strings.Trim(code, "0123456789"))
	}
}

func TestDigitsPanicsBelowTheMinimum(t *testing.T) {
	t.Parallel()

	for _, n := range []int{random.MinDigits - 1, 0, -1} {
		require.Panics(t, func() { random.Digits(n) }, "n=%d", n)
	}
}

// Every position is uniform, the first one included. Drawing an integer in
// [100000, 999999], the pattern this replaces, never starts with 0: that
// fails here loudly.
func TestDigitsKeepLeadingZeros(t *testing.T) {
	t.Parallel()

	const samples = 20000
	counts := map[byte]int{}
	for range samples {
		counts[random.Digits(random.MinDigits)[0]]++
	}

	require.Positive(t, counts['0'], "no code started with 0")
	stat := chiSquare(counts, 10, samples)
	require.Less(
		t,
		stat,
		33.7,
		"the first digit is not uniform (chi-square %.1f)",
		stat,
	)
}
