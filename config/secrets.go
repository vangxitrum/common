package config

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// Secret tag values, for the `secret` struct tag.
const (
	// SecretRequired marks a field the process must not start without.
	SecretRequired = "true"

	// SecretOptional marks a field that holds a secret but may be empty,
	// for a feature that is off until it is configured.
	SecretOptional = "optional"
)

// checkSecretTag validates the `secret` tag on one field at bind time.
//
// A field carrying a secret must not ship with a value. A `default` or
// `releaseDefault` reaches production, so one on a secret field means every
// deployment that never overrode it is using a value that is in the source
// tree, and therefore in every clone, image and CI log. `devDefault` and
// `testDefault` are fine: they exist so local work is convenient and they do
// not reach a release build. Note that getDefault already requires a
// `releaseDefault` alongside a `devDefault`, so the sanctioned shape is
//
//	Password string `secret:"true" devDefault:"dev-only" releaseDefault:""`
//
// which states in the tag itself that nothing ships.
//
// This panics rather than returning an error because Bind runs from a
// command's init: there is no caller to hand an error to, and a secret with a
// shipped default is not a condition anything can recover from. The panic
// names both the field and the flag, because a panic in init takes down every
// role of the binary and the message is all the operator gets.
func checkSecretTag(tag reflect.StructTag, flagname, fieldName string) {
	kind, ok := tag.Lookup("secret")
	if !ok {
		return
	}

	switch kind {
	case SecretRequired, SecretOptional:
	default:
		panic(fmt.Sprintf(
			"invalid secret tag %q on field %s (flag %s): must be %q or %q",
			kind, fieldName, flagname, SecretRequired, SecretOptional,
		))
	}

	for _, name := range []string{"default", "releaseDefault"} {
		def, ok := tag.Lookup(name)
		if !ok || def == "" {
			continue
		}
		panic(fmt.Sprintf(
			"field %s (flag %s) is tagged secret but has %s:%q; "+
				"a secret must not ship with a value. Use devDefault for local "+
				"convenience, or leave it unset and supply it at deploy time.",
			fieldName, flagname, name, def,
		))
	}
}

// MissingSecret is one required secret that was not configured.
type MissingSecret struct {
	// Field is the Go path to the field, such as "Wallet.Password".
	Field string

	// Flag is the name an operator sets, such as "wallet.password".
	Flag string

	// Help is the field's help tag, so the message says what the value is.
	Help string
}

func (m MissingSecret) String() string {
	if m.Help == "" {
		return fmt.Sprintf("%s (--%s)", m.Field, m.Flag)
	}
	return fmt.Sprintf("%s (--%s): %s", m.Field, m.Flag, m.Help)
}

// CheckSecrets reports the required secrets in config that are still empty.
//
// Call it once at startup, after [Load], and refuse to start if it returns an
// error. Together with the bind-time rule that a secret cannot carry a
// shipped default, this is what makes "nobody set the password" a failure to
// boot rather than a silently weak deployment:
//
//	if err := config.CheckSecrets(&cfg); err != nil {
//	    return err
//	}
//
// config must be a struct or a pointer to one. Only string fields tagged
// `secret:"true"` are checked; `secret:"optional"` is allowed to be empty.
func CheckSecrets(config any) error {
	val := reflect.ValueOf(config)
	for val.Kind() == reflect.Pointer {
		if val.IsNil() {
			return fmt.Errorf("CheckSecrets: nil config")
		}
		val = val.Elem()
	}
	if val.Kind() != reflect.Struct {
		return fmt.Errorf("CheckSecrets: expected a struct, got %s", val.Kind())
	}

	missing := findMissingSecrets(val, "", "")
	if len(missing) == 0 {
		return nil
	}

	lines := make([]string, 0, len(missing))
	for _, m := range missing {
		lines = append(lines, "  - "+m.String())
	}
	return fmt.Errorf(
		"%d required secret(s) are not configured:\n%s",
		len(missing), strings.Join(lines, "\n"),
	)
}

// findMissingSecrets walks val the way bindConfig does, so the flag names it
// reports are the ones an operator actually sets.
func findMissingSecrets(val reflect.Value, fieldPrefix, flagPrefix string) []MissingSecret {
	var missing []MissingSecret

	typ := val.Type()
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		fieldval := val.Field(i)

		if !fieldval.CanInterface() {
			// An unexported field cannot be set from a flag or a file, so it
			// cannot be an unconfigured secret.
			continue
		}

		flagname := field.Tag.Get("flagname")
		if flagname == "" {
			flagname = hyphenate(snakeCase(field.Name))
		}
		if field.Tag.Get("noprefix") != "true" {
			flagname = flagPrefix + flagname
		}
		fieldpath := fieldPrefix + field.Name

		switch field.Type.Kind() {
		case reflect.Struct:
			if field.Anonymous {
				missing = append(missing, findMissingSecrets(fieldval, fieldPrefix, flagPrefix)...)
			} else {
				missing = append(missing, findMissingSecrets(fieldval, fieldpath+".", flagname+".")...)
			}
		case reflect.Array:
			digits := len(strconv.Itoa(fieldval.Len()))
			for j := 0; j < fieldval.Len(); j++ {
				padding := strings.Repeat("0", digits-len(strconv.Itoa(j)))
				missing = append(missing, findMissingSecrets(
					fieldval.Index(j),
					fmt.Sprintf("%s[%d].", fieldpath, j),
					fmt.Sprintf("%s.%s%d.", flagname, padding, j),
				)...)
			}
		default:
			if field.Tag.Get("secret") != SecretRequired {
				continue
			}
			if fieldval.Kind() == reflect.String && fieldval.String() == "" {
				missing = append(missing, MissingSecret{
					Field: fieldpath,
					Flag:  flagname,
					Help:  field.Tag.Get("help"),
				})
			}
		}
	}

	return missing
}
