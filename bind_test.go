package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"

	config "gitlab.internal/tuan.quang.tran/aioz-config"
)

func newFlagSet() *pflag.FlagSet {
	return pflag.NewFlagSet("test", pflag.ContinueOnError)
}

// unsupportedNested reproduces the shape that made the coordinator panic at
// startup: a bindable command config whose nested struct holds a field of a
// width BindFlags cannot bind (int32).
type unsupportedNested struct {
	Server struct {
		MaxAttempts int32 `help:"give up after this many failed repairs" default:"10"`
	}
}

// TestBindUnsupportedFieldTypePanicIdentifiesField guards the diagnostic, not
// the failure: binding an unsupported type must panic, but the message has to
// name the flag and the Go field so the offender is findable without decoding
// a stack trace.
func TestBindUnsupportedFieldTypePanicIdentifiesField(t *testing.T) {
	t.Parallel()

	defer func() {
		r := recover()
		require.NotNil(t, r, "expected a panic for an unsupported field type")
		msg, ok := r.(string)
		require.Truef(t, ok, "expected a string panic value, got %T: %v", r, r)

		for _, want := range []string{
			"int32",               // the offending type
			"server.max-attempts", // the flag the operator sees
			"MaxAttempts",         // the Go field to edit
		} {
			require.Containsf(
				t, msg, want,
				"panic message %q does not mention %q", msg, want,
			)
		}
	}()

	config.BindFlags(
		newFlagSet(),
		&unsupportedNested{},
		config.UseTestDefaults(),
	)
}

// TestBindSupportedFieldTypes is the positive counterpart: every type the panic
// message advertises as supported must actually bind, with its default parsed.
func TestBindSupportedFieldTypes(t *testing.T) {
	t.Parallel()

	type supported struct {
		Int      int           `help:"int"      default:"1"`
		Int64    int64         `help:"int64"    default:"2"`
		Uint     uint          `help:"uint"     default:"3"`
		Uint64   uint64        `help:"uint64"   default:"4"`
		Float64  float64       `help:"float64"  default:"5.5"`
		Str      string        `help:"string"   default:"six"`
		Bool     bool          `help:"bool"     default:"true"`
		Duration time.Duration `help:"duration" default:"7s"`
		Strings  []string      `help:"strings"  default:"a,b"`
	}

	var cfg supported
	flags := newFlagSet()
	config.BindFlags(flags, &cfg, config.UseTestDefaults())

	for _, name := range []string{
		"int", "int64", "uint", "uint64", "float64", "str", "bool",
		"duration", "strings",
	} {
		require.NotNilf(t, flags.Lookup(name), "flag %q was not bound", name)
	}

	require.Equal(t, 1, cfg.Int)
	require.Equal(t, int64(2), cfg.Int64)
	require.Equal(t, uint(3), cfg.Uint)
	require.Equal(t, uint64(4), cfg.Uint64)
	require.InEpsilon(t, 5.5, cfg.Float64, 0)
	require.Equal(t, "six", cfg.Str)
	require.True(t, cfg.Bool)
	require.Equal(t, 7*time.Second, cfg.Duration)
	require.Equal(t, []string{"a", "b"}, cfg.Strings)
}

// TestBindDefaultTagPrecedence covers which of the four default tags wins for
// each kind of build. Getting this wrong means a binary silently runs on
// another environment's defaults.
func TestBindDefaultTagPrecedence(t *testing.T) {
	t.Parallel()

	type tagged struct {
		All      string `default:"plain" devDefault:"dev" releaseDefault:"release" testDefault:"test"`
		NoTest   string `default:"plain" devDefault:"dev" releaseDefault:"release"`
		OnlyBase string `default:"plain"`
	}

	for _, tc := range []struct {
		name                  string
		opt                   config.BindOption
		all, noTest, onlyBase string
	}{
		{"dev", config.UseDevDefaults(), "dev", "dev", "plain"},
		{"release", config.UseReleaseDefaults(), "release", "release", "plain"},
		// test falls back to the dev value when no testDefault is given
		{"test", config.UseTestDefaults(), "test", "dev", "plain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var cfg tagged
			config.BindFlags(newFlagSet(), &cfg, tc.opt)

			require.Equal(t, tc.all, cfg.All)
			require.Equal(t, tc.noTest, cfg.NoTest)
			require.Equal(t, tc.onlyBase, cfg.OnlyBase)
		})
	}
}

// TestBindPanicsOnDefaultOnlyForOtherBuild catches a config that would silently
// bind to the zero value: a field that defines only the default for the build
// kind you are not doing has no value to fall back to.
func TestBindPanicsOnDefaultOnlyForOtherBuild(t *testing.T) {
	t.Parallel()

	type devOnly struct {
		Address string `devDefault:"127.0.0.1:8080"`
	}

	require.PanicsWithValue(
		t,
		`"releaseDefault" missing but "devDefault" defined for address`,
		func() {
			config.BindFlags(
				newFlagSet(),
				&devOnly{},
				config.UseReleaseDefaults(),
			)
		},
	)
}

