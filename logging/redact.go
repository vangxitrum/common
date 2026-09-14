package logging

import (
	"log/slog"
	"slices"
)

// Redacted is what a sensitive attribute's value is logged as.
const Redacted = "[redacted]"

// redactKeys are the attribute keys whose values are never logged, in
// normalized form: lowercase, with - written as _.
var redactKeys = []string{
	"password", "passwd",
	"secret", "client_secret", "api_secret", "secret_key", "private_key",
	"token", "access_token", "refresh_token", "id_token", "auth_token",
	"authorization", "proxy_authorization",
	"cookie", "set_cookie",
	"otp",
	"api_key", "x_api_key",
}

// RedactKeys returns the built-in sensitive keys.
func RedactKeys() []string {
	return slices.Clone(redactKeys)
}

// RedactAttr returns a ReplaceAttr that logs the value of a sensitive
// attribute as [redacted], keeping its key so the line still says what was
// there. A key is sensitive when, lowercased and with - as _, it equals one of
// RedactKeys or extra exactly - not when it contains one: "passwordless" and
// "token_count" are not secrets, and code, key and session are too common to
// claim. Every attribute under a sensitive group name is redacted too.
//
// logging.New and Bootstrap install it; logger.Config.ReplaceAttr takes it
// for a handler built by hand. It allocates nothing for an attribute it
// leaves alone.
//
// It is a safety net, not the control. It sees keys, not values: a token
// inside an http.Header, a struct or a request body is logged as it is. The
// control is secret.Token, which redacts itself wherever it is formatted.
func RedactAttr(extra ...string) func(groups []string, a slog.Attr) slog.Attr {
	set := make(map[string]struct{}, len(redactKeys)+len(extra))
	longest := 0
	for _, key := range slices.Concat(redactKeys, extra) {
		n := string(normalize(nil, key))
		set[n] = struct{}{}
		longest = max(longest, len(n))
	}

	sensitive := func(key string) bool {
		if key == "" || len(key) > longest {
			return false
		}
		var buf [64]byte
		_, ok := set[string(normalize(buf[:0], key))]
		return ok
	}

	return func(groups []string, a slog.Attr) slog.Attr {
		if sensitive(a.Key) {
			return slog.String(a.Key, Redacted)
		}
		for _, group := range groups {
			if sensitive(group) {
				return slog.String(a.Key, Redacted)
			}
		}
		return a
	}
}

// normalize appends key to dst lowercased, with - as _. ASCII only: every
// sensitive key is.
func normalize(dst []byte, key string) []byte {
	for i := range len(key) {
		c := key[i]
		switch {
		case c >= 'A' && c <= 'Z':
			c += 'a' - 'A'
		case c == '-':
			c = '_'
		}
		dst = append(dst, c)
	}
	return dst
}
