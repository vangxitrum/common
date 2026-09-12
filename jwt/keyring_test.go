package jwt_test

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gitlab.internal/tuan.quang.tran/aioz-common/jwt"

	"github.com/stretchr/testify/require"
)

var ttl = jwt.KeyRingOptions{Signer: []jwt.SignerOption{jwt.WithTTL(time.Hour)}}

// writePEM writes one PEM block to dir/name with mode perm.
func writePEM(
	t *testing.T,
	dir, name, blockType string,
	der []byte,
	perm os.FileMode,
) string {
	t.Helper()
	path := filepath.Join(dir, name)
	data := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	require.NoError(t, os.WriteFile(path, data, perm))
	require.NoError(t, os.Chmod(path, perm))
	return path
}

func pkcs8(t *testing.T, key crypto.Signer) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	return der
}

func pkix(t *testing.T, pub crypto.PublicKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	require.NoError(t, err)
	return der
}

func edKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	key, err := jwt.GenerateKey(jwt.KeyTypeEd25519, 0)
	require.NoError(t, err)
	return key.(ed25519.PrivateKey)
}

// copyFile copies src into dir under its own base name.
func copyFile(t *testing.T, src, dir string) {
	t.Helper()
	data, err := os.ReadFile(src)
	require.NoError(t, err)
	require.NoError(
		t,
		os.WriteFile(filepath.Join(dir, filepath.Base(src)), data, 0o600),
	)
}

func b64decode(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	require.NoError(t, err)
	return b
}

