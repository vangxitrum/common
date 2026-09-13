package secret

import (
	"strings"

	"gitlab.internal/tuan.quang.tran/aioz-common/random"
)

const (
	// CodeGroups is how many dash-separated groups a recovery code has.
	CodeGroups = 4

	// CodeGroupLen is how many characters are in each group.
	CodeGroupLen = 4

	// codeLen is the total number of characters in a code, dashes excluded.
	codeLen = CodeGroups * CodeGroupLen

	// crockfordAlphabet is Crockford base32: the digits plus the letters,
	// less I, L, O and U. I, L and O are excluded because they are misread as
	// 1, 1 and 0; U because it is how "FUCK" and friends appear by accident.
	crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
)

// NewCodes mints n single-use recovery codes: the [Token]s to show the operator
// once, and the [Hash]es to store.
//
// Crockford base32, not RFC 4648, and the difference is the reason this
// function exists. Both avoid emitting 0/O and 1/I: RFC 4648's output alphabet
// is A-Z2-7, so a minted code never contains a 0 or a 1. But ambiguity is a
// parsing property, not an alphabet property, and RFC 4648 provides no input
// folding at all. A person who reads O off a printed card and types 0 is
// rejected, with no way back, at the exact moment they have already lost their
// phone. Crockford's decode side folds O to 0, and I and L to 1, which is what
// actually helps. See [NormalizeCode].
//
// Each code is 16 characters, 80 bits, which is far past guessing for a value
// that is checked against one account and burned on use.
func NewCodes(n int) (codes []Token, hashes []Hash) {
	codes = make([]Token, 0, n)
	hashes = make([]Hash, 0, n)

	for range n {
		code := formatCode(randCrockford(codeLen))
		codes = append(codes, Token(code))
		hashes = append(hashes, HashOf(NormalizeCode(code)))
	}
	return codes, hashes
}

// NormalizeCode makes a typed code match a minted one.
//
// It uppercases, drops everything that is not a letter or a digit (so the
// dashes are optional, and so are the spaces someone types instead), and then
// folds the ambiguous characters: O to 0, and I and L to 1.
//
// Codes are hashed after normalizing, so a code is stored in exactly one form
// however it was typed.
func NormalizeCode(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))

	for _, r := range strings.ToUpper(s) {
		switch {
		case r == 'O':
			sb.WriteRune('0')
		case r == 'I' || r == 'L':
			sb.WriteRune('1')
		case (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z'):
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// VerifyCode normalizes a typed code and then verifies it, so a caller cannot
// forget the normalizing half.
func VerifyCode(typed string, stored Hash) bool {
	return Verify(NormalizeCode(typed), stored)
}

// randCrockford returns n random characters from the Crockford alphabet.
//
// It is aioz-common/random.String over the same alphabet. The alphabet has
// exactly 32 characters, which divides 256, so no byte is ever rejected.
func randCrockford(n int) string {
	return random.String(n, random.Crockford)
}

// formatCode groups a code for a human to read off a screen or a card.
func formatCode(s string) string {
	groups := make([]string, 0, CodeGroups)
	for i := 0; i < len(s); i += CodeGroupLen {
		groups = append(groups, s[i:min(i+CodeGroupLen, len(s))])
	}
	return strings.Join(groups, "-")
}
