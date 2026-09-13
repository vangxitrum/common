package jwt

import (
	"context"
	"crypto"
	"time"

	"gitlab.internal/tuan.quang.tran/aioz-common/secret"
	"gitlab.internal/tuan.quang.tran/aioz-common/time2"

	gojwt "github.com/golang-jwt/jwt/v5"
)

// Signer mints tokens with one key and one algorithm.
//
// It is safe for concurrent use.
type Signer struct {
	alg   Algorithm
	key   any
	keyID string
	typ   string

	issuer        string
	audience      []string
	ttl           time.Duration
	allowNoExpiry bool
}

// SignerOption configures a Signer.
type SignerOption func(*Signer)

// WithKeyID sets the "kid" header, which tells a verifier which key to use.
//
// Set it as soon as there is any chance of a second key existing. Adding a
// "kid" later means every already-issued token lacks one, so the rotation you
// wanted the header for is exactly the moment it is not there.
func WithKeyID(id string) SignerOption {
	return func(s *Signer) { s.keyID = id }
}

// WithType sets the "typ" header, which says what kind of token this is.
//
// Use [TypeAccessToken] for an access token. Without an explicit type every
// token this package mints says "JWT", so a verifier cannot tell an access
// token from any other token the same key signs; see [RequireType].
func WithType(typ string) SignerOption {
	return func(s *Signer) { s.typ = typ }
}

// WithIssuer sets the "iss" claim on every token this Signer mints.
func WithIssuer(issuer string) SignerOption {
	return func(s *Signer) { s.issuer = issuer }
}

// WithAudience sets the "aud" claim on every token this Signer mints.
func WithAudience(audience ...string) SignerOption {
	return func(s *Signer) { s.audience = audience }
}

// WithTTL sets how long a minted token stays valid, when the claims do not
// carry an explicit ExpiresAt.
//
// Keep it short. A JWT cannot be revoked before it expires, so the TTL is
// exactly the window in which a stolen token still works.
func WithTTL(d time.Duration) SignerOption {
	return func(s *Signer) { s.ttl = d }
}

// AllowTokensThatNeverExpire lets this Signer mint a token with no "exp".
//
// Such a token is a permanent credential: it cannot be revoked, and it stays
// valid after the subject is deleted. The option is spelled out in full so
// that it is legible in a code review, and so that nobody reaches it by
// leaving a field at its zero value.
func AllowTokensThatNeverExpire() SignerOption {
	return func(s *Signer) { s.allowNoExpiry = true }
}

// NewHMACSigner returns a Signer that signs with a shared secret.
//
// Both sides hold the same key, so anyone who can verify can also mint. Use
// this only within one trust domain; across a boundary use [NewKeySigner],
// where the verifier holds only a public key.
//
// The secret must be at least as long as the algorithm's hash output, because
// it is guessable offline from a single captured token. secret.Bytes is a
// good way to produce one.
func NewHMACSigner(alg Algorithm, key []byte, opts ...SignerOption) (*Signer, error) {
	if !alg.IsHMAC() {
		return nil, Error.Errorf("%s is not an HMAC algorithm; use NewKeySigner", alg)
	}
	if min := algorithms[alg].minSecretLen; len(key) < min {
		return nil, Error.Errorf(
			"%s needs a secret of at least %d bytes, got %d", alg, min, len(key))
	}

	return newSigner(alg, key, opts...), nil
}

// NewKeySigner returns a Signer that signs with a private key, so verifiers
// need only the matching public key.
//
// This is what to use across a trust boundary: a verifier holding the public
// key can check a token but cannot mint one.
func NewKeySigner(alg Algorithm, key crypto.PrivateKey, opts ...SignerOption) (*Signer, error) {
	if alg.IsHMAC() {
		return nil, Error.Errorf("%s takes a shared secret; use NewHMACSigner", alg)
	}
	if !alg.Valid() {
		return nil, Error.Errorf("unknown algorithm %q", alg)
	}
	if err := checkKeyType(alg, key); err != nil {
		return nil, err
	}

	return newSigner(alg, key, opts...), nil
}

func newSigner(alg Algorithm, key any, opts ...SignerOption) *Signer {
	s := &Signer{alg: alg, key: key}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Algorithm returns the algorithm this Signer signs with.
func (s *Signer) Algorithm() Algorithm { return s.alg }

// KeyID returns the "kid" this Signer stamps, or "" if it sets none.
func (s *Signer) KeyID() string { return s.keyID }

// Sign returns a signed token for claims.
//
// The result is a secret.Token, so it cannot reach a log line or a database
// column by accident: call Reveal at the one place it is handed over.
//
// The clock comes from ctx, so a test can mint a token that expired last week
// by advancing a time2 machine rather than by sleeping.
func (s *Signer) Sign(ctx context.Context, claims Claims) (secret.Token, error) {
	now := time2.Now(ctx).UTC()

	if claims.Issuer == "" {
		claims.Issuer = s.issuer
	}
	if len(claims.Audience) == 0 {
		claims.Audience = s.audience
	}
	if claims.IssuedAt.IsZero() {
		claims.IssuedAt = now
	}
	if claims.ExpiresAt.IsZero() && s.ttl > 0 {
		claims.ExpiresAt = now.Add(s.ttl)
	}

	if claims.ExpiresAt.IsZero() && !s.allowNoExpiry {
		return "", Error.Errorf(
			"refusing to mint a token with no expiry: set Claims.ExpiresAt, " +
				"pass WithTTL, or opt in with AllowTokensThatNeverExpire")
	}

	mapped, err := claims.toMap()
	if err != nil {
		return "", err
	}

	token := gojwt.NewWithClaims(gojwt.GetSigningMethod(string(s.alg)), mapped)
	if s.keyID != "" {
		token.Header["kid"] = s.keyID
	}
	if s.typ != "" {
		token.Header["typ"] = s.typ
	}

	signed, err := token.SignedString(s.key)
	if err != nil {
		return "", Error.Wrap(err)
	}
	return secret.Token(signed), nil
}
