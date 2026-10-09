package jwt_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitlab.internal/tuan.quang.tran/aioz-common/jwt"

	"github.com/stretchr/testify/require"
)

var randReader = rand.Reader

// What a genkey command does, end to end: generate, write, load, sign, verify.
func TestGenerateWriteLoad(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	for _, tc := range []struct {
		kind jwt.KeyType
		bits int
		alg  jwt.Algorithm
	}{
		{"", 0, jwt.EdDSA},
		{jwt.KeyTypeEd25519, 0, jwt.EdDSA},
		{jwt.KeyTypeRSA, 2048, jwt.RS256},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			key, err := jwt.GenerateKey(tc.kind, tc.bits)
			require.NoError(t, err)

			signerDir := filepath.Join(t.TempDir(), "keys")
			priv, err := jwt.WritePrivateKey(signerDir, "2026-09", key)
			require.NoError(t, err)
			require.Equal(t, filepath.Join(signerDir, "2026-09.pem"), priv.Path)
			require.Equal(t, tc.alg, priv.Alg)
			require.True(t, priv.Private)

			verifierDir := t.TempDir()
			pub, err := jwt.WritePublicKey(verifierDir, "2026-09", key.Public())
			require.NoError(t, err)
			require.Equal(
				t,
				filepath.Join(verifierDir, "2026-09.pub.pem"),
				pub.Path,
			)
			require.Equal(t, priv.ID, pub.ID)
			require.False(t, pub.Private)

			dirInfo, err := os.Stat(signerDir)
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm())
			for path, want := range map[string]os.FileMode{priv.Path: 0o600, pub.Path: 0o644} {
				fi, err := os.Stat(path)
				require.NoError(t, err)
				require.Equalf(t, want, fi.Mode().Perm(), "%s", path)
			}

			kr, err := jwt.LoadKeyRing(signerDir, "2026-09", ttl)
			require.NoError(t, err)
			require.Equal(t, priv.ID, kr.Active().ID)
			require.False(t, kr.Active().LoosePermissions)

			token, err := kr.Signer().Sign(ctx, jwt.Claims{Subject: "user-1"})
			require.NoError(t, err)

			// A verifier that only ever saw the public file.
			// It needs a private key of its own to load, but routes by kid.
			_, err = jwt.WritePrivateKey(verifierDir, "local", edKey(t))
			require.NoError(t, err)
			remote, err := jwt.LoadKeyRing(verifierDir, "local", ttl)
			require.NoError(t, err)
			_, err = remote.Ring().Verify(ctx, token.Reveal())
			require.NoError(t, err)
		})
	}
}

func TestGenerateKeyDefaultRSASize(t *testing.T) {
	if testing.Short() {
		t.Skip("3072-bit RSA generation is slow")
	}
	t.Parallel()

	key, err := jwt.GenerateKey(jwt.KeyTypeRSA, 0)
	require.NoError(t, err)
	require.Equal(t, 3072, key.(*rsa.PrivateKey).N.BitLen())
}

func TestGenerateKeyRefuses(t *testing.T) {
	t.Parallel()

	_, err := jwt.GenerateKey(jwt.KeyTypeRSA, 1024)
	require.Error(t, err)
	require.Contains(t, err.Error(), "1024")

	_, err = jwt.GenerateKey("ecdsa", 0)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown key type")
}

// A key file may be the only copy of a key that signed tokens still in
// flight, so a second write under the same name is refused, and the first
// file is left exactly as it was.
func TestWriteKeyNeverOverwrites(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	first := edKey(t)
	info, err := jwt.WritePrivateKey(dir, "k", first)
	require.NoError(t, err)
	before, err := os.ReadFile(info.Path)
	require.NoError(t, err)

	_, err = jwt.WritePrivateKey(dir, "k", edKey(t))
	require.Error(t, err)
	require.Contains(t, err.Error(), "already exists")
	require.Contains(t, err.Error(), info.Path)

	after, err := os.ReadFile(info.Path)
	require.NoError(t, err)
	require.Equal(t, before, after)

	_, err = jwt.WritePublicKey(dir, "k", first.Public())
	require.NoError(t, err)
	_, err = jwt.WritePublicKey(dir, "k", first.Public())
	require.Error(t, err)
	require.Contains(t, err.Error(), "already exists")
}

func TestWriteKeyRefusesBadNames(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"",
		".hidden",
		"..data",
		"k.pem",
		"K.PEM",
		"k.pub.pem",
		"k.pub",
		"K.PUB",
		"a/b",
		"../escape",
		"has space",
		strings.Repeat("a", 65),
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			_, err := jwt.WritePrivateKey(dir, name, edKey(t))
			require.Errorf(t, err, "name %q", name)

			_, err = jwt.WritePublicKey(dir, name, edKey(t).Public())
			require.Errorf(t, err, "name %q", name)

			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			require.Empty(t, entries, "nothing written for a refused name")
		})
	}

	_, err := jwt.WritePrivateKey(
		t.TempDir(),
		strings.Repeat("a", 64),
		edKey(t),
	)
	require.NoError(t, err, "64 characters is the limit, not past it")
}

// A key LoadKeyRing would refuse is refused before any file is written.
func TestWriteKeyRefusesUnusableKeys(t *testing.T) {
	t.Parallel()

	small, err := rsa.GenerateKey(randReader, 1024)
	require.NoError(t, err)

	dir := t.TempDir()
	_, err = jwt.WritePrivateKey(dir, "small", small)
	require.Error(t, err)
	_, err = jwt.WritePrivateKey(dir, "ec", ecKey(t))
	require.Error(t, err)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries)
}

// A generated secret is exactly what the HMAC constructors accept, and
// different every time.
func TestGenerateSecret(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	for alg, n := range map[jwt.Algorithm]int{
		jwt.HS256: 32,
		jwt.HS384: 48,
		jwt.HS512: 64,
	} {
		t.Run(string(alg), func(t *testing.T) {
			key, err := jwt.GenerateSecret(alg)
			require.NoError(t, err)
			require.Len(t, key, n)

			other, err := jwt.GenerateSecret(alg)
			require.NoError(t, err)
			require.NotEqual(t, key, other)

			signer, err := jwt.NewHMACSigner(alg, key, jwt.WithTTL(time.Minute))
			require.NoError(t, err)
			verifier, err := jwt.NewHMACVerifier(alg, key)
			require.NoError(t, err)

			token, err := signer.Sign(ctx, jwt.Claims{Subject: "user-1"})
			require.NoError(t, err)
			claims, err := verifier.Verify(ctx, token.Reveal())
			require.NoError(t, err)
			require.Equal(t, "user-1", claims.Subject)
		})
	}
}

func TestGenerateSecretRefusesKeyPairAlgorithms(t *testing.T) {
	t.Parallel()

	for _, alg := range []jwt.Algorithm{jwt.EdDSA, jwt.RS256, jwt.ES256, "none", ""} {
		_, err := jwt.GenerateSecret(alg)
		require.ErrorContains(t, err, "has no shared secret", alg)
	}
}
