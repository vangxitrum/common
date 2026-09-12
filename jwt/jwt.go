// Package jwt signs and verifies JSON Web Tokens with a pinned algorithm.
//
// A JWT is the opposite of the credentials in aioz-common/secret. Those are
// opaque: the string carries no information, the server decides what it means
// on every request, and revoking one is a DELETE. A JWT is self-describing: it
// carries signed claims the verifier trusts without a lookup. That is the
// whole point of it, and it is also where its entire problem class lives.
//
// So reach for this only when the verifier genuinely cannot reach the issuer's
// database - a third party, an edge worker, another company's service. When
// both ends already talk to the same coordinator on every request, an opaque
// token from aioz-common/secret is strictly better: same cost, instant
// revocation, and nothing to get wrong.
//
// # What this package refuses to let you get wrong
//
// The algorithm is pinned at construction and the header's "alg" is never
// trusted. This is not a preference. A verifier that believes the header is
// vulnerable to algorithm confusion: an attacker takes a token you signed with
// RS256, re-signs it with HS256 using your PUBLIC key as the HMAC secret, and
// a naive verifier - which has that public key - accepts it. The same hole
// accepts alg "none". Here the algorithm comes from the constructor, a token
// whose header disagrees is rejected before any signature work, and "none" is
// not a value this package can express.
//
// An expiry is required, on both sides, unless you opt out in writing. A JWT
// cannot be revoked before it expires, so one without "exp" is a permanent
// credential.
//
// An HMAC key must be at least as long as the hash it feeds. A short shared
// secret is brute-forceable offline against a single captured token.
//
// A signed token comes back as a secret.Token, so it cannot reach a log line
// or a database column by accident. Call Reveal to hand it over.
package jwt

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"

	"github.com/zeebo/errs/v2"
)

// Error tags every failure this package produces.
var Error = errs.Tag("jwt")

// Algorithm is a JWS signing algorithm.
//
// The "none" algorithm is deliberately absent: it is not a signing algorithm,
// it is the absence of one, and every JWT vulnerability list starts with a
// verifier that accepted it.
type Algorithm string

// The algorithms this package supports, grouped by the key they take.
const (
	// HMAC with a shared secret. Both sides hold the same key, so the
	// verifier can also mint tokens. Use these only inside one trust domain.
	HS256 Algorithm = "HS256"
	HS384 Algorithm = "HS384"
	HS512 Algorithm = "HS512"

	// RSA PKCS#1 v1.5. The verifier holds only the public key.
	RS256 Algorithm = "RS256"
	RS384 Algorithm = "RS384"
	RS512 Algorithm = "RS512"

	// RSA PSS, the modern padding. Prefer these over RS* for a new system.
	PS256 Algorithm = "PS256"
	PS384 Algorithm = "PS384"
	PS512 Algorithm = "PS512"

	// ECDSA. Shorter keys and signatures than RSA at equivalent strength.
	ES256 Algorithm = "ES256"
	ES384 Algorithm = "ES384"
	ES512 Algorithm = "ES512"

	// EdDSA over Ed25519. The best default for a new asymmetric deployment:
	// small keys, fast, and no parameter choices to get wrong.
	EdDSA Algorithm = "EdDSA"
)

// TypeAccessToken is the "typ" header of an OAuth 2.0 access token in the JWT
// profile of RFC 9068.
//
// Stamp it with [WithType] and require it with [RequireType]. A service that
// accepts more than one kind of token from the same issuer - an access token
// and an ID token, say - otherwise has no way to tell them apart, and a token
// minted for one purpose is replayed for the other (RFC 8725 section 3.11).
const TypeAccessToken = "at+jwt"

// IsHMAC reports whether a takes a shared secret rather than a key pair.
func (a Algorithm) IsHMAC() bool {
	switch a {
	case HS256, HS384, HS512:
		return true
	default:
		return false
	}
}

// String returns the algorithm name as it appears in a JWS header.
func (a Algorithm) String() string { return string(a) }

// Valid reports whether a is an algorithm this package supports.
func (a Algorithm) Valid() bool {
	_, ok := algorithms[a]
	return ok
}

// minSecretLen is the shortest shared secret each HMAC algorithm accepts: the
// output size of its hash.
//
// RFC 7518 requires a key at least as long as the hash output, and the reason
// is concrete rather than ceremonial. An HMAC secret is verified offline: an
// attacker with one captured token can guess against it as fast as hardware
// allows, with no rate limit and nobody watching. A short secret is a
// password, and this is not the package for passwords.
var algorithms = map[Algorithm]struct {
	minSecretLen int
	keyType      string
}{
	HS256: {minSecretLen: 32},
	HS384: {minSecretLen: 48},
	HS512: {minSecretLen: 64},

	RS256: {keyType: "RSA"},
	RS384: {keyType: "RSA"},
	RS512: {keyType: "RSA"},
	PS256: {keyType: "RSA"},
	PS384: {keyType: "RSA"},
	PS512: {keyType: "RSA"},

	ES256: {keyType: "ECDSA"},
	ES384: {keyType: "ECDSA"},
	ES512: {keyType: "ECDSA"},

	EdDSA: {keyType: "Ed25519"},
}

// checkKeyType reports whether key is the kind of key alg needs.
//
// It exists because the underlying library reports a mismatched key only when
// it comes to sign or verify, which turns a wiring mistake into a runtime
// error on a live request. Catching it in the constructor turns it into a
// startup failure instead.
func checkKeyType(alg Algorithm, key crypto.PublicKey) error {
	want := algorithms[alg].keyType

	var got string
	switch key.(type) {
	case *rsa.PublicKey, *rsa.PrivateKey:
		got = "RSA"
	case *ecdsa.PublicKey, *ecdsa.PrivateKey:
		got = "ECDSA"
	case ed25519.PublicKey, ed25519.PrivateKey:
		got = "Ed25519"
	default:
		return Error.Errorf("%s needs an %s key, got %T", alg, want, key)
	}

	if got != want {
		return Error.Errorf("%s needs an %s key, got %s", alg, want, got)
	}
	return nil
}
