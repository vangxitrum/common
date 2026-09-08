package config_test

import (
	"testing"

	config "gitlab.internal/tuan.quang.tran/aioz-common/config"

	"github.com/stretchr/testify/require"
)

// shippedPlaceholder reproduces the shape this rule exists to make impossible:
// a secret with a value in the source tree, so every deployment that never
// overrode it shares a password that is in every clone, image and CI log.
type shippedPlaceholder struct {
	Wallet struct {
		Password string `help:"wallet encryption password" secret:"true" default:"very-strong-password"`
	}
}

type shippedReleaseDefault struct {
	Wallet struct {
		Password string `help:"wallet encryption password" secret:"true" releaseDefault:"placeholder"`
	}
}

// devOnlyDefault is the sanctioned way to keep local work convenient: a
// devDefault for the developer, and an explicitly empty releaseDefault, which
// getDefault already requires alongside it. The pair states in the tag itself
// that nothing ships.
type devOnlyDefault struct {
	Wallet struct {
		Password string `help:"wallet encryption password" secret:"true" devDefault:"dev-only" releaseDefault:""`
	}
}

type badSecretTag struct {
	Token string `help:"an api token" secret:"yes"`
}

// TestBindRejectsAShippedSecret guards the diagnostic as much as the failure.
// A panic in Bind runs from a command's init and takes down every role of the
// binary, so the message is all the operator gets: it has to name the field,
// the flag, and the offending value.
func TestBindRejectsAShippedSecret(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		cfg  any
		want []string
	}{
		"default": {
			cfg:  &shippedPlaceholder{},
			want: []string{"Password", "wallet.password", "default", "very-strong-password"},
		},
		"releaseDefault": {
			cfg:  &shippedReleaseDefault{},
			want: []string{"Password", "wallet.password", "releaseDefault", "placeholder"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			defer func() {
				r := recover()
				require.NotNil(t, r, "expected a panic for a secret with a shipped value")
				msg, ok := r.(string)
				require.Truef(t, ok, "expected a string panic value, got %T: %v", r, r)

				for _, want := range tc.want {
					require.Containsf(t, msg, want, "panic message should mention %q: %s", want, msg)
				}
			}()

			config.BindFlags(newFlagSet(), tc.cfg)
		})
	}
}

// A devDefault does not reach a release build, so it is exactly the escape
// hatch that keeps local work convenient without shipping anything.
func TestBindAllowsADevDefaultOnASecret(t *testing.T) {
	t.Parallel()

	require.NotPanics(t, func() {
		config.BindFlags(newFlagSet(), &devOnlyDefault{}, config.UseDevDefaults())
	})
	require.NotPanics(t, func() {
		config.BindFlags(newFlagSet(), &devOnlyDefault{})
	})
}

// And the release build of that same struct really does come up empty, so
// CheckSecrets stops it. This is the pairing that matters: the dev convenience
// must not become the production value.
func TestADevDefaultDoesNotReachRelease(t *testing.T) {
	t.Parallel()

	var cfg devOnlyDefault
	config.BindFlags(newFlagSet(), &cfg)
	require.Empty(t, cfg.Wallet.Password)
}

func TestBindRejectsAnInvalidSecretTag(t *testing.T) {
	t.Parallel()

	defer func() {
		r := recover()
		require.NotNil(t, r)
		msg, _ := r.(string)
		require.Contains(t, msg, "invalid secret tag")
		require.Contains(t, msg, "Token")
	}()

	config.BindFlags(newFlagSet(), &badSecretTag{})
}

type serviceConfig struct {
	Wallet struct {
		Password string `help:"wallet encryption password" secret:"true"`
	}
	Telemetry struct {
		BearerToken string `help:"remote write bearer token" secret:"optional"`
	}
	Public string `help:"not a secret"`
}

func TestCheckSecrets(t *testing.T) {
	t.Parallel()

	t.Run("reports a required secret that is unset", func(t *testing.T) {
		t.Parallel()

		var cfg serviceConfig
		err := config.CheckSecrets(&cfg)

		require.Error(t, err)
		require.Contains(t, err.Error(), "Wallet.Password")
		require.Contains(t, err.Error(), "--wallet.password")
		require.Contains(t, err.Error(), "wallet encryption password",
			"the help text should say what the operator is being asked for")
	})

	t.Run("passes once it is set", func(t *testing.T) {
		t.Parallel()

		var cfg serviceConfig
		cfg.Wallet.Password = "supplied at deploy time"
		require.NoError(t, config.CheckSecrets(&cfg))
	})

	t.Run("an optional secret may stay empty", func(t *testing.T) {
		t.Parallel()

		var cfg serviceConfig
		cfg.Wallet.Password = "set"
		require.NoError(t, config.CheckSecrets(&cfg))
		require.Empty(t, cfg.Telemetry.BearerToken)
	})

	t.Run("accepts a value as well as a pointer", func(t *testing.T) {
		t.Parallel()

		var cfg serviceConfig
		cfg.Wallet.Password = "set"
		require.NoError(t, config.CheckSecrets(cfg))
	})
}

type twoSecrets struct {
	A struct {
		Key string `help:"first" secret:"true"`
	}
	B struct {
		Key string `help:"second" secret:"true"`
	}
}

// Reporting one missing secret at a time turns a single deployment fix into as
// many restarts as there are missing values.
func TestCheckSecretsReportsAllOfThem(t *testing.T) {
	t.Parallel()

	err := config.CheckSecrets(&twoSecrets{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "2 required secret(s)")
	require.Contains(t, err.Error(), "A.Key")
	require.Contains(t, err.Error(), "B.Key")
}

type embeddedSecret struct {
	Inner
	Named struct {
		Token string `help:"named" secret:"true"`
	}
}

type Inner struct {
	Embedded string `help:"embedded" secret:"true"`
}

// An embedded struct contributes its fields at the parent's level, the same
// way Bind treats it, so the reported flag name is the one that works.
func TestCheckSecretsFollowsEmbeddingLikeBind(t *testing.T) {
	t.Parallel()

	err := config.CheckSecrets(&embeddedSecret{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "--embedded")
	require.NotContains(t, err.Error(), "--inner.embedded")
	require.Contains(t, err.Error(), "--named.token")
}

func TestCheckSecretsRejectsANonStruct(t *testing.T) {
	t.Parallel()

	require.Error(t, config.CheckSecrets(42))
	require.Error(t, config.CheckSecrets("nope"))
	require.Error(t, config.CheckSecrets((*serviceConfig)(nil)))
}

// The two halves are one rule: Bind makes a shipped secret impossible,
// CheckSecrets makes an unset one a failure to boot. Neither is sufficient
// alone, and this is the shape a service should actually have.
func TestBindThenCheckSecretsIsTheWholeRule(t *testing.T) {
	t.Parallel()

	var cfg serviceConfig
	flags := newFlagSet()

	require.NotPanics(t, func() { config.BindFlags(flags, &cfg) })
	require.Error(t, config.CheckSecrets(&cfg), "an unconfigured service must not start")

	require.NoError(t, flags.Parse([]string{"--wallet.password", "from the deploy"}))
	require.NoError(t, config.CheckSecrets(&cfg))
	require.Equal(t, "from the deploy", cfg.Wallet.Password)
}
