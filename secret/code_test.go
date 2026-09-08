package secret_test

import (
	"regexp"
	"strings"
	"testing"

	"gitlab.internal/tuan.quang.tran/aioz-common/secret"

	"github.com/stretchr/testify/require"
)

func TestNewCodesShape(t *testing.T) {
	t.Parallel()

	codes, hashes := secret.NewCodes(10)
	require.Len(t, codes, 10)
	require.Len(t, hashes, 10)

	shape := regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{4}(-[0-9A-HJKMNP-TV-Z]{4}){3}$`)

	seen := map[string]bool{}
	for i, code := range codes {
		plain := code.Reveal()
		require.Regexp(t, shape, plain)
		require.NotContains(t, plain, "I")
		require.NotContains(t, plain, "L")
		require.NotContains(t, plain, "O")
		require.NotContains(t, plain, "U")
		require.False(t, seen[plain], "duplicate code %q", plain)
		seen[plain] = true

		require.True(t, secret.VerifyCode(plain, hashes[i]))
	}
}

func TestNewCodesZero(t *testing.T) {
	t.Parallel()

	codes, hashes := secret.NewCodes(0)
	require.Empty(t, codes)
	require.Empty(t, hashes)
}

// TestVerifyCodeFoldsWhatAPersonMistypes is the centerpiece. Every one of
// these is a real way somebody reads a code off a card and types it, and every
// one of them is rejected by depin's current RFC 4648 implementation.
func TestVerifyCodeFoldsWhatAPersonMistypes(t *testing.T) {
	t.Parallel()

	codes, hashes := secret.NewCodes(1)
	code, hash := codes[0].Reveal(), hashes[0]

	typings := map[string]string{
		"as minted":          code,
		"lowercase":          strings.ToLower(code),
		"no dashes":          strings.ReplaceAll(code, "-", ""),
		"spaces for dashes":  strings.ReplaceAll(code, "-", " "),
		"surrounding space":  "  " + code + "\n",
		"O typed for zero":   strings.ReplaceAll(code, "0", "O"),
		"o typed for zero":   strings.ReplaceAll(strings.ToLower(code), "0", "o"),
		"I typed for one":    strings.ReplaceAll(code, "1", "I"),
		"l typed for one":    strings.ReplaceAll(strings.ToLower(code), "1", "l"),
		"L typed for one":    strings.ReplaceAll(code, "1", "L"),
		"everything at once": strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(code, "-", ""), "0", "O"), "1", "l")),
	}

	for name, typed := range typings {
		t.Run(name, func(t *testing.T) {
			require.True(t, secret.VerifyCode(typed, hash), "typed %q", typed)
		})
	}
}

func TestVerifyCodeRejectsAWrongCode(t *testing.T) {
	t.Parallel()

	codes, hashes := secret.NewCodes(2)

	require.False(t, secret.VerifyCode(codes[1].Reveal(), hashes[0]))
	require.False(t, secret.VerifyCode("", hashes[0]))
	require.False(t, secret.VerifyCode("ZZZZ-ZZZZ-ZZZZ-ZZZZ", hashes[0]))

	// One character off, which the folding must not paper over.
	code := codes[0].Reveal()
	swapped := strings.Replace(code, string(code[0]), "9", 1)
	if swapped != code {
		require.False(t, secret.VerifyCode(swapped, hashes[0]))
	}
}

func TestNormalizeCode(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ in, want string }{
		"folds O":            {"O0O", "000"},
		"folds I and L":      {"I1L", "111"},
		"uppercases":         {"abc", "ABC"},
		"strips separators":  {"AB-CD EF", "ABCDEF"},
		"strips punctuation": {"A.B,C!", "ABC"},
		"empty":              {"", ""},
		"only separators":    {"---", ""},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, secret.NormalizeCode(tc.in))
		})
	}
}

// The stored hash is of the normalized form, so a code hashed straight from
// its printed form must not verify by accident through a different path.
func TestCodesAreStoredNormalized(t *testing.T) {
	t.Parallel()

	codes, hashes := secret.NewCodes(1)
	code := codes[0].Reveal()

	require.Equal(t, secret.HashOf(secret.NormalizeCode(code)), hashes[0])
	require.NotEqual(t, secret.HashOf(code), hashes[0], "the dashes must not be part of the hashed form")
}
