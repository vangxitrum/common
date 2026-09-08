// Package config binds Go configuration structs to command-line flags,
// loads their values from YAML files and environment variables, and
// generates a default configuration file.
//
// A configuration struct describes its own defaults, help text and
// persistence behavior through struct tags. Bind walks the struct and
// registers a flag per field; Load reads config.yaml (and secrets.yaml) plus
// the environment into the struct; SaveConfig writes a config file in which
// values still at their default are commented out.
//
// See README.md for the full struct tag reference.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/pflag"
)

const (
	// AnySource is a source annotation for config values that can come from
	// a flag or file.
	AnySource = "any"

	// FlagSource is a source annotation for config values that just come from
	// flags (i.e. are never persisted to file).
	FlagSource = "flag"

	// basicHelpAnnotationName is the name of the annotation used to indicate
	// a flag should be included in basic usage/help.
	basicHelpAnnotationName = "basic-help"
)

var (
	allSources = []string{
		AnySource,
		FlagSource,
	}

	flagNameRegex = regexp.MustCompile(`^\p{Ll}[\p{Ll}\p{N}-]*$`)
)

// BindOption is an option for BindFlags and Bind.
type BindOption struct {
	isDev   *bool
	isTest  *bool
	isSetup *bool
	varfn   func(vars map[string]confVar)
	prefix  string
}

// BindOpt is a deprecated alias for BindOption.
//
// Deprecated: use BindOption.
type BindOpt = BindOption

// ConfDir sets variables for default options called $CONFDIR.
func ConfDir(path string) BindOption {
	return ConfigVar("CONFDIR", filepath.Clean(os.ExpandEnv(path)))
}

// IdentityDir sets a variable for the default option called $IDENTITYDIR.
func IdentityDir(path string) BindOption {
	return ConfigVar("IDENTITYDIR", filepath.Clean(os.ExpandEnv(path)))
}

// DataDir sets a variable for the default option called $DATADIR.
func DataDir(path string) BindOption {
	return ConfigVar("DATADIR", filepath.Clean(os.ExpandEnv(path)))
}

// ConfigVar sets a variable for the default option called name.
func ConfigVar(name, val string) BindOption {
	name = strings.ToUpper(name)
	return BindOption{varfn: func(vars map[string]confVar) {
		vars[name] = confVar{val: val, nested: false}
	}}
}

// SetupMode issues the bind in a mode where it does not ignore fields with the
// `setup:"true"` tag.
func SetupMode() BindOption {
	setup := true
	return BindOption{isSetup: &setup}
}

// UseDevDefaults forces the bind call to use development defaults unless
// something else is provided as a subsequent option.
// Without a specific defaults setting, BindFlags determines which defaults to
// use from DefaultsType.
func UseDevDefaults() BindOption {
	dev := true
	test := false
	return BindOption{isDev: &dev, isTest: &test}
}

// UseReleaseDefaults forces the bind call to use release defaults unless
// something else is provided as a subsequent option.
func UseReleaseDefaults() BindOption {
	dev := false
	test := false
	return BindOption{isDev: &dev, isTest: &test}
}

// UseTestDefaults forces the bind call to use test defaults unless
// something else is provided as a subsequent option.
func UseTestDefaults() BindOption {
	dev := false
	test := true
	return BindOption{isDev: &dev, isTest: &test}
}

// FlagPrefix defines the prefix that bound flag names are given.
func FlagPrefix(prefix string) BindOption {
	return BindOption{
		prefix: prefix,
	}
}

type confVar struct {
	val    string
	nested bool
}

