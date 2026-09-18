// Package upload checks a file a client sends before a service keeps it, as
// OWASP's File Upload cheat sheet asks.
//
// Our upload endpoints today take the client's Content-Type as the file's
// type, check the size after the whole body has been parsed to disk, and use
// the client's filename. Each of those is a claim the uploader controls: a
// PHP script named cat.png with Content-Type image/png passes all three.
//
// A Policy decides instead. The extension and the sniffed content must name
// the same allowed Type; the size is enforced while reading, failing rather
// than truncating; and the client's filename is used only as a display name,
// reduced to something safe to show and log.
//
//	var avatars = upload.Policy{MaxBytes: 5 << 20, Types: []upload.Type{upload.PNG, upload.JPEG, upload.JPG}}
//
//	upload.LimitRequest(w, r, 6<<20) // before anything parses the body
//	fh, err := c.FormFile("avatar")   // gin or echo
//	f, err := avatars.Open(fh)        // ErrTooLarge: 413, ErrRefused: 415
//	defer f.Close()
//	key := uuid.New().String()        // store under a name the client did not choose
//	_, err = store.Put(ctx, key, f,   // a read past MaxBytes fails the Put
//		s3client.PutParams{ContentType: f.Type.MIME})
//
// # What this package does not do
//
// It does not store files. Key them by a random id, never by the client's
// name; on a filesystem, open them with os.OpenInRoot so a name cannot climb
// out of the directory. It does not re-encode images, scan for malware or
// unpack archives, and it never allows SVG or HTML, which are active content
// served from our own origin.
package upload

import (
	"bytes"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"path"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/zeebo/errs/v2"
)

// Error tags every failure this package produces.
var Error = errs.Tag("upload")

// The two ways an upload is turned away. Test with errors.Is.
var (
	// ErrTooLarge is a file over the policy's MaxBytes: answer 413.
	ErrTooLarge = errors.New("upload: too large")
	// ErrRefused is a file whose extension or content is not allowed, or an
	// empty one: answer 415.
	ErrRefused = errors.New("upload: refused")
)

// Type is an allowed kind of file: its extension, lowercase with the dot,
// and the type http.DetectContentType reports for its content.
type Type struct {
	Extension string
	MIME      string
}

// The image types. JPEG has two extensions, so it has two Types.
var (
	PNG  = Type{Extension: ".png", MIME: "image/png"}
	JPEG = Type{Extension: ".jpeg", MIME: "image/jpeg"}
	JPG  = Type{Extension: ".jpg", MIME: "image/jpeg"}
	GIF  = Type{Extension: ".gif", MIME: "image/gif"}
	WebP = Type{Extension: ".webp", MIME: "image/webp"}
)

// Policy is what an endpoint accepts.
type Policy struct {
	// MaxBytes is the largest file accepted. Required.
	MaxBytes int64
	// Types are the kinds of file accepted. Required.
	Types []Type
	// MaxNameLen bounds File.Name in bytes; 0 is 255.
	MaxNameLen int
}

const (
	sniffLen       = 512 // all http.DetectContentType reads
	defaultNameLen = 255
)

// activeExtensions and activeMIME are content a browser runs when it is
// served from our origin. No policy may allow them.
var (
	activeExtensions = []string{
		".svg",
		".svgz",
		".html",
		".htm",
		".xhtml",
		".xml",
		".js",
		".mjs",
	}
	activeMIME = []string{
		"image/svg+xml",
		"text/html",
		"application/xhtml+xml",
		"text/xml",
		"application/xml",
		"text/javascript",
		"application/javascript",
	}
)

func (p Policy) validate() error {
	switch {
	case p.MaxBytes <= 0:
		return Error.Errorf("policy has no size limit")
	case len(p.Types) == 0:
		return Error.Errorf("policy allows no types")
	case p.MaxNameLen < 0:
		return Error.Errorf("policy name length must not be negative")
	}
	for _, t := range p.Types {
		if !strings.HasPrefix(t.Extension, ".") ||
			t.Extension != strings.ToLower(t.Extension) {
			return Error.Errorf(
				"type %q: want a lowercase extension with its dot",
				t.Extension,
			)
		}
		if t.MIME == "" {
			return Error.Errorf("type %q: no MIME type", t.Extension)
		}
		base, _, _ := strings.Cut(t.MIME, ";")
		if slices.Contains(activeExtensions, t.Extension) ||
			slices.Contains(activeMIME, strings.TrimSpace(base)) {
			return Error.Errorf(
				"type %q: active content is never allowed",
				t.Extension,
			)
		}
	}
	return nil
}

// File is an accepted upload. Read it to the end: past MaxBytes, Read fails
// with ErrTooLarge instead of stopping short.
type File struct {
	// Type is the allowed type the file matched.
	Type Type
	// Name is DisplayName of the client's filename: for showing and
	// logging, never for storing.
	Name string

	r      io.Reader
	closer io.Closer
}

// Read reads the file, replaying the bytes Check sniffed.
func (f *File) Read(p []byte) (int, error) { return f.r.Read(p) }

