package jwt_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"

	"gitlab.internal/tuan.quang.tran/aioz-common/jwt"
	"gitlab.internal/tuan.quang.tran/aioz-common/secret"
	"gitlab.internal/tuan.quang.tran/aioz-common/time2"

	"github.com/stretchr/testify/require"
)

func hmacSecret(t *testing.T) []byte {
	t.Helper()
	return secret.Bytes(32)
}

func TestRoundTripEveryKeyStyle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	sec := hmacSecret(t)

	t.Run("shared secret", func(t *testing.T) {
		signer, err := jwt.NewHMACSigner(jwt.HS256, sec, jwt.WithTTL(time.Hour))
		require.NoError(t, err)
		verifier, err := jwt.NewHMACVerifier(jwt.HS256, sec)
		require.NoError(t, err)

		token, err := signer.Sign(ctx, jwt.Claims{Subject: "user-1"})
		require.NoError(t, err)

		got, err := verifier.Verify(ctx, token.Reveal())
		require.NoError(t, err)
		require.Equal(t, "user-1", got.Subject)
	})

	t.Run("public/private key", func(t *testing.T) {
		signer, err := jwt.NewKeySigner(jwt.EdDSA, priv, jwt.WithTTL(time.Hour))
		require.NoError(t, err)
		verifier, err := jwt.NewKeyVerifier(jwt.EdDSA, pub)
		require.NoError(t, err)

		token, err := signer.Sign(ctx, jwt.Claims{Subject: "user-1"})
		require.NoError(t, err)

		got, err := verifier.Verify(ctx, token.Reveal())
		require.NoError(t, err)
		require.Equal(t, "user-1", got.Subject)
	})
}

// The signed token is a secret.Token, so it inherits the redaction: a JWT
// carries a live credential and must not reach a log line by accident.
func TestSignedTokenIsRedacted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	sec := hmacSecret(t)
	signer, err := jwt.NewHMACSigner(jwt.HS256, sec, jwt.WithTTL(time.Hour))
	require.NoError(t, err)

	token, err := signer.Sign(ctx, jwt.Claims{Subject: "user-1"})
	require.NoError(t, err)

	require.Equal(t, secret.Redacted, token.String())

	_, err = json.Marshal(token)
	require.Error(t, err, "a JWT must not marshal into a response body by accident")

	require.NotEmpty(t, token.Reveal())
}

func TestConstructorsRejectTheWrongKeyStyle(t *testing.T) {
	t.Parallel()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	sec := hmacSecret(t)

	_, err = jwt.NewHMACSigner(jwt.EdDSA, sec)
	require.Error(t, err, "EdDSA is not an HMAC algorithm")

	_, err = jwt.NewKeySigner(jwt.HS256, priv)
	require.Error(t, err, "HS256 takes a shared secret")

	_, err = jwt.NewHMACVerifier(jwt.RS256, sec)
	require.Error(t, err)

	_, err = jwt.NewKeyVerifier(jwt.HS256, pub)
	require.Error(t, err)
}

// A mismatched key type is caught at construction rather than on a live
// request, which is the difference between a failed startup and a 500.
func TestConstructorRejectsAMismatchedKeyType(t *testing.T) {
	t.Parallel()

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	_, err = jwt.NewKeyVerifier(jwt.RS256, pub)
	require.Error(t, err)
	require.Contains(t, err.Error(), "RSA")

	_, err = jwt.NewKeyVerifier(jwt.ES256, pub)
	require.Error(t, err)
	require.Contains(t, err.Error(), "ECDSA")
}

// An HMAC secret is guessable offline from one captured token, so a short one
// is refused rather than warned about.
func TestShortHMACSecretIsRefused(t *testing.T) {
	t.Parallel()

	for alg, minLen := range map[jwt.Algorithm]int{
		jwt.HS256: 32, jwt.HS384: 48, jwt.HS512: 64,
	} {
		_, err := jwt.NewHMACSigner(alg, make([]byte, minLen-1))
		require.Errorf(t, err, "%s accepted a short secret", alg)

		_, err = jwt.NewHMACSigner(alg, make([]byte, minLen))
		require.NoErrorf(t, err, "%s rejected a secret of exactly the minimum", alg)

		_, err = jwt.NewHMACVerifier(alg, make([]byte, minLen-1))
		require.Error(t, err)
	}
}

