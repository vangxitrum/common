package mail_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/mail"
)

func TestParseAddressAcceptsWhatPeopleType(t *testing.T) {
	t.Parallel()

	for input, want := range map[string]string{
		"user@example.com":                       "user@example.com",
		"  user@example.com\n":                   "user@example.com",
		"User.Name+tag@Example.COM":              "User.Name+tag@example.com",
		"o_k-1@sub.example.co.uk":                "o_k-1@sub.example.co.uk",
		"a@xn--bcher-kva.example":                "a@xn--bcher-kva.example",
		"!#$%&*+/=?^_{|}~-@ex.am.pl":             "!#$%&*+/=?^_{|}~-@ex.am.pl",
		"x@a-b.example":                          "x@a-b.example",
		strings.Repeat("l", 64) + "@example.com": strings.Repeat("l", 64) + "@example.com",
	} {
		addr, err := mail.ParseAddress(input)
		require.NoError(t, err, "%q should parse", input)
		require.Equal(t, want, addr.String(), "%q", input)
	}
}

// The HTML standard's pattern, tightened where OWASP says to: no quotes or
// backticks, a dotted public domain, and the RFC 5321 length limits.
func TestParseAddressRefuses(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("a", 63)
	for name, input := range map[string]string{
		"empty":                 "",
		"no at":                 "user",
		"no local part":         "@example.com",
		"no domain":             "user@",
		"two ats":               "a@b@example.com",
		"a display name":        "Bob <bob@example.com>",
		"a quoted local part":   `"bob"@example.com`,
		"an apostrophe":         "o'neil@example.com",
		"a backtick":            "back`tick@example.com",
		"a space":               "user name@example.com",
		"a NUL byte":            "user\x00@example.com",
		"non-ASCII":             "üser@example.com",
		"a leading dot":         ".user@example.com",
		"a trailing dot":        "user.@example.com",
		"two dots in a row":     "us..er@example.com",
		"a single-label domain": "user@localhost",
		"an empty label":        "user@example..com",
		"a label starting -":    "user@-example.com",
		"a label ending -":      "user@example-.com",
		"a domain trailing dot": "user@example.com.",
		"an IP address":         "user@1.2.3.4",
		"an IP literal":         "user@[127.0.0.1]",
		"an underscore domain":  "user@ex_ample.com",
		"a 65-character local":  strings.Repeat("l", 65) + "@example.com",
		"a 64-character label":  "user@" + strings.Repeat("d", 64) + ".com",
		"over 254 characters":   "user@" + long + "." + long + "." + long + "." + long + ".com",
		"whitespace inside":     "user@exa mple.com",
	} {
		_, err := mail.ParseAddress(input)
		mailErr := requireMailError(t, err)
		require.Equal(t, mail.CodeInvalid, mailErr.Code, name)
	}
}

// An address is personal data: the error says which rule failed, never what
// was typed.
func TestParseAddressErrorsDoNotEchoTheInput(t *testing.T) {
	t.Parallel()

	_, err := mail.ParseAddress("secret-person@localhost")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "secret-person")
}

func TestAddressDomain(t *testing.T) {
	t.Parallel()

	addr, err := mail.ParseAddress("User@Mail.Example.COM")
	require.NoError(t, err)
	require.Equal(t, "mail.example.com", addr.Domain())
}

// Canonical is the uniqueness key: lowercase, plus the rules Google documents
// for gmail.com and nothing else. Delivery always uses String.
func TestCanonical(t *testing.T) {
	t.Parallel()

	for input, want := range map[string]string{
		"User@Example.com":               "user@example.com",
		"John.Smith@Gmail.com":           "johnsmith@gmail.com",
		"j.o.h.n.smith@gmail.com":        "johnsmith@gmail.com",
		"john.smith@googlemail.com":      "johnsmith@gmail.com",
		"john.smith+news@gmail.com":      "johnsmith+news@gmail.com",
		"John.Smith@acme.com":            "john.smith@acme.com",
		"j.smith+tag@outlook.com":        "j.smith+tag@outlook.com",
		"first.last@mail.gmail.com.test": "first.last@mail.gmail.com.test",
	} {
		addr, err := mail.ParseAddress(input)
		require.NoError(t, err, "%q", input)
		require.Equal(t, want, addr.Canonical(), "%q", input)
	}
}

// aioz-map's FormatEmail dropped dots for every .com and tags everywhere,
// which merges different people. Only gmail.com ignores dots - on a Google
// Workspace domain "dots do change your address" - and OWASP advises against
// stripping tags.
func TestCanonicalDoesNotMergeDifferentPeople(t *testing.T) {
	t.Parallel()

	for _, pair := range [][2]string{
		{"john.smith@acme.com", "johnsmith@acme.com"},
		{"john.smith@outlook.com", "johnsmith@outlook.com"},
		{"alice+work@example.com", "alice@example.com"},
		{"alice+work@gmail.com", "alice@gmail.com"},
	} {
		a, err := mail.ParseAddress(pair[0])
		require.NoError(t, err)
		b, err := mail.ParseAddress(pair[1])
		require.NoError(t, err)
		require.NotEqual(
			t,
			a.Canonical(),
			b.Canonical(),
			"%s and %s are different addresses",
			pair[0],
			pair[1],
		)
	}
}

// Delivery goes to what the person typed, not to the uniqueness key: a mail to
// johnsmith@gmail.com arrives either way, but the typed form is theirs.
func TestStringKeepsWhatWasTyped(t *testing.T) {
	t.Parallel()

	addr, err := mail.ParseAddress("John.Smith@Gmail.com")
	require.NoError(t, err)
	require.Equal(t, "John.Smith@gmail.com", addr.String())
	require.Equal(t, "johnsmith@gmail.com", addr.Canonical())
}
