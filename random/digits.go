package random

import "fmt"

// MinDigits is the shortest one-time code [Digits] will make: six decimal
// digits, the floor NIST SP 800-63B-4 section 3.1.3.2 sets for an
// out-of-band secret.
const MinDigits = 6

// Digits returns n uniformly random decimal digits, for a one-time code sent
// by email or SMS. It panics if n is below [MinDigits].
//
// Every position is uniform, leading zeros included: "000417" is exactly as
// likely as "999999". Keep the result a string. Converting it to an integer
// drops the leading zeros, and drawing an integer in [100000, 999999] instead
// throws away a tenth of the code space.
//
// Six digits is only a million values, so generating the code is the easy
// part. The code is only as strong as the way it is stored and checked:
//
//   - Store it hashed with a keyed HMAC (HMAC-SHA256 under a server-held key
//     kept out of the database) or with bcrypt. Never store it in plaintext,
//     and never with aioz-common/secret.HashOf: an unkeyed SHA-256 of a
//     six-digit code is reversed by hashing all million candidates, which
//     takes well under a second.
//   - Accept it at most once, expire it within ten minutes, cap failed
//     attempts atomically in the database, and do not reset that count when a
//     new code is sent.
//   - Compare in constant time, with crypto/hmac.Equal.
//
// The result is a plain string because this package sits below
// aioz-common/secret. Wrap it in a secret.Token before it can reach a log line.
func Digits(n int) string {
	if n < MinDigits {
		panic(
			fmt.Sprintf(
				"random: %d digits is below the %d-digit minimum",
				n,
				MinDigits,
			),
		)
	}
	return String(n, Decimal)
}