func TestExpiryIsRequiredOnBothSides(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	sec := hmacSecret(t)

	t.Run("signing refuses a token with no expiry", func(t *testing.T) {
		signer, err := jwt.NewHMACSigner(jwt.HS256, sec)
		require.NoError(t, err)

		_, err = signer.Sign(ctx, jwt.Claims{Subject: "user-1"})
		require.Error(t, err)
		require.Contains(t, err.Error(), "no expiry")
	})

	t.Run("verifying refuses a token with no expiry", func(t *testing.T) {
		signer, err := jwt.NewHMACSigner(jwt.HS256, sec, jwt.AllowTokensThatNeverExpire())
		require.NoError(t, err)
		token, err := signer.Sign(ctx, jwt.Claims{Subject: "user-1"})
		require.NoError(t, err)

		strict, err := jwt.NewHMACVerifier(jwt.HS256, sec)
		require.NoError(t, err)
		_, err = strict.Verify(ctx, token.Reveal())
		require.Error(t, err)

		lax, err := jwt.NewHMACVerifier(jwt.HS256, sec, jwt.AcceptTokensThatNeverExpire())
		require.NoError(t, err)
		_, err = lax.Verify(ctx, token.Reveal())
		require.NoError(t, err, "the opt-out has to actually work")
	})
}

// Expiry is driven by the context clock, so a test advances time instead of
// sleeping for an hour.
func TestExpiryUsesTheContextClock(t *testing.T) {
	t.Parallel()

	ctx, machine := time2.WithNewMachine(context.Background())
	sec := hmacSecret(t)

	signer, err := jwt.NewHMACSigner(jwt.HS256, sec, jwt.WithTTL(time.Hour))
	require.NoError(t, err)
	verifier, err := jwt.NewHMACVerifier(jwt.HS256, sec)
	require.NoError(t, err)

	token, err := signer.Sign(ctx, jwt.Claims{Subject: "user-1"})
	require.NoError(t, err)

	_, err = verifier.Verify(ctx, token.Reveal())
	require.NoError(t, err, "valid immediately after minting")

	machine.Advance(59 * time.Minute)
	_, err = verifier.Verify(ctx, token.Reveal())
	require.NoError(t, err, "still valid a minute before expiry")

	machine.Advance(2 * time.Minute)
	_, err = verifier.Verify(ctx, token.Reveal())
	require.Error(t, err, "expired a minute after expiry")
}

func TestLeewayToleratesSkew(t *testing.T) {
	t.Parallel()

	ctx, machine := time2.WithNewMachine(context.Background())
	sec := hmacSecret(t)

	signer, err := jwt.NewHMACSigner(jwt.HS256, sec, jwt.WithTTL(time.Minute))
	require.NoError(t, err)
	token, err := signer.Sign(ctx, jwt.Claims{Subject: "user-1"})
	require.NoError(t, err)

	machine.Advance(90 * time.Second)

	strict, err := jwt.NewHMACVerifier(jwt.HS256, sec)
	require.NoError(t, err)
	_, err = strict.Verify(ctx, token.Reveal())
	require.Error(t, err)

	lenient, err := jwt.NewHMACVerifier(jwt.HS256, sec, jwt.WithLeeway(time.Minute))
	require.NoError(t, err)
	_, err = lenient.Verify(ctx, token.Reveal())
	require.NoError(t, err)
}

