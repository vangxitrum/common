package mail_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/mail"
	"gitlab.internal/tuan.quang.tran/aioz-common/stats/testcontext"
	"gitlab.internal/tuan.quang.tran/aioz-common/time2"
)

const (
	testKey  = "re_test_key_do_not_log"
	testFrom = "AIOZ <no-reply@aioz.io>"
)

// reply is one scripted answer from the fake Resend API.
type reply struct {
	status     int    // 0 hangs up without answering
	name       string // the error name Resend puts in the body
	retryAfter string
	id         string
}

// request is what the fake saw.
type request struct {
	auth string
	key  string
	body map[string]any
}

// fakeResend plays POST /emails from a script, repeating the last reply.
type fakeResend struct {
	mu       sync.Mutex
	script   []reply
	requests []request
}

func newFake(t *testing.T, script ...reply) (*fakeResend, *httptest.Server) {
	t.Helper()
	fake := &fakeResend{script: script}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	return fake, server
}

func (f *fakeResend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)

	f.mu.Lock()
	f.requests = append(f.requests, request{
		auth: r.Header.Get("Authorization"),
		key:  r.Header.Get("Idempotency-Key"),
		body: body,
	})
	next := f.script[min(len(f.requests), len(f.script))-1]
	f.mu.Unlock()

	if next.status == 0 {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if next.retryAfter != "" {
		w.Header().Set("Retry-After", next.retryAfter)
	}
	w.WriteHeader(next.status)
	if next.status < 300 {
		_, _ = fmt.Fprintf(w, `{"id":%q}`, next.id)
		return
	}
	_, _ = fmt.Fprintf(
		w,
		`{"statusCode":%d,"name":%q,"message":"scripted failure"}`,
		next.status,
		next.name,
	)
}

func (f *fakeResend) seen() []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]request(nil), f.requests...)
}

// logBuffer is a slog destination a test can search after the fact.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newLogger(out *logBuffer) *slog.Logger {
	return slog.New(
		slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelDebug}),
	)
}

func resendConfig(serverURL string) mail.ResendConfig {
	return mail.ResendConfig{
		APIKey:       testKey,
		MaxAttempts:  3,
		RetryWaitMin: time.Millisecond,
		RetryWaitMax: 2 * time.Millisecond,
		BaseURL:      serverURL,
	}
}

func newResend(t *testing.T, serverURL string, logs *logBuffer) *mail.Resend {
	t.Helper()
	if logs == nil {
		logs = &logBuffer{}
	}
	sender, err := mail.NewResend(
		newLogger(logs),
		testFrom,
		resendConfig(serverURL),
	)
	require.NoError(t, err)
	return sender
}

func message() mail.Message {
	return mail.Message{
		To:      []string{"user@example.com"},
		Subject: "Your code is 123456",
		HTML:    "<p>Your code is 123456</p>",
	}
}

func requireMailError(t *testing.T, err error) *mail.Error {
	t.Helper()
	var mailErr *mail.Error
	require.ErrorAs(t, err, &mailErr)
	return mailErr
}

func TestResendSendsTheMessage(t *testing.T) {
	t.Parallel()

	fake, server := newFake(t, reply{status: http.StatusOK, id: "email-1"})
	receipt, err := newResend(t, server.URL, nil).Send(t.Context(), message())
	require.NoError(t, err)
	require.Equal(t, "email-1", receipt.ID)

	seen := fake.seen()
	require.Len(t, seen, 1)
	require.Equal(t, "Bearer "+testKey, seen[0].auth)
	require.Len(
		t,
		seen[0].key,
		32,
		"every send carries a generated idempotency key",
	)
	require.Equal(
		t,
		testFrom,
		seen[0].body["from"],
		"an empty From uses the configured sender",
	)
	require.Equal(t, []any{"user@example.com"}, seen[0].body["to"])
	require.Equal(t, "Your code is 123456", seen[0].body["subject"])
	require.Equal(t, "<p>Your code is 123456</p>", seen[0].body["html"])
}

// A retried send carries the same key, so Resend treats it as the same email
// rather than sending a second one.
func TestResendRetriesARateLimitWithTheSameKey(t *testing.T) {
	t.Parallel()

	fake, server := newFake(
		t,
		reply{
			status:     http.StatusTooManyRequests,
			name:       "rate_limit_exceeded",
			retryAfter: "0",
		},
		reply{status: http.StatusOK, id: "email-2"},
	)
	receipt, err := newResend(t, server.URL, nil).Send(t.Context(), message())
	require.NoError(t, err)
	require.Equal(t, "email-2", receipt.ID)

	seen := fake.seen()
	require.Len(t, seen, 2)
	require.NotEmpty(t, seen[0].key)
	require.Equal(t, seen[0].key, seen[1].key)
}