// The kid is an RFC 7638 thumbprint, so it has to match the published vectors
// exactly: a JWKS consumer recomputes it and compares.
func TestThumbprintMatchesTheRFCVectors(t *testing.T) {
	t.Parallel()

	// RFC 7638 section 3.1.
	n := "0vx7agoebGcQSuuPiLJXZptN9nndrQmbXEps2aiAFbWhM78LhWx4cbbfAAtVT86zwu1RK7aPFFxuhDR1L6tSoc_BJ" +
		"ECPebWKRXjBZCiFV4n3oknjhMstn64tZ_2W-5JsGY4Hc5n9yBXArwl93lqt7_RN5w6Cf0h4QyQ5v-65YGjQR0_FDW2" +
		"QvzqY368QQMicAtaSqzs8KJZgnYb9c7d0zgdAZHzu6qMQvRL5hajrn1n91CbOpbISD08qNLyrdkt-bFTWhAI4vMQFh6" +
		"WeZu0fM4lFd2NcRwr3XPksINHaQ-G_xBniIqbw0Ls1jF44-csFCur-kEgU8awapJzKnqDKgw"
	rsaPub := &rsa.PublicKey{
		N: new(big.Int).SetBytes(b64decode(t, n)),
		E: int(new(big.Int).SetBytes(b64decode(t, "AQAB")).Int64()),
	}

	// RFC 8037 appendix A.3.
	edPub := ed25519.PublicKey(
		b64decode(t, "11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo"),
	)

	for _, tc := range []struct {
		name string
		pub  crypto.PublicKey
		want string
	}{
		{"RFC 7638 RSA", rsaPub, "NzbLsXh8uDCcd-6MNwXF4W_7noWXFZAfHkxZsRGC9Xs"},
		{"RFC 8037 Ed25519", edPub, "kPrK_qmxVWaYVA9wwBF6Iuo3vVzz7TxHCTwXBygrS4k"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := jwt.Thumbprint(tc.pub)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}

	_, err := jwt.Thumbprint(&ecKey(t).PublicKey)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported")
}

func TestAlgorithmFor(t *testing.T) {
	t.Parallel()

	small, err := rsa.GenerateKey(randReader, 1024)
	require.NoError(t, err)

	for _, tc := range []struct {
		name    string
		pub     crypto.PublicKey
		want    jwt.Algorithm
		wantErr string
	}{
		{name: "RSA 2048", pub: &rsaKey(t).PublicKey, want: jwt.RS256},
		{name: "Ed25519", pub: edKey(t).Public(), want: jwt.EdDSA},
		{name: "ECDSA", pub: &ecKey(t).PublicKey, wantErr: "*ecdsa.PublicKey"},
		{name: "RSA 1024", pub: &small.PublicKey, wantErr: "1024 bits"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := jwt.AlgorithmFor(tc.pub)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// Each supported encoding loads, and the ring signs and verifies with the
// algorithm the key calls for.
func TestLoadKeyRingEveryEncoding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	rsaPriv := rsaKey(t)
	edPriv := edKey(t)

	for _, tc := range []struct {
		name  string
		write func(t *testing.T, dir string)
		alg   jwt.Algorithm
	}{
		{
			name: "RSA PKCS#1",
			write: func(t *testing.T, dir string) {
				writePEM(t, dir, "k.pem", "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(rsaPriv), 0o600)
			},
			alg: jwt.RS256,
		},
		{
			name: "RSA PKCS#8",
			write: func(t *testing.T, dir string) {
				writePEM(t, dir, "k.pem", "PRIVATE KEY", pkcs8(t, rsaPriv), 0o600)
			},
			alg: jwt.RS256,
		},
		{
			name: "Ed25519 PKCS#8",
			write: func(t *testing.T, dir string) {
				writePEM(t, dir, "k.pem", "PRIVATE KEY", pkcs8(t, edPriv), 0o600)
			},
			alg: jwt.EdDSA,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.write(t, dir)

			kr, err := jwt.LoadKeyRing(dir, "k", ttl)
			require.NoError(t, err)
			require.Equal(t, tc.alg, kr.Signer().Algorithm())
			require.Equal(t, tc.alg, kr.Active().Alg)
			require.True(t, kr.Active().Active)
			require.Equal(t, kr.Active().ID, kr.Signer().KeyID())

			token, err := kr.Signer().Sign(ctx, jwt.Claims{Subject: "user-1"})
			require.NoError(t, err)
			got, err := kr.Ring().Verify(ctx, token.Reveal())
			require.NoError(t, err)
			require.Equal(t, "user-1", got.Subject)
		})
	}

	// A public-only file verifies tokens signed by its private half, which is
	// held somewhere else.
	for _, tc := range []struct {
		name      string
		priv      crypto.Signer
		blockType string
		der       []byte
		alg       jwt.Algorithm
	}{
		{"RSA PKIX", rsaPriv, "PUBLIC KEY", pkix(t, &rsaPriv.PublicKey), jwt.RS256},
		{"RSA PKCS#1", rsaPriv, "RSA PUBLIC KEY", x509.MarshalPKCS1PublicKey(&rsaPriv.PublicKey), jwt.RS256},
		{"Ed25519 PKIX", edPriv, "PUBLIC KEY", pkix(t, edPriv.Public()), jwt.EdDSA},
	} {
		t.Run("public only "+tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writePEM(
				t,
				dir,
				"active.pem",
				"PRIVATE KEY",
				pkcs8(t, edKey(t)),
				0o600,
			)
			writePEM(t, dir, "other.pub.pem", tc.blockType, tc.der, 0o644)

			kr, err := jwt.LoadKeyRing(dir, "active", ttl)
			require.NoError(t, err)

			kid, err := jwt.Thumbprint(tc.priv.Public())
			require.NoError(t, err)
			require.ElementsMatch(
				t,
				[]string{kr.Active().ID, kid},
				kr.Ring().KeyIDs(),
			)

			elsewhere, err := jwt.NewKeySigner(
				tc.alg,
				tc.priv,
				jwt.WithTTL(time.Hour),
				jwt.WithKeyID(kid),
			)
			require.NoError(t, err)
			token, err := elsewhere.Sign(ctx, jwt.Claims{Subject: "user-1"})
			require.NoError(t, err)

			_, err = kr.Ring().Verify(ctx, token.Reveal())
			require.NoError(t, err)
		})
	}
}

// Every refusal names the file, because the reader is looking at a directory
// listing and needs to know which entry to fix.
func TestLoadKeyRingErrorsNameTheFile(t *testing.T) {
	t.Parallel()

	small, err := rsa.GenerateKey(randReader, 1024)
	require.NoError(t, err)

	for _, tc := range []struct {
		name    string
		setup   func(t *testing.T, dir string) (loadDir, active, wantPath string)
		wantErr string
	}{
		{
			name: "directory missing",
			setup: func(t *testing.T, dir string) (string, string, string) {
				missing := filepath.Join(dir, "nope")
				return missing, "a", missing
			},
		},
		{
			name: "no keys",
			setup: func(t *testing.T, dir string) (string, string, string) {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "README.txt"), nil, 0o644))
				return dir, "a", dir
			},
			wantErr: "no keys",
		},
		{
			name: "active not named",
			setup: func(t *testing.T, dir string) (string, string, string) {
				writePEM(t, dir, "a.pem", "PRIVATE KEY", pkcs8(t, edKey(t)), 0o600)
				return dir, "", dir
			},
			wantErr: "no active key",
		},
		{
			name: "active not present",
			setup: func(t *testing.T, dir string) (string, string, string) {
				writePEM(t, dir, "a.pem", "PRIVATE KEY", pkcs8(t, edKey(t)), 0o600)
				return dir, "b", filepath.Join(dir, "b.pem")
			},
			wantErr: "not found",
		},
		{
			name: "active is public only",
			setup: func(t *testing.T, dir string) (string, string, string) {
				writePEM(t, dir, "a.pem", "PRIVATE KEY", pkcs8(t, edKey(t)), 0o600)
				writePEM(t, dir, "b.pub.pem", "PUBLIC KEY", pkix(t, edKey(t).Public()), 0o644)
				return dir, "b", filepath.Join(dir, "b.pem")
			},
			wantErr: "only a public half",
		},
		{
			name: "not PEM",
			setup: func(t *testing.T, dir string) (string, string, string) {
				path := filepath.Join(dir, "a.pem")
				require.NoError(t, os.WriteFile(path, []byte("not a key"), 0o600))
				return dir, "a", path
			},
			wantErr: "no PEM block",
		},
		{
			name: "PEM with a broken body",
			setup: func(t *testing.T, dir string) (string, string, string) {
				return dir, "a", writePEM(t, dir, "a.pem", "PRIVATE KEY", []byte("garbage"), 0o600)
			},
		},
		{
			name: "ECDSA private key",
			setup: func(t *testing.T, dir string) (string, string, string) {
				return dir, "a", writePEM(t, dir, "a.pem", "PRIVATE KEY", pkcs8(t, ecKey(t)), 0o600)
			},
			wantErr: "unsupported",
		},
		{
			name: "ECDSA public key",
			setup: func(t *testing.T, dir string) (string, string, string) {
				writePEM(t, dir, "a.pem", "PRIVATE KEY", pkcs8(t, edKey(t)), 0o600)
				return dir, "a", writePEM(t, dir, "b.pub.pem", "PUBLIC KEY", pkix(t, &ecKey(t).PublicKey), 0o644)
			},
			wantErr: "unsupported",
		},
		{
			name: "RSA under 2048 bits",
			setup: func(t *testing.T, dir string) (string, string, string) {
				return dir, "a", writePEM(t, dir, "a.pem", "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(small), 0o600)
			},
			wantErr: "1024 bits",
		},
		{
			name: "a name holding two different keys",
			setup: func(t *testing.T, dir string) (string, string, string) {
				writePEM(t, dir, "a.pem", "PRIVATE KEY", pkcs8(t, edKey(t)), 0o600)
				return dir, "a", writePEM(t, dir, "a.pub.pem", "PUBLIC KEY", pkix(t, edKey(t).Public()), 0o644)
			},
			wantErr: "different keys",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loadDir, active, wantPath := tc.setup(t, t.TempDir())

			_, err := jwt.LoadKeyRing(loadDir, active, ttl)
			require.Error(t, err)
			require.Contains(t, err.Error(), wantPath)
			if tc.wantErr != "" {
				require.Contains(t, err.Error(), tc.wantErr)
			}
		})
	}
}

