package secret

import "strings"

const (
	// KeyIDLen is the length of a key's public id, in base62 characters.
	//
	// About 71 bits. The id is public, so what it needs is collision
	// resistance, not unguessability: at ten million keys the birthday
	// probability is around 1e-7, and the column carries a unique constraint
	// anyway, so a collision is a retryable insert rather than a breach.
	KeyIDLen = 12

	// KeySecretLen is the length of a key's secret half, in base62
	// characters. 43 characters is 256.03 bits, matching [TokenBytes].
	KeySecretLen = 43

	// KeyChecksumLen is the length of the trailing checksum, in base62
	// characters. Six holds a whole CRC32.
	KeyChecksumLen = 6

	// KeyPrefixMaxLen is the longest accepted prefix.
	KeyPrefixMaxLen = 12

	keySeparator = "_"
)

// Key is an API key: a public id you can index and log, and a secret shown
// once.
type Key struct {
	// Prefix names the scheme, and is part of the credential's identity.
	Prefix string

	// ID is public and indexable. It is the lookup column.
	ID string

	// Secret is the whole presentable string, "<prefix>_<id><secret><crc>".
	// Show it to its owner once; store the Hash.
	Secret Token

	// Hash is what you store.
	Hash Hash
}

// KeyRef is everything [ParseKey] can learn from a presented key without a
// database. It holds no secret, so it is safe to log and to put in a span.
type KeyRef struct {
	Prefix string `json:"prefix"`
	ID     string `json:"id"`
}

// NewKey mints a key under prefix.
//
// The presented string embeds the id, and that is the point. Verification is a
// parse, one indexed row by id, and one constant-time compare:
//
//	ref, err := secret.ParseKey(presented)
//	stored := db.HashByKeyID(ctx, ref.ID)
//	ok := secret.Verify(presented, stored)
//
// Without an embedded id the only possible implementation is a scan over every
// stored hash, which is how depin's certificate/authorization ended up doing a
// linear bytes.Equal over a user's whole set.
//
// The prefix is also the versioning scheme. The whole presented string is
// hashed, prefix included, so a key minted later under "aioz2" can never
// verify against an "aioz" hash, and moving to a new scheme needs no flag day.
//
// It returns an error only for an invalid prefix: 1 to [KeyPrefixMaxLen]
// characters, lowercase letters and digits only.
func NewKey(prefix string) (Key, error) {
	if err := validatePrefix(prefix); err != nil {
		return Key{}, err
	}

	id := randBase62(KeyIDLen)
	body := prefix + keySeparator + id + randBase62(KeySecretLen)
	presented := body + checksum(body)

	return Key{
		Prefix: prefix,
		ID:     id,
		Secret: Token(presented),
		Hash:   HashOf(presented),
	}, nil
}

// ParseKey validates a presented key's shape and checksum offline and returns
// its public half.
//
// The checksum is a CRC32 and is emphatically not a MAC. It proves nothing
// about authenticity: anyone can compute it. It buys two things. A secret
// scanner can tell a real key from a random word with no call to us, so a key
// leaked into a public repository is reported without us operating a
// validation endpoint. And a mistyped or truncated key is rejected before it
// costs a database round trip.
//
// A key that parses is still unauthenticated. Only [Verify] against the stored
// [Hash] settles that.
func ParseKey(s string) (KeyRef, error) {
	prefix, rest, found := strings.Cut(s, keySeparator)
	if !found {
		return KeyRef{}, Error.Errorf("key has no %q separator", keySeparator)
	}
	if err := validatePrefix(prefix); err != nil {
		return KeyRef{}, err
	}
	if want := KeyIDLen + KeySecretLen + KeyChecksumLen; len(rest) != want {
		return KeyRef{}, Error.Errorf("key body is %d characters, want %d", len(rest), want)
	}
	if !isBase62(rest) {
		return KeyRef{}, Error.Errorf("key body is not base62")
	}

	body, sum := s[:len(s)-KeyChecksumLen], s[len(s)-KeyChecksumLen:]
	if checksum(body) != sum {
		return KeyRef{}, Error.Errorf("key checksum does not match")
	}

	return KeyRef{Prefix: prefix, ID: rest[:KeyIDLen]}, nil
}

// validatePrefix enforces lowercase alphanumerics, so the prefix is one word,
// case-stable through infrastructure that lowercases, and contains no
// separator of its own.
func validatePrefix(prefix string) error {
	if prefix == "" {
		return Error.Errorf("key prefix is empty")
	}
	if len(prefix) > KeyPrefixMaxLen {
		return Error.Errorf("key prefix %q is longer than %d characters", prefix, KeyPrefixMaxLen)
	}
	for i := 0; i < len(prefix); i++ {
		c := prefix[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return Error.Errorf("key prefix %q must be lowercase letters and digits only", prefix)
		}
	}
	return nil
}
