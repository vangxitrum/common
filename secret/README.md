# secret

Mints bearer secrets, and stores only their hashes.

The rule this package exists to enforce is that a secret is never at rest in a
form that can be replayed. Mint a token, hand the plaintext to whoever will
present it, store the `Hash`. A database dump, a backup, or a `SELECT` in a
support session then yields nothing usable.

It exists because there was no shared way to do this, so every service rolled
its own, and the divergence produced real weaknesses: a bearer token stored
verbatim and compared with `bytes.Equal` inside a linear scan; recovery codes
whose comment claimed an unambiguous alphabet that the implementation did not
provide; and three incompatible policies for what to do when the random source
fails. One audited package makes the next security fix a version bump instead
of a hunt through four idioms.

```go
import "gitlab.internal/tuan.quang.tran/aioz-common/secret"
```

## Usage

```go
token, hash := secret.NewToken()

session := Session{UserID: user.ID, TokenHash: hash}
db.CreateSession(ctx, session)

http.SetCookie(w, &http.Cookie{Name: "session", Value: token.Reveal()})
```

Later:

```go
stored := db.SessionByTokenHash(ctx, secret.HashOf(presented))
```

or, where you hold the hash already:

```go
if !secret.Verify(presented, stored.TokenHash) {
    return apierr.Unauthorized(...)
}
```

## Two types, and the split is the design

**`Token` is plaintext.** It renders as `secret.Token(redacted)` through `fmt`,
`slog`, `String` and `GoString`, and it **errors** on `MarshalText`,
`MarshalJSON` and `driver.Valuer`. `Reveal()` is the only way out, so a
reviewer can find every place a secret leaves the process by searching for one
word.

**`Hash` is what you store.** It has the full `MarshalText`/`MarshalJSON`/
`Valuer`/`Scanner` set, because it is safe to store and safe to log.

A secret flows in freely and out only through `Reveal`.

### Why the marshallers error instead of emitting the placeholder

A marshaller that returns the literal string `secret.Token(redacted)` ships a
bug that looks like a successful deploy: the client stores that string as its
token and fails at the *next* request, in a different service, with nothing
pointing back here. An error fails the handler on the first test run, at the
line that is wrong.

`Value()` is the highest-value method in the package. `Token`'s kind is
`string`, so without a `driver.Valuer`, `database/sql`'s default converter
reaches it by reflection and **silently stores the plaintext**. That is
verbatim the bug this package was written to retire, and it is closed by a
method rather than by discipline.

### Two holes redaction cannot close

Both are asserted by tests, so they stay known rather than being rediscovered:

- **An unexported `Token` struct field.** `fmt.Sprintf("%+v", s)` where the
  field is unexported prints the underlying string, because `fmt` cannot call
  methods on unexported fields. There is nothing the type can do.
- **`%p`.** `fmt` handles `%p` and `%T` before it consults `fmt.Formatter`, so
  `Format` never runs and `%p` on a non-pointer falls through to the path that
  reflects the value out. `go vet` does not warn either, and that is worth
  knowing: vet's printf check skips any type implementing `fmt.Formatter`, so
  the very method that closes every other verb is what silences the tool for
  this one. (`%T` goes the same way but only prints the type name.)

## Errors, and where they are not

This package returns an error only when it is **given** something: a prefix, a
string to parse, a column to scan. It never returns one for being asked to
**make** something.

Since Go 1.24, `crypto/rand.Read` cannot return a non-nil error. If the system
entropy source is broken, the runtime terminates the process. So a generator's
error would be provably always nil, and a `(T, error)` like that is worse than
none: every call site grows an untestable branch and reviewers stop reading
them. `uuid.Must` in this module exists for the same reason.

A length below `MinBytes` **panics**, because `n` is a constant at essentially
every call site: it is a programmer error in the same class as a slice bound.

## API keys

```go
key, err := secret.NewKey("aioz")
// key.ID     -> public, indexable, safe to log
// key.Secret -> "aioz_<id><secret><crc>", show once
// key.Hash   -> store this
```

Verification is a parse, one indexed row, one constant-time compare:

