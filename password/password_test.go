package password_test

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/config"
	"gitlab.internal/tuan.quang.tran/aioz-common/memory"
	"gitlab.internal/tuan.quang.tran/aioz-common/password"
)

const good = "correct horse battery staple"

// Made by the reference implementation (the argon2 CLI from alpine 3.22's
// argon2 package, salt "saltsaltsaltsalt", -id -t 2 -k 19456 -p 1 -l 32):
// our defaults, so no rehash is due.
const (
	referenceASCII = "$argon2id$v=19$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0c2FsdA$QKHrg5tayLGcN+Y0HVPNaBqykOVLUxlMkZycXE1uWRM"
	// The NFC bytes of "café au lait plaît".
	referenceNFC = "$argon2id$v=19$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0c2FsdA$o3uI6MZ8QXCqqfqXkioRy9HWKc7qy2NFyckJlMqsRuk"
)

// Made by htpasswd -bnBC 10 (apache2-utils, alpine 3.22), which is
// crypt_blowfish: a different bcrypt from x/crypto's.
const (
	legacyBcrypt = "$2y$10$v.x1YHl0vq0Ow8PmXaQiIu5iJTcXYgmUHLJLzd5Rd2HLBpGJxTZpi" // good
	// 80 times "a"; bcrypt read the first 72 bytes.
	legacyBcryptLong = "$2y$10$cbo.sRrt3ZZilvx35Q73re5DmADGLukmXWgNhnseLIq16hbDqURPS"
)

func newHasher(t testing.TB, cfg password.Config) *password.Hasher {
	t.Helper()
	h, err := password.New(cfg)
	require.NoError(t, err)
	return h
}

func verify(
	t *testing.T,
	h *password.Hasher,
	pw, stored string,
) password.Result {
	t.Helper()
	result, err := h.Verify(t.Context(), pw, stored)
	require.NoError(t, err)
	return result
}

func hash(t *testing.T, h *password.Hasher, pw string) string {
	t.Helper()
	stored, err := h.Hash(t.Context(), pw)
	require.NoError(t, err)
	return stored
}

func TestHashThenVerify(t *testing.T) {
	t.Parallel()
	h := newHasher(t, password.Config{})

	stored := hash(t, h, good)
	require.True(
		t,
		strings.HasPrefix(stored, "$argon2id$v=19$m=19456,t=2,p=1$"),
		stored,
	)

	require.Equal(t, password.Result{OK: true}, verify(t, h, good, stored))
	require.Equal(t, password.Result{}, verify(t, h, good+"!", stored))
}

func TestEveryHashHasItsOwnSalt(t *testing.T) {
	t.Parallel()
	h := newHasher(t, password.Config{})

	require.NotEqual(t, hash(t, h, good), hash(t, h, good))
}

func TestReferenceImplementationHashesVerify(t *testing.T) {
	t.Parallel()
	h := newHasher(t, password.Config{})

	require.Equal(
		t,
		password.Result{OK: true},
		verify(t, h, good, referenceASCII),
	)
	require.False(
		t,
		verify(t, h, "correct horse battery stapl", referenceASCII).OK,
	)
}

// The reference CLI hashed the composed form. A keyboard or browser that sends
// the decomposed one - e followed by a combining accent - must still log in.
func TestDecomposedInputMatchesAComposedHash(t *testing.T) {
	t.Parallel()
	h := newHasher(t, password.Config{})

	composed := "caf\u00e9 au lait pla\u00eet"
	decomposed := "cafe\u0301 au lait plai\u0302t"
	require.NotEqual(t, composed, decomposed)

	require.True(t, verify(t, h, composed, referenceNFC).OK)
	require.True(t, verify(t, h, decomposed, referenceNFC).OK)
}

// x/crypto's argon2id known-answer vectors, password "password", salt
// "somesalt", written as PHC strings. They are far below the floor and verify
// anyway, flagged for rehash: a hash made weaker than today's policy must
// still let its owner in, once, to be upgraded.
func TestWeakHashesVerifyAndNeedRehash(t *testing.T) {
	t.Parallel()
	h := newHasher(t, password.Config{})

	for _, v := range []struct {
		m, t, p uint32
		key     string
	}{
		{64, 2, 1, "068d62b26455936aa6ebe60060b0a65870dbfa3ddf8d41f7"},
		{64, 2, 2, "350ac37222f436ccb5c0972f1ebd3bf6b958bf2071841362"},
		{4096, 4, 4, "145db9733a9f4ee43edf33c509be96b934d505a4efb33c5a"},
	} {
		key, err := hex.DecodeString(v.key)
		require.NoError(t, err)
		stored := fmt.Sprintf(
			"$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
			v.m,
			v.t,
			v.p,
			base64.RawStdEncoding.EncodeToString([]byte("somesalt")),
			base64.RawStdEncoding.EncodeToString(key),
		)

		require.Equal(
			t,
			password.Result{OK: true, NeedsRehash: true},
			verify(t, h, "password", stored),
			stored,
		)
		require.False(t, verify(t, h, "Password", stored).OK, stored)
	}
}

