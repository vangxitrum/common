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
password, and this is not the package for passwords.

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

## Rotating a key

```go
ring, err := jwt.NewRing(oldVerifier, newVerifier)  // each with ExpectKeyID
claims, err := ring.Verify(ctx, presented)
```

Publish the new key to every verifier first, then start signing with it. Tokens
minted under the old key keep verifying until they expire, and only then is the
old key removed. Without a ring, rotation invalidates every token already in
flight.

Two things about the `kid`:

- **Set it from the first day.** Adding one later means every already-issued
  token lacks it, so the rotation you wanted the header for is exactly the
  moment it is not there.
- **A kid is routing, never trust.** `Ring.Verify` reads the header without
  verifying it, because the key needed to verify is what the header selects.
  That is safe only because a kid chooses among keys already trusted: an
  unknown kid is rejected, and the chosen verifier still checks the signature
  and the pinned algorithm. Naming a known kid does not skip anything, and a
  test asserts it.

`NewRing` requires a kid on every member rather than falling back to "try them
all", because a ring that tries every key accepts a token signed by any key it
has ever known and hides the fact that the old one was never retired.

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
encryption (JWE). Key material comes from the caller, in whatever form it
already stores keys.

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

// SignerOption:   WithKeyID, WithIssuer, WithAudience, WithTTL,
//                 AllowTokensThatNeverExpire
// VerifierOption: ExpectKeyID, RequireIssuer, RequireAudience, WithLeeway,
//                 AcceptTokensThatNeverExpire
```