func TestNotBeforeIsHonored(t *testing.T) {
	t.Parallel()

	ctx, machine := time2.WithNewMachine(context.Background())
	sec := hmacSecret(t)

	signer, err := jwt.NewHMACSigner(jwt.HS256, sec)
	require.NoError(t, err)
	verifier, err := jwt.NewHMACVerifier(jwt.HS256, sec)
	require.NoError(t, err)

	now := time2.Now(ctx)
	token, err := signer.Sign(ctx, jwt.Claims{
		Subject:   "user-1",
		NotBefore: now.Add(time.Hour),
		ExpiresAt: now.Add(2 * time.Hour),
	})
	require.NoError(t, err)

	_, err = verifier.Verify(ctx, token.Reveal())
	require.Error(t, err, "not valid yet")

	machine.Advance(90 * time.Minute)
	_, err = verifier.Verify(ctx, token.Reveal())
	require.NoError(t, err)
}

// Without an audience check a token minted for one service is accepted by
// every other service that trusts the same issuer.
func TestIssuerAndAudienceAreEnforced(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	sec := hmacSecret(t)
	signer, err := jwt.NewHMACSigner(jwt.HS256, sec,
		jwt.WithTTL(time.Hour),
		jwt.WithIssuer("coord"),
		jwt.WithAudience("edgeserver"),
	)
	require.NoError(t, err)

	token, err := signer.Sign(ctx, jwt.Claims{Subject: "user-1"})
	require.NoError(t, err)

	right, err := jwt.NewHMACVerifier(jwt.HS256, sec,
		jwt.RequireIssuer("coord"), jwt.RequireAudience("edgeserver"))
	require.NoError(t, err)
	got, err := right.Verify(ctx, token.Reveal())
	require.NoError(t, err)
	require.Equal(t, "coord", got.Issuer)
	require.Equal(t, []string{"edgeserver"}, got.Audience)

	wrongAudience, err := jwt.NewHMACVerifier(jwt.HS256, sec,
		jwt.RequireAudience("versioncontrol"))
	require.NoError(t, err)
	_, err = wrongAudience.Verify(ctx, token.Reveal())
	require.Error(t, err, "a token for edgeserver must not work at versioncontrol")

	wrongIssuer, err := jwt.NewHMACVerifier(jwt.HS256, sec, jwt.RequireIssuer("someone-else"))
	require.NoError(t, err)
	_, err = wrongIssuer.Verify(ctx, token.Reveal())
	require.Error(t, err)
}

func TestExtraClaimsRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	sec := hmacSecret(t)
	signer, err := jwt.NewHMACSigner(jwt.HS256, sec, jwt.WithTTL(time.Hour))
	require.NoError(t, err)
	verifier, err := jwt.NewHMACVerifier(jwt.HS256, sec)
	require.NoError(t, err)

	token, err := signer.Sign(ctx, jwt.Claims{
		Subject: "user-1",
		Extra:   map[string]any{"role": "admin", "tier": float64(3)},
	})
	require.NoError(t, err)

	got, err := verifier.Verify(ctx, token.Reveal())
	require.NoError(t, err)

	role, ok := got.GetString("role")
	require.True(t, ok)
	require.Equal(t, "admin", role)

	tier, ok := got.Get("tier")
	require.True(t, ok)
	require.Equal(t, float64(3), tier)
}

// A registered claim carries meaning the verifier acts on, so an Extra entry
// must not be able to overwrite one.
func TestExtraClaimCannotShadowARegisteredOne(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	signer, err := jwt.NewHMACSigner(jwt.HS256, hmacSecret(t), jwt.WithTTL(time.Hour))
	require.NoError(t, err)

	for _, name := range []string{"iss", "sub", "aud", "exp", "nbf", "iat", "jti"} {
		_, err := signer.Sign(ctx, jwt.Claims{
			Subject: "user-1",
			Extra:   map[string]any{name: "hijacked"},
		})
		require.Errorf(t, err, "Extra[%q] should be refused", name)
	}
}

func TestGarbageInput(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	verifier, err := jwt.NewHMACVerifier(jwt.HS256, hmacSecret(t))
	require.NoError(t, err)

	for _, in := range []string{"", "not.a.token", "a.b", "....", "eyJhbGciOiJIUzI1NiJ9"} {
		_, err := verifier.Verify(ctx, in)
		require.Errorf(t, err, "input %q", in)
		require.NotPanics(t, func() { _, _ = verifier.Verify(ctx, in) })
	}
}
