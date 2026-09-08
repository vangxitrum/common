package jwt

import (
	"encoding/json"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
)

// Claims is the payload of a token: the registered claims RFC 7519 defines,
// plus whatever else the issuer put in.
//
// The registered claims are named fields rather than map entries because they
// have meanings the verifier acts on, and a typo in a map key is silent.
type Claims struct {
	// Issuer is who minted the token ("iss").
	Issuer string

	// Subject is who the token is about ("sub"), usually a user or node id.
	Subject string

	// Audience is who the token is for ("aud"). A verifier that requires an
	// audience refuses a token minted for a different service, which is what
	// stops a token from one of your APIs being replayed against another.
	Audience []string

	// ID is a unique identifier for this token ("jti"), for a replay cache.
	ID string

	// IssuedAt, NotBefore and ExpiresAt are "iat", "nbf" and "exp".
	IssuedAt  time.Time
	NotBefore time.Time
	ExpiresAt time.Time

	// Extra carries any non-registered claims. Keys that collide with a
	// registered claim are refused at signing time rather than silently
	// overwriting a field the verifier trusts.
	Extra map[string]any
}

// registered lists the claim names Extra may not use.
var registered = map[string]bool{
	"iss": true, "sub": true, "aud": true,
	"jti": true, "iat": true, "nbf": true, "exp": true,
}

// Get returns a non-registered claim.
func (c Claims) Get(name string) (any, bool) {
	v, ok := c.Extra[name]
	return v, ok
}

// GetString returns a non-registered claim as a string.
func (c Claims) GetString(name string) (string, bool) {
	v, ok := c.Extra[name]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// toMap renders the claims as the map the underlying library signs.
func (c Claims) toMap() (gojwt.MapClaims, error) {
	m := gojwt.MapClaims{}

	for k, v := range c.Extra {
		if registered[k] {
			return nil, Error.Errorf(
				"extra claim %q collides with a registered claim; set the field instead", k)
		}
		m[k] = v
	}

	if c.Issuer != "" {
		m["iss"] = c.Issuer
	}
	if c.Subject != "" {
		m["sub"] = c.Subject
	}
	if c.ID != "" {
		m["jti"] = c.ID
	}
	switch len(c.Audience) {
	case 0:
	case 1:
		// A single audience is written as a bare string. RFC 7519 allows both,
		// and some verifiers only accept the string form.
		m["aud"] = c.Audience[0]
	default:
		m["aud"] = c.Audience
	}
	if !c.IssuedAt.IsZero() {
		m["iat"] = c.IssuedAt.Unix()
	}
	if !c.NotBefore.IsZero() {
		m["nbf"] = c.NotBefore.Unix()
	}
	if !c.ExpiresAt.IsZero() {
		m["exp"] = c.ExpiresAt.Unix()
	}

	return m, nil
}

// claimsFromMap reads the claims back out of a verified token.
func claimsFromMap(m gojwt.MapClaims) Claims {
	c := Claims{Extra: map[string]any{}}

	for k, v := range m {
		if !registered[k] {
			c.Extra[k] = v
		}
	}

	c.Issuer, _ = m["iss"].(string)
	c.Subject, _ = m["sub"].(string)
	c.ID, _ = m["jti"].(string)

	switch aud := m["aud"].(type) {
	case string:
		c.Audience = []string{aud}
	case []any:
		for _, a := range aud {
			if s, ok := a.(string); ok {
				c.Audience = append(c.Audience, s)
			}
		}
	case []string:
		c.Audience = aud
	}

	c.IssuedAt = numericDate(m["iat"])
	c.NotBefore = numericDate(m["nbf"])
	c.ExpiresAt = numericDate(m["exp"])

	return c
}

// numericDate converts a JSON numeric date to a time.
//
// JSON has one number type, so a claim that was written as an int64 comes back
// as a float64 after a round trip. Both forms are accepted.
func numericDate(v any) time.Time {
	switch n := v.(type) {
	case float64:
		return time.Unix(int64(n), 0).UTC()
	case int64:
		return time.Unix(n, 0).UTC()
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return time.Unix(i, 0).UTC()
		}
	}
	return time.Time{}
}
