package jwt_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gitlab.internal/tuan.quang.tran/aioz-common/jwt"

	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
)

func rsaKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	return k
}

func ecKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	return k
}

func validClaims() jwt.Claims {
	return jwt.Claims{Subject: "user-1", ExpiresAt: time.Now().Add(time.Hour)}
}

// TestAlgorithmConfusion is the attack this package exists to stop.
//
// The verifier holds an RSA PUBLIC key, which is not secret. An attacker takes
// that public key, uses its bytes as an HMAC shared secret, and signs a token
// of their own choosing with HS256. A verifier that reads "alg" from the
// header and picks a method accordingly will hand the public key to the HMAC
// verifier and accept the forgery.
func TestAlgorithmConfusion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	key := rsaKey(t)

	verifier, err := jwt.NewKeyVerifier(jwt.RS256, &key.PublicKey)
	require.NoError(t, err)

	// The attacker knows the public key, because it is public.
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	require.NoError(t, err)

	forged := gojwt.NewWithClaims(gojwt.SigningMethodHS256, gojwt.MapClaims{
		"sub": "admin",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	forgedString, err := forged.SignedString(publicDER)
	require.NoError(t, err)

	_, err = verifier.Verify(ctx, forgedString)
	require.Error(t, err, "an HS256 token must never be accepted by an RS256 verifier")
	require.Contains(t, err.Error(), "signing method")
}

// A token with alg "none" carries no signature at all. It must be rejected,
// and the package must offer no way to produce one.
func TestNoneAlgorithmIsRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	key := rsaKey(t)
	verifier, err := jwt.NewKeyVerifier(jwt.RS256, &key.PublicKey)
	require.NoError(t, err)

	// Hand-built, because the library will not mint one either.
	enc := func(v any) string {
		b, err := json.Marshal(v)
		require.NoError(t, err)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	unsigned := strings.Join([]string{
		enc(map[string]string{"alg": "none", "typ": "JWT"}),
		enc(map[string]any{"sub": "admin", "exp": time.Now().Add(time.Hour).Unix()}),
		"",
	}, ".")

	_, err = verifier.Verify(ctx, unsigned)
	require.Error(t, err)
}

// Every supported algorithm must reject a token signed with any other one,
// not only the RS256/HS256 pair that is famous for it.
func TestEveryVerifierRejectsEveryOtherAlgorithm(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	secret32 := make([]byte, 64)
	_, err := rand.Read(secret32)
	require.NoError(t, err)
	rsaK, ecK := rsaKey(t), ecKey(t)

	type pair struct {
		signer   *jwt.Signer
		verifier *jwt.Verifier
	}
	mk := func(t *testing.T, alg jwt.Algorithm) pair {
		t.Helper()
		switch {
		case alg.IsHMAC():
			s, err := jwt.NewHMACSigner(alg, secret32)
			require.NoError(t, err)
			v, err := jwt.NewHMACVerifier(alg, secret32)
			require.NoError(t, err)
			return pair{s, v}
		case strings.HasPrefix(string(alg), "ES"):
			s, err := jwt.NewKeySigner(alg, ecK)
			require.NoError(t, err)
			v, err := jwt.NewKeyVerifier(alg, &ecK.PublicKey)
			require.NoError(t, err)
			return pair{s, v}
		default:
			s, err := jwt.NewKeySigner(alg, rsaK)
			require.NoError(t, err)
			v, err := jwt.NewKeyVerifier(alg, &rsaK.PublicKey)
			require.NoError(t, err)
			return pair{s, v}
		}
	}

	algs := []jwt.Algorithm{
		jwt.HS256, jwt.HS384, jwt.HS512,
		jwt.RS256, jwt.PS256, jwt.ES256,
	}

	for _, signAlg := range algs {
		signed, err := mk(t, signAlg).signer.Sign(ctx, validClaims())
		require.NoError(t, err)

		for _, verifyAlg := range algs {
			if verifyAlg == signAlg {
				continue
			}
			_, err := mk(t, verifyAlg).verifier.Verify(ctx, signed.Reveal())
			require.Errorf(t, err, "a %s verifier accepted a %s token", verifyAlg, signAlg)
		}
	}
}

// A signature that does not match must fail even when everything else is fine.
func TestTamperedTokenIsRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	key := ecKey(t)
	signer, err := jwt.NewKeySigner(jwt.ES256, key)
	require.NoError(t, err)
	verifier, err := jwt.NewKeyVerifier(jwt.ES256, &key.PublicKey)
	require.NoError(t, err)

	signed, err := signer.Sign(ctx, jwt.Claims{
		Subject:   "user-1",
		ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)

	parts := strings.Split(signed.Reveal(), ".")
	require.Len(t, parts, 3)

	// Rewrite the payload to claim a different subject, keeping the signature.
	tampered := parts[0] + "." + base64.RawURLEncoding.EncodeToString(
		[]byte(`{"sub":"admin","exp":`+
			gojwt.NewNumericDate(time.Now().Add(time.Hour)).Format(time.RFC3339)+`}`),
	) + "." + parts[2]

	_, err = verifier.Verify(ctx, tampered)
	require.Error(t, err)
}

// A key pair is not interchangeable with another key pair.
func TestWrongKeyIsRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	signer, err := jwt.NewKeySigner(jwt.ES256, ecKey(t))
	require.NoError(t, err)
	other := ecKey(t)
	verifier, err := jwt.NewKeyVerifier(jwt.ES256, &other.PublicKey)
	require.NoError(t, err)

	signed, err := signer.Sign(ctx, validClaims())
	require.NoError(t, err)

	_, err = verifier.Verify(ctx, signed.Reveal())
	require.Error(t, err)
}

