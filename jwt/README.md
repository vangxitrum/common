# jwt

Signs and verifies JSON Web Tokens with a pinned algorithm, for a shared
secret or a key pair.

```go
import "gitlab.internal/tuan.quang.tran/aioz-common/jwt"
```

## Read this before reaching for it

A JWT is the opposite of the credentials in [`secret`](../secret/README.md).
Those are **opaque**: the string carries no information, the server decides
what it means on every request, and revoking one is a `DELETE`. A JWT is
**self-describing**: it carries signed claims the verifier trusts without a
lookup. That is the whole point of it, and it is also where its entire problem
class lives, because a token nobody looks up is a token nobody can take back.

So use this when the verifier genuinely cannot reach the issuer's database: a
third party, an edge worker, another company's service. When both ends already
talk to the same coordinator on every request, an opaque token from `secret` is
strictly better - same cost, instant revocation, nothing to get wrong.

## Usage

### A shared secret, inside one trust domain

```go
key := secret.Bytes(32)

signer, err := jwt.NewHMACSigner(jwt.HS256, key,
    jwt.WithTTL(15*time.Minute),
    jwt.WithIssuer("coord"),
    jwt.WithAudience("edgeserver"),
)

token, err := signer.Sign(ctx, jwt.Claims{Subject: "user-1"})
// token is a secret.Token: hand it over with token.Reveal()
```

```go
verifier, err := jwt.NewHMACVerifier(jwt.HS256, key,
    jwt.RequireIssuer("coord"),
    jwt.RequireAudience("edgeserver"),
)

claims, err := verifier.Verify(ctx, presented)
```

Both sides hold the same key, so **anyone who can verify can also mint**. That
is the reason to stop at the edge of one trust domain.

### A key pair, across a trust boundary

```go
public, private, _ := ed25519.GenerateKey(rand.Reader)

signer, err := jwt.NewKeySigner(jwt.EdDSA, private, jwt.WithTTL(time.Hour))
verifier, err := jwt.NewKeyVerifier(jwt.EdDSA, public)
```

The verifier holds only the public half, so it can check a token but not mint
one. `EdDSA` is the best default for anything new: small keys, fast, and no
parameter choices to get wrong. `ES*` is the next choice; prefer `PS*` over
`RS*` if you must use RSA.

## What the package refuses to let you get wrong

**The algorithm is pinned at construction and the header's `alg` is never
trusted.** This is not a preference. Given a token you signed with RS256, an
attacker re-signs their own claims with HS256 using your **public** key as the
HMAC secret. A verifier that reads `alg` from the header and picks a method
accordingly holds that public key, so it accepts the forgery. The same hole
accepts `alg: "none"`.

Here the algorithm comes from the constructor, a token whose header disagrees
is rejected before any signature work, and `none` is not a value this package
can express. `TestAlgorithmConfusion` performs the real attack; a companion
check confirms the same forgery is accepted by a naive verifier, so the test is
not passing vacuously.

**An expiry is required on both sides.** A JWT cannot be revoked before it
expires, so one without `exp` is a permanent credential that outlives the
subject being deleted. Opting out is spelled `AllowTokensThatNeverExpire` and
`AcceptTokensThatNeverExpire`, in full, so it is legible in a review and
unreachable by leaving a field at its zero value.

**An HMAC secret must be at least as long as the hash it feeds** - 32 bytes for
HS256, 48 for HS384, 64 for HS512. An HMAC secret is guessed offline against a
single captured token, with no rate limit and nobody watching. A short one is a
password, and this is not the package for passwords. `GenerateSecret(alg)`
returns one of exactly the right length, and refuses an algorithm that has no
shared secret; `aiozkey jwt secret` (see [`tools/aiozkey`](../tools/aiozkey/README.md))
prints one for a secret store.

**A signed token is a `secret.Token`**, so it cannot reach a log line or a
database column by accident. `Reveal()` at the one place it is handed over.

**A mismatched key type fails at construction**, not on a live request, so
wiring an Ed25519 key into an RS256 verifier is a startup failure rather than a
500 in production.

**An `Extra` claim cannot shadow a registered one.** `iss`, `sub`, `aud`,
`exp`, `nbf`, `iat` and `jti` carry meaning the verifier acts on, so writing
one through the map is refused rather than silently overwriting it.

## Set an audience

`RequireAudience` is optional in the API and not optional in practice. Without
it, a token minted for one of your services is accepted by every other service
that trusts the same issuer, which turns a token leak in the least important
service into a compromise of the most important one.

## Type your tokens

```go
signer, err := jwt.NewKeySigner(jwt.EdDSA, private,
    jwt.WithTTL(15*time.Minute),
    jwt.WithType(jwt.TypeAccessToken),       // "typ": "at+jwt"
)
verifier, err := jwt.NewKeyVerifier(jwt.EdDSA, public,
    jwt.RequireAudience("api"),
    jwt.RequireType(jwt.TypeAccessToken),
)
```

