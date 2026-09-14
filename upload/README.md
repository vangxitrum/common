# upload

Checks a file a client sends before a service keeps it, as OWASP's File Upload
cheat sheet asks.

```go
import "gitlab.internal/tuan.quang.tran/aioz-common/upload"
```

Our upload endpoints today take the client's `Content-Type` as the file's
type, check the size after the whole body has been parsed to disk, and use the
client's filename. Each is a claim the uploader controls: a PHP script named
`cat.png` sent as `image/png` passes all three.

## Usage

```go
var avatars = upload.Policy{
    MaxBytes: 5 << 20,
    Types:    []upload.Type{upload.PNG, upload.JPEG, upload.JPG, upload.WebP},
}

func (h *Handler) Avatar(c *gin.Context) {
    upload.LimitRequest(c.Writer, c.Request, 6<<20) // before FormFile parses anything

    fh, err := c.FormFile("avatar")
    var tooLarge *http.MaxBytesError
    if errors.As(err, &tooLarge) {
        c.AbortWithStatus(http.StatusRequestEntityTooLarge)
        return
    }
    // ... other err: 400

    f, err := avatars.Open(fh)
    switch {
    case errors.Is(err, upload.ErrTooLarge):
        c.AbortWithStatus(http.StatusRequestEntityTooLarge)
        return
    case errors.Is(err, upload.ErrRefused):
        c.AbortWithStatus(http.StatusUnsupportedMediaType)
        return
    case err != nil:
        c.AbortWithStatus(http.StatusInternalServerError) // a bad Policy
        return
    }
    defer f.Close()

    key := uuid.New().String() // never the client's filename
    if err := h.store.Put(c, key, f.Type.MIME, f); err != nil { // fails past MaxBytes
        // ...
    }
    // f.Name is safe to show back and to log
}
```

echo is the same with `c.Request()`, `c.Response()` and `c.FormFile`.

## What a Policy checks

- **The extension and the content must name the same allowed `Type`.** The
  extension is the last path element's, lowercased, after the trailing dots
  and spaces Windows drops (`cat.php.` is PHP there). The content is sniffed
  with `http.DetectContentType` on the first 512 bytes. `shell.php.jpg`
  holding PHP fails on content; `cat.jpg.php` fails on extension.
- **The client's `Content-Type` is never read.**
- **Empty files are refused.**
- **The size is enforced while reading.** A read past `MaxBytes` fails with
  `ErrTooLarge`; `io.LimitReader` would stop quietly and a truncated file
  would be stored as a whole one. `Open` also refuses a declared size over
  the limit before opening the file.
- **A Policy must be bounded**: `MaxBytes` and at least one `Type` are
  required, and no `Type` may be active content (SVG, HTML, XML,
  JavaScript), which a browser runs when it is served from our origin. That
  is a server error, not `ErrRefused`.

`LimitRequest` is `http.MaxBytesReader` on the request body. Call it before
anything parses the body: `ParseMultipartForm` and `FormFile` spool the whole
body to disk first, so a check on the file alone comes after the disk is
full. Leave room above `MaxBytes` for the form's own framing.

## Names

`File.Name` is `DisplayName` of the client's filename: the last path element,
valid UTF-8, without control or bidirectional-override characters (which make
`cat<U+202E>gnp.exe` display as `catexe.png`), trimmed, at most `MaxNameLen`
bytes cut on a character boundary with the extension kept, and `file` when
nothing is left. It is safe to show and to log. It is still the client's text:
**store under a random id**, never under this name.

## What this package does not do

- **Storage.** Key files by a random id. On a filesystem, open them with
  `os.OpenInRoot` so no name can climb out of the directory, and serve them
  from a different origin than the application, with
  `Content-Disposition: attachment` and `X-Content-Type-Options: nosniff`.
- **Image re-encoding, malware scanning, archive unpacking.** Sniffing proves
  the file starts like a PNG, not that the rest is only a PNG. Where that
  matters, decode and re-encode the image.

## API

```go
type Type struct{ Extension, MIME string }

var PNG, JPEG, JPG, GIF, WebP Type

type Policy struct {
    MaxBytes   int64
    Types      []Type
    MaxNameLen int // 0 is 255
}

func (p Policy) Open(fh *multipart.FileHeader) (*File, error)
func (p Policy) Check(filename string, r io.Reader) (*File, error)

type File struct {
    Type Type
    Name string
    // contains filtered or unexported fields
}
func (f *File) Read(p []byte) (int, error)
func (f *File) Close() error

func DisplayName(name string, maxLen int) string
func LimitRequest(w http.ResponseWriter, r *http.Request, max int64)

var ErrTooLarge, ErrRefused error
var Error = errs.Tag("upload")
```