func TestResendSendsTheCallersKey(t *testing.T) {
	t.Parallel()

	fake, server := newFake(t, reply{status: http.StatusOK, id: "email-3"})
	msg := message()
	msg.IdempotencyKey = "receipt:tx-42"

	_, err := newResend(t, server.URL, nil).Send(t.Context(), msg)
	require.NoError(t, err)
	require.Equal(t, "receipt:tx-42", fake.seen()[0].key)
}

// Resend names the failure in the body, and only the name separates a
// rate limit worth waiting out from a quota that no retry will fix.
func TestResendClassifiesFailures(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		reply     reply
		code      string
		temporary bool
		attempts  int
	}{
		{"daily quota", reply{status: 429, name: "daily_quota_exceeded"}, mail.CodeQuota, false, 1},
		{"monthly quota", reply{status: 429, name: "monthly_quota_exceeded"}, mail.CodeQuota, false, 1},
		{"key reused for another body", reply{status: 409, name: "invalid_idempotent_request"}, mail.CodeConflict, false, 1},
		{"validation", reply{status: 400, name: "validation_error"}, mail.CodeInvalid, false, 1},
		{"missing field", reply{status: 422, name: "missing_required_field"}, mail.CodeInvalid, false, 1},
		{"bad key", reply{status: 401, name: "missing_api_key"}, mail.CodeAuth, false, 1},
		{"unverified domain", reply{status: 403, name: "validation_error"}, mail.CodeRejected, false, 1},
		{"the same key in flight", reply{status: 409, name: "concurrent_idempotent_requests"}, mail.CodeConflict, true, 3},
		{"application error", reply{status: 500, name: "application_error"}, mail.CodeUnavailable, true, 3},
		{"unavailable", reply{status: 503, name: "service_unavailable"}, mail.CodeUnavailable, true, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake, server := newFake(t, tc.reply)
			_, err := newResend(t, server.URL, nil).Send(t.Context(), message())

			mailErr := requireMailError(t, err)
			require.Equal(t, tc.code, mailErr.Code)
			require.Equal(t, tc.reply.status, mailErr.Status)
			require.Equal(t, tc.temporary, mailErr.Temporary)
			require.Len(t, fake.seen(), tc.attempts)
		})
	}
}

func TestResendRecoversFromATemporaryFailure(t *testing.T) {
	t.Parallel()

	fake, server := newFake(
		t,
		reply{
			status: http.StatusServiceUnavailable,
			name:   "service_unavailable",
		},
		reply{status: http.StatusOK, id: "email-4"},
	)
	receipt, err := newResend(t, server.URL, nil).Send(t.Context(), message())
	require.NoError(t, err)
	require.Equal(t, "email-4", receipt.ID)
	require.Len(t, fake.seen(), 2)
}

// A dropped connection is the case retries exist for, and the key makes it
// safe: the first attempt may have been accepted before the line went down.
func TestResendRetriesAConnectionThatDropped(t *testing.T) {
	t.Parallel()

	fake, server := newFake(
		t,
		reply{status: 0},
		reply{status: http.StatusOK, id: "email-5"},
	)
	receipt, err := newResend(t, server.URL, nil).Send(t.Context(), message())
	require.NoError(t, err)
	require.Equal(t, "email-5", receipt.ID)

	seen := fake.seen()
	require.Len(t, seen, 2)
	require.Equal(t, seen[0].key, seen[1].key)
}

func TestResendReportsATransportFailure(t *testing.T) {
	t.Parallel()

	_, server := newFake(t, reply{status: 0})
	_, err := newResend(t, server.URL, nil).Send(t.Context(), message())

	mailErr := requireMailError(t, err)
	require.Equal(t, mail.CodeTransport, mailErr.Code)
	require.True(t, mailErr.Temporary)
}

func TestResendWaitsAsLongAsRetryAfterSays(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	fake, server := newFake(
		t,
		reply{
			status:     http.StatusTooManyRequests,
			name:       "rate_limit_exceeded",
			retryAfter: "2",
		},
		reply{status: http.StatusOK, id: "email-6"},
	)
	sender := newResend(t, server.URL, nil)
	clock, machine := time2.WithNewMachine(ctx)

	ctx.Go(func() error {
		_, err := sender.Send(clock, message())
		return err
	})

	require.True(t, machine.BlockThenAdvance(ctx, 1, 2*time.Second))
	ctx.Wait()
	require.Len(t, fake.seen(), 2)
}

