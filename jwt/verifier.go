package jwt

import (
	"context"
	"crypto"
	"time"

	"gitlab.internal/tuan.quang.tran/aioz-common/time2"

	gojwt "github.com/golang-jwt/jwt/v5"
)

// Verifier checks tokens against one key and one algorithm.
//
// It is safe for concurrent use.
type Verifier struct {
	alg   Algorithm
	key   any
	keyID string

	issuer        string
	audience      string
	leeway        time.Duration
	allowNoExpiry bool
}

// VerifierOption configures a Verifier.
type VerifierOption func(*Verifier)

// ExpectKeyID tells the Verifier which "kid" it answers for, so a [Ring] can
// route to it. It does not by itself require a token to carry that kid.
func ExpectKeyID(id string) VerifierOption {
	return func(v *Verifier) { v.keyID = id }
}

// RequireIssuer rejects a token whose "iss" is not issuer.
func RequireIssuer(issuer string) VerifierOption {
	return func(v *Verifier) { v.issuer = issuer }
}

// RequireAudience rejects a token whose "aud" does not include audience.
//
// Set it. Without an audience check, a token minted for one of your services
// is accepted by every other service that trusts the same issuer, which turns
// a token leak in the least important service into a compromise of the most
// important one.
func RequireAudience(audience string) VerifierOption {
	return func(v *Verifier) { v.audience = audience }
}

// WithLeeway tolerates up to d of clock skew on the time-based claims.
//
// A minute is plenty between machines that run NTP. Large values are how an
// expired token keeps working.
func WithLeeway(d time.Duration) VerifierOption {
	return func(v *Verifier) { v.leeway = d }
}

// AcceptTokensThatNeverExpire lets this Verifier accept a token with no "exp".
//
// Such a token can never be revoked. Spelled out in full so a reviewer sees
// it, and so nobody arrives here by leaving a field at its zero value.
func AcceptTokensThatNeverExpire() VerifierOption {
	return func(v *Verifier) { v.allowNoExpiry = true }
}

// NewHMACVerifier returns a Verifier for tokens signed with a shared secret.
//
// Holding this key means being able to mint tokens as well as check them.
func NewHMACVerifier(alg Algorithm, key []byte, opts ...VerifierOption) (*Verifier, error) {
	if !alg.IsHMAC() {
		return nil, Error.Errorf("%s is not an HMAC algorithm; use NewKeyVerifier", alg)
	}
	if min := algorithms[alg].minSecretLen; len(key) < min {
		return nil, Error.Errorf(
			"%s needs a secret of at least %d bytes, got %d", alg, min, len(key))
	}

	return newVerifier(alg, key, opts...), nil
}

// NewKeyVerifier returns a Verifier that checks signatures with a public key
// and therefore cannot mint tokens.
func NewKeyVerifier(alg Algorithm, key crypto.PublicKey, opts ...VerifierOption) (*Verifier, error) {
	if alg.IsHMAC() {
		return nil, Error.Errorf("%s takes a shared secret; use NewHMACVerifier", alg)
	}
	if !alg.Valid() {
		return nil, Error.Errorf("unknown algorithm %q", alg)
	}
	if err := checkKeyType(alg, key); err != nil {
		return nil, err
	}

	return newVerifier(alg, key, opts...), nil
}

func newVerifier(alg Algorithm, key any, opts ...VerifierOption) *Verifier {
	v := &Verifier{alg: alg, key: key}
	for _, opt := range opts {
		opt(v)
	}
	return v
}

// Algorithm returns the only algorithm this Verifier accepts.
func (v *Verifier) Algorithm() Algorithm { return v.alg }

// KeyID returns the "kid" this Verifier answers for, or "".
func (v *Verifier) KeyID() string { return v.keyID }

// Verify checks a token's signature and claims and returns its payload.
//
// The algorithm is the one this Verifier was built with. The token's own
// "alg" header is compared against it and never used to choose a method,
// which is what closes algorithm confusion and the "none" algorithm.
//
// The clock comes from ctx, so a test can expire a token by advancing a time2
// machine instead of sleeping.
func (v *Verifier) Verify(ctx context.Context, token string) (Claims, error) {
	opts := []gojwt.ParserOption{
		// The pin. The parser rejects a token whose header names anything
		// else before it does any signature work.
		gojwt.WithValidMethods([]string{string(v.alg)}),
		gojwt.WithTimeFunc(func() time.Time { return time2.Now(ctx) }),
		gojwt.WithLeeway(v.leeway),
	}
	if !v.allowNoExpiry {
		opts = append(opts, gojwt.WithExpirationRequired())
	}
	if v.issuer != "" {
		opts = append(opts, gojwt.WithIssuer(v.issuer))
	}
	if v.audience != "" {
		opts = append(opts, gojwt.WithAudience(v.audience))
	}

	parsed, err := gojwt.NewParser(opts...).Parse(token, v.keyFunc)
	if err != nil {
		return Claims{}, Error.Wrap(err)
	}

	mapped, ok := parsed.Claims.(gojwt.MapClaims)
	if !ok {
		return Claims{}, Error.Errorf("unexpected claims type %T", parsed.Claims)
	}
	return claimsFromMap(mapped), nil
}

// keyFunc supplies the key, and re-checks the algorithm.
//
// WithValidMethods already rejects a mismatched header, so this is a second
// gate on the same property. It is here deliberately: this is the callback the
// library hands the parsed header to, and it is the exact place where every
// published algorithm-confusion bug was written. A reader comparing this
// against a vulnerable implementation should find the check, not its absence.
func (v *Verifier) keyFunc(token *gojwt.Token) (any, error) {
	if alg, _ := token.Header["alg"].(string); alg != string(v.alg) {
		return nil, Error.Errorf("token algorithm %q does not match the expected %q", alg, v.alg)
	}
	return v.key, nil
}
