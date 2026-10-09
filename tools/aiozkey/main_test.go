package main_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/jwt"
)

// binary is the aiozkey the tests run: built once, from this package, the way
// `go install` builds the one people run.
var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "aiozkey-e2e-")
	if err != nil {
		panic(err)
	}
	binary = filepath.Join(dir, "aiozkey")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		panic("building aiozkey: " + err.Error())
	}

	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// run executes the binary and returns its stdout, stderr and exit code.
func run(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	var out, errOut bytes.Buffer
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	if exitErr, ok := err.(*exec.ExitError); ok {
		return out.String(), errOut.String(), exitErr.ExitCode()
	}
	require.NoError(t, err)
	return out.String(), errOut.String(), 0
}

var kidLine = regexp.MustCompile(`(?m)^kid:\s+(\S+)$`)

// What a service operator does: generate a key pair, point LoadKeyRing at the
// directory, and sign and verify with it.
func TestKeygenLoadsIntoKeyRing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	for _, tc := range []struct {
		args []string
		alg  jwt.Algorithm
	}{
		{nil, jwt.EdDSA},
		{[]string{"--type", "rsa", "--rsa-bits", "2048"}, jwt.RS256},
	} {
		t.Run(string(tc.alg), func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(t.TempDir(), "keys")

			stdout, stderr, code := run(t,
				append([]string{"jwt", "keygen", "--dir", dir, "--name", "2026-10"}, tc.args...)...)
			require.Zero(t, code, stderr)
			require.Contains(t, stdout, "alg:     "+string(tc.alg))
			require.NotContains(t, stdout, "PRIVATE KEY")

			match := kidLine.FindStringSubmatch(stdout)
			require.Len(t, match, 2, stdout)

			priv, err := os.Stat(filepath.Join(dir, "2026-10.pem"))
			require.NoError(t, err)
			if runtime.GOOS != "windows" {
				require.Equal(t, os.FileMode(0o600), priv.Mode().Perm())
			}

			keys, err := jwt.LoadKeyRing(dir, "2026-10", jwt.KeyRingOptions{
				Signer: []jwt.SignerOption{jwt.WithTTL(time.Minute)},
			})
			require.NoError(t, err)
			require.Equal(t, match[1], keys.Active().ID, "printed kid is the one tokens carry")
			require.Equal(t, tc.alg, keys.Active().Alg)

			token, err := keys.Signer().Sign(ctx, jwt.Claims{Subject: "user-1"})
			require.NoError(t, err)
			claims, err := keys.Ring().Verify(ctx, token.Reveal())
			require.NoError(t, err)
			require.Equal(t, "user-1", claims.Subject)
		})
	}
}

// The public key can go to a directory of its own, which is a verify-only
// key ring.
func TestKeygenSeparatePublicDir(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	privDir := filepath.Join(t.TempDir(), "private")
	pubDir := filepath.Join(t.TempDir(), "public")

	_, stderr, code := run(t, "jwt", "keygen", "--dir", privDir, "--public-dir", pubDir, "--name", "k1")
	require.Zero(t, code, stderr)
	require.NoFileExists(t, filepath.Join(privDir, "k1.pub.pem"))
	require.FileExists(t, filepath.Join(pubDir, "k1.pub.pem"))

	signing, err := jwt.LoadKeyRing(privDir, "k1", jwt.KeyRingOptions{
		Signer: []jwt.SignerOption{jwt.WithTTL(time.Minute)},
	})
	require.NoError(t, err)
	token, err := signing.Signer().Sign(ctx, jwt.Claims{Subject: "user-1"})
	require.NoError(t, err)

	pemBytes, err := os.ReadFile(filepath.Join(pubDir, "k1.pub.pem"))
	require.NoError(t, err)
	pub, err := jwt.ParsePublicKeyPEM(pemBytes)
	require.NoError(t, err)
	verifier, err := jwt.NewKeyVerifier(jwt.EdDSA, pub)
	require.NoError(t, err)
	_, err = verifier.Verify(ctx, token.Reveal())
	require.NoError(t, err)
}