// Hashes from before this package verify against the raw input, exactly as
// they were made, and always need a rehash.
func TestLegacyBcryptVerifiesAndNeedsRehash(t *testing.T) {
	t.Parallel()
	h := newHasher(t, password.Config{})

	require.Equal(
		t,
		password.Result{OK: true, NeedsRehash: true},
		verify(t, h, good, legacyBcrypt),
	)
	require.Equal(t, password.Result{}, verify(t, h, good+"!", legacyBcrypt))

	for _, prefix := range []string{"$2a$", "$2b$"} {
		stored := prefix + strings.TrimPrefix(legacyBcrypt, "$2y$")
		require.True(t, verify(t, h, good, stored).OK, prefix)
	}
}

// bcrypt used only the first 72 bytes. The user typed all 80 and must still
// get in; that the first 72 alone also match is what the old hash is.
func TestLegacyBcryptOverSeventyTwoBytes(t *testing.T) {
	t.Parallel()
	h := newHasher(t, password.Config{})

	require.True(t, verify(t, h, strings.Repeat("a", 80), legacyBcryptLong).OK)
	require.True(t, verify(t, h, strings.Repeat("a", 72), legacyBcryptLong).OK)
	require.False(t, verify(t, h, strings.Repeat("a", 71), legacyBcryptLong).OK)
}

// Legacy hashes verify the bytes the user typed, not their NFC form: this is
// htpasswd's hash of the decomposed "café au lait plaît", and it matches
// those bytes only, exactly as it did before this package.
func TestLegacyBcryptVerifiesTheRawInput(t *testing.T) {
	t.Parallel()
	h := newHasher(t, password.Config{})
	stored := "$2y$10$ZJYPUYIGzmtzq37esMNIfuuhAzIe0gW/bywv5eUIYAaWVwGPrCIxC"

	require.True(t, verify(t, h, "café au lait plaît", stored).OK)
	require.False(t, verify(t, h, "café au lait plaît", stored).OK)
}

// Response time must not tell an attacker whether an account exists, so every
// path through Verify costs one derivation - including the account that is
// not there, which is checked against a dummy hash. A timing test would be
// flaky; counting is exact.
func TestEveryVerifyDerivesExactlyOnce(t *testing.T) {
	t.Parallel()
	h := newHasher(t, password.Config{})
	stored := hash(t, h, good)
	count, _ := password.Instrument(h)

	for _, c := range []struct {
		name, pw, stored string
		ok               bool
	}{
		{"an unknown user", good, "", false},
		{"a wrong password", "wrong", stored, false},
		{"the right password", good, stored, true},
		{"a legacy hash, right", good, legacyBcrypt, true},
		{"a legacy hash, wrong", "wrong", legacyBcrypt, false},
	} {
		before := count.Load()
		result, err := h.Verify(t.Context(), c.pw, c.stored)
		require.NoError(t, err, c.name)
		require.Equal(t, c.ok, result.OK, c.name)
		require.Equal(t, before+1, count.Load(), c.name)
	}
}