// A signature proves who minted a token, not what it was minted for. The same
// issuer key may sign ID tokens, refresh tokens or internal tokens that carry
// every claim an access token needs; an access-token verifier that does not
// check "typ" accepts any of them (RFC 8725 section 3.11).
func TestAccessTokenVerifierRefusesOtherTokenTypes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	key := ecKey(t)
	verifier, err := jwt.NewKeyVerifier(jwt.ES256, &key.PublicKey,
		jwt.RequireAudience("api"), jwt.RequireType(jwt.TypeAccessToken))
	require.NoError(t, err)

	claims := jwt.Claims{Subject: "user-1", Audience: []string{"api"}, ExpiresAt: time.Now().Add(time.Hour)}

	// Validly signed by the real key, with the right audience, but typed "JWT"
	// - what every JWT library stamps by default.
	idTokenSigner, err := jwt.NewKeySigner(jwt.ES256, key)
	require.NoError(t, err)
	idToken, err := idTokenSigner.Sign(ctx, claims)
	require.NoError(t, err)

	_, err = verifier.Verify(ctx, idToken.Reveal())
	require.Error(t, err, "a token typed JWT must not pass as an access token")

	// And with no "typ" at all.
	untyped := gojwt.NewWithClaims(gojwt.SigningMethodES256, gojwt.MapClaims{
		"sub": "user-1", "aud": "api", "exp": time.Now().Add(time.Hour).Unix(),
	})
	delete(untyped.Header, "typ")
	untypedString, err := untyped.SignedString(key)
	require.NoError(t, err)

	_, err = verifier.Verify(ctx, untypedString)
	require.Error(t, err, "a token with no type must not pass as an access token")

	// The companion check: the same token is fine once it says what it is, so
	// the refusals above are about the type and nothing else.
	accessSigner, err := jwt.NewKeySigner(jwt.ES256, key, jwt.WithType(jwt.TypeAccessToken))
	require.NoError(t, err)
	accessToken, err := accessSigner.Sign(ctx, claims)
	require.NoError(t, err)
	_, err = verifier.Verify(ctx, accessToken.Reveal())
	require.NoError(t, err)
}
