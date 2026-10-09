package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CacheType is how a response may be stored once it is sent.
type CacheType string

// The four policies a GET endpoint declares. Each one is also the value of
// x-cache-policy.type in the endpoint's OpenAPI operation.
const (
	// Public may be stored by browsers and shared caches for MaxAge. A MaxAge
	// of zero stores it but revalidates on every use.
	Public CacheType = "public"
	// Private may be stored by the caller's own browser only, for MaxAge.
	// The response depends on who is asking.
	Private CacheType = "private"
	// Revalidate may be stored by the caller's browser but is checked with the
	// server on every use: a 304 when its ETag still matches. For data that
	// changes at any moment and is worth not resending.
	Revalidate CacheType = "revalidate"
	// NoStore is never stored. Tokens, personal data, money. It is what a
	// route without a policy gets, and what every error gets.
	NoStore CacheType = "no-store"
)

// CachePolicy is one endpoint's caching decision.
type CachePolicy struct {
	Type   CacheType
	MaxAge time.Duration
	// StaleWhileRevalidate lets a Public response be served stale for this
	// long while a fresh one is fetched in the background.
	StaleWhileRevalidate time.Duration
}

// Validate reports a policy that cannot mean what it says.
func (p CachePolicy) Validate() error {
	switch p.Type {
	case Public, Private:
	case Revalidate, NoStore:
		if p.MaxAge != 0 || p.StaleWhileRevalidate != 0 {
			return Error.Errorf(
				"cache policy %s takes no max-age or stale-while-revalidate",
				p.Type,
			)
		}
	default:
		return Error.Errorf(
			"cache policy %q: want public, private, revalidate or no-store",
			p.Type,
		)
	}
	if p.MaxAge < 0 || p.StaleWhileRevalidate < 0 {
		return Error.Errorf("cache policy durations must not be negative")
	}
	if p.Type != Public && p.StaleWhileRevalidate != 0 {
		return Error.Errorf(
			"stale-while-revalidate is only for a public policy",
		)
	}
	return nil
}

// CacheControl is the Cache-Control value the policy sends.
func (p CachePolicy) CacheControl() string {
	switch p.Type {
	case Public:
		if p.MaxAge == 0 {
			return "public, no-cache"
		}
		v := "public, max-age=" + seconds(p.MaxAge)
		if p.StaleWhileRevalidate > 0 {
			v += ", stale-while-revalidate=" + seconds(p.StaleWhileRevalidate)
		}
		return v
	case Private:
		return "private, max-age=" + seconds(p.MaxAge)
	case Revalidate:
		return "private, no-cache"
	default:
		return "no-store"
	}
}

// Apply sets the policy's headers on h, replacing whatever was there - in
// particular httpsec's default no-store. A response that depends on the caller
// varies by Authorization, so no cache ever answers one user with another's.
func (p CachePolicy) Apply(h http.Header) {
	h.Set("Cache-Control", p.CacheControl())
	if p.Type == Private || p.Type == Revalidate {
		h.Add("Vary", "Authorization")
	}
}

// Wrap is the net/http middleware for one route's policy.
func (p CachePolicy) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.Apply(w.Header())
		next.ServeHTTP(w, r)
	})
}

func seconds(
	d time.Duration,
) string {
	return strconv.FormatInt(int64(d/time.Second), 10)
}

// ETag makes a strong entity tag from a version: a row's updated_at in
// nanoseconds, a revision counter, a content hash. Build it from what the
// resource IS, not from the response body: a body hash changes with field
// order and compression and costs a full render to compute.
//
// A version with characters an entity tag cannot hold is hashed.
func ETag(version string) string {
	if !etagSafe(version) {
		sum := sha256.Sum256([]byte(version))
		version = hex.EncodeToString(sum[:16])
	}
	return `"` + version + `"`
}

// WeakETag is ETag for a representation that is equivalent but not
// byte-identical across versions, such as one rendered with a timestamp.
func WeakETag(version string) string { return "W/" + ETag(version) }

func etagSafe(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x21 || c == '"' || c > 0x7e {
			return false
		}
	}
	return true
}

// NotModified reports whether the client already holds etag, by the weak
// comparison RFC 9110 section 13.1.2 prescribes for If-None-Match on GET and
// HEAD. Any other method is never answered with a 304.
func NotModified(r *http.Request, etag string) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	header := r.Header.Get("If-None-Match")
	if header == "" || etag == "" {
		return false
	}
	if strings.TrimSpace(header) == "*" {
		return true
	}
	want := strings.TrimPrefix(etag, "W/")
	for _, candidate := range strings.Split(header, ",") {
		if strings.TrimPrefix(strings.TrimSpace(candidate), "W/") == want {
			return true
		}
	}
	return false
}

// CheckETag sets the ETag header and, when the client already holds it,
// answers 304 Not Modified and returns true: the handler then returns without
// rendering a body. Call it after loading the version and before the
// expensive part.
//
//	if httpapi.CheckETag(w, r, httpapi.ETag(video.Version)) {
//	    return
//	}
func CheckETag(w http.ResponseWriter, r *http.Request, etag string) bool {
	w.Header().Set("ETag", etag)
	if !NotModified(r, etag) {
		return false
	}
	w.WriteHeader(http.StatusNotModified)
	return true
}
