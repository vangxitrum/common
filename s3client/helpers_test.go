package s3client_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/memory"
	"gitlab.internal/tuan.quang.tran/aioz-common/s3client"
)

const (
	testBucket    = "test-bucket"
	testAccessKey = "AKIDTESTTESTTESTTEST"
	testSecretKey = "s3client-test-secret-key-never-logged"
)

// recorder is one request the scripted server saw, captured before the handler
// answered it.
type recorded struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   []byte
}

// server is a scripted S3 service. Every request it sees is checked against
// the compatibility assertions, so a change that reintroduces a header the
// service rejects fails every test rather than one.
type server struct {
	t *testing.T

	mu       sync.Mutex
	requests []recorded

	// handle answers one request. It is given the recorded request so a
	// handler can branch on the query without re-reading the body.
	handle func(w http.ResponseWriter, r *http.Request, got recorded)

	// skipCompat turns off the per-request compatibility assertions, for the
	// test that checks the aws profile does the opposite.
	skipCompat bool

	httptest *httptest.Server
}

func newServer(
	t *testing.T,
	handle func(w http.ResponseWriter, r *http.Request, got recorded),
) *server {
	t.Helper()

	s := &server{t: t, handle: handle}
	s.httptest = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.httptest.Close)
	return s
}

func (s *server) serve(w http.ResponseWriter, r *http.Request) {
	body, err := readAll(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	got := recorded{
		Method: r.Method,
		Path:   r.URL.Path,
		Query:  r.URL.RawQuery,
		Header: r.Header.Clone(),
		Body:   body,
	}

	s.mu.Lock()
	s.requests = append(s.requests, got)
	skip := s.skipCompat
	s.mu.Unlock()

	if !skip {
		assertNoFlexibleChecksum(s.t, got)
		assertPathStyle(s.t, got)
	}
	s.handle(w, r, got)
}

// seen returns every request the server has answered.
func (s *server) seen() []recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recorded(nil), s.requests...)
}

// count returns how many requests the server has answered.
func (s *server) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

// config returns a Config pointed at this server, with the bucket check off
// unless a test asks for it.
func (s *server) config() s3client.Config {
	return s3client.Config{
		Compat:          s3client.CompatAIOZ,
		Endpoint:        s.httptest.URL,
		Region:          "us-east-1",
		Bucket:          testBucket,
		AccessKeyID:     testAccessKey,
		SecretAccessKey: testSecretKey,
		PartSize:        5 * memory.MiB,
		Concurrency:     2,
	}
}

// client returns a client pointed at this server.
func (s *server) client(t *testing.T) *s3client.Client {
	t.Helper()
	c, err := s3client.New(discard(), s.config())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, c.Close()) })
	return c
}

// clientWith returns a client pointed at this server with config changed.
func (s *server) clientWith(
	t *testing.T,
	change func(*s3client.Config),
) (*s3client.Client, error) {
	t.Helper()
	config := s.config()
	change(&config)
	return s3client.New(discard(), config)
}

// assertNoFlexibleChecksum is the assertion the whole package exists for: our
// service rejects the checksum headers the SDK now sends by default.
func assertNoFlexibleChecksum(t *testing.T, got recorded) {
	t.Helper()

	for header := range got.Header {
		lower := strings.ToLower(header)
		if strings.HasPrefix(lower, "x-amz-checksum-") {
			t.Errorf("%s %s sent a flexible checksum header %q",
				got.Method, got.Path, header)
		}
	}
	if v := got.Header.Get("X-Amz-Sdk-Checksum-Algorithm"); v != "" {
		t.Errorf("%s %s sent X-Amz-Sdk-Checksum-Algorithm %q",
			got.Method, got.Path, v)
	}
	if v := got.Header.Get("X-Amz-Content-Sha256"); strings.Contains(v, "STREAMING") {
		t.Errorf("%s %s sent an aws-chunked body: X-Amz-Content-Sha256 %q",
			got.Method, got.Path, v)
	}
}

// assertPathStyle checks the bucket is in the path, not the host: our endpoint
// has no per-bucket DNS.
func assertPathStyle(t *testing.T, got recorded) {
	t.Helper()

	if !strings.HasPrefix(got.Path, "/"+testBucket) {
		t.Errorf("%s %s is not path style: the path does not start with /%s",
			got.Method, got.Path, testBucket)
	}
}

// discard returns a logger that writes nothing.
func discard() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// intoBuffer returns a logger writing to buf, for the test that checks no
// secret reaches the log.
func intoBuffer(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
}

func readAll(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer func() { _ = r.Body.Close() }()

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r.Body); err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return buf.Bytes(), nil
}

// errorXML writes an S3 error document.
func errorXML(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w,
		`<?xml version="1.0" encoding="UTF-8"?>`+
			`<Error><Code>%s</Code><Message>%s</Message></Error>`,
		code, message)
}
