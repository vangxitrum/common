package secret

import (
	"crypto/sha256"
	"crypto/subtle"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// SchemeSHA256 names the hash scheme a bare 64-character hex value uses.
const SchemeSHA256 = "sha256"

// Hash is the stored form of a secret: lowercase hex SHA-256. Safe to store,
// safe to log, safe to put in an error message.
//
// SHA-256 and unsalted, and that is deliberate. bcrypt's slowness defends a
// low-entropy secret against offline guessing, and a salt defends against a
// rainbow table; a 256-bit random input has no guessing attack and no table.
// Running bcrypt on every request would make session lookup the slowest part
// of the API for no gain. What is required here is only one-wayness.
//
// This is the wrong type for anything a human chose. Use bcrypt or argon2id in
// the service for that.
type Hash string

// HashOf returns the stored form of a presented secret.
func HashOf(presented string) Hash {
	sum := sha256.Sum256([]byte(presented))
	return Hash(hex.EncodeToString(sum[:]))
}

// Verify reports whether presented hashes to stored, in constant time.
//
// It is the single entry point for checking a secret, so that a future hash
// scheme has exactly one place to dispatch from. An unrecognised scheme
// returns false; it never panics and never returns true.
func Verify(presented string, stored Hash) bool {
	switch stored.Scheme() {
	case SchemeSHA256:
		return HashOf(presented).Equal(stored)
	default:
		return false
	}
}

// Equal reports whether two hashes match, in constant time.
//
// For comparing two values that are already stored, such as a loop over a set
// of recovery-code hashes. Prefer [Verify] when one side is a presented
// secret: it is where scheme dispatch lives.
func (h Hash) Equal(other Hash) bool {
	return subtle.ConstantTimeCompare([]byte(h), []byte(other)) == 1
}

// Scheme reports which hash scheme produced h.
//
// A bare 64-character hex value is [SchemeSHA256], which is what every value
// written today looks like and what depin's existing columns already hold. A
// future scheme prefixes its name, as "$name$<digest>", so the stored value
// says how to check it and no schema column or backfill is needed to move.
func (h Hash) Scheme() string {
	s := string(h)
	if strings.HasPrefix(s, "$") {
		if end := strings.Index(s[1:], "$"); end > 0 {
			return s[1 : 1+end]
		}
		return ""
	}
	if isLowerHex(s) && len(s) == sha256.Size*2 {
		return SchemeSHA256
	}
	return ""
}

// Valid reports whether h is a hash this package can check.
func (h Hash) Valid() bool { return h.Scheme() != "" }

// IsZero reports whether h is the zero value.
func (h Hash) IsZero() bool { return h == "" }

// String returns the hash. A hash is safe to print: that is the point of
// storing one.
func (h Hash) String() string { return string(h) }

// MarshalText implements encoding.TextMarshaler.
func (h Hash) MarshalText() ([]byte, error) { return []byte(h), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (h *Hash) UnmarshalText(b []byte) error {
	*h = Hash(b)
	return nil
}

// MarshalJSON implements json.Marshaler.
func (h Hash) MarshalJSON() ([]byte, error) { return json.Marshal(string(h)) }

// UnmarshalJSON implements json.Unmarshaler.
func (h *Hash) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return Error.Wrap(err)
	}
	*h = Hash(s)
	return nil
}

// Value implements driver.Valuer.
func (h Hash) Value() (driver.Value, error) { return string(h), nil }

// Scan implements sql.Scanner.
func (h *Hash) Scan(value any) error {
	switch v := value.(type) {
	case nil:
		*h = ""
	case string:
		*h = Hash(v)
	case []byte:
		*h = Hash(v)
	default:
		return Error.Errorf("cannot scan %T into a Hash", value)
	}
	return nil
}

// isLowerHex reports whether s is non-empty and made only of 0-9a-f.
func isLowerHex(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
