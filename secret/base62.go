package secret

import (
	"hash/crc32"
	"strings"

	"gitlab.internal/tuan.quang.tran/aioz-common/random"
)

// base62Alphabet is digits, then upper, then lower.
//
// It deliberately contains no "_", so the single underscore in a key is an
// unambiguous separator, and no "-", "+", "/" or "=", so a whole key is one
// word: it double-click-selects, survives a URL, a header, a shell argument
// and a YAML scalar unquoted, and does not get broken across a line.
const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// randBase62 returns n cryptographically random base62 characters.
//
// It is aioz-common/random.String over the same alphabet, which samples by
// rejection: the naive b%62 over the whole byte range makes the first eight
// characters of the alphabet slightly more likely, a real bias in a secret's
// first character.
//
// Unlike [Bytes] this has no minimum: it is a building block, and its callers
// (which do have minimums) are in this package.
func randBase62(n int) string {
	return random.String(n, random.Base62)
}

// isBase62 reports whether s is non-empty and made only of alphabet characters.
func isBase62(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(base62Alphabet, s[i]) < 0 {
			return false
		}
	}
	return true
}

// checksum returns the CRC32 of body as exactly [KeyChecksumLen] base62
// characters, most significant first.
//
// 62^6 is 5.68e10, comfortably more than 2^32, so six characters hold the
// whole value with room to spare and the encoding is fixed width.
func checksum(body string) string {
	v := crc32.Checksum([]byte(body), crc32.MakeTable(crc32.Castagnoli))

	out := make([]byte, KeyChecksumLen)
	for i := KeyChecksumLen - 1; i >= 0; i-- {
		out[i] = base62Alphabet[v%62]
		v /= 62
	}
	return string(out)
}
