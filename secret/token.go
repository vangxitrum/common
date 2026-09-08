package secret

import (
	"database/sql"
	"database/sql/driver"
	"encoding"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
)

// Redacted is what a Token renders as anywhere other than [Token.Reveal].
//
// It is a fixed string on purpose: it is greppable, so a log pipeline can
// alert on a secret having reached a log line at all.
const Redacted = "secret.Token(redacted)"

// TokenBytes is the entropy of a token from [NewToken]: 32 bytes, so guessing
// stays infeasible where a token is the only thing between the internet and a
// session and an attacker can guess without limit.
const TokenBytes = 32

// Token is a bearer secret in plaintext: a session, a CSRF token, an invite, a
// password reset, a recovery code, or the presentable half of a [Key].
//
// It renders as [Redacted] through fmt (every verb, via fmt.Formatter), slog,
// String and GoString. It refuses to marshal to JSON or text and refuses to be
// a SQL parameter, each with an error rather than a placeholder: a placeholder
// in a response body is a bug that ships looking like a working deploy and
// fails later, in another service, with nothing pointing back here.
//
// The refusal to be a SQL parameter is the load-bearing one. Token's kind is
// string, so without a driver.Valuer, database/sql's default converter reaches
// it by reflection and silently stores the plaintext.
//
// [Token.Reveal] is the only way to the plaintext, and it is meant to be
// conspicuous at review.
//
// Two leaks redaction cannot close, both covered by tests so they stay known:
// fmt printing an *unexported* Token field of a struct, because fmt cannot
// call methods on unexported fields; and %p, because fmt handles it before it
// consults [Token.Format]. See the package README.
type Token string

var (
	_ fmt.Formatter            = Token("")
	_ fmt.Stringer             = Token("")
	_ fmt.GoStringer           = Token("")
	_ slog.LogValuer           = Token("")
	_ json.Marshaler           = Token("")
	_ encoding.TextMarshaler   = Token("")
	_ driver.Valuer            = Token("")
	_ json.Unmarshaler         = (*Token)(nil)
	_ encoding.TextUnmarshaler = (*Token)(nil)
	_ sql.Scanner              = (*Token)(nil)
)

// NewToken mints a token with [TokenBytes] of entropy and returns it with the
// [Hash] to store. The plaintext is never returned again.
func NewToken() (Token, Hash) { return NewTokenN(TokenBytes) }

// NewTokenN mints a token with n bytes of entropy. It panics if n is below
// [MinBytes].
func NewTokenN(n int) (Token, Hash) {
	t := Token(hex.EncodeToString(Bytes(n)))
	return t, t.Hash()
}

// Reveal returns the plaintext.
//
// It is the only way out, so a reviewer can find every place a secret leaves
// the process by searching for this one word.
func (t Token) Reveal() string { return string(t) }

// Hash returns the stored form, so the common case never needs Reveal.
func (t Token) Hash() Hash { return HashOf(string(t)) }

// IsZero reports whether t is the zero value.
func (t Token) IsZero() bool { return t == "" }

// String returns [Redacted].
func (t Token) String() string { return Redacted }

// GoString returns [Redacted].
func (t Token) GoString() string { return Redacted }

// Format writes [Redacted] for every verb.
//
// fmt consults fmt.Stringer for %v, %s, %q, %x and %X only. Any other verb,
// %d among them, takes fmt's badVerb path, which reflects into the value and
// prints the underlying string. Implementing fmt.Formatter is what actually
// closes fmt.
func (t Token) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, Redacted)
}

// LogValue returns [Redacted], so slog cannot log the plaintext.
func (t Token) LogValue() slog.Value { return slog.StringValue(Redacted) }

// MarshalText always fails. Marshal the [Hash] instead, or call
// [Token.Reveal] explicitly at the one place the secret is handed over.
func (t Token) MarshalText() ([]byte, error) {
	return nil, Error.Errorf(
		"refusing to marshal a Token; store its Hash, or call Reveal to hand the secret over once",
	)
}

// MarshalJSON always fails, for the same reason as [Token.MarshalText].
func (t Token) MarshalJSON() ([]byte, error) {
	return nil, Error.Errorf(
		"refusing to marshal a Token; store its Hash, or call Reveal to hand the secret over once",
	)
}

// Value always fails: a Token must never reach a column. Store its [Hash].
func (t Token) Value() (driver.Value, error) {
	return nil, Error.Errorf(
		"refusing to use a Token as a SQL parameter; store its Hash instead",
	)
}

// UnmarshalText implements encoding.TextUnmarshaler. A secret flows in freely.
func (t *Token) UnmarshalText(b []byte) error {
	*t = Token(b)
	return nil
}

// UnmarshalJSON implements json.Unmarshaler.
func (t *Token) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return Error.Wrap(err)
	}
	*t = Token(s)
	return nil
}

// Scan implements sql.Scanner, so a token presented from a column or a driver
// can be read back in. Writing one out is what Value refuses.
func (t *Token) Scan(value any) error {
	switch v := value.(type) {
	case nil:
		*t = ""
	case string:
		*t = Token(v)
	case []byte:
		*t = Token(v)
	default:
		return Error.Errorf("cannot scan %T into a Token", value)
	}
	return nil
}