// Waiting as long as a remote asks is a way for whoever runs it to hold this
// service's goroutines. Past 30 seconds the send gives up and says why.
func TestResendStopsOnALongRetryAfter(t *testing.T) {
	t.Parallel()

	fake, server := newFake(
		t,
		reply{
			status:     http.StatusTooManyRequests,
			name:       "rate_limit_exceeded",
			retryAfter: "3600",
		},
		reply{status: http.StatusOK, id: "never"},
	)
	_, err := newResend(t, server.URL, nil).Send(t.Context(), message())

	mailErr := requireMailError(t, err)
	require.Equal(t, mail.CodeRateLimited, mailErr.Code)
	require.Equal(t, time.Hour, mailErr.RetryAfter)
	require.True(t, mailErr.Temporary)
	require.Len(t, fake.seen(), 1)
}

func TestResendStopsWhenTheContextIsCancelled(t *testing.T) {
	t.Parallel()

	fake, server := newFake(t, reply{status: http.StatusOK, id: "never"})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := newResend(t, server.URL, nil).Send(ctx, message())
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, fake.seen())
}

// A message that cannot be sent is refused before it costs a request.
func TestAnInvalidMessageMakesNoRequest(t *testing.T) {
	t.Parallel()

	many := make([]string, 51)
	for i := range many {
		many[i] = fmt.Sprintf("user%d@example.com", i)
	}

	for name, mutate := range map[string]func(*mail.Message){
		"no recipient":        func(m *mail.Message) { m.To = nil },
		"a malformed address": func(m *mail.Message) { m.To = []string{"not an address"} },
		"a malformed cc":      func(m *mail.Message) { m.Cc = []string{"@"} },
		"no subject":          func(m *mail.Message) { m.Subject = "" },
		"no body":             func(m *mail.Message) { m.HTML, m.Text = "", "" },
		"too many recipients": func(m *mail.Message) { m.To = many },
		"a malformed sender":  func(m *mail.Message) { m.From = "nobody" },
		"an overlong key":     func(m *mail.Message) { m.IdempotencyKey = strings.Repeat("k", 257) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			fake, server := newFake(
				t,
				reply{status: http.StatusOK, id: "never"},
			)
			msg := message()
			mutate(&msg)

			_, err := newResend(t, server.URL, nil).Send(t.Context(), msg)
			require.Equal(t, mail.CodeInvalid, requireMailError(t, err).Code)
			require.Empty(t, fake.seen())
		})
	}
}

// The API key, the code in the body and the recipient's address never reach a
// log line or an error string, including on the retry path that logs most.
func TestResendKeepsSecretsOutOfLogsAndErrors(t *testing.T) {
	t.Parallel()

	logs := &logBuffer{}
	_, server := newFake(
		t,
		reply{
			status: http.StatusServiceUnavailable,
			name:   "service_unavailable",
		},
		reply{status: http.StatusBadRequest, name: "validation_error"},
	)
	_, err := newResend(t, server.URL, logs).Send(t.Context(), message())
	require.Error(t, err)

	for _, leak := range []string{testKey, "123456", "user@example.com"} {
		require.NotContains(t, logs.String(), leak)
		require.NotContains(t, err.Error(), leak)
	}
	require.Contains(
		t,
		logs.String(),
		"retrying a mail send",
		"the retry was logged",
	)
}

func TestNewResendRefusesAMissingKeyOrSender(t *testing.T) {
	t.Parallel()

	logger := newLogger(&logBuffer{})

	config := resendConfig("http://127.0.0.1:1")
	config.APIKey = ""
	_, err := mail.NewResend(logger, testFrom, config)
	require.Error(t, err)

	_, err = mail.NewResend(logger, "", resendConfig("http://127.0.0.1:1"))
	require.Error(t, err)

	_, err = mail.NewResend(
		logger,
		"not an address",
		resendConfig("http://127.0.0.1:1"),
	)
	require.Error(t, err)
}

func TestErrorMessageNamesTheCode(t *testing.T) {
	t.Parallel()

	err := &mail.Error{Code: mail.CodeQuota, Status: 429}
	require.Contains(t, err.Error(), "quota")
	require.True(t, errors.As(fmt.Errorf("wrapped: %w", err), new(*mail.Error)))
}
