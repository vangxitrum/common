// Package secret mints bearer secrets and stores only their hashes.
//
// The rule the package exists to enforce is that a secret is never at rest in
// a form that can be replayed. A caller mints a token, hands the plaintext to
// whoever will present it, and stores the [Hash]. A database dump, a backup,
// or a SELECT in a support session then yields nothing usable.
//
// Two types divide the work, and the split is visible in their method sets:
//
//   - [Token] is plaintext. It redacts itself in fmt, slog, JSON and SQL, and
//     refuses to marshal or to be written to a column at all. [Token.Reveal]
//     is the only way out.
//   - [Hash] is what you store. It has the full complement of MarshalText,
//     MarshalJSON, driver.Valuer and sql.Scanner, because it is safe to store
//     and safe to log.
//
// A secret flows in freely and out only through Reveal.
//
// This package returns an error only when it is given something - a prefix, a
// string to parse, a column to scan - never when it is asked to make
// something. Since Go 1.24 crypto/rand.Read cannot fail: if the system
// entropy source is broken the runtime terminates the process. So there is no
// error to propagate, and no "fall back to the clock" branch to write.
//
// For an identifier rather than a secret, use aioz-common/uuid. A UUIDv7
// carries about 74 random bits behind a 48-bit millisecond timestamp: it is a
// database key, not a bearer token.
//
// This is also the wrong package for anything a human chose. A password needs
// a slow, tunable hash (bcrypt, argon2id) and belongs in the service that
// authenticates it; nothing here takes a parameter named password, and
// [MinBytes] refuses any length a person could have typed.
package secret

import (
	"crypto/rand"
	"fmt"

	"github.com/zeebo/errs/v2"
)

// Error tags every failure this package produces.
var Error = errs.Tag("secret")

// MinBytes is the entropy floor, 128 bits. Anything below it is a mistake
// rather than a trade-off, so it is refused rather than warned about.
const MinBytes = 16

// Bytes returns n cryptographically random bytes.
//
// It panics if n is below [MinBytes]. n is a constant at essentially every
// call site, so a short length is a programmer error in the same class as a
// slice bound, not a runtime condition a caller can handle.
func Bytes(n int) []byte {
	if n < MinBytes {
		panic(fmt.Sprintf("secret: %d bytes is below the %d-byte minimum", n, MinBytes))
	}
	b := make([]byte, n)
	// crypto/rand.Read terminates the process rather than returning an error,
	// so the only honest thing to do with its result is ignore it.
	_, _ = rand.Read(b)
	return b
}