// A Kubernetes secret mount is a directory of symlinks into a "..data"
// directory, next to the timestamped directory it points at. Only the
// symlinks are keys.
func TestLoadKeyRingSkipsWhatIsNotAKey(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	data := filepath.Join(dir, "..2026_09_11_00_00_00.000000001")
	require.NoError(t, os.Mkdir(data, 0o700))
	writePEM(t, data, "a.pem", "PRIVATE KEY", pkcs8(t, edKey(t)), 0o600)
	require.NoError(
		t,
		os.Symlink(filepath.Base(data), filepath.Join(dir, "..data")),
	)
	require.NoError(
		t,
		os.Symlink(
			filepath.Join("..data", "a.pem"),
			filepath.Join(dir, "a.pem"),
		),
	)

	// Garbage that would fail to parse if it were read.
	require.NoError(
		t,
		os.WriteFile(filepath.Join(dir, ".hidden.pem"), []byte("x"), 0o600),
	)
	nested := filepath.Join(dir, "nested")
	require.NoError(t, os.Mkdir(nested, 0o700))
	require.NoError(
		t,
		os.WriteFile(filepath.Join(nested, "b.pem"), []byte("x"), 0o600),
	)
	require.NoError(
		t,
		os.WriteFile(filepath.Join(dir, "new.key"), []byte("x"), 0o600),
	)
	require.NoError(
		t,
		os.WriteFile(filepath.Join(dir, "README.txt"), []byte("x"), 0o600),
	)

	kr, err := jwt.LoadKeyRing(dir, "a", ttl)
	require.NoError(t, err)

	require.Len(t, kr.Keys(), 1)
	require.Equal(t, filepath.Join(dir, "a.pem"), kr.Keys()[0].Path)
	require.ElementsMatch(t, []string{"new.key", "README.txt"}, kr.Skipped())
}

