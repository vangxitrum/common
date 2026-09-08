package apierr

import (
	"fmt"
	"regexp"
	"strings"
)

// reasonPattern is lowercase kebab-case: the slug a client branches on.
var reasonPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// statusWords are the words that describe an HTTP status rather than a cause.
// A reason built only from these carries no information the status code does
// not already carry, which is the failure this package exists to prevent: in
// one real service every missing thing - contract, file, segment - answered
// with the single slug "not-found", so a client could not tell them apart.
//
// The rule is that a reason names the subject: "contract-not-found", not
// "not-found". One status maps to many reasons; never the reverse.
var statusWords = map[string]bool{
	"bad-request":              true,
	"unauthorized":             true,
	"forbidden":                true,
	"not-found":                true,
	"conflict":                 true,
	"already-exists":           true,
	"too-many-requests":        true,
	"internal":                 true,
	"internal-error":           true,
	"internal-server-error":    true,
	"unavailable":              true,
	"service-unavailable":      true,
	"error":                    true,
	"failed":                   true,
	"invalid":                  true,
	"unknown":                  true,
	"method-not-allowed":       true,
	"unprocessable":            true,
	"unprocessable-entity":     true,
	"payload-too-large":        true,
	"request-entity-too-large": true,
}

// ValidReason reports why reason is unusable, or nil when it is fine.
//
// It is exported so a policy test can hold every reason slug in the codebase
// to the same rule, rather than trusting each author to remember it.
func ValidReason(reason string) error {
	switch {
	case reason == "":
		return fmt.Errorf("a reason must not be empty")
	case !reasonPattern.MatchString(reason):
		return fmt.Errorf("reason %q must be lowercase kebab-case, for example %q",
			reason, "contract-not-found")
	case statusWords[reason]:
		return fmt.Errorf(
			"reason %q only restates the HTTP status, so a client cannot tell "+
				"two failures apart; name the subject instead, for example %q",
			reason, subjectExample(reason))
	}
	return nil
}

// subjectExample suggests a better slug for a status-word reason.
func subjectExample(reason string) string {
	if strings.HasSuffix(reason, "not-found") {
		return "contract-not-found"
	}
	return "contract-" + reason
}