// TestBindRejectsInvalidFlagName guards the flagname tag, which otherwise lets
// a struct produce a flag no operator can type.
func TestBindRejectsInvalidFlagName(t *testing.T) {
	t.Parallel()

	type badName struct {
		Address string `flagname:"Server_Address" default:"x"`
	}

	defer func() {
		r := recover()
		require.NotNil(t, r)
		msg, ok := r.(string)
		require.True(t, ok)
		require.Contains(t, msg, "Server_Address")
		require.Contains(t, msg, "Address", "the Go field must be named")
	}()

	config.BindFlags(newFlagSet(), &badName{}, config.UseTestDefaults())
}

// TestBindFieldFiltering covers the tags that decide whether a field becomes a
// flag at all, and how its name is built.
func TestBindFieldFiltering(t *testing.T) {
	t.Parallel()

	type Embedded struct {
		Inherited string `default:"e"`
	}
	type Nested struct {
		Value string `default:"n"`
	}
	type filtered struct {
		// an embedded struct must be an exported type: reflection cannot
		// address the fields of an unexported embedded one
		Embedded
		Nested   Nested
		Skipped  string `noflag:"true" default:"s"`
		Global   string `              default:"g"  noprefix:"true"`
		Internal string `                                           internal:"true"`
		SetupOnl string `              default:"so"                                 setup:"true"`
		Renamed  string `              default:"r"                                               flagname:"custom-name"`
		HTTPAddr string `              default:"h"`
	}

	flags := newFlagSet()
	config.BindFlags(
		flags,
		&filtered{},
		config.UseTestDefaults(),
		config.FlagPrefix("app"),
	)

	// an embedded struct is flattened, a named one gets a dotted prefix
	require.NotNil(t, flags.Lookup("app.inherited"))
	require.NotNil(t, flags.Lookup("app.nested.value"))

	require.Nil(t, flags.Lookup("app.skipped"), "noflag fields are skipped")
	require.NotNil(t, flags.Lookup("global"), "noprefix drops the prefix")
	require.Nil(t, flags.Lookup("app.internal"), "internal fields are skipped")
	require.Nil(
		t,
		flags.Lookup("app.setup-onl"),
		"setup fields need SetupMode",
	)
	require.NotNil(t, flags.Lookup("app.custom-name"))

	// field names become hyphenated snake case, acronyms included
	require.NotNil(t, flags.Lookup("app.http-addr"))
}

// TestBindSetupModeIncludesSetupFields is the counterpart: a setup command asks
// for those fields explicitly.
func TestBindSetupModeIncludesSetupFields(t *testing.T) {
	t.Parallel()

	type withSetup struct {
		Overwrite bool `setup:"true" default:"false"`
	}

	flags := newFlagSet()
	config.BindFlags(
		flags,
		&withSetup{},
		config.UseTestDefaults(),
		config.SetupMode(),
	)

	f := flags.Lookup("overwrite")
	require.NotNil(t, f)
	require.Equal(t, []string{"true"}, f.Annotations["setup"])
}

// TestBindArrayFieldsAreIndexed covers array fields, whose flags are numbered
// with a zero padded index so that they sort correctly in the config file.
func TestBindArrayFieldsAreIndexed(t *testing.T) {
	t.Parallel()

	type entry struct {
		Name string `default:"x"`
	}
	type withArray struct {
		Entries [12]entry
	}

	flags := newFlagSet()
	config.BindFlags(flags, &withArray{}, config.UseTestDefaults())

	require.NotNil(t, flags.Lookup("entries.00.name"))
	require.NotNil(t, flags.Lookup("entries.11.name"))
}

// TestBindRecordsAnnotations checks that every tag SaveConfig later reads is
// actually recorded on the flag.
func TestBindRecordsAnnotations(t *testing.T) {
	t.Parallel()

	type annotated struct {
		User       string `user:"true" default:"u"`
		Hidden     string `            default:"h" hidden:"true"`
		Deprecated string `            default:"d"               deprecated:"true"`
		FlagOnly   string `            default:"f"                                 source:"flag"`
	}

	flags := newFlagSet()
	config.BindFlags(flags, &annotated{}, config.UseTestDefaults())

	require.Equal(
		t,
		[]string{"true"},
		flags.Lookup("user").Annotations["user"],
	)
	require.Equal(
		t,
		[]string{"true"},
		flags.Lookup("hidden").Annotations["hidden"],
	)
	require.True(t, flags.Lookup("hidden").Hidden)
	require.Equal(
		t,
		[]string{"true"},
		flags.Lookup("deprecated").Annotations["deprecated"],
	)
	require.True(t, flags.Lookup("deprecated").Hidden)
	require.Equal(
		t,
		[]string{config.FlagSource},
		flags.Lookup("flag-only").Annotations["source"],
	)
}

