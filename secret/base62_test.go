package secret

import (
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRandBase62Shape(t *testing.T) {
	t.Parallel()

	for _, n := range []int{1, 6, 12, 43, 200} {
		s := randBase62(n)
		require.Len(t, s, n)
		require.True(t, isBase62(s), "%q is not base62", s)
	}

	require.NotEqual(t, randBase62(43), randBase62(43))
}

// TestRandBase62IsUnbiased is the guard on the rejection sampling. Reducing a
// whole byte mod 62 without discarding 248..255 makes the first eight
// characters of the alphabet about 3% more likely, which this catches and a
// shape test does not.
func TestRandBase62IsUnbiased(t *testing.T) {
	t.Parallel()

	const samples = 200000

	counts := map[byte]int{}
	for range samples / 100 {
		for _, c := range []byte(randBase62(100)) {
			counts[c]++
		}
	}

	require.Len(t, counts, 62, "every character in the alphabet should appear")

	// Pearson's chi-square against a uniform expectation. With 61 degrees of
	// freedom the 0.999 critical value is about 112; a b%62 bias over this
	// many samples lands in the thousands, so the threshold separates the two
	// without being flaky.
	expected := float64(samples) / 62
	var chiSquare float64
	for _, observed := range counts {
		d := float64(observed) - expected
		chiSquare += d * d / expected
	}
	require.Less(t, chiSquare, 112.0, "distribution is not uniform (chi-square %.1f)", chiSquare)
}

func TestIsBase62(t *testing.T) {
	t.Parallel()

	require.True(t, isBase62("0aZ9"))
	require.False(t, isBase62(""))
	require.False(t, isBase62("has_underscore"))
	require.False(t, isBase62("has-dash"))
	require.False(t, isBase62("has space"))
	require.False(t, isBase62("+/="))
}

func TestChecksumIsFixedWidthAndStable(t *testing.T) {
	t.Parallel()

	for _, body := range []string{"", "a", strings.Repeat("x", 1000), "aioz_abc"} {
		c := checksum(body)
		require.Len(t, c, KeyChecksumLen)
		require.True(t, isBase62(c))
		require.Equal(t, c, checksum(body), "checksum must be deterministic")
	}

	require.NotEqual(t, checksum("a"), checksum("b"))
}

// TestChecksumCoversTheWholeCRCRange proves six characters are enough: the
// largest CRC32 still encodes without overflowing the fixed width.
func TestChecksumCoversTheWholeCRCRange(t *testing.T) {
	t.Parallel()

	require.Greater(t, math.Pow(62, KeyChecksumLen), float64(math.MaxUint32))
}