// Close closes the file Open opened. It is a no-op for Check.
func (f *File) Close() error {
	if f.closer == nil {
		return nil
	}
	return f.closer.Close()
}

// Open checks a form file, as gin's and echo's FormFile return it. A declared
// size over MaxBytes is refused before the file is opened. The client's
// Content-Type is never read.
func (p Policy) Open(fh *multipart.FileHeader) (*File, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	if fh.Size > p.MaxBytes {
		return nil, Error.Errorf("%w: over %d bytes", ErrTooLarge, p.MaxBytes)
	}
	src, err := fh.Open()
	if err != nil {
		return nil, Error.Wrap(err)
	}
	f, err := p.Check(fh.Filename, src)
	if err != nil {
		_ = src.Close()
		return nil, err
	}
	f.closer = src
	return f, nil
}

// Check reads the start of r and accepts it when filename's extension and the
// sniffed content name the same allowed Type. The returned File reads r from
// the beginning.
func (p Policy) Check(filename string, r io.Reader) (*File, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}

	ext := extension(filename)
	var candidates []Type
	for _, t := range p.Types {
		if t.Extension == ext {
			candidates = append(candidates, t)
		}
	}
	if len(candidates) == 0 {
		return nil, Error.Errorf("%w: extension not allowed", ErrRefused)
	}

	// One byte past a small limit is enough to know the file is over it.
	head := make([]byte, min(sniffLen, p.MaxBytes+1))
	n, err := io.ReadFull(r, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) &&
		!errors.Is(err, io.EOF) {
		return nil, Error.Wrap(err)
	}
	head = head[:n]
	switch {
	case n == 0:
		return nil, Error.Errorf("%w: empty file", ErrRefused)
	case int64(n) > p.MaxBytes:
		return nil, Error.Errorf("%w: over %d bytes", ErrTooLarge, p.MaxBytes)
	}

	sniffed := http.DetectContentType(head)
	for _, t := range candidates {
		if t.MIME == sniffed {
			return &File{
				Type: t,
				Name: DisplayName(filename, p.MaxNameLen),
				r: &limitReader{
					r:    io.MultiReader(bytes.NewReader(head), r),
					left: p.MaxBytes,
					max:  p.MaxBytes,
				},
			}, nil
		}
	}
	return nil, Error.Errorf(
		"%w: content is %s, not %s",
		ErrRefused,
		sniffed,
		candidates[0].MIME,
	)
}

// extension is the lowercase extension of the last path element, after the
// trailing dots and spaces Windows would drop: "cat.php." is PHP there.
func extension(filename string) string {
	name := path.Base(strings.ReplaceAll(filename, `\`, "/"))
	name = strings.TrimRight(name, ". ")
	return strings.ToLower(path.Ext(name))
}

// limitReader fails once more than max bytes have been read, where
// io.LimitReader would report a clean EOF and a truncated file would be
// stored as a whole one.
type limitReader struct {
	r    io.Reader
	left int64
	max  int64
}

func (l *limitReader) Read(p []byte) (int, error) {
	if l.left < 0 {
		return 0, Error.Errorf("%w: over %d bytes", ErrTooLarge, l.max)
	}
	if int64(len(p)) > l.left+1 {
		p = p[:l.left+1]
	}
	n, err := l.r.Read(p)
	if int64(n) > l.left {
		n = int(l.left)
		l.left = -1
		return n, Error.Errorf("%w: over %d bytes", ErrTooLarge, l.max)
	}
	l.left -= int64(n)
	return n, err
}

// DisplayName reduces a client's filename to one safe to show and log: the
// last path element, valid UTF-8, without control or bidirectional-override
// characters (which make "cat<U+202E>gnp.exe" display as "catexe.png"), trimmed,
// and at most maxLen bytes, cut on a character boundary with the extension
// kept. An empty result is "file". maxLen 0 is 255.
//
// It is still the client's text: store files under a random id, never this.
func DisplayName(name string, maxLen int) string {
	if maxLen <= 0 {
		maxLen = defaultNameLen
	}
	name = strings.ToValidUTF8(name, "�")
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(path.Base(strings.ReplaceAll(name, `\`, "/")))
	switch name {
	case "", ".", "..", "/":
		return "file"
	}
	if len(name) <= maxLen {
		return name
	}
	ext := path.Ext(name)
	if len(ext) >= maxLen {
		return cut(name, maxLen)
	}
	return cut(strings.TrimSuffix(name, ext), maxLen-len(ext)) + ext
}

// cut returns the longest prefix of s of at most n bytes that ends on a
// character boundary.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// LimitRequest caps r's body at max bytes. Call it before anything parses the
// body: ParseMultipartForm, and gin's and echo's FormFile, spool a whole body
// to disk before a handler sees a file, so a size check on the file comes too
// late. Past the cap, parsing fails with *http.MaxBytesError: answer 413.
// Leave room above the policy's MaxBytes for the form's own framing.
func LimitRequest(w http.ResponseWriter, r *http.Request, max int64) {
	r.Body = http.MaxBytesReader(w, r.Body, max)
}
