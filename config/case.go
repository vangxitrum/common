package config

import (
	"strings"
	"unicode"
)

// hyphenate converts snake_case to hyphen-case.
func hyphenate(val string) string {
	return strings.ReplaceAll(val, "_", "-")
}

// snakeCase converts a Go identifier to snake_case, splitting on
// lower-to-upper transitions and at the end of an acronym.
func snakeCase(val string) string {
	if len(val) <= 1 {
		return strings.ToLower(val)
	}
	runes := []rune(val)
	rv := make([]rune, 0, len(runes))
	for i := range runes {
		rv = append(rv, unicode.ToLower(runes[i]))
		if i < len(runes)-1 &&
			unicode.IsLower(runes[i]) &&
			unicode.IsUpper(runes[i+1]) {
			// lower-to-uppercase case
			rv = append(rv, '_')
		} else if i < len(runes)-2 &&
			unicode.IsUpper(runes[i]) &&
			unicode.IsUpper(runes[i+1]) &&
			unicode.IsLower(runes[i+2]) {
			// end-of-acronym case
			rv = append(rv, '_')
		}
	}
	return string(rv)
}
