package secret

import (
	"crypto/rand"
	"hash/crc32"
	"strings"
)

// base62Alphabet is digits, then upper, then lower.
//
// It deliberately contains no "_", so the single underscore in a key is an
// unambiguous separator, and no "-", "+", "/" or "=", so a whole key is one
// word: it double-click-selects, survives a URL, a header, a shell argument
// and a YAML scalar unquoted, and does not get broken across a line.
const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// base62Max is the largest byte value that can be reduced mod 62 without bias:
// 4*62 = 248, so bytes 248..255 are discarded rather than folded.
const base62Max = 248

// randBase62 returns n cryptographically random base62 characters.
//
// It samples by rejection rather than by reducing a big.Int. Reduction loses
// leading zeros and yields a variable-width result, and the naive b%62 over
// the whole byte range makes the first eight characters of the alphabet
// slightly more likely, which is a real bias in a secret's first character.
//
// Unlike [Bytes] this has no minimum: it is a building block, and its callers
// (which do have minimums) are in this package.
func randBase62(n int) string {
	var sb strings.Builder
	sb.Grow(n)

	// Draw in blocks, with headroom for the ~3% of bytes that get rejected.
	buf := make([]byte, n+n/8+8)
	for sb.Len() < n {
		// crypto/rand.Read terminates the process rather than returning an
		// error, so the only honest thing to do with its result is ignore it.
		_, _ = rand.Read(buf)
		for _, b := range buf {
			if b >= base62Max {
				continue
			}
			sb.WriteByte(base62Alphabet[b%62])
			if sb.Len() == n {
				break
			}
		}
	}
	return sb.String()
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
