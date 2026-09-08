package jwt

import (
	"context"

	gojwt "github.com/golang-jwt/jwt/v5"
)

// Ring verifies against several keys, choosing by the token's "kid" header.
//
// It exists so a key can be rotated without a flag day. Publish the new key to
// every verifier first, then start signing with it; tokens minted under the
// old key keep verifying until they expire, and only then is the old key
// removed. Without a ring, rotation means every already-issued token becomes
// invalid the moment the signer changes.
//
// A Ring is immutable once built, and safe for concurrent use.
type Ring struct {
	byKeyID map[string]*Verifier
}

// NewRing returns a Ring over verifiers, each of which must have been built
// with [ExpectKeyID].
//
// Requiring a kid on every member is what keeps the ring honest. A ring that
// falls back to "try them all" turns a rotation into a system that accepts a
// token signed by any key it has ever known, and hides the fact that the old
// key was never actually retired.
func NewRing(verifiers ...*Verifier) (*Ring, error) {
	if len(verifiers) == 0 {
		return nil, Error.Errorf("a ring needs at least one verifier")
	}

	ring := &Ring{byKeyID: make(map[string]*Verifier, len(verifiers))}
	for _, v := range verifiers {
		if v.keyID == "" {
			return nil, Error.Errorf(
				"every verifier in a ring needs a key id; build it with ExpectKeyID")
		}
		if _, dup := ring.byKeyID[v.keyID]; dup {
			return nil, Error.Errorf("duplicate key id %q in ring", v.keyID)
		}
		ring.byKeyID[v.keyID] = v
	}
	return ring, nil
}

// KeyIDs returns the key ids this Ring knows, for diagnostics.
func (r *Ring) KeyIDs() []string {
	ids := make([]string, 0, len(r.byKeyID))
	for id := range r.byKeyID {
		ids = append(ids, id)
	}
	return ids
}

// Verify routes a token to the verifier named by its "kid" header.
//
// The header is read WITHOUT verifying anything, because the key needed to
// verify is what it selects. That is safe only because a kid selects among
// keys we already trust: an unknown kid is rejected, and the chosen verifier
// then checks the signature and the pinned algorithm as usual. A kid is a
// routing hint, never a grant.
func (r *Ring) Verify(ctx context.Context, token string) (Claims, error) {
	parsed, _, err := gojwt.NewParser().ParseUnverified(token, gojwt.MapClaims{})
	if err != nil {
		return Claims{}, Error.Wrap(err)
	}

	keyID, _ := parsed.Header["kid"].(string)
	if keyID == "" {
		return Claims{}, Error.Errorf("token carries no key id")
	}

	verifier, ok := r.byKeyID[keyID]
	if !ok {
		return Claims{}, Error.Errorf("unknown key id %q", keyID)
	}
	return verifier.Verify(ctx, token)
}