// BindFlags sets flags on a FlagSet that match the configuration struct
// 'config'. This works by traversing the config struct using the 'reflect'
// package. Use Bind instead when a *cobra.Command is available: only Bind
// records the struct for a later Load.
func BindFlags(flags FlagSet, config any, opts ...BindOption) {
	ptrtype := reflect.TypeOf(config)
	if ptrtype.Kind() != reflect.Pointer {
		panic(
			fmt.Sprintf(
				"invalid config type: %#v. Expecting pointer to struct.",
				config,
			),
		)
	}
	isDev := false
	isTest := false
	setupCommand := false
	vars := map[string]confVar{}
	prefix := ""
	for _, opt := range opts {
		if opt.varfn != nil {
			opt.varfn(vars)
		}
		if opt.isDev != nil {
			isDev = *opt.isDev
		}
		if opt.isTest != nil {
			isTest = *opt.isTest
		}
		if opt.isSetup != nil {
			setupCommand = *opt.isSetup
		}
		if opt.prefix != "" {
			prefix = strings.TrimSuffix(opt.prefix, ".") + "."
		}
	}

	bindConfig(
		flags,
		prefix,
		reflect.ValueOf(config).Elem(),
		vars,
		setupCommand,
		false,
		isDev,
		isTest,
	)
}

func bindConfig(
	flags FlagSet,
	prefix string,
	val reflect.Value,
	vars map[string]confVar,
	setupCommand, setupStruct bool,
	isDev, isTest bool,
) {
	if val.Kind() != reflect.Struct {
		panic(
			fmt.Sprintf(
				"invalid config type: %#v. Expecting struct.",
				val.Interface(),
			),
		)
	}
	typ := val.Type()
	resolvedVars := make(map[string]string, len(vars))
	{
		structpath := strings.ReplaceAll(
			prefix,
			".",
			string(filepath.Separator),
		)
		for k, v := range vars {
			if !v.nested {
				resolvedVars[k] = v.val
				continue
			}
			resolvedVars[k] = filepath.Join(v.val, structpath)
		}
	}

	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		fieldval := val.Field(i)

		if field.Tag.Get("noflag") == "true" {
			continue
		}

		flagname := field.Tag.Get("flagname")
		if flagname != "" {
			if !flagNameRegex.MatchString(flagname) {
				panic(
					fmt.Sprintf(
						"invalid flag name %q for field %s: must begin with a lowercase letter followed by zero or more lowercase letters, numbers, or hyphens",
						flagname,
						field.Name,
					),
				)
			}
		} else {
			flagname = hyphenate(snakeCase(field.Name))
		}

		if field.Tag.Get("noprefix") != "true" {
			flagname = prefix + flagname
		}

		checkSecretTag(field.Tag, flagname, field.Name)

		onlyForSetup := (field.Tag.Get("setup") == "true") || setupStruct
		// ignore setup params for non setup commands
		if !setupCommand && onlyForSetup {
			continue
		}

		if !fieldval.CanAddr() {
			panic(fmt.Sprintf("cannot addr field %s in %s", field.Name, typ))
		}

		fieldref := fieldval.Addr()
		if !fieldref.CanInterface() {
			panic(
				fmt.Sprintf(
					"cannot get interface of field %s in %s",
					field.Name,
					typ,
				),
			)
		}

		fieldaddr := fieldref.Interface()
		if fieldvalue, ok := fieldaddr.(pflag.Value); ok {
			help := field.Tag.Get("help")
			def := getDefault(field.Tag, isTest, isDev, flagname)

			if field.Tag.Get("internal") == "true" {
				if def != "" {
					panic(
						"unapplicable default value set for internal flag: " + flagname,
					)
				}
				continue
			}

			err := fieldvalue.Set(def)
			if err != nil {
				panic(
					fmt.Sprintf(
						"invalid default value for %s: %#v, %v",
						flagname,
						def,
						err,
					),
				)
			}
			flags.Var(fieldvalue, flagname, help)

			applyAnnotations(flags, flagname, field.Tag, onlyForSetup)
			continue
		}

		switch field.Type.Kind() {
		case reflect.Struct:
			if field.Anonymous {
				bindConfig(
					flags,
					prefix,
					fieldval,
					vars,
					setupCommand,
					onlyForSetup,
					isDev,
					isTest,
				)
			} else {
				bindConfig(
					flags,
					flagname+".",
					fieldval,
					vars,
					setupCommand,
					onlyForSetup,
					isDev,
					isTest,
				)
			}
		case reflect.Array:
			digits := len(strconv.Itoa(fieldval.Len()))
			for j := 0; j < fieldval.Len(); j++ {
				padding := strings.Repeat("0", digits-len(strconv.Itoa(j)))
				bindConfig(
					flags,
					fmt.Sprintf("%s.%s%d.", flagname, padding, j),
					fieldval.Index(j),
					vars,
					setupCommand,
					onlyForSetup,
					isDev,
					isTest,
				)
			}
		default:
			help := field.Tag.Get("help")
			def := getDefault(field.Tag, isTest, isDev, flagname)

			if field.Tag.Get("internal") == "true" {
				if def != "" {
					panic(
						"unapplicable default value set for internal flag: " + flagname,
					)
				}
				continue
			}

			def = expand(resolvedVars, def)

			bindScalar(flags, flagname, help, def, field, fieldval)
			applyAnnotations(flags, flagname, field.Tag, onlyForSetup)
		}
	}
}

