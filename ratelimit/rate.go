package ratelimit

import (
	"crypto/sha256"
	"encoding/hex"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Rate is Count requests per Period: a burst of Count, then one every
// Period/Count as the window refills.
type Rate struct {
	Count  int
	Period time.Duration
}

// String formats the rate as Rates parses it: 5/1m0s.
func (r Rate) String() string {
	return strconv.Itoa(r.Count) + "/" + r.Period.String()
}

// interval is the time between requests at the sustained rate, in
// microseconds, the unit both stores keep time in.
func (r Rate) interval() int64 {
	return r.Period.Microseconds() / int64(r.Count)
}

func (r Rate) validate() error {
	switch {
	case r.Count <= 0:
		return Error.Errorf("rate %s: count must be positive", r)
	case r.Period <= 0:
		return Error.Errorf("rate %s: period must be positive", r)
	case r.interval() < 1:
		return Error.Errorf("rate %s: more than one request per microsecond", r)
	}
	return nil
}

// Rates is a set of windows checked together, such as a short one against
// bursts and a long one against a slow, steady attack. It is a pflag.Value, so
// a service's config can carry one: `default:"5/1m,20/1h"`.
type Rates []Rate

// String formats the rates as Set parses them.
func (r Rates) String() string {
	parts := make([]string, len(r))
	for i, rate := range r {
		parts[i] = rate.String()
	}
	return strings.Join(parts, ",")
}

// Set parses a comma-separated list of count/period pairs, such as
// "5/1m,20/1h". An empty string is no rates.
func (r *Rates) Set(value string) error {
	var rates Rates
	for part := range strings.SplitSeq(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		countText, periodText, ok := strings.Cut(part, "/")
		if !ok {
			return Error.Errorf("rate %q: want count/period, like 5/1m", part)
		}
		count, err := strconv.Atoi(strings.TrimSpace(countText))
		if err != nil {
			return Error.Errorf("rate %q: count: %w", part, err)
		}
		period, err := time.ParseDuration(strings.TrimSpace(periodText))
		if err != nil {
			return Error.Errorf("rate %q: period: %w", part, err)
		}
		rate := Rate{Count: count, Period: period}
		if err := rate.validate(); err != nil {
			return err
		}
		rates = append(rates, rate)
	}
	*r = rates
	return nil
}

// Type names the flag's value type in help output.
func (*Rates) Type() string { return "rates" }

// Rule is a named set of rates: "login-ip", "otp-email". The name is part of
// every key and every metric, so it must be short, lowercase and fixed in
// code - never derived from a request.
type Rule struct {
	Name  string
	Rates Rates
}

func (r Rule) validate() error {
	if len(r.Name) == 0 || len(r.Name) > 32 {
		return Error.Errorf("rule name %q: want 1 to 32 characters", r.Name)
	}
	for _, c := range []byte(r.Name) {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return Error.Errorf(
				"rule name %q: want only a-z, 0-9 and -",
				r.Name,
			)
		}
	}
	if len(r.Rates) == 0 {
		return Error.Errorf("rule %s: no rates", r.Name)
	}
	for _, rate := range r.Rates {
		if err := rate.validate(); err != nil {
			return Error.Errorf("rule %s: %w", r.Name, err)
		}
	}
	return nil
}

// Key is one identity limited under one rule: this email at login, this
// address at signup.
//
// The value is hashed at once and never stored or logged, so an email or an
// address does not sit in Redis, and an attacker-sized value cannot make an
// attacker-sized key. An invalid key is carried to Allow, which returns the
// error: the caller has one place to check.
type Key struct {
	rule   string
	rates  Rates
	digest string
	err    error
}

// Key limits value under the rule. An empty value is an error rather than a
// key: every request without one would share a bucket, and one attacker could
// then deny them all.
func (r Rule) Key(value string) Key {
	if err := r.validate(); err != nil {
		return Key{err: err}
	}
	if value == "" {
		return Key{err: Error.Errorf("rule %s: empty key value", r.Name)}
	}
	sum := sha256.Sum256([]byte(value))
	return Key{
		rule:   r.Name,
		rates:  slices.Clone(r.Rates),
		digest: hex.EncodeToString(sum[:16]),
	}
}

// IPKey limits a client address under the rule. An IPv4-mapped address is its
// IPv4 address, and an IPv6 address is keyed by its /64: one subscriber is
// usually handed a whole /64, so keying single addresses would give an
// attacker 2^64 fresh buckets.
func (r Rule) IPKey(addr netip.Addr) Key {
	if !addr.IsValid() {
		return Key{err: Error.Errorf("rule %s: invalid address", r.Name)}
	}
	addr = addr.Unmap().WithZone("")
	if addr.Is6() {
		prefix, err := addr.Prefix(64)
		if err != nil {
			return Key{err: Error.Wrap(err)}
		}
		return r.Key(prefix.String())
	}
	return r.Key(addr.String())
}

func (k Key) validate() error {
	if k.err != nil {
		return k.err
	}
	if k.digest == "" {
		return Error.Errorf("zero key: build keys with Rule.Key or Rule.IPKey")
	}
	return nil
}

// Result is the answer for one Allow.
type Result struct {
	Allowed bool
	// RetryAfter is how long until the same request would be allowed; zero
	// when it is.
	RetryAfter time.Duration
}

// RetryAfterSeconds is RetryAfter for an HTTP Retry-After header: rounded up,
// because "retry in 0 seconds" means now, and at least 1 when denied.
func (r Result) RetryAfterSeconds() int64 {
	if r.Allowed {
		return 0
	}
	seconds := int64((r.RetryAfter + time.Second - 1) / time.Second)
	return max(seconds, 1)
}
