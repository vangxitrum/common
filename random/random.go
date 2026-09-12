// Package random produces uniformly distributed values from the operating
// system's CSPRNG, for anything an attacker must not be able to guess: codes,
// keys, nonces, identifiers that double as credentials.
//
// It exists because the two obvious ways to do this are both wrong. math/rand
// is predictable - seeded from the clock it is worse than predictable - so this
// module's linter refuses it outside the two retry-jitter sites. And the
// obvious conversion from a random byte to one of k symbols, b%k, is biased
// whenever k does not divide 256. For base62, 256 = 4*62 + 8: the byte values
// 248..255 wrap onto the first eight characters, which then have five byte
// values each against four for the other 54, so each of them comes up 25% more
// often. Everything here samples by rejection instead - those leftover bytes
// are dropped and drawn again - so every value in range is exactly equally
// likely.
//
// Nothing here returns an error. Since Go 1.24 crypto/rand.Read cannot fail: if
// the system entropy source is broken the runtime terminates the process. So
// there is no error to propagate and no "fall back to the clock" branch to
// write. A call that cannot be right - a negative length, an alphabet with a
// duplicate - panics, like an out-of-range slice index.
//
// There is no entropy floor on these primitives, because a short random string
// is sometimes exactly what is wanted. Floors belong to the purpose-named APIs
// built on them: [Digits] for one-time codes, and aioz-common/secret for bearer
// secrets, which refuses anything under 128 bits. This package must never
// import secret; secret imports it.
package random

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
)

// Bytes returns n bytes from crypto/rand. It panics if n is negative.
//
// For a bearer secret use aioz-common/secret instead, which enforces a floor
// and returns a type that cannot be logged.
func Bytes(n int) []byte {
	if n < 0 {
		panic(fmt.Sprintf("random: negative length %d", n))
	}
	b := make([]byte, n)
	readRand(b)
	return b
}

// IntN returns a uniformly random int in [0, n). It panics if n is not
// positive.
//
// It is the replacement for math/rand's IntN wherever the result matters: a
// random index, a random pick from a list an attacker can see.
func IntN(n int) int {
	if n <= 0 {
		panic(fmt.Sprintf("random: IntN bound %d is not positive", n))
	}
	return int(uint64n(uint64(n), readRand))
}

// readRand fills b from crypto/rand.
func readRand(b []byte) {
	// crypto/rand.Read terminates the process rather than returning an error,
	// so the only honest thing to do with its result is ignore it.
	_, _ = rand.Read(b)
}

// uint64n returns a uniform value in [0, n), reading 8 bytes at a time.
//
// The values 0 .. 2^64 mod n - 1 are the incomplete last block of n, so they
// are redrawn; what remains is an exact multiple of n values, and v%n is then
// uniform. -n%n computes 2^64 mod n in uint64 arithmetic: -n wraps to 2^64-n,
// which has the same remainder.
//
// read is a parameter rather than a package variable so a test can script it
// without there being anything to swap at runtime.
func uint64n(n uint64, read func([]byte)) uint64 {
	reject := -n % n

	var buf [8]byte
	for {
		read(buf[:])
		if v := binary.LittleEndian.Uint64(buf[:]); v >= reject {
			return v % n
		}
	}
}