A signature proves who minted a token, not what it was minted for. An issuer
that signs ID tokens, refresh tokens or internal tokens with the same key as
its access tokens hands an attacker a supply of validly signed tokens that
carry every claim an access token needs; without a type check, an API accepts
any of them (RFC 8725 §3.11). `RequireType` refuses a token whose `typ` header
is missing or different. `TypeAccessToken` is the `at+jwt` type of RFC 9068,
the JWT profile for OAuth 2.0 access tokens.

The comparison ignores case and an `application/` prefix on either side, as RFC
7515 §4.1.9 allows, so `at+jwt` and `application/AT+JWT` are the same type.
Without `WithType` every token says `JWT`, which is what every other JWT
library stamps too, and is exactly the value an access-token verifier should
refuse. A `Ring` member built with `RequireType` enforces it through the ring.

## Rotating a key

Keep the keys as PEM files in one directory and load them with
`LoadKeyRing`. The file names are the only configuration: one names the
active key, and every key in the directory verifies.

```go
keys, err := jwt.LoadKeyRing("/etc/coord/jwt", cfg.ActiveKey, jwt.KeyRingOptions{
    Signer:   []jwt.SignerOption{jwt.WithTTL(15 * time.Minute), jwt.WithType(jwt.TypeAccessToken)},
    Verifier: []jwt.VerifierOption{jwt.RequireAudience("api"), jwt.RequireType(jwt.TypeAccessToken)},
})
for _, name := range keys.Skipped() {
    slog.Warn("not a key file", "name", name)
}
for _, k := range keys.Keys() {
    if k.LoosePermissions {
        slog.Warn("private key is readable by group or other", "path", k.Path)
    }
}

token, err := keys.Signer().Sign(ctx, jwt.Claims{Subject: "user-1"})
claims, err := keys.Ring().Verify(ctx, presented)
```

The directory is flat:

| File | What it is |
| --- | --- |
| `<name>.pem` | a private key, PKCS#8 or RSA PKCS#1. It signs when it is the active key, and its public half verifies either way. |
| `<name>.pub.pem` | a public key only, PKIX or RSA PKCS#1. It verifies and never signs. |
| `.*` | ignored, which covers the `..data` symlinks of a Kubernetes secret mount. |
| anything else | ignored, and listed in `Skipped()` so a key saved as `new.key` by mistake is not silently missing. |

Keys are Ed25519 (`EdDSA`) or RSA of at least 2048 bits (`RS256`); anything
else fails the load, naming the file. So does a missing directory, an empty
one, an active name with no `<name>.pem`, and a `<name>.pem` and
`<name>.pub.pem` that hold different keys.

A rotation is three steps, each a file change and a restart, and each one
finished on every replica before the next begins:

1. **Publish.** Add `new.pem` next to `old.pem`, still active `old`. Every
   replica now verifies tokens from the new key, though none mints them yet.
2. **Switch.** Change the active name to `new`. Replicas mint with the new key,
   and tokens minted under the old one keep verifying, because `old.pem` is
   still in the directory.
3. **Retire.** Once the longest TTL has passed since the switch, delete
   `old.pem`. Its tokens are now refused as an unknown kid.

The order is the point. Switching before publishing means a replica that has
not restarted yet refuses every token the new key signs. Retiring before the
TTL has passed logs out everyone holding a token from the old key. To stop
signing with a key while its tokens still verify - it is leaving a machine,
say - replace `old.pem` with `old.pub.pem` and delete that at the end of the
TTL instead.

Two things about the `kid`:

- **It is the key's RFC 7638 thumbprint**, not a name anyone chose. Every
  replica computes the same kid for the same key with no coordination, a
  renamed or copied file is still the same key (and merges into one ring
  member), and two different keys cannot collide. The file name is for people,
  the kid is for tokens.
- **A kid is routing, never trust.** `Ring.Verify` reads the header without
  verifying it, because the key needed to verify is what the header selects.
  That is safe only because a kid chooses among keys already trusted: an
  unknown kid is rejected, and the chosen verifier still checks the signature
  and the pinned algorithm. Naming a known kid while signing with another key
  gets nowhere, and a test asserts it.

Keys are made by the service, not by hand with `openssl`: ship a `genkey`
command built on `GenerateKey`, `WritePrivateKey` and `WritePublicKey`. It
writes `<name>.pem` with mode 0600 and `<name>.pub.pem` with 0644, refuses to
overwrite an existing file, since that file may be the only copy of a key
whose tokens are still in flight, and refuses a name `LoadKeyRing` would not
read back. [`aiozkey jwt keygen`](../tools/aiozkey/README.md) is that command,
ready-built, for a service that has no `genkey` of its own.

