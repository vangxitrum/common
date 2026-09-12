package mail

import (
	"fmt"
	"strings"
)

const (
	maxLocalLen   = 64  // RFC 5321 section 4.5.3.1.1
	maxAddressLen = 254 // RFC 5321's path limit, less the angle brackets
	maxLabelLen   = 63  // RFC 1035 section 2.3.4
)

// Address is an email address a person typed, checked by [ParseAddress].
//
// It has two forms, and the difference matters. [Address.String] is where mail
// is delivered: what the person typed, with only the domain lowercased.
// [Address.Canonical] is the uniqueness key - the value to put a unique index
// on - so that one person cannot hold two accounts through a capital letter or
// a Gmail dot. Deliver to the first, deduplicate on the second.
//
// The zero Address is not a valid address.
type Address struct {
	local  string // as typed
	domain string // lowercased
}

// ParseAddress checks an address a person typed into a form.
//
// It is deliberately stricter than net/mail.ParseAddress, which parses mail
// headers and so accepts a display name ("Bob <bob@example.com>"), comments,
// quoted local parts and a domain with no dot. The rules are the HTML
// standard's definition of a valid email address - itself "a willful violation
// of RFC 5322", whose full syntax is too lax for a form field - tightened where
// OWASP's input validation cheat sheet says to:
//
//   - one @, and at most 254 characters in all, 64 of them before the @;
//   - before the @: letters, digits, dots and !#$%&*+/=?^_{|}~- - no quotes or
//     backticks, and no dot at either end or two in a row;
//   - after the @: at least two labels of letters, digits and hyphens, each at
//     most 63 characters and not starting or ending with a hyphen, the last
//     not all digits (so not an IP address);
//   - ASCII only: an internationalized domain must be given in its xn-- form.
//
// Surrounding whitespace is trimmed. A failure is an [*Error] with Code
// [CodeInvalid] that names the rule, never the input: an address is personal
// data.
//
// Passing says the address is well formed, not that it exists or that the
// person owns it. Only a message they answer proves that: send a code from
// [gitlab.internal/tuan.quang.tran/aioz-common/random.Digits] or a link token
// of at least 32 random characters, single use and short lived. Do not try to
// block disposable domains (new ones appear daily) or strip +tags (people use
// them to trace leaks, and removing them is trivially worked around).
func ParseAddress(input string) (Address, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return Address{}, invalid("the address is empty")
	}
	if len(s) > maxAddressLen {
		return Address{}, invalid(
			fmt.Sprintf(
				"the address is longer than %d characters",
				maxAddressLen,
			),
		)
	}

	at := strings.IndexByte(s, '@')
	if at < 0 || at != strings.LastIndexByte(s, '@') {
		return Address{}, invalid("the address needs exactly one @")
	}
	local, domain := s[:at], strings.ToLower(s[at+1:])

	if err := checkLocal(local); err != nil {
		return Address{}, err
	}
	if err := checkDomain(domain); err != nil {
		return Address{}, err
	}
	return Address{local: local, domain: domain}, nil
}

func checkLocal(local string) error {
	switch {
	case local == "":
		return invalid("the address has nothing before the @")
	case len(local) > maxLocalLen:
		return invalid(
			fmt.Sprintf(
				"the part before the @ is longer than %d characters",
				maxLocalLen,
			),
		)
	case local[0] == '.' || local[len(local)-1] == '.':
		return invalid("the part before the @ starts or ends with a dot")
	case strings.Contains(local, ".."):
		return invalid("the part before the @ has two dots in a row")
	}
	for i := range len(local) {
		if !isLocalByte(local[i]) {
			return invalid(
				"the part before the @ has a character that is not allowed",
			)
		}
	}
	return nil
}

// isLocalByte reports whether c may appear before the @: the HTML standard's
// atext characters and the dot, less the apostrophe and the backtick, which
// OWASP recommends refusing.
func isLocalByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte(".!#$%&*+/=?^_{|}~-", c) >= 0
}

func checkDomain(domain string) error {
	if domain == "" {
		return invalid("the address has nothing after the @")
	}

	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return invalid("the domain needs a dot")
	}
	for _, label := range labels {
		switch {
		case label == "":
			return invalid("the domain has an empty label")
		case len(label) > maxLabelLen:
			return invalid(
				fmt.Sprintf(
					"a domain label is longer than %d characters",
					maxLabelLen,
				),
			)
		case label[0] == '-' || label[len(label)-1] == '-':
			return invalid("a domain label starts or ends with a hyphen")
		}
		for i := range len(label) {
			c := label[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return invalid("the domain has a character that is not allowed")
			}
		}
	}

	if strings.Trim(labels[len(labels)-1], "0123456789") == "" {
		return invalid("the domain ends in a number, like an IP address")
	}
	return nil
}

// String is the address to deliver to: as typed, with the domain lowercased.
func (a Address) String() string {
	return a.local + "@" + a.domain
}

// Domain is the lowercased domain, for logs and metrics that must not carry
// the whole address.
func (a Address) Domain() string {
	return a.domain
}

// Canonical is the uniqueness key: the whole address lowercased, plus the rules
// Google documents for gmail.com - dots before the @ are ignored, and
// googlemail.com is the same mailbox.
//
// It applies them to gmail.com and googlemail.com only. On a Google Workspace
// domain "dots do change your address", and elsewhere a dot is part of the
// name, so john.smith@acme.com and johnsmith@acme.com stay two people. It keeps
// +tags: stripping them merges addresses at providers that treat + literally,
// and OWASP advises against it.
func (a Address) Canonical() string {
	local, domain := strings.ToLower(a.local), a.domain
	if domain == "gmail.com" || domain == "googlemail.com" {
		local = strings.ReplaceAll(local, ".", "")
		domain = "gmail.com"
	}
	return local + "@" + domain
}
