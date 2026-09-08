package secret_test

import (
	"bytes"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"testing"

	"gitlab.internal/tuan.quang.tran/aioz-common/secret"

	"github.com/stretchr/testify/require"
)

// positive control: without this the whole file passes trivially against a
// Token that is always empty.
func TestNewTokenShape(t *testing.T) {
	t.Parallel()

	tok, hash := secret.NewToken()

	require.Regexp(t, regexp.MustCompile(`^[0-9a-f]{64}$`), tok.Reveal())
	require.Len(t, tok.Reveal(), secret.TokenBytes*2)
	require.Equal(t, secret.HashOf(tok.Reveal()), hash)
	require.True(t, secret.Verify(tok.Reveal(), hash))
	require.False(t, tok.IsZero())

	other, _ := secret.NewToken()
	require.NotEqual(t, tok, other)
}

func TestNewTokenNRefusesWeakEntropy(t *testing.T) {
	t.Parallel()

	require.Panics(t, func() { secret.NewTokenN(secret.MinBytes - 1) })
	require.Panics(t, func() { secret.NewTokenN(0) })
	require.NotPanics(t, func() { secret.NewTokenN(secret.MinBytes) })
}

type exported struct {
	T secret.Token
}

type nested struct {
	Inner exported
}

type unexportedField struct {
	tok secret.Token //nolint:unused // read by fmt through reflection, see the test
}

// TestTokenIsRedacted walks every channel a secret can escape through and
// asserts, for each, that the plaintext is absent and Redacted is present.
func TestTokenIsRedacted(t *testing.T) {
	t.Parallel()

	tok, _ := secret.NewToken()
	plaintext := tok.Reveal()

	noLeak := func(t *testing.T, out string) {
		t.Helper()
		require.NotContains(t, out, plaintext, "plaintext leaked")
	}
	check := func(t *testing.T, out string) {
		t.Helper()
		noLeak(t, out)
		require.Contains(t, out, secret.Redacted)
	}

	t.Run("every fmt verb", func(t *testing.T) {
		// %d is the case that fails with only a Stringer: it takes fmt's
		// badVerb path, which reflects the underlying string out.
		for _, format := range []string{
			"%v", "%s", "%q", "%x", "%X", "%d", "%#v", "%+v", "%8s", "%-20v", "%t",
		} {
			t.Run(format, func(t *testing.T) {
				check(t, fmt.Sprintf(format, tok))
			})
		}
	})

	t.Run("every shape fmt sees it in", func(t *testing.T) {
		shapes := map[string]any{
			"bare":    tok,
			"pointer": &tok,
			"any":     any(tok),
			"slice":   []secret.Token{tok},
			"map":     map[string]secret.Token{"k": tok},
			"struct":  exported{T: tok},
			"nested":  nested{Inner: exported{T: tok}},
		}
		for name, v := range shapes {
			t.Run(name, func(t *testing.T) {
				check(t, fmt.Sprintf("%v", v))
				check(t, fmt.Sprintf("%+v", v))
				check(t, fmt.Sprint(v))
				check(t, strings.TrimSuffix(fmt.Sprintln(v), "\n"))
			})
		}
	})

	t.Run("error strings", func(t *testing.T) {
		check(t, fmt.Errorf("token was %v", tok).Error())
		check(t, secret.Error.Errorf("token was %v", tok).Error())
	})

	t.Run("slog", func(t *testing.T) {
		logged := func(attr slog.Attr) string {
			var buf bytes.Buffer
			slog.New(slog.NewJSONHandler(&buf, nil)).Info("msg", attr)
			return buf.String()
		}

		t.Run("the token itself", func(t *testing.T) {
			check(t, logged(slog.Any("tok", tok)))
		})

		// slog does not walk struct fields, so a struct carrying a Token is
		// handed to json.Marshal, which our MarshalJSON refuses. The line
		// carries a visible "!ERROR" instead of the redaction. Noisy, safe,
		// and diagnosable, which is the trade the refusal buys.
		t.Run("a struct carrying one", func(t *testing.T) {
			out := logged(slog.Any("wrapper", exported{T: tok}))
			noLeak(t, out)
			require.Contains(t, out, "!ERROR")
		})
	})
}

// TestTokenRefusesToLeaveTheProcess asserts the outbound paths error rather
// than emit a placeholder. A handler that returns the literal redaction string
// ships a bug that looks like a working deploy.
func TestTokenRefusesToLeaveTheProcess(t *testing.T) {
	t.Parallel()

	tok, _ := secret.NewToken()

	t.Run("json.Marshal", func(t *testing.T) {
		_, err := json.Marshal(tok)
		require.Error(t, err)
	})

	t.Run("json.Marshal of a struct field", func(t *testing.T) {
		_, err := json.Marshal(exported{T: tok})
		require.Error(t, err)
	})

	t.Run("MarshalText", func(t *testing.T) {
		_, err := tok.MarshalText()
		require.Error(t, err)
	})

	// The proof that database/sql's reflect fallback cannot reach the
	// plaintext. This is the certificate/authorization bug's headstone.
	t.Run("database/sql default converter", func(t *testing.T) {
		_, err := driver.DefaultParameterConverter.ConvertValue(tok)
		require.Error(t, err)
	})

	t.Run("Value", func(t *testing.T) {
		_, err := tok.Value()
		require.Error(t, err)
	})
}

func TestTokenFlowsInFreely(t *testing.T) {
	t.Parallel()

	tok, _ := secret.NewToken()
	plaintext := tok.Reveal()

	t.Run("UnmarshalText", func(t *testing.T) {
		var got secret.Token
		require.NoError(t, got.UnmarshalText([]byte(plaintext)))
		require.Equal(t, plaintext, got.Reveal())
	})

	t.Run("UnmarshalJSON", func(t *testing.T) {
		var got secret.Token
		require.NoError(t, json.Unmarshal([]byte(`"`+plaintext+`"`), &got))
		require.Equal(t, plaintext, got.Reveal())

		require.Error(t, json.Unmarshal([]byte(`42`), &got))
	})

	t.Run("Scan", func(t *testing.T) {
		var got secret.Token
		require.NoError(t, got.Scan(plaintext))
		require.Equal(t, plaintext, got.Reveal())

		require.NoError(t, got.Scan([]byte(plaintext)))
		require.Equal(t, plaintext, got.Reveal())

		require.NoError(t, got.Scan(nil))
		require.True(t, got.IsZero())

		require.Error(t, got.Scan(42))
	})
}

// TestDocumentedHoles asserts the two channels redaction cannot close, so
// nobody assumes coverage that does not exist. Both are in the README.
//
// If either of these fails, Go changed and the hole is gone: delete the case
// and update the README.
func TestDocumentedHoles(t *testing.T) {
	t.Parallel()

	tok, _ := secret.NewToken()

	// fmt cannot call methods on an unexported field, so it reflects the
	// underlying string out. Nothing the type can do.
	t.Run("unexported struct field", func(t *testing.T) {
		out := fmt.Sprintf("%+v", unexportedField{tok: tok})
		require.Contains(t, out, tok.Reveal())
	})

	// fmt handles %p and %T before it consults Formatter, so Format never runs
	// and %p on a non-pointer falls to badVerb, which reflects the value out.
	//
	// go vet does not catch it either, and that is worth knowing: vet's printf
	// check skips any type implementing fmt.Formatter, so the very method that
	// closes every other verb is what silences the tool for this one. (%T is
	// handled the same way but only prints the type name, so it is safe.)
	t.Run("%p", func(t *testing.T) {
		out := fmt.Sprintf("%p", tok)
		require.Contains(t, out, tok.Reveal())
	})
}
