package upload_test

import (
	"bytes"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/upload"
)

// The first bytes http.DetectContentType recognizes each format by, padded
// to a plausible size.
var (
	pngBytes  = pad("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR", 600)
	jpegBytes = pad("\xff\xd8\xff\xe0\x00\x10JFIF\x00", 600)
	gifBytes  = pad("GIF89a\x01\x00\x01\x00", 600)
	webpBytes = pad("RIFF\x00\x00\x00\x00WEBPVP8 ", 600)
	phpBytes  = []byte("<?php system($_GET['c']); ?>")
	htmlBytes = []byte("<!DOCTYPE html><script>alert(1)</script>")
)

func pad(prefix string, n int) []byte {
	b := make([]byte, n)
	copy(b, prefix)
	for i := len(prefix); i < n; i++ {
		b[i] = byte(i)
	}
	return b
}

var images = upload.Policy{
	MaxBytes: 1 << 20,
	Types: []upload.Type{
		upload.PNG,
		upload.JPEG,
		upload.JPG,
		upload.GIF,
		upload.WebP,
	},
}

func check(
	t *testing.T,
	p upload.Policy,
	name string,
	content []byte,
) (*upload.File, error) {
	t.Helper()
	return p.Check(name, bytes.NewReader(content))
}

func TestARealImagePassesAndReadsBackWhole(t *testing.T) {
	t.Parallel()

	for name, c := range map[string]struct {
		content []byte
		want    upload.Type
	}{
		"cat.png":  {pngBytes, upload.PNG},
		"cat.jpeg": {jpegBytes, upload.JPEG},
		"cat.jpg":  {jpegBytes, upload.JPG},
		"cat.gif":  {gifBytes, upload.GIF},
		"cat.webp": {webpBytes, upload.WebP},
		"CAT.PNG":  {pngBytes, upload.PNG},
	} {
		f, err := check(t, images, name, c.content)
		require.NoError(t, err, name)
		require.Equal(t, c.want, f.Type, name)

		got, err := io.ReadAll(f)
		require.NoError(t, err, name)
		require.Equal(
			t,
			c.content,
			got,
			"%s: the sniffed bytes are replayed",
			name,
		)
	}
}

// The extension and the content must name the same allowed type. Either one
// alone is a claim the uploader controls.
func TestContentMustMatchTheExtension(t *testing.T) {
	t.Parallel()

	for name, content := range map[string][]byte{
		"shell.jpg":     phpBytes,
		"page.png":      htmlBytes,
		"cat.jpg":       pngBytes,
		"cat.png":       gifBytes,
		"cat.php.png":   phpBytes,
		"cat.png.php":   pngBytes,
		"cat.php.":      pngBytes,
		"cat.php. . .":  pngBytes,
		"cat":           pngBytes,
		"cat.svg":       []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`),
		"cat.png\x00.x": pngBytes,
	} {
		_, err := check(t, images, name, content)
		require.ErrorIs(t, err, upload.ErrRefused, "%q", name)
	}
}

// Windows drops trailing dots and spaces from a name, so they are dropped
// before the extension is read: "cat.png. " is a PNG, "cat.php." is PHP.
func TestTrailingDotsAndSpacesAreIgnored(t *testing.T) {
	t.Parallel()

	f, err := check(t, images, "cat.png. ", pngBytes)
	require.NoError(t, err)
	require.Equal(t, upload.PNG, f.Type)
}

// An empty file is refused even by a policy that allows plain text, which is
// what http.DetectContentType calls no bytes at all.
func TestAnEmptyFileIsRefused(t *testing.T) {
	t.Parallel()

	_, err := check(t, images, "cat.png", nil)
	require.ErrorIs(t, err, upload.ErrRefused)

	text := upload.Policy{MaxBytes: 1 << 20, Types: []upload.Type{{Extension: ".txt", MIME: "text/plain; charset=utf-8"}}}
	_, err = check(t, text, "notes.txt", nil)
	require.ErrorIs(t, err, upload.ErrRefused)
	_, err = check(t, text, "notes.txt", []byte("hello"))
	require.NoError(t, err)
}

// Past MaxBytes the read fails. io.LimitReader would stop quietly at the
// limit and the service would store the first megabyte of a file as if it
// were the whole thing.
func TestTooLargeFailsInsteadOfTruncating(t *testing.T) {
	t.Parallel()
	p := upload.Policy{MaxBytes: 1000, Types: []upload.Type{upload.PNG}}

	exact, err := check(t, p, "cat.png", pngBytes[:600])
	require.NoError(t, err)
	_, err = io.ReadAll(exact)
	require.NoError(t, err)

	atLimit, err := check(t, p, "cat.png", pad("\x89PNG\r\n\x1a\n", 1000))
	require.NoError(t, err)
	got, err := io.ReadAll(atLimit)
	require.NoError(t, err)
	require.Len(t, got, 1000)

	over, err := check(t, p, "cat.png", pad("\x89PNG\r\n\x1a\n", 1001))
	require.NoError(
		t,
		err,
		"the head is fine; the size is only known by reading",
	)
	_, err = io.ReadAll(over)
	require.ErrorIs(t, err, upload.ErrTooLarge)
}

func TestAHeadLargerThanTheLimitIsTooLarge(t *testing.T) {
	t.Parallel()
	p := upload.Policy{MaxBytes: 100, Types: []upload.Type{upload.PNG}}

	_, err := check(t, p, "cat.png", pngBytes)
	require.ErrorIs(t, err, upload.ErrTooLarge)
}

// multipartFile builds the *multipart.FileHeader gin's and echo's FormFile
// return, with the Content-Type the client claimed.
func multipartFile(
	t *testing.T,
	name, contentType string,
	content []byte,
) *multipart.FileHeader {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="file"; filename="`+name+`"`)
	h.Set("Content-Type", contentType)
	part, err := w.CreatePart(h)
	require.NoError(t, err)
	_, err = part.Write(content)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	form, err := multipart.NewReader(&body, w.Boundary()).ReadForm(10 << 20)
	require.NoError(t, err)
	t.Cleanup(func() { _ = form.RemoveAll() })
	return form.File["file"][0]
}

