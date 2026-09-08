package secret_test

import (
	"crypto/sha256"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"gitlab.internal/tuan.quang.tran/aioz-common/secret"

	"github.com/stretchr/testify/require"
)

func TestHashOfShape(t *testing.T) {
	t.Parallel()

	h := secret.HashOf("hello")
	require.Len(t, string(h), sha256.Size*2)
	require.Equal(t, strings.ToLower(string(h)), string(h))
	require.True(t, h.Valid())
	require.Equal(t, secret.SchemeSHA256, h.Scheme())
	require.False(t, h.IsZero())
}

func TestHashOfIsDeterministic(t *testing.T) {
	t.Parallel()

	require.Equal(t, secret.HashOf("x"), secret.HashOf("x"))
	require.NotEqual(t, secret.HashOf("x"), secret.HashOf("y"))
}

// TestHashOfMatchesDepinHashToken pins the stored form to what depin's
// coord/admin/credentials.go HashToken already writes: sha256 of the presented
// string, hex encoded, lowercase, unsalted. If this fails, migrating
// coord/admin onto this package stops being a no-op and needs a backfill.
func TestHashOfMatchesDepinHashToken(t *testing.T) {
	t.Parallel()

	depinHashToken := func(token string) string {
		sum := sha256.Sum256([]byte(token))
		return hex.EncodeToString(sum[:])
	}

	for _, in := range []string{"", "x", strings.Repeat("a", 64), "3f2a9c"} {
		require.Equal(t, depinHashToken(in), string(secret.HashOf(in)), "input %q", in)
	}

	// A literal vector, so the test still means something if the helper above
	// is ever "simplified" into calling the package under test.
	require.Equal(t,
		"2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881",
		string(secret.HashOf("x")))
}

func TestEqualAgreesWithComparison(t *testing.T) {
	t.Parallel()

	base := secret.HashOf("secret")
	same := secret.HashOf("secret")

	differLast := secret.Hash(string(base)[:len(base)-1] + "0")
	if differLast == base {
		differLast = secret.Hash(string(base)[:len(base)-1] + "1")
	}
	differFirst := secret.Hash("0" + string(base)[1:])
	if differFirst == base {
		differFirst = secret.Hash("1" + string(base)[1:])
	}

	for name, pair := range map[string][2]secret.Hash{
		"identical":     {base, same},
		"differ last":   {base, differLast},
		"differ first":  {base, differFirst},
		"differ length": {base, secret.Hash(string(base)[:10])},
		"both empty":    {"", ""},
		"empty vs set":  {"", base},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, pair[0] == pair[1], pair[0].Equal(pair[1]))
		})
	}
}

func TestVerify(t *testing.T) {
	t.Parallel()

	stored := secret.HashOf("presented")
	require.True(t, secret.Verify("presented", stored))
	require.False(t, secret.Verify("Presented", stored))
	require.False(t, secret.Verify("", stored))
}

func TestVerifyUnknownScheme(t *testing.T) {
	t.Parallel()

	for _, stored := range []secret.Hash{
		"",
		"not-a-hash",
		"$s3$deadbeef", // a future scheme this build cannot check
		secret.Hash(strings.Repeat("Z", 64)),
		secret.Hash(strings.Repeat("A", 64)), // uppercase hex is not what we write
	} {
		require.False(t, secret.Verify("presented", stored), "stored %q", stored)
		require.NotPanics(t, func() { secret.Verify("presented", stored) })
	}
}

func TestScheme(t *testing.T) {
	t.Parallel()

	require.Equal(t, secret.SchemeSHA256, secret.HashOf("x").Scheme())
	require.Equal(t, "s3", secret.Hash("$s3$deadbeef").Scheme())
	require.Equal(t, "argon2id", secret.Hash("$argon2id$whatever").Scheme())
	require.Empty(t, secret.Hash("$unterminated").Scheme())
	require.Empty(t, secret.Hash("short").Scheme())
	require.Empty(t, secret.Hash("").Scheme())
}

func TestHashRoundTrips(t *testing.T) {
	t.Parallel()

	h := secret.HashOf("round trip")

	t.Run("text", func(t *testing.T) {
		b, err := h.MarshalText()
		require.NoError(t, err)
		var got secret.Hash
		require.NoError(t, got.UnmarshalText(b))
		require.Equal(t, h, got)
	})

	t.Run("json", func(t *testing.T) {
		b, err := json.Marshal(h)
		require.NoError(t, err)
		require.Equal(t, `"`+string(h)+`"`, string(b))
		var got secret.Hash
		require.NoError(t, json.Unmarshal(b, &got))
		require.Equal(t, h, got)
	})

	t.Run("sql", func(t *testing.T) {
		v, err := h.Value()
		require.NoError(t, err)
		require.Equal(t, string(h), v)

		var got secret.Hash
		require.NoError(t, got.Scan(string(h)))
		require.Equal(t, h, got)

		require.NoError(t, got.Scan([]byte(h)))
		require.Equal(t, h, got)

		require.NoError(t, got.Scan(nil))
		require.True(t, got.IsZero())

		require.Error(t, got.Scan(42))
	})

	t.Run("sql default converter accepts it", func(t *testing.T) {
		// The mirror of the Token test: a Hash is meant to reach a column.
		v, err := driver.DefaultParameterConverter.ConvertValue(h)
		require.NoError(t, err)
		require.Equal(t, string(h), v)
	})

	t.Run("json unmarshal rejects a non-string", func(t *testing.T) {
		var got secret.Hash
		require.Error(t, json.Unmarshal([]byte(`42`), &got))
	})
}