```go
ref, err := secret.ParseKey(presented)
stored := db.HashByKeyID(ctx, ref.ID)
ok := secret.Verify(presented, stored)
```

**The embedded id is the point.** Without it, the only possible implementation
is a scan over every stored hash, which is exactly how a linear `bytes.Equal`
over a user's whole key set gets written. With it, lookup is O(1).

**The prefix is the versioning scheme.** It is mandatory and it is inside the
hashed material, so a future scheme mints `aioz2_...` and an old key can never
verify as a new one. Zero bytes added today, no flag day tomorrow. On the
stored side, `Hash.Scheme()` reads a `$name$` prefix and returns `"sha256"` for
a bare 64-hex value, so a future `HashOf` emits `$s3$<hex>` and `Verify`
dispatches on the stored value's own shape. No schema column, no backfill.

**The checksum is a CRC32 and is not a MAC.** Anyone can compute it; it proves
nothing about authenticity. It buys two things: a third-party secret scanner
can tell a real key from a random word with no call to us, so a key leaked into
a public repository gets reported without us running a validation endpoint; and
a mistyped key is rejected before it costs a database round trip. A key that
parses is still unauthenticated.

The body is base62 because *fixed prefix + fixed length + restricted alphabet*
is what makes a high-precision scanner rule possible (`aioz_[0-9A-Za-z]{61}`).
A hex or base64 body is unmatchable.

## Recovery codes

```go
codes, hashes := secret.NewCodes(10)
```

Crockford base32, `XXXX-XXXX-XXXX-XXXX`, and the choice is the whole reason
this is here rather than left to `encoding/base32`.

RFC 4648's *output* alphabet is `A-Z2-7`, so a minted code never contains a `0`
or a `1`, and it is tempting to call that unambiguous. It is unambiguous in one
direction only. It provides **no input folding**: a person who reads `O` off a
printed card and types `0` is rejected, with no way back, at the exact moment
they have already lost their phone. Ambiguity is a *parsing* property, not an
alphabet property. `NormalizeCode` folds `O` to `0` and `I` and `L` to `1`,
uppercases, and drops the dashes and spaces; `VerifyCode` calls it so a caller
cannot forget.

## What this package is not for

**Passwords.** A password needs a slow, tunable hash. Use bcrypt or argon2id in
the service. Nothing here takes a parameter named `password`, and `MinBytes`
refuses any length a person could have typed. Adding `golang.org/x/crypto` here
would also put it in every consumer's build graph for a feature one package in
one service uses.

**Identifiers.** Use [`uuid`](../uuid/README.md). `uuid.New()` is UUIDv7:
about 74 random bits behind a 48-bit millisecond timestamp. It is a database
key, not a bearer token.

**Encryption, JWTs, TOTP, secret storage, expiry and revocation.** Out of
scope, deliberately. A second, subtly different AES helper in a shared library
is how you get two incompatible ciphertexts and no way to tell which produced a
given blob.

## API

```go
const MinBytes = 16
const TokenBytes = 32
const Redacted = "secret.Token(redacted)"
var Error = errs.Tag("secret")

func Bytes(n int) []byte

type Token string
func NewToken() (Token, Hash)
func NewTokenN(n int) (Token, Hash)
func (t Token) Reveal() string
func (t Token) Hash() Hash
func (t Token) IsZero() bool
// String, GoString, Format, LogValue      -> Redacted
// MarshalText, MarshalJSON, Value         -> error
// UnmarshalText, UnmarshalJSON, Scan      -> work

type Hash string
func HashOf(presented string) Hash
func Verify(presented string, stored Hash) bool
func (h Hash) Equal(other Hash) bool
func (h Hash) Scheme() string
func (h Hash) Valid() bool
func (h Hash) IsZero() bool
// String, MarshalText/JSON, UnmarshalText/JSON, Value, Scan

type Key struct { Prefix, ID string; Secret Token; Hash Hash }
type KeyRef struct { Prefix, ID string }
func NewKey(prefix string) (Key, error)
func ParseKey(s string) (KeyRef, error)

func NewCodes(n int) (codes []Token, hashes []Hash)
func NormalizeCode(s string) string
func VerifyCode(typed string, stored Hash) bool
```
