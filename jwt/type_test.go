package jwt_test

import (
	"context"
	"testing"
	"time"

	"gitlab.internal/tuan.quang.tran/aioz-common/jwt"

	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
)

// header returns the unverified header of a token, for inspecting what a
// Signer stamped.
func header(t *testing.T, token string) map[string]any {
	t.Helper()
	parsed, _, err := gojwt.NewParser().
		ParseUnverified(token, gojwt.MapClaims{})
	require.NoError(t, err)
	return parsed.Header
}

func TestTypeIsStampedAndRoundTrips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	key := edKey(t)
	signer, err := jwt.NewKeySigner(jwt.EdDSA, key,
		jwt.WithTTL(time.Hour), jwt.WithType(jwt.TypeAccessToken))
	require.NoError(t, err)
	verifier, err := jwt.NewKeyVerifier(jwt.EdDSA, key.Public(),
		jwt.RequireType(jwt.TypeAccessToken))
	require.NoError(t, err)

	token, err := signer.Sign(ctx, jwt.Claims{Subject: "user-1"})
	require.NoError(t, err)
	require.Equal(t, "at+jwt", header(t, token.Reveal())["typ"])

	got, err := verifier.Verify(ctx, token.Reveal())
	require.NoError(t, err)
	require.Equal(t, "user-1", got.Subject)
}

// RFC 9068 accepts "at+jwt" and "application/at+jwt", and RFC 7515 makes the
// media type case-insensitive, so every spelling of the same type is one type.
func TestRequireType(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	key := edKey(t)

	// sign mints a token with the given typ header; "-" deletes it.
	sign := func(t *testing.T, typ string) string {
		t.Helper()
		token := gojwt.NewWithClaims(gojwt.SigningMethodEdDSA, gojwt.MapClaims{
			"sub": "user-1",
			"exp": time.Now().Add(time.Hour).Unix(),
		})
		if typ == "-" {
			delete(token.Header, "typ")
		} else {
			token.Header["typ"] = typ
		}
		signed, err := token.SignedString(key)
		require.NoError(t, err)
		return signed
	}

	for _, tc := range []struct {
		name    string
		require string
		typ     string
		wantErr string
	}{
		{name: "exact", require: "at+jwt", typ: "at+jwt"},
		{name: "prefix and case on the token", require: "at+jwt", typ: "application/AT+JWT"},
		{name: "prefix on the requirement", require: "application/at+jwt", typ: "at+jwt"},
		{name: "prefix on both", require: "application/at+jwt", typ: "Application/At+Jwt"},
		{name: "missing", require: "at+jwt", typ: "-", wantErr: "no type"},
		{name: "empty", require: "at+jwt", typ: "", wantErr: "no type"},
		{name: "the default", require: "at+jwt", typ: "JWT", wantErr: "not the required"},
		{name: "a different token type", require: "at+jwt", typ: "rt+jwt", wantErr: "not the required"},
		{name: "a prefix is not a wildcard", require: "at+jwt", typ: "application/", wantErr: "not the required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verifier, err := jwt.NewKeyVerifier(
				jwt.EdDSA,
				key.Public(),
				jwt.RequireType(tc.require),
			)
			require.NoError(t, err)

			_, err = verifier.Verify(ctx, sign(t, tc.typ))
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}

	// Without RequireType the header is not looked at.
	lax, err := jwt.NewKeyVerifier(jwt.EdDSA, key.Public())
	require.NoError(t, err)
	for _, typ := range []string{"-", "JWT", "anything"} {
		_, err := lax.Verify(ctx, sign(t, typ))
		require.NoErrorf(t, err, "typ %q", typ)
	}
}

// A Ring hands the token to the member verifier, so a member built with
// RequireType enforces it through the ring.
func TestRingEnforcesMemberType(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	key := edKey(t)
	verifier, err := jwt.NewKeyVerifier(jwt.EdDSA, key.Public(),
		jwt.ExpectKeyID("k1"), jwt.RequireType(jwt.TypeAccessToken))
	require.NoError(t, err)
	ring, err := jwt.NewRing(verifier)
	require.NoError(t, err)

	typed, err := jwt.NewKeySigner(jwt.EdDSA, key, jwt.WithTTL(time.Hour),
		jwt.WithKeyID("k1"), jwt.WithType(jwt.TypeAccessToken))
	require.NoError(t, err)
	untyped, err := jwt.NewKeySigner(
		jwt.EdDSA,
		key,
		jwt.WithTTL(time.Hour),
		jwt.WithKeyID("k1"),
	)
	require.NoError(t, err)

	good, err := typed.Sign(ctx, jwt.Claims{Subject: "user-1"})
	require.NoError(t, err)
	_, err = ring.Verify(ctx, good.Reveal())
	require.NoError(t, err)

	bad, err := untyped.Sign(ctx, jwt.Claims{Subject: "user-1"})
	require.NoError(t, err)
	_, err = ring.Verify(ctx, bad.Reveal())
	require.Error(t, err)
	require.Contains(t, err.Error(), "type")
}

// RFC 9068 section 4 checks the type before the signature and the claims, so
// a token of the wrong type is refused as the wrong type, whatever else is
// wrong with it. Reading the header before the signature is checked is safe
// because the type check can only reject: accepting still takes a valid
// signature.
func TestTypeIsCheckedBeforeSignatureAndClaims(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	key := edKey(t)
	verifier, err := jwt.NewKeyVerifier(jwt.EdDSA, key.Public(),
		jwt.RequireType(jwt.TypeAccessToken))
	require.NoError(t, err)

	mint := func(t *testing.T, signingKey any, typ string, exp time.Time) string {
		t.Helper()
		token := gojwt.NewWithClaims(gojwt.SigningMethodEdDSA, gojwt.MapClaims{
			"sub": "user-1",
			"exp": exp.Unix(),
		})
		token.Header["typ"] = typ
		signed, err := token.SignedString(signingKey)
		require.NoError(t, err)
		return signed
	}
	later := time.Now().Add(time.Hour)

	// A type mismatch reads the same as it did when it was checked last: the
	// parser's keyfunc plumbing is not part of the message or the error chain.
	requireTypeMismatch := func(t *testing.T, err error) {
		t.Helper()
		require.Error(t, err)
		require.Contains(t, err.Error(), "not the required")
		require.NotContains(t, err.Error(), "keyfunc")
		require.NotErrorIs(t, err, gojwt.ErrTokenUnverifiable)
	}

	t.Run("the wrong type with a bad signature", func(t *testing.T) {
		_, err := verifier.Verify(ctx, mint(t, edKey(t), "rt+jwt", later))
		requireTypeMismatch(t, err)
	})

	t.Run("the wrong type, expired", func(t *testing.T) {
		_, err := verifier.Verify(ctx, mint(t, key, "rt+jwt", time.Now().Add(-time.Hour)))
		requireTypeMismatch(t, err)
	})

	t.Run("the right type does not excuse a bad signature", func(t *testing.T) {
		_, err := verifier.Verify(ctx, mint(t, edKey(t), jwt.TypeAccessToken, later))
		require.Error(t, err)
		require.NotContains(t, err.Error(), "type")
	})
}