```go
key, err := jwt.GenerateKey(jwt.KeyTypeEd25519, 0)
info, err := jwt.WritePrivateKey(dir, "2026-09", key)
_, err = jwt.WritePublicKey(publicDir, "2026-09", key.Public())
fmt.Println(info.Path, info.ID)
```

`KeyRing.DeriveSecret(info)` turns the active key into 32 bytes of HKDF-SHA256
output, for a service that needs a symmetric secret - a cookie key, a CSRF key
- and would rather not provision a second one. It is the same on every replica
and changes when the active key does, so whatever it protects rotates with the
signing key. Use a different `info` for each purpose.

Rings built by hand still work for keys that do not live in files:

```go
ring, err := jwt.NewRing(oldVerifier, newVerifier)  // each with ExpectKeyID
```

`NewRing` requires a kid on every member rather than falling back to "try them
all", because a ring that tries every key accepts a token signed by any key it
has ever known and hides the fact that the old one was never retired. Set a kid
from the first day: adding one later means every already-issued token lacks
it, so the rotation you wanted the header for is exactly the moment it is not
there.

## Testing

`Sign` and `Verify` take a `context.Context` and read the clock from it through
[`time2`](../time2/README.md), so a test expires a token by advancing a time
machine rather than by sleeping:

```go
ctx, machine := time2.WithNewMachine(context.Background())
token, _ := signer.Sign(ctx, claims)

machine.Advance(2 * time.Hour)
_, err := verifier.Verify(ctx, token.Reveal())  // expired
```

## Not in scope

No JWKS endpoint or fetcher, no refresh-token flow, no revocation list, no
encryption (JWE). Key material comes from the caller, either as keys it already
holds or as a directory of PEM files for `LoadKeyRing`.

## API

```go
type Algorithm string
const (
    HS256, HS384, HS512               // shared secret
    RS256, RS384, RS512               // RSA PKCS#1 v1.5
    PS256, PS384, PS512               // RSA PSS
    ES256, ES384, ES512               // ECDSA
    EdDSA                             // Ed25519
)

type Claims struct {
    Issuer, Subject, ID string
    Audience            []string
    IssuedAt, NotBefore, ExpiresAt time.Time
    Extra map[string]any
}
func (c Claims) Get(name string) (any, bool)
func (c Claims) GetString(name string) (string, bool)

func NewHMACSigner(alg Algorithm, key []byte, opts ...SignerOption) (*Signer, error)
func NewKeySigner(alg Algorithm, key crypto.PrivateKey, opts ...SignerOption) (*Signer, error)
func (s *Signer) Sign(ctx context.Context, claims Claims) (secret.Token, error)

func NewHMACVerifier(alg Algorithm, key []byte, opts ...VerifierOption) (*Verifier, error)
func NewKeyVerifier(alg Algorithm, key crypto.PublicKey, opts ...VerifierOption) (*Verifier, error)
func (v *Verifier) Verify(ctx context.Context, token string) (Claims, error)

func NewRing(verifiers ...*Verifier) (*Ring, error)
func (r *Ring) Verify(ctx context.Context, token string) (Claims, error)

const TypeAccessToken = "at+jwt"

type KeyInfo struct {
    Name, ID, Path   string
    Alg              Algorithm
    Private, Active  bool
    LoosePermissions bool
}
type KeyRingOptions struct {
    Signer   []SignerOption
    Verifier []VerifierOption
}
func LoadKeyRing(dir, active string, opts KeyRingOptions) (*KeyRing, error)
func (k *KeyRing) Signer() *Signer
func (k *KeyRing) Ring() *Ring
func (k *KeyRing) Keys() []KeyInfo
func (k *KeyRing) Active() KeyInfo
func (k *KeyRing) Skipped() []string
func (k *KeyRing) DeriveSecret(info string) []byte

func Thumbprint(pub crypto.PublicKey) (string, error)
func AlgorithmFor(pub crypto.PublicKey) (Algorithm, error)
func ParsePrivateKeyPEM(b []byte) (crypto.Signer, error)
func ParsePublicKeyPEM(b []byte) (crypto.PublicKey, error)

type KeyType string
const KeyTypeEd25519, KeyTypeRSA KeyType
func GenerateKey(kind KeyType, rsaBits int) (crypto.Signer, error)
func GenerateSecret(alg Algorithm) ([]byte, error)
func WritePrivateKey(dir, name string, key crypto.Signer) (KeyInfo, error)
func WritePublicKey(dir, name string, pub crypto.PublicKey) (KeyInfo, error)

// SignerOption:   WithKeyID, WithType, WithIssuer, WithAudience, WithTTL,
//                 AllowTokensThatNeverExpire
// VerifierOption: ExpectKeyID, RequireType, RequireIssuer, RequireAudience,
//                 WithLeeway, AcceptTokensThatNeverExpire
```
