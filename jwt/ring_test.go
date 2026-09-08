package jwt_test

import (
	"context"
	"testing"
	"time"

	"gitlab.internal/tuan.quang.tran/aioz-common/jwt"

	"github.com/stretchr/testify/require"
)

// TestRingRotatesWithoutInvalidatingLiveTokens is the reason a ring exists.
func TestRingRotatesWithoutInvalidatingLiveTokens(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	oldKey, newKey := hmacSecret(t), hmacSecret(t)

	oldSigner, err := jwt.NewHMACSigner(jwt.HS256, oldKey,
		jwt.WithTTL(time.Hour), jwt.WithKeyID("2026-01"))
	require.NoError(t, err)
	newSigner, err := jwt.NewHMACSigner(jwt.HS256, newKey,
		jwt.WithTTL(time.Hour), jwt.WithKeyID("2026-09"))
	require.NoError(t, err)

	oldVerifier, err := jwt.NewHMACVerifier(jwt.HS256, oldKey, jwt.ExpectKeyID("2026-01"))
	require.NoError(t, err)
	newVerifier, err := jwt.NewHMACVerifier(jwt.HS256, newKey, jwt.ExpectKeyID("2026-09"))
	require.NoError(t, err)

	// A token minted before the rotation.
	older, err := oldSigner.Sign(ctx, jwt.Claims{Subject: "user-1"})
	require.NoError(t, err)

	ring, err := jwt.NewRing(oldVerifier, newVerifier)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"2026-01", "2026-09"}, ring.KeyIDs())

	// Both the old and the new token verify while the old key is still live.
	newer, err := newSigner.Sign(ctx, jwt.Claims{Subject: "user-2"})
	require.NoError(t, err)

	got, err := ring.Verify(ctx, older.Reveal())
	require.NoError(t, err)
	require.Equal(t, "user-1", got.Subject)

	got, err = ring.Verify(ctx, newer.Reveal())
	require.NoError(t, err)
	require.Equal(t, "user-2", got.Subject)

	// Once the old key is retired, its tokens stop working and the new ones
	// keep going. This is the step a rotation without a ring cannot do.
	retired, err := jwt.NewRing(newVerifier)
	require.NoError(t, err)

	_, err = retired.Verify(ctx, older.Reveal())
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown key id")

	_, err = retired.Verify(ctx, newer.Reveal())
	require.NoError(t, err)
}

// A kid selects among keys we already trust. It must not be able to introduce
// one, nor to skip the signature check on the key it selects.
func TestRingKeyIDIsRoutingNotTrust(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	realKey, attackerKey := hmacSecret(t), hmacSecret(t)

	verifier, err := jwt.NewHMACVerifier(jwt.HS256, realKey, jwt.ExpectKeyID("real"))
	require.NoError(t, err)
	ring, err := jwt.NewRing(verifier)
	require.NoError(t, err)

	// The attacker names a kid the ring knows, but signs with their own key.
	forger, err := jwt.NewHMACSigner(jwt.HS256, attackerKey,
		jwt.WithTTL(time.Hour), jwt.WithKeyID("real"))
	require.NoError(t, err)
	forged, err := forger.Sign(ctx, jwt.Claims{Subject: "admin"})
	require.NoError(t, err)

	_, err = ring.Verify(ctx, forged.Reveal())
	require.Error(t, err, "naming a known kid must not skip the signature check")
}

func TestRingRejectsTokensItCannotRoute(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	key := hmacSecret(t)
	verifier, err := jwt.NewHMACVerifier(jwt.HS256, key, jwt.ExpectKeyID("k1"))
	require.NoError(t, err)
	ring, err := jwt.NewRing(verifier)
	require.NoError(t, err)

	noKid, err := jwt.NewHMACSigner(jwt.HS256, key, jwt.WithTTL(time.Hour))
	require.NoError(t, err)
	token, err := noKid.Sign(ctx, jwt.Claims{Subject: "user-1"})
	require.NoError(t, err)

	_, err = ring.Verify(ctx, token.Reveal())
	require.Error(t, err)
	require.Contains(t, err.Error(), "no key id")

	_, err = ring.Verify(ctx, "garbage")
	require.Error(t, err)
}

// A ring that silently accepted a verifier with no kid would fall back to
// "try them all", which hides a key that was never actually retired.
func TestNewRingRequiresKeyIDs(t *testing.T) {
	t.Parallel()

	withID, err := jwt.NewHMACVerifier(jwt.HS256, hmacSecret(t), jwt.ExpectKeyID("k1"))
	require.NoError(t, err)
	withoutID, err := jwt.NewHMACVerifier(jwt.HS256, hmacSecret(t))
	require.NoError(t, err)

	_, err = jwt.NewRing(withoutID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "key id")

	_, err = jwt.NewRing(withID, withoutID)
	require.Error(t, err)

	_, err = jwt.NewRing()
	require.Error(t, err)

	dup, err := jwt.NewHMACVerifier(jwt.HS256, hmacSecret(t), jwt.ExpectKeyID("k1"))
	require.NoError(t, err)
	_, err = jwt.NewRing(withID, dup)
	require.Error(t, err, "a duplicate kid makes routing ambiguous")
}

// A ring may hold different algorithms, each still pinned to its own key.
func TestRingMayMixAlgorithms(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	sec := hmacSecret(t)
	ec := ecKey(t)

	hmacVerifier, err := jwt.NewHMACVerifier(jwt.HS256, sec, jwt.ExpectKeyID("shared"))
	require.NoError(t, err)
	keyVerifier, err := jwt.NewKeyVerifier(jwt.ES256, &ec.PublicKey, jwt.ExpectKeyID("ecdsa"))
	require.NoError(t, err)

	ring, err := jwt.NewRing(hmacVerifier, keyVerifier)
	require.NoError(t, err)

	hmacSigner, err := jwt.NewHMACSigner(jwt.HS256, sec, jwt.WithTTL(time.Hour), jwt.WithKeyID("shared"))
	require.NoError(t, err)
	keySigner, err := jwt.NewKeySigner(jwt.ES256, ec, jwt.WithTTL(time.Hour), jwt.WithKeyID("ecdsa"))
	require.NoError(t, err)

	for _, signer := range []*jwt.Signer{hmacSigner, keySigner} {
		token, err := signer.Sign(ctx, jwt.Claims{Subject: "user-1"})
		require.NoError(t, err)

		got, err := ring.Verify(ctx, token.Reveal())
		require.NoError(t, err)
		require.Equal(t, "user-1", got.Subject)
	}

	// And routing to the wrong entry still fails on the algorithm pin.
	crossSigner, err := jwt.NewHMACSigner(jwt.HS256, sec, jwt.WithTTL(time.Hour), jwt.WithKeyID("ecdsa"))
	require.NoError(t, err)
	crossToken, err := crossSigner.Sign(ctx, jwt.Claims{Subject: "user-1"})
	require.NoError(t, err)

	_, err = ring.Verify(ctx, crossToken.Reveal())
	require.Error(t, err, "an HS256 token routed to the ES256 entry must be refused")
}