func TestMalformedHashesAreErrors(t *testing.T) {
	t.Parallel()
	h := newHasher(t, password.Config{})

	salt, key := "c2FsdHNhbHRzYWx0c2FsdA", "QKHrg5tayLGcN+Y0HVPNaBqykOVLUxlMkZycXE1uWRM"
	for name, stored := range map[string]string{
		"plain text":            good,
		"argon2i":               "$argon2i$v=19$m=19456,t=2,p=1$" + salt + "$" + key,
		"argon2d":               "$argon2d$v=19$m=19456,t=2,p=1$" + salt + "$" + key,
		"an old version":        "$argon2id$v=16$m=19456,t=2,p=1$" + salt + "$" + key,
		"no version":            "$argon2id$m=19456,t=2,p=1$" + salt + "$" + key,
		"parameters reordered":  "$argon2id$v=19$t=2,m=19456,p=1$" + salt + "$" + key,
		"a leading zero":        "$argon2id$v=19$m=019456,t=2,p=1$" + salt + "$" + key,
		"a plus sign":           "$argon2id$v=19$m=+19456,t=2,p=1$" + salt + "$" + key,
		"an extra parameter":    "$argon2id$v=19$m=19456,t=2,p=1,k=1$" + salt + "$" + key,
		"no key":                "$argon2id$v=19$m=19456,t=2,p=1$" + salt,
		"an extra field":        "$argon2id$v=19$m=19456,t=2,p=1$" + salt + "$" + key + "$x",
		"padded base64":         "$argon2id$v=19$m=19456,t=2,p=1$" + salt + "==$" + key,
		"url base64":            "$argon2id$v=19$m=19456,t=2,p=1$" + salt + "$" + strings.ReplaceAll(key, "+", "-"),
		"memory over 1 GiB":     "$argon2id$v=19$m=1048577,t=2,p=1$" + salt + "$" + key,
		"65 iterations":         "$argon2id$v=19$m=19456,t=65,p=1$" + salt + "$" + key,
		"no iterations":         "$argon2id$v=19$m=19456,t=0,p=1$" + salt + "$" + key,
		"no parallelism":        "$argon2id$v=19$m=19456,t=2,p=0$" + salt + "$" + key,
		"256 lanes":             "$argon2id$v=19$m=19456,t=2,p=256$" + salt + "$" + key,
		"a 4-byte salt":         "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$" + key,
		"an 8-byte key":         "$argon2id$v=19$m=19456,t=2,p=1$" + salt + "$QKHrg5tayLE",
		"trailing space":        referenceASCII + " ",
		"a line break":          "$argon2id$v=19$m=19456,t=2,p=1$" + salt + "$" + key[:20] + "\n" + key[20:],
		"non-canonical base64":  "$argon2id$v=19$m=19456,t=2,p=1$" + salt + "$" + key[:len(key)-1] + "N",
		"a truncated bcrypt":    legacyBcrypt[:40],
		"a bcrypt cost of 31":   "$2b$31$" + strings.TrimPrefix(legacyBcrypt, "$2y$10$"),
		"an unknown identifier": "$scrypt$ln=16,r=8,p=1$" + salt + "$" + key,
	} {
		result, err := h.Verify(t.Context(), good, stored)
		require.Error(t, err, name)
		require.False(t, result.OK, name)
	}
}

// Past MaxLength there is nothing to derive: the input is refused before any
// work, so a megabyte of password costs the server nothing.
func TestAnOverlongPasswordIsRefusedWithoutDeriving(t *testing.T) {
	t.Parallel()
	h := newHasher(t, password.Config{})
	stored := hash(t, h, good)
	count, _ := password.Instrument(h)

	_, err := h.Verify(t.Context(), strings.Repeat("a", 1<<20), stored)
	require.ErrorIs(t, err, password.ErrTooLong)
	_, err = h.Verify(t.Context(), strings.Repeat("a", 257), stored)
	require.ErrorIs(t, err, password.ErrTooLong)
	_, err = h.Hash(t.Context(), strings.Repeat("a", 257))
	require.ErrorIs(t, err, password.ErrTooLong)

	require.Zero(t, count.Load())
}

// Length is counted in characters as a person sees them: code points after
// NFC, so neither an emoji (four bytes) nor an accent typed as two code
// points counts more than once.
func TestCheckCountsCharacters(t *testing.T) {
	t.Parallel()
	h := newHasher(t, password.Config{})

	require.NoError(t, h.Check(strings.Repeat("a", 15)))
	require.ErrorIs(t, h.Check(strings.Repeat("a", 14)), password.ErrTooShort)

	require.NoError(t, h.Check(strings.Repeat("\U0001F511", 15)))
	require.ErrorIs(
		t,
		h.Check(strings.Repeat("\U0001F511", 14)),
		password.ErrTooShort,
	)

	require.NoError(t, h.Check(strings.Repeat("e\u0301", 15)))
	require.ErrorIs(t, h.Check(strings.Repeat("e\u0301", 14)), password.ErrTooShort)

	require.NoError(t, h.Check(strings.Repeat("a", 256)))
	require.ErrorIs(t, h.Check(strings.Repeat("a", 257)), password.ErrTooLong)
}

func TestCheckRefusesInvalidUTF8(t *testing.T) {
	t.Parallel()
	h := newHasher(t, password.Config{})

	err := h.Check("fifteen-or-more-\xff")
	require.Error(t, err)
	require.NotErrorIs(t, err, password.ErrTooShort)
	require.NotErrorIs(t, err, password.ErrTooLong)
}

