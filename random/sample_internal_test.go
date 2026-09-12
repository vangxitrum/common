package random

import (
	"encoding/binary"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// blocks returns a read function that fills its buffer from the next block,
// padding the rest with pad, and fails the test on a read beyond the last
// block: a sampler that asks for more than it should have needed is the bug.
func blocks(
	t *testing.T,
	pad byte,
	data ...[]byte,
) (read func([]byte), reads func() int) {
	t.Helper()

	var n int
	read = func(b []byte) {
		if n >= len(data) {
			t.Fatalf(
				"read %d: the sampler asked for more random bytes than the test supplied",
				n+1,
			)
		}
		copied := copy(b, data[n])
		for i := copied; i < len(b); i++ {
			b[i] = pad
		}
		n++
	}
	return read, func() int { return n }
}

// stream returns a read function that serves data across as many reads as the
// sampler makes, padding with pad once it runs out, and fails the test on a
// read that starts after data is used up: the sampler had everything it needed.
func stream(t *testing.T, pad byte, data []byte) func([]byte) {
	t.Helper()

	exhausted := false
	return func(b []byte) {
		if exhausted {
			t.Fatal("the sampler kept reading after the scripted bytes ran out")
		}
		copied := copy(b, data)
		data = data[copied:]
		for i := copied; i < len(b); i++ {
			b[i] = pad
		}
		exhausted = len(data) == 0
	}
}

func uint64le(v uint64) []byte {
	return binary.LittleEndian.AppendUint64(nil, v)
}

// Every index gets exactly the same number of the 256 byte values, for every
// alphabet size. Fed the bytes 0..255 in order, the sampler accepts the first
// limit of them and nothing else, so this is the whole distribution, counted
// rather than estimated.
func TestSampleGivesEveryIndexTheSameNumberOfByteValues(t *testing.T) {
	t.Parallel()

	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(i)
	}

	for k := 1; k <= 256; k++ {
		limit := 256 - 256%k
		read, _ := blocks(t, 0xFF, all)

		dst := make([]byte, limit)
		sample(dst, k, read)

		counts := make([]int, k)
		for _, index := range dst {
			require.Less(
				t,
				int(index),
				k,
				"k=%d produced an index out of range",
				k,
			)
			counts[index]++
		}
		for index, count := range counts {
			require.Equal(t, 256/k, count, "k=%d index %d", k, index)
		}
	}
}

// A byte at or above the limit is dropped, not folded into the range, which is
// what folding would bias.
func TestSampleDropsBytesAtOrAboveTheLimit(t *testing.T) {
	t.Parallel()

	rejected := func(from, to int) []byte {
		var out []byte
		for b := from; b <= to; b++ {
			out = append(out, byte(b))
		}
		return out
	}

	for _, tc := range []struct {
		name   string
		k      int
		stream []byte
		want   byte
	}{
		{name: "base62 drops 248..255", k: 62, stream: append(rejected(248, 255), 5), want: 5},
		{name: "the worst case drops 129..255", k: 129, stream: append(rejected(129, 255), 0), want: 0},
		{name: "255 drops only 255", k: 255, stream: []byte{255, 7}, want: 7},
		{name: "256 drops nothing", k: 256, stream: []byte{255}, want: 255},
		{name: "32 drops nothing", k: 32, stream: []byte{255}, want: 31},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dst := make([]byte, 1)
			sample(dst, tc.k, stream(t, 0xFF, tc.stream))

			require.Equal(t, tc.want, dst[0])
		})
	}
}

func TestSampleRefillsWhenAWholeBlockIsRejected(t *testing.T) {
	t.Parallel()

	// For k=10 the limit is 250, so a block of 0xFF yields nothing.
	read, reads := blocks(t, 0xFF, nil, []byte{3})
	dst := make([]byte, 1)
	sample(dst, 10, read)

	require.Equal(t, byte(3), dst[0])
	require.Equal(t, 2, reads())
}

func TestSampleDoesNotReadForAnEmptyDestination(t *testing.T) {
	t.Parallel()

	read, reads := blocks(t, 0xFF)
	sample(nil, 10, read)
	require.Equal(t, 0, reads())
}

// The public API caps alphabets at 95 printable characters, so the sizes where
// rejection is heaviest can only be reached here.
func TestSampleIsUniformForLargeAlphabets(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		k         int
		threshold float64 // chi-square at 0.9999 for k-1 degrees of freedom
	}{
		{k: 129, threshold: 196},
		{k: 255, threshold: 348},
	} {
		const samples = 200000

		dst := make([]byte, samples)
		sample(dst, tc.k, readRand)

		counts := make([]int, tc.k)
		for _, index := range dst {
			counts[index]++
		}

		expected := float64(samples) / float64(tc.k)
		var chiSquare float64
		for _, observed := range counts {
			d := float64(observed) - expected
			chiSquare += d * d / expected
		}
		require.Less(
			t,
			chiSquare,
			tc.threshold,
			"k=%d is not uniform (chi-square %.1f)",
			tc.k,
			chiSquare,
		)
	}
}

// For n=3, 2^64 mod 3 is 1, so the single value below it is redrawn.
func TestUint64nRejectsBelowTheRemainder(t *testing.T) {
	t.Parallel()

	read, reads := blocks(t, 0, uint64le(0), uint64le(1))
	require.Equal(t, uint64(1), uint64n(3, read))
	require.Equal(t, 2, reads())
}

// The top of the range is accepted: bounding with <= MaxUint64-MaxUint64%n
// instead would accept one value too many and bias the result towards 0.
func TestUint64nAcceptsTheLargestValue(t *testing.T) {
	t.Parallel()

	read, reads := blocks(t, 0, uint64le(math.MaxUint64))
	require.Equal(t, uint64(math.MaxUint64%3), uint64n(3, read))
	require.Equal(t, 1, reads())
}

func TestUint64nNeverRejectsForAPowerOfTwo(t *testing.T) {
	t.Parallel()

	read, reads := blocks(t, 0, uint64le(0))
	require.Equal(t, uint64(0), uint64n(8, read))
	require.Equal(t, 1, reads())
}

// Just past half the range, almost half of all values are redrawn.
func TestUint64nNearTheTopOfTheRange(t *testing.T) {
	t.Parallel()

	n := uint64(1)<<63 + 1
	read, reads := blocks(t, 0, uint64le(1<<63-2), uint64le(1<<63-1))
	require.Equal(t, uint64(1<<63-1), uint64n(n, read))
	require.Equal(t, 2, reads())
}
