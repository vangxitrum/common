package random

import "fmt"

// Alphabet is the set of characters a random string is drawn from.
//
// It must be at least two distinct printable ASCII characters. The string is
// built byte by byte, so a multi-byte character would come out as invalid
// UTF-8, and a control character in a generated value is always a bug.
type Alphabet string

const (
	// Decimal is the ten digits, for numeric codes.
	Decimal Alphabet = "0123456789"

	// Base62 is digits, then upper, then lower: one word that survives a URL,
	// a header, a shell argument and a YAML scalar unquoted. The order is part
	// of the contract - aioz-common/secret encodes API-key checksums with it.
	Base62 Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

	// Crockford is Crockford base32, the digits plus the letters less I, L, O
	// and U, for anything a person reads and types back.
	Crockford Alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
)

// String returns n characters drawn uniformly from alphabet. It panics if n is
// negative or the alphabet is invalid.
func String(n int, alphabet Alphabet) string {
	if n < 0 {
		panic(fmt.Sprintf("random: negative length %d", n))
	}
	alphabet.mustValidate()

	out := make([]byte, n)
	sample(out, len(alphabet), readRand)
	for i, index := range out {
		out[i] = alphabet[index]
	}
	return string(out)
}

// mustValidate panics unless a is at least two distinct printable ASCII
// characters.
func (a Alphabet) mustValidate() {
	if len(a) < 2 {
		panic(
			fmt.Sprintf(
				"random: alphabet %q has fewer than two characters",
				string(a),
			),
		)
	}

	var seen [128]bool
	for i := range len(a) {
		c := a[i]
		if c < 0x20 || c >= 0x7f {
			panic(
				fmt.Sprintf(
					"random: alphabet byte %#x is not printable ASCII",
					c,
				),
			)
		}
		if seen[c] {
			panic(fmt.Sprintf("random: alphabet has %q twice", c))
		}
		seen[c] = true
	}
}

// sample fills dst with uniform indexes in [0, k), for k from 1 to 256.
//
// limit is the largest multiple of k that fits in a byte's 256 values. Bytes
// below it map onto the k indexes exactly limit/k times each; bytes at or above
// it are dropped rather than folded in, which is what would bias the first
// 256%k indexes. limit is an int on purpose: as a byte it would wrap to 0
// whenever k divides 256, reject everything, and never return.
//
// Reads come in blocks sized for the expected rejection rate, so one read
// almost always suffices; at worst (k = 129) about half the bytes are dropped.
func sample(dst []byte, k int, read func([]byte)) {
	if len(dst) == 0 {
		return
	}

	limit := 256 - 256%k
	buf := make([]byte, len(dst)*256/limit+16)

	for i := 0; i < len(dst); {
		read(buf)
		for _, b := range buf {
			if int(b) >= limit {
				continue
			}
			dst[i] = byte(int(b) % k)
			i++
			if i == len(dst) {
				break
			}
		}
	}
}