// bindScalar registers a single non-struct field on flags, converting def to
// the field's type.
func bindScalar(
	flags FlagSet,
	flagname, help, def string,
	field reflect.StructField,
	fieldval reflect.Value,
) {
	fieldaddr := fieldval.Addr().Interface()
	check := func(err error) {
		if err != nil {
			panic(
				fmt.Sprintf(
					"invalid default value for %s: %#v",
					flagname,
					def,
				),
			)
		}
	}
	switch field.Type {
	case reflect.TypeOf(int(0)):
		val, err := strconv.ParseInt(def, 0, strconv.IntSize)
		check(err)
		flags.IntVar(fieldaddr.(*int), flagname, int(val), help)
	case reflect.TypeOf(int64(0)):
		val, err := strconv.ParseInt(def, 0, 64)
		check(err)
		flags.Int64Var(fieldaddr.(*int64), flagname, val, help)
	case reflect.TypeOf(uint(0)):
		val, err := strconv.ParseUint(def, 0, strconv.IntSize)
		check(err)
		flags.UintVar(fieldaddr.(*uint), flagname, uint(val), help)
	case reflect.TypeOf(uint64(0)):
		val, err := strconv.ParseUint(def, 0, 64)
		check(err)
		flags.Uint64Var(fieldaddr.(*uint64), flagname, val, help)
	case reflect.TypeOf(time.Duration(0)):
		val, err := time.ParseDuration(def)
		check(err)
		flags.DurationVar(fieldaddr.(*time.Duration), flagname, val, help)
	case reflect.TypeOf(float64(0)):
		val, err := strconv.ParseFloat(def, 64)
		check(err)
		flags.Float64Var(fieldaddr.(*float64), flagname, val, help)
	case reflect.TypeOf(string("")):
		if field.Tag.Get("path") == "true" {
			// NB: conventionally unix path separators are used in default values
			def = filepath.FromSlash(def)
		}
		flags.StringVar(fieldaddr.(*string), flagname, def, help)
	case reflect.TypeOf(bool(false)):
		val, err := strconv.ParseBool(def)
		check(err)
		flags.BoolVar(fieldaddr.(*bool), flagname, val, help)
	case reflect.TypeOf([]string(nil)):
		// allow either a single string, or comma separated values for defaults
		defaultValues := []string{}
		if def != "" {
			defaultValues = strings.Split(def, ",")
		}
		flags.StringSliceVar(
			fieldaddr.(*[]string),
			flagname,
			defaultValues,
			help,
		)
	default:
		panic(fmt.Sprintf(
			"invalid field type %s for config flag %q (field %s.%s): supported types are int, int64, uint, uint64, float64, string, bool, time.Duration and []string",
			field.Type.String(),
			flagname,
			fieldval.Type().String(),
			field.Name,
		))
	}
}