// TestBindRejectsUnknownSource guards the source annotation's small vocabulary;
// a typo there would silently persist a flag-only value.
func TestBindRejectsUnknownSource(t *testing.T) {
	t.Parallel()

	type badSource struct {
		Value string `source:"file" default:"v"`
	}

	require.Panics(t, func() {
		config.BindFlags(newFlagSet(), &badSource{}, config.UseTestDefaults())
	})
}

// stringListValue is a minimal pflag.Value, standing in for the custom types
// real configs use for identity paths and node URLs.
type stringListValue struct {
	values []string
}

func (v *stringListValue) String() string { return strings.Join(v.values, ",") }
func (v *stringListValue) Type() string   { return "stringList" }

func (v *stringListValue) Set(s string) error {
	if s == "" {
		v.values = nil
		return nil
	}
	v.values = strings.Split(s, ",")
	return nil
}

// TestBindCustomPflagValue covers the escape hatch for types BindFlags does not
// know: a field implementing pflag.Value binds through its own Set.
func TestBindCustomPflagValue(t *testing.T) {
	t.Parallel()

	type withCustom struct {
		Nodes stringListValue `help:"node list" default:"one,two"`
	}

	var cfg withCustom
	flags := newFlagSet()
	config.BindFlags(flags, &cfg, config.UseTestDefaults())

	require.NotNil(t, flags.Lookup("nodes"))
	require.Equal(t, []string{"one", "two"}, cfg.Nodes.values)
}

// TestBindExpandsDirectoryVariables covers the $CONFDIR family, which is how a
// default can point inside a directory only known at startup.
func TestBindExpandsDirectoryVariables(t *testing.T) {
	t.Parallel()

	type dirs struct {
		Data     string `path:"true" default:"$CONFDIR/data"`
		Identity string `path:"true" default:"$IDENTITYDIR/identity.cert"`
		Cache    string `path:"true" default:"$DATADIR/cache"`
		Custom   string `path:"true" default:"$CUSTOM/thing"`
	}

	var cfg dirs
	config.BindFlags(
		newFlagSet(),
		&cfg,
		config.UseTestDefaults(),
		config.ConfDir("/etc/app"),
		config.IdentityDir("/etc/identity"),
		config.DataDir("/var/lib/app"),
		config.ConfigVar("CUSTOM", "/opt/custom"),
	)

	require.Equal(t, "/etc/app/data", cfg.Data)
	require.Equal(t, "/etc/identity/identity.cert", cfg.Identity)
	require.Equal(t, "/var/lib/app/cache", cfg.Cache)
	require.Equal(t, "/opt/custom/thing", cfg.Custom)
}

// TestBindRequiresPointerToStruct pins the two argument mistakes that would
// otherwise bind a copy and silently discard every loaded value.
func TestBindRequiresPointerToStruct(t *testing.T) {
	t.Parallel()

	type cfg struct {
		Value string `default:"v"`
	}

	require.Panics(t, func() {
		config.BindFlags(newFlagSet(), cfg{}, config.UseTestDefaults())
	}, "a struct passed by value cannot be filled in")

	require.Panics(t, func() {
		s := "not a struct"
		config.BindFlags(newFlagSet(), &s, config.UseTestDefaults())
	})
}

// TestFindFlagIn covers the early argument scan that runs before cobra parses
// anything, so that --config-dir can be resolved in time to expand defaults
// against it.
func TestFindFlagIn(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"equals form", []string{"app", "--config-dir=/etc/app"}, "/etc/app"},
		{"space form", []string{"app", "--config-dir", "/etc/app"}, "/etc/app"},
		{"absent", []string{"app", "run"}, ""},
		{"prefix only", []string{"app", "--config-dir-other=/x"}, ""},
		{"trailing without value", []string{"app", "--config-dir"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tc.want, config.FindFlagIn(tc.args, "config-dir"))
		})
	}
}

// TestDefaultsTypeFollowsSetRelease covers the release switch. Its predecessor
// in depin was dead code (`if false`), so every binary silently ran on dev
// defaults; this asserts the switch is actually wired up.
func TestDefaultsTypeFollowsSetRelease(t *testing.T) {
	// not parallel: SetRelease is process wide
	t.Cleanup(func() { config.SetRelease(false) })

	config.SetRelease(false)
	require.Equal(t, "dev", config.DefaultsType())

	config.SetRelease(true)
	require.Equal(t, "release", config.DefaultsType())
}
