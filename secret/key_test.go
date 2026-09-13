package secret_test

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"gitlab.internal/tuan.quang.tran/aioz-common/secret"

	"github.com/stretchr/testify/require"
)

const testPrefix = "aioz"

func TestNewKeyShape(t *testing.T) {
	t.Parallel()

	key, err := secret.NewKey(testPrefix)
	require.NoError(t, err)

	presented := key.Secret.Reveal()

	wantLen := len(testPrefix) + 1 + secret.KeyIDLen + secret.KeySecretLen + secret.KeyChecksumLen
	require.Len(t, presented, wantLen)
	require.Equal(t, 66, wantLen, "the documented length for a four-character prefix")

	require.True(t, strings.HasPrefix(presented, testPrefix+"_"))
	require.Regexp(t, regexp.MustCompile(`^aioz_[0-9A-Za-z]{61}$`), presented,
		"the shape a secret scanner matches on")

	require.Equal(t, testPrefix, key.Prefix)
	require.Len(t, key.ID, secret.KeyIDLen)
	require.Contains(t, presented, key.ID)
	require.Equal(t, secret.HashOf(presented), key.Hash)
}

func TestNewKeyIsUnique(t *testing.T) {
	t.Parallel()

	seen := map[string]bool{}
	for range 100 {
		key, err := secret.NewKey(testPrefix)
		require.NoError(t, err)
		require.False(t, seen[key.ID], "duplicate id %q", key.ID)
		seen[key.ID] = true
	}
}

func TestParseKeyRoundTrip(t *testing.T) {
	t.Parallel()

	key, err := secret.NewKey(testPrefix)
	require.NoError(t, err)

	ref, err := secret.ParseKey(key.Secret.Reveal())
	require.NoError(t, err)
	require.Equal(t, key.Prefix, ref.Prefix)
	require.Equal(t, key.ID, ref.ID)
}

func TestVerifyKey(t *testing.T) {
	t.Parallel()

	key, err := secret.NewKey(testPrefix)
	require.NoError(t, err)
	require.True(t, secret.Verify(key.Secret.Reveal(), key.Hash))

	other, err := secret.NewKey(testPrefix)
	require.NoError(t, err)
	require.False(t, secret.Verify(other.Secret.Reveal(), key.Hash))

	// Same id, different secret: the id is a lookup handle, not a credential.
	forged := strings.Replace(other.Secret.Reveal(), other.ID, key.ID, 1)
	require.False(t, secret.Verify(forged, key.Hash))
}

// TestParseKeyRejectsEverySingleCharacterMutation is the checksum's actual
// job, tested exhaustively rather than on a sample.
func TestParseKeyRejectsEverySingleCharacterMutation(t *testing.T) {
	t.Parallel()

	key, err := secret.NewKey(testPrefix)
	require.NoError(t, err)
	presented := key.Secret.Reveal()

	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

	var mutations int
	for i := range len(presented) {
		for j := range len(alphabet) {
			if presented[i] == alphabet[j] {
				continue
			}
			mutated := presented[:i] + string(alphabet[j]) + presented[i+1:]
			if mutated == presented {
				continue
			}
			mutations++
			_, err := secret.ParseKey(mutated)
			require.Error(t, err, "mutation at %d to %q was accepted", i, string(alphabet[j]))
		}
	}
	require.Positive(t, mutations)
}

func TestParseKeyRejectsTruncationAtEveryOffset(t *testing.T) {
	t.Parallel()

	key, err := secret.NewKey(testPrefix)
	require.NoError(t, err)
	presented := key.Secret.Reveal()

	for i := range len(presented) {
		_, err := secret.ParseKey(presented[:i])
		require.Error(t, err, "truncation to %d characters was accepted", i)
	}
}

func TestParseKeyRejectsASwappedPrefix(t *testing.T) {
	t.Parallel()

	key, err := secret.NewKey(testPrefix)
	require.NoError(t, err)

	body := strings.TrimPrefix(key.Secret.Reveal(), testPrefix)
	_, err = secret.ParseKey("aioz2" + body)
	require.Error(t, err, "the checksum covers the prefix, so re-labeling a key must fail")
}