// Each derivation holds Memory, so a burst of logins must queue rather than
// allocate without bound and get the process OOM-killed.
func TestDerivationsWaitForASlot(t *testing.T) {
	t.Parallel()
	h := newHasher(t, password.Config{MaxConcurrent: 2})
	count, peak := password.Instrument(h)

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			_, err := h.Hash(t.Context(), good)
			assert.NoError(t, err)
		})
	}
	wg.Wait()

	require.Equal(t, int64(8), count.Load())
	require.LessOrEqual(t, peak.Load(), int64(2))
}

func TestACanceledContextIsNotWaitedOn(t *testing.T) {
	t.Parallel()
	h := newHasher(t, password.Config{})
	stored := hash(t, h, good)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := h.Hash(ctx, good)
	require.ErrorIs(t, err, context.Canceled)
	_, err = h.Verify(ctx, good, stored)
	require.ErrorIs(t, err, context.Canceled)
}

// OWASP's argon2id pairs, each an equal defense: memory in KiB for a number
// of iterations. Anything below its pair is refused.
func TestNewEnforcesTheOWASPFloor(t *testing.T) {
	t.Parallel()

	for _, pair := range []struct {
		kib        int64
		iterations int
	}{
		{47104, 1},
		{19456, 2},
		{12288, 3},
		{9216, 4},
		{7168, 5},
		{7168, 10},
	} {
		at := password.Config{
			Memory:     memory.Size(pair.kib) * memory.KiB,
			Iterations: pair.iterations,
		}
		_, err := password.New(at)
		require.NoError(t, err, "%d KiB, t=%d", pair.kib, pair.iterations)

		below := at
		below.Memory -= memory.KiB
		_, err = password.New(below)
		require.Error(t, err, "%d KiB, t=%d", pair.kib-1, pair.iterations)
	}
}

func TestNewRejects(t *testing.T) {
	t.Parallel()

	for name, cfg := range map[string]password.Config{
		"memory over 1 GiB":          {Memory: memory.GiB + memory.KiB, Iterations: 1},
		"memory not in whole KiB":    {Memory: 19*memory.MiB + 1},
		"negative memory":            {Memory: -memory.MiB},
		"65 iterations":              {Iterations: 65},
		"negative iterations":        {Iterations: -1},
		"256 lanes":                  {Parallelism: 256},
		"negative parallelism":       {Parallelism: -1},
		"negative concurrency":       {MaxConcurrent: -1},
		"a minimum below 8":          {MinLength: 7},
		"a maximum below 64":         {MaxLength: 63},
		"a minimum over the maximum": {MinLength: 100, MaxLength: 64},
	} {
		_, err := password.New(cfg)
		require.Error(t, err, name)
	}
}

func TestConfigBinds(t *testing.T) {
	t.Parallel()

	var cfg struct {
		Password password.Config
	}
	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	require.NotPanics(t, func() { config.BindFlags(flags, &cfg) })

	require.Equal(t, 19*memory.MiB, cfg.Password.Memory)
	require.Equal(t, 2, cfg.Password.Iterations)
	require.Equal(t, 1, cfg.Password.Parallelism)
	require.Equal(t, 0, cfg.Password.MaxConcurrent)
	require.Equal(t, 15, cfg.Password.MinLength)
	require.Equal(t, 256, cfg.Password.MaxLength)
}

// A decoded hash encodes back to exactly the same string. Anything the
// decoder accepts and could not reproduce - a leading zero, padding, a
// reordered parameter - is laxness a tampered row could use.
func FuzzPHCRoundTrip(f *testing.F) {
	f.Add(referenceASCII)
	f.Add(referenceNFC)
	f.Add(
		"$argon2id$v=19$m=64,t=2,p=1$c29tZXNhbHQ$Bo1ismRVk2qm6+YAYLCmWHDb+j3fjUH3",
	)
	f.Add(
		"$argon2id$v=19$m=019456,t=2,p=1$c2FsdHNhbHRzYWx0c2FsdA$QKHrg5tayLGcN+Y0HVPNaBqykOVLUxlMkZycXE1uWRM",
	)
	f.Fuzz(func(t *testing.T, stored string) {
		out, err := password.RoundTrip(stored)
		if err != nil {
			return
		}
		require.Equal(t, stored, out)
	})
}

func BenchmarkHashAtDefaults(b *testing.B) {
	h := newHasher(b, password.Config{MaxConcurrent: 1})
	for b.Loop() {
		if _, err := h.Hash(b.Context(), good); err != nil {
			b.Fatal(err)
		}
	}
}