// An existing key is never replaced, and a failed run leaves no half pair.
func TestKeygenNeverOverwrites(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	_, stderr, code := run(t, "jwt", "keygen", "--dir", dir, "--name", "k1")
	require.Zero(t, code, stderr)
	before, err := os.ReadFile(filepath.Join(dir, "k1.pem"))
	require.NoError(t, err)

	_, stderr, code = run(t, "jwt", "keygen", "--dir", dir, "--name", "k1")
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "refusing to overwrite a key")
	after, err := os.ReadFile(filepath.Join(dir, "k1.pem"))
	require.NoError(t, err)
	require.Equal(t, before, after)

	// Only the public half exists: the private key this run writes must be
	// removed again when the public write is refused.
	_, stderr, code = run(t, "jwt", "keygen", "--dir", dir, "--name", "k2")
	require.Zero(t, code, stderr)
	require.NoError(t, os.Remove(filepath.Join(dir, "k2.pem")))
	_, stderr, code = run(t, "jwt", "keygen", "--dir", dir, "--name", "k2")
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "k2.pub.pem already exists")
	require.NoFileExists(t, filepath.Join(dir, "k2.pem"))
}

func TestSecretIsAcceptedByHMACSigner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	for _, tc := range []struct {
		args   []string
		alg    jwt.Algorithm
		decode func(string) ([]byte, error)
		n      int
	}{
		{nil, jwt.HS256, base64.StdEncoding.DecodeString, 32},
		{[]string{"--alg", "HS384"}, jwt.HS384, base64.StdEncoding.DecodeString, 48},
		{[]string{"--alg", "hs512", "--encoding", "hex"}, jwt.HS512, hex.DecodeString, 64},
	} {
		t.Run(string(tc.alg), func(t *testing.T) {
			t.Parallel()
			stdout, stderr, code := run(t, append([]string{"jwt", "secret"}, tc.args...)...)
			require.Zero(t, code, stderr)
			require.Empty(t, stderr)

			key, err := tc.decode(strings.TrimSuffix(stdout, "\n"))
			require.NoError(t, err)
			require.Len(t, key, tc.n)

			signer, err := jwt.NewHMACSigner(tc.alg, key, jwt.WithTTL(time.Minute))
			require.NoError(t, err)
			verifier, err := jwt.NewHMACVerifier(tc.alg, key)
			require.NoError(t, err)
			token, err := signer.Sign(ctx, jwt.Claims{Subject: "user-1"})
			require.NoError(t, err)
			_, err = verifier.Verify(ctx, token.Reveal())
			require.NoError(t, err)

			again, _, _ := run(t, append([]string{"jwt", "secret"}, tc.args...)...)
			require.NotEqual(t, stdout, again)
		})
	}
}

func TestBadInputFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"jwt", "secret", "--alg", "RS256"}, "has no shared secret"},
		{[]string{"jwt", "secret", "--encoding", "base32"}, "unknown encoding"},
		{[]string{"jwt", "keygen", "--name", "k1"}, `required flag(s) "dir" not set`},
		{[]string{"jwt", "keygen", "--dir", dir}, `required flag(s) "name" not set`},
		{[]string{"jwt", "keygen", "--dir", dir, "--name", "k1.pub"}, "must not end in"},
		{[]string{"jwt", "keygen", "--dir", dir, "--name", "k1", "--type", "ecdsa"}, "unknown key type"},
		{[]string{"jwt", "keygen", "--dir", dir, "--name", "k1", "--type", "rsa", "--rsa-bits", "1024"}, "refused"},
	} {
		stdout, stderr, code := run(t, tc.args...)
		require.Equal(t, 1, code, tc.args)
		require.Empty(t, stdout, tc.args)
		require.Contains(t, stderr, tc.want, tc.args)
	}
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries, "a refused run writes nothing")
}

func TestVersion(t *testing.T) {
	t.Parallel()
	stdout, stderr, code := run(t, "version")
	require.Zero(t, code, stderr)
	require.Contains(t, stdout, "go:      "+runtime.Version())
}