// The kid is derived from the key, so the same key under several names is one
// ring member, and the private entry is the one that signs.
func TestLoadKeyRingMergesDuplicateKeys(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	dir := t.TempDir()
	a, b := edKey(t), edKey(t)
	writePEM(t, dir, "a.pub.pem", "PUBLIC KEY", pkix(t, a.Public()), 0o644)
	writePEM(t, dir, "a.pem", "PRIVATE KEY", pkcs8(t, a), 0o600)
	writePEM(t, dir, "copy.pem", "PRIVATE KEY", pkcs8(t, a), 0o600)
	writePEM(t, dir, "b.pub.pem", "PUBLIC KEY", pkix(t, b.Public()), 0o644)

	kr, err := jwt.LoadKeyRing(dir, "a", ttl)
	require.NoError(t, err)

	keys := kr.Keys()
	require.Len(t, keys, 4, "every file stays visible")
	names := make([]string, 0, len(keys))
	for _, k := range keys {
		names = append(names, k.Name)
	}
	require.Equal(t, []string{"a", "a", "b", "copy"}, names, "sorted by name")
	require.Equal(t, keys[0].ID, keys[1].ID)
	require.Equal(t, keys[0].ID, keys[3].ID)

	require.Len(t, kr.Ring().KeyIDs(), 2, "one member per distinct key")

	// The copy is a signer for the same kid, so it can be made active too.
	viaCopy, err := jwt.LoadKeyRing(dir, "copy", ttl)
	require.NoError(t, err)
	require.Equal(t, kr.Active().ID, viaCopy.Active().ID)

	token, err := viaCopy.Signer().Sign(ctx, jwt.Claims{Subject: "user-1"})
	require.NoError(t, err)
	_, err = kr.Ring().Verify(ctx, token.Reveal())
	require.NoError(t, err)
}

func TestLoadKeyRingFlagsLoosePermissions(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writePEM(t, dir, "loose.pem", "PRIVATE KEY", pkcs8(t, edKey(t)), 0o644)
	writePEM(t, dir, "tight.pem", "PRIVATE KEY", pkcs8(t, edKey(t)), 0o600)
	writePEM(
		t,
		dir,
		"public.pub.pem",
		"PUBLIC KEY",
		pkix(t, edKey(t).Public()),
		0o644,
	)

	kr, err := jwt.LoadKeyRing(dir, "tight", ttl)
	require.NoError(t, err)

	got := map[string]bool{}
	for _, k := range kr.Keys() {
		got[k.Name] = k.LoosePermissions
	}
	require.Equal(
		t,
		map[string]bool{"loose": true, "tight": false, "public": false},
		got,
	)
}