func TestOpenChecksAFormFile(t *testing.T) {
	t.Parallel()

	f, err := images.Open(
		multipartFile(t, "../../cat.png", "image/png", pngBytes),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	require.Equal(t, upload.PNG, f.Type)
	require.Equal(t, "cat.png", f.Name, "the display name, never a path")

	got, err := io.ReadAll(f)
	require.NoError(t, err)
	require.Equal(t, pngBytes, got)
}

// The client's Content-Type is the uploader's claim, and is never read.
func TestTheClientContentTypeIsIgnored(t *testing.T) {
	t.Parallel()

	_, err := images.Open(multipartFile(t, "cat.png", "image/png", phpBytes))
	require.ErrorIs(t, err, upload.ErrRefused)
}

func TestOpenRefusesADeclaredSizeOverTheLimit(t *testing.T) {
	t.Parallel()
	// The head fits under the limit, so only the declared size can refuse
	// this before the file is opened and read.
	p := upload.Policy{MaxBytes: 1000, Types: []upload.Type{upload.PNG}}

	_, err := p.Open(multipartFile(t, "cat.png", "image/png", pad("\x89PNG\r\n\x1a\n", 5000)))
	require.ErrorIs(t, err, upload.ErrTooLarge)
}

func TestAPolicyMustBeBounded(t *testing.T) {
	t.Parallel()

	for name, p := range map[string]upload.Policy{
		"no size limit":              {Types: []upload.Type{upload.PNG}},
		"a negative size limit":      {MaxBytes: -1, Types: []upload.Type{upload.PNG}},
		"no types":                   {MaxBytes: 1 << 20},
		"SVG":                        {MaxBytes: 1 << 20, Types: []upload.Type{{Extension: ".svg", MIME: "image/svg+xml"}}},
		"HTML":                       {MaxBytes: 1 << 20, Types: []upload.Type{{Extension: ".html", MIME: "text/html; charset=utf-8"}}},
		"an extension without a dot": {MaxBytes: 1 << 20, Types: []upload.Type{{Extension: "png", MIME: "image/png"}}},
		"no MIME":                    {MaxBytes: 1 << 20, Types: []upload.Type{{Extension: ".png"}}},
	} {
		_, err := p.Check("cat.png", bytes.NewReader(pngBytes))
		require.Error(t, err, name)
		require.False(
			t,
			errors.Is(err, upload.ErrRefused) ||
				errors.Is(err, upload.ErrTooLarge),
			"%s: a bad policy is the server's fault, not the upload's",
			name,
		)
	}
}

// A display name is shown back to people and written into logs, so it is the
// last path element only, with nothing that rewrites how text around it reads.
func TestDisplayName(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		in     string
		maxLen int
		want   string
	}{
		{"cat.png", 255, "cat.png"},
		{"../../etc/passwd", 255, "passwd"},
		{`C:\Users\a\cat.png`, 255, "cat.png"},
		{"/", 255, "file"},
		{"..", 255, "file"},
		{"", 255, "file"},
		{"   ", 255, "file"},
		{" cat.png ", 255, "cat.png"},
		{"cat\u202egnp.exe", 255, "catgnp.exe"},
		{"cat\u2066.png", 255, "cat.png"},
		{"a\x00b\x1f.png", 255, "ab.png"},
		{"\xff.png", 255, "\ufffd.png"},
		{strings.Repeat("a", 300) + ".png", 20, strings.Repeat("a", 16) + ".png"},
		{strings.Repeat("é", 20) + ".png", 11, "ééé.png"},
		{"x." + strings.Repeat("b", 30), 10, "x.bbbbbbbb"},
	} {
		require.Equal(t, c.want, upload.DisplayName(c.in, c.maxLen), "%q", c.in)
	}
}

// ParseMultipartForm spools a body to disk before anyone looks at a file, so
// the body is capped before parsing - not the file checked after.
func TestLimitRequestCapsTheBodyBeforeParsing(t *testing.T) {
	t.Parallel()

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("file", "cat.png")
	require.NoError(t, err)
	_, err = part.Write(pad("\x89PNG\r\n\x1a\n", 5000))
	require.NoError(t, err)
	require.NoError(t, w.Close())

	r := httptest.NewRequest(http.MethodPost, "/upload", &body)
	r.Header.Set("Content-Type", w.FormDataContentType())
	upload.LimitRequest(httptest.NewRecorder(), r, 1000)

	var tooLarge *http.MaxBytesError
	require.ErrorAs(t, r.ParseMultipartForm(1<<20), &tooLarge)
}
