package mail_test

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/mail"
)

func builtin() fstest.MapFS {
	return fstest.MapFS{
		"templates/login_code.html": {
			Data: []byte(`<p>Your code is {{.Code}}</p>`),
		},
		"templates/receipt.html": {Data: []byte(`<p>Paid {{.Amount}}</p>`)},
	}
}

func TestTemplatesRender(t *testing.T) {
	t.Parallel()

	templates, err := mail.ParseTemplates(builtin(), "templates/*.html", "")
	require.NoError(t, err)

	body, err := templates.Render(
		"login_code.html",
		map[string]any{"Code": "000417"},
	)
	require.NoError(t, err)
	require.Equal(t, "<p>Your code is 000417</p>", body)

	require.True(t, templates.Lookup("receipt.html"))
	require.False(t, templates.Lookup("missing.html"))
}

// html/template, not text/template: data is escaped, so a name a user typed
// cannot put markup into an email.
func TestTemplatesEscapeData(t *testing.T) {
	t.Parallel()

	templates, err := mail.ParseTemplates(builtin(), "templates/*.html", "")
	require.NoError(t, err)

	body, err := templates.Render(
		"login_code.html",
		map[string]any{"Code": `<script>alert(1)</script>`},
	)
	require.NoError(t, err)
	require.NotContains(t, body, "<script>")
	require.Contains(t, body, "&lt;script&gt;")
}

func TestAnUnknownTemplateIsAnError(t *testing.T) {
	t.Parallel()

	templates, err := mail.ParseTemplates(builtin(), "templates/*.html", "")
	require.NoError(t, err)

	_, err = templates.Render("missing.html", nil)
	require.Error(t, err)
}

// A deployment can change the wording without a release.
func TestAnOverrideDirectoryReplacesTheBuiltInSet(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "login_code.html"),
		[]byte(`<p>Overridden {{.Code}}</p>`), 0o600))

	templates, err := mail.ParseTemplates(builtin(), "templates/*.html", dir)
	require.NoError(t, err)

	body, err := templates.Render(
		"login_code.html",
		map[string]any{"Code": "1"},
	)
	require.NoError(t, err)
	require.Equal(t, "<p>Overridden 1</p>", body)
}

// A configured directory that is missing or empty is a deployment mistake,
// and saying so at startup beats sending an email with no body.
func TestAMissingOrEmptyOverrideDirectoryIsAnError(t *testing.T) {
	t.Parallel()

	_, err := mail.ParseTemplates(
		builtin(),
		"templates/*.html",
		filepath.Join(t.TempDir(), "nope"),
	)
	require.Error(t, err)

	_, err = mail.ParseTemplates(builtin(), "templates/*.html", t.TempDir())
	require.Error(t, err)
}