func TestParseKeyRejectsMalformed(t *testing.T) {
	t.Parallel()

	for name, in := range map[string]string{
		"empty":            "",
		"no separator":     strings.Repeat("a", 66),
		"empty prefix":     "_" + strings.Repeat("a", 61),
		"body too short":   testPrefix + "_" + strings.Repeat("a", 60),
		"body too long":    testPrefix + "_" + strings.Repeat("a", 62),
		"body not base62":  testPrefix + "_" + strings.Repeat("a", 60) + "-",
		"uppercase prefix": "AIOZ_" + strings.Repeat("a", 61),
		"prefix too long":  strings.Repeat("a", 13) + "_" + strings.Repeat("a", 61),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := secret.ParseKey(in)
			require.Error(t, err)
		})
	}
}

func TestNewKeyValidatesPrefix(t *testing.T) {
	t.Parallel()

	for name, prefix := range map[string]string{
		"empty":       "",
		"too long":    strings.Repeat("a", secret.KeyPrefixMaxLen+1),
		"uppercase":   "AIOZ",
		"underscore":  "ai_oz",
		"dash":        "ai-oz",
		"punctuation": "aioz!",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := secret.NewKey(prefix)
			require.Error(t, err)
		})
	}

	for _, prefix := range []string{"a", "aioz", "aioz2", "a1", strings.Repeat("a", secret.KeyPrefixMaxLen)} {
		_, err := secret.NewKey(prefix)
		require.NoError(t, err, "prefix %q", prefix)
	}
}

// KeyRef must marshal: it is the public half, and it is what a log line or a
// span carries.
func TestKeyRefMarshals(t *testing.T) {
	t.Parallel()

	key, err := secret.NewKey(testPrefix)
	require.NoError(t, err)

	ref, err := secret.ParseKey(key.Secret.Reveal())
	require.NoError(t, err)

	b, err := json.Marshal(ref)
	require.NoError(t, err)
	require.NotContains(t, string(b), key.Secret.Reveal())
	require.Contains(t, string(b), key.ID)
}

// A Key carries a Token, so marshaling the whole struct must fail rather than
// quietly ship the secret.
func TestKeyItselfRefusesToMarshal(t *testing.T) {
	t.Parallel()

	key, err := secret.NewKey(testPrefix)
	require.NoError(t, err)

	_, err = json.Marshal(key)
	require.Error(t, err)
}

// A hint lets a person tell their keys apart in a dashboard - the prefix and
// the last four characters, the way Stripe and OpenAI show keys - without the
// dashboard ever holding the key.
func TestKeyHintShowsThePrefixAndTheLastFour(t *testing.T) {
	t.Parallel()

	key, err := secret.NewKey(testPrefix)
	require.NoError(t, err)
	presented := key.Secret.Reveal()

	hint := key.Hint()
	require.Equal(t, testPrefix+"_..."+presented[len(presented)-secret.KeyHintLen:], hint)
	require.Len(t, hint, len(testPrefix)+len("_...")+secret.KeyHintLen)
	require.NotContains(t, hint, key.ID, "the hint shows the tail, not the lookup id")
}

// ParseKey returns the hint too, so a service can say which key a request
// used from the presented string alone.
func TestParseKeyReturnsTheHint(t *testing.T) {
	t.Parallel()

	key, err := secret.NewKey(testPrefix)
	require.NoError(t, err)

	ref, err := secret.ParseKey(key.Secret.Reveal())
	require.NoError(t, err)
	require.Equal(t, key.Hint(), ref.Hint)
}

// The hint is meant to be stored and shown, so unlike the Token it marshals.
func TestKeyRefWithHintMarshals(t *testing.T) {
	t.Parallel()

	key, err := secret.NewKey(testPrefix)
	require.NoError(t, err)
	ref, err := secret.ParseKey(key.Secret.Reveal())
	require.NoError(t, err)

	b, err := json.Marshal(ref)
	require.NoError(t, err)
	require.Contains(t, string(b), `"hint":"`+key.Hint()+`"`)
}
