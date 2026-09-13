package mail

import (
	"fmt"
	"html/template"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Templates is a set of HTML email bodies, parsed once at startup.
//
// It is html/template, not text/template, on purpose: data is escaped for the
// context it lands in, so a name a user typed cannot put markup - or a link -
// into an email another person receives.
type Templates struct {
	set *template.Template
}

// ParseTemplates parses the templates matching pattern from builtin, or, when
// overrideDir is not empty, every file in overrideDir instead.
//
// builtin is meant to be the service's own embedded files:
//
//	//go:embed templates/*.html
//	var emails embed.FS
//
//	templates, err := mail.ParseTemplates(emails, "templates/*.html", cfg.TemplateDir)
//
// Embedding makes the bodies part of the binary, so an image cannot ship
// without them and fail on its first email. The override directory is how a
// deployment changes wording without a release. A directory that is configured
// but missing or empty is an error: saying so at startup beats sending an
// email with no body.
func ParseTemplates(
	builtin fs.FS,
	pattern, overrideDir string,
) (*Templates, error) {
	if overrideDir == "" {
		set, err := template.ParseFS(builtin, pattern)
		if err != nil {
			return nil, fmt.Errorf(
				"mail: parsing the built-in templates: %w",
				err,
			)
		}
		return &Templates{set: set}, nil
	}

	if _, err := os.Stat(overrideDir); err != nil {
		return nil, fmt.Errorf("mail: template directory: %w", err)
	}

	var paths []string
	err := filepath.WalkDir(
		overrideDir,
		func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() {
				paths = append(paths, path)
			}
			return nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("mail: template directory: %w", err)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf(
			"mail: template directory %q has no templates",
			overrideDir,
		)
	}

	set, err := template.ParseFiles(paths...)
	if err != nil {
		return nil, fmt.Errorf("mail: parsing the override templates: %w", err)
	}
	return &Templates{set: set}, nil
}

// Render executes the named template with data.
func (t *Templates) Render(name string, data any) (string, error) {
	if t.set.Lookup(name) == nil {
		return "", fmt.Errorf("mail: no template named %q", name)
	}

	var out strings.Builder
	if err := t.set.ExecuteTemplate(&out, name, data); err != nil {
		return "", fmt.Errorf("mail: rendering %q: %w", name, err)
	}
	return out.String(), nil
}

// Lookup reports whether the set has a template named name, so a service can
// test that every email it sends has a body.
func (t *Templates) Lookup(name string) bool {
	return t.set.Lookup(name) != nil
}