// applyAnnotations records the tag-driven annotations on a bound flag and
// hides it when the tags call for it.
func applyAnnotations(
	flags FlagSet,
	flagname string,
	tag reflect.StructTag,
	onlyForSetup bool,
) {
	if onlyForSetup {
		SetBoolAnnotation(flags, flagname, "setup", true)
	}
	if tag.Get("user") == "true" {
		SetBoolAnnotation(flags, flagname, "user", true)
	}
	if tag.Get(basicHelpAnnotationName) == "true" {
		SetBoolAnnotation(flags, flagname, basicHelpAnnotationName, true)
	}

	markHidden := false
	if tag.Get("hidden") == "true" {
		markHidden = true
		SetBoolAnnotation(flags, flagname, "hidden", true)
	}
	if tag.Get("deprecated") == "true" {
		markHidden = true
		SetBoolAnnotation(flags, flagname, "deprecated", true)
	}
	if source := tag.Get("source"); source != "" {
		setSourceAnnotation(flags, flagname, source)
	}
	if markHidden {
		if err := flags.MarkHidden(flagname); err != nil {
			panic(fmt.Sprintf("mark hidden failed %s: %v", flagname, err))
		}
	}
}

// getDefault resolves which of the default tags applies, given the kind of
// defaults in use.
func getDefault(
	tag reflect.StructTag,
	isTest, isDev bool,
	flagname string,
) string {
	var order []string
	var opposites []string
	switch {
	case isTest:
		order = []string{"testDefault", "devDefault", "default"}
		opposites = []string{"releaseDefault"}
	case isDev:
		order = []string{"devDefault", "default"}
		opposites = []string{"releaseDefault", "testDefault"}
	default:
		order = []string{"releaseDefault", "default"}
		opposites = []string{"devDefault", "testDefault"}
	}

	for _, name := range order {
		if val, ok := tag.Lookup(name); ok {
			return val
		}
	}

	for _, name := range opposites {
		if _, ok := tag.Lookup(name); ok {
			panic(
				fmt.Sprintf(
					"%q missing but %q defined for %v",
					order[0],
					name,
					flagname,
				),
			)
		}
	}

	return ""
}

func setSourceAnnotation(flagset any, name, source string) {
	switch source {
	case AnySource:
	case FlagSource:
	default:
		panic(
			fmt.Sprintf(
				"invalid source annotation %q for %s: must be one of %q",
				source,
				name,
				allSources,
			),
		)
	}

	setStringAnnotation(flagset, name, "source", source)
}

// setStringAnnotation records a string annotation. It is a no-op unless
// flagset is a *pflag.FlagSet.
func setStringAnnotation(flagset any, name, key, value string) {
	flags, ok := flagset.(*pflag.FlagSet)
	if !ok {
		return
	}

	err := flags.SetAnnotation(name, key, []string{value})
	if err != nil {
		panic(
			fmt.Sprintf(
				"unable to set %s annotation for %s: %v",
				key,
				name,
				err,
			),
		)
	}
}

// SetBoolAnnotation sets an annotation on flagset with a value of
// []string{"true"|"false"}. It is a no-op unless flagset is a *pflag.FlagSet.
func SetBoolAnnotation(flagset any, name, key string, value bool) {
	flags, ok := flagset.(*pflag.FlagSet)
	if !ok {
		return
	}

	err := flags.SetAnnotation(name, key, []string{strconv.FormatBool(value)})
	if err != nil {
		panic(
			fmt.Sprintf(
				"unable to set %s annotation for %s: %v",
				key,
				name,
				err,
			),
		)
	}
}

func expand(vars map[string]string, val string) string {
	return os.Expand(val, func(key string) string { return vars[key] })
}