// TestKeyRingRotation is the reason a key ring exists: each step of a
// rotation is a file change on one replica, and at no point does a replica
// that is one step behind or ahead refuse a live token.
func TestKeyRingRotation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	keys := t.TempDir()
	oldInfo, err := jwt.WritePrivateKey(keys, "old", edKey(t))
	require.NoError(t, err)
	newInfo, err := jwt.WritePrivateKey(keys, "new", edKey(t))
	require.NoError(t, err)
	oldPriv, err := os.ReadFile(oldInfo.Path)
	require.NoError(t, err)
	oldKey, err := jwt.ParsePrivateKeyPEM(oldPriv)
	require.NoError(t, err)
	_, err = jwt.WritePublicKey(keys, "old", oldKey.Public())
	require.NoError(t, err)

	replica := func(t *testing.T, active string, files ...string) *jwt.KeyRing {
		t.Helper()
		dir := t.TempDir()
		for _, f := range files {
			copyFile(t, filepath.Join(keys, f), dir)
		}
		kr, err := jwt.LoadKeyRing(dir, active, ttl)
		require.NoError(t, err)
		return kr
	}

	// Before: every replica holds only the old key and signs with it.
	before := replica(t, "old", "old.pem")
	require.Equal(t, oldInfo.ID, before.Active().ID)
	t1, err := before.Signer().Sign(ctx, jwt.Claims{Subject: "minted-before"})
	require.NoError(t, err)

	// Publish: the new key is on this replica but not yet active.
	published := replica(t, "old", "old.pem", "new.pem")

	// Switch: another replica is already signing with the new key.
	switched := replica(t, "new", "old.pem", "new.pem")
	require.Equal(t, newInfo.ID, switched.Active().ID)
	t2, err := switched.Signer().Sign(ctx, jwt.Claims{Subject: "minted-after"})
	require.NoError(t, err)

	_, err = published.Ring().Verify(ctx, t2.Reveal())
	require.NoError(
		t,
		err,
		"a replica that has published the new key accepts its tokens",
	)
	_, err = switched.Ring().Verify(ctx, t1.Reveal())
	require.NoError(
		t,
		err,
		"a replica that has switched still accepts old tokens",
	)

	// Retire: the old private key is gone.
	retired := replica(t, "new", "new.pem")
	_, err = retired.Ring().Verify(ctx, t1.Reveal())
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown key id")
	_, err = retired.Ring().Verify(ctx, t2.Reveal())
	require.NoError(t, err)

	// Retiring the private half only: the public file keeps old tokens valid.
	publicOnly := replica(t, "new", "new.pem", "old.pub.pem")
	_, err = publicOnly.Ring().Verify(ctx, t1.Reveal())
	require.NoError(t, err)

	// An attacker who names a known kid but signs with their own key gets
	// nothing: the kid routes, and the routed key still checks the signature.
	forger, err := jwt.NewKeySigner(jwt.EdDSA, edKey(t),
		jwt.WithTTL(time.Hour), jwt.WithKeyID(oldInfo.ID))
	require.NoError(t, err)
	forged, err := forger.Sign(ctx, jwt.Claims{Subject: "admin"})
	require.NoError(t, err)
	_, err = published.Ring().Verify(ctx, forged.Reveal())
	require.Error(t, err)
}

// Verifier options reach every member of the ring, so a ring built with
// RequireType enforces it whichever key a token names.
func TestKeyRingAppliesVerifierOptions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	dir := t.TempDir()
	priv := edKey(t)
	info, err := jwt.WritePrivateKey(dir, "k", priv)
	require.NoError(t, err)

	kr, err := jwt.LoadKeyRing(dir, "k", jwt.KeyRingOptions{
		Signer: []jwt.SignerOption{
			jwt.WithTTL(time.Hour),
			jwt.WithType(jwt.TypeAccessToken),
		},
		Verifier: []jwt.VerifierOption{jwt.RequireType(jwt.TypeAccessToken)},
	})
	require.NoError(t, err)

	token, err := kr.Signer().Sign(ctx, jwt.Claims{Subject: "user-1"})
	require.NoError(t, err)
	_, err = kr.Ring().Verify(ctx, token.Reveal())
	require.NoError(t, err)

	untyped, err := jwt.NewKeySigner(
		jwt.EdDSA,
		priv,
		jwt.WithTTL(time.Hour),
		jwt.WithKeyID(info.ID),
	)
	require.NoError(t, err)
	other, err := untyped.Sign(ctx, jwt.Claims{Subject: "user-1"})
	require.NoError(t, err)
	_, err = kr.Ring().Verify(ctx, other.Reveal())
	require.Error(t, err, "same key, same kid, wrong type")

	// And the kid is the ring's, even if an option tried to set another.
	overridden, err := jwt.LoadKeyRing(dir, "k", jwt.KeyRingOptions{
		Signer: []jwt.SignerOption{
			jwt.WithTTL(time.Hour),
			jwt.WithKeyID("chosen-by-hand"),
		},
	})
	require.NoError(t, err)
	require.Equal(t, info.ID, overridden.Signer().KeyID())
}

func TestDeriveSecret(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_, err := jwt.WritePrivateKey(dir, "a", edKey(t))
	require.NoError(t, err)
	_, err = jwt.WritePrivateKey(dir, "b", edKey(t))
	require.NoError(t, err)

	first, err := jwt.LoadKeyRing(dir, "a", ttl)
	require.NoError(t, err)
	second, err := jwt.LoadKeyRing(dir, "a", ttl)
	require.NoError(t, err)
	other, err := jwt.LoadKeyRing(dir, "b", ttl)
	require.NoError(t, err)

	secret := first.DeriveSecret("cookie")
	require.Len(t, secret, 32)
	require.Equal(
		t,
		secret,
		second.DeriveSecret("cookie"),
		"every replica derives the same value",
	)
	require.NotEqual(
		t,
		secret,
		first.DeriveSecret("csrf"),
		"info separates uses",
	)
	require.NotEqual(
		t,
		secret,
		other.DeriveSecret("cookie"),
		"a new active key rotates it",
	)
}
