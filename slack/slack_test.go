package slack_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/slack"
	"gitlab.internal/tuan.quang.tran/aioz-common/slack/slacktest"
)

const (
	testToken   = "xoxb-test-token-do-not-log"
	testChannel = "C0123456789"
)

// reply is one scripted answer from the fake Slack API.
type reply struct {
	status     int    // 0 hangs up without answering
	body       string // the raw JSON body; empty sends an ok:true answer
	retryAfter string
}

// request is what the fake saw.
type request struct {
	auth        string
	contentType string
	path        string
	body        map[string]any
}

// fakeSlack plays POST /chat.postMessage from a script, repeating the last
// reply once the script runs out.
type fakeSlack struct {
	mu       sync.Mutex
	script   []reply
	requests []request
}

func newFake(t *testing.T, script ...reply) (*fakeSlack, *httptest.Server) {
	t.Helper()
	fake := &fakeSlack{script: script}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	return fake, server
}

func (f *fakeSlack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)

	f.mu.Lock()
	f.requests = append(f.requests, request{
		auth:        r.Header.Get("Authorization"),
		contentType: r.Header.Get("Content-Type"),
		path:        r.URL.Path,
		body:        body,
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

	body2 := next.body
	if body2 == "" {
		body2 = fmt.Sprintf(`{"ok":true,"channel":%q,"ts":"1700000000.000100"}`,
			testChannel)
	}
	_, _ = w.Write([]byte(body2))
}

func (f *fakeSlack) seen() []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]request(nil), f.requests...)
}

// ok builds an ok:false answer with Slack's error string.
func notOK(name string) string {
	return fmt.Sprintf(`{"ok":false,"error":%q}`, name)
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

func slackConfig(serverURL string) slack.SlackConfig {
	return slack.SlackConfig{
		BotToken:     testToken,
		MaxAttempts:  3,
		RetryWaitMin: time.Millisecond,
		RetryWaitMax: 2 * time.Millisecond,
		BaseURL:      serverURL,
	}
}

func newSlack(t *testing.T, serverURL string, logs *logBuffer) *slack.Slack {
	t.Helper()
	if logs == nil {
		logs = &logBuffer{}
	}
	notifier, err := slack.NewSlack(
		newLogger(logs), testChannel, slackConfig(serverURL),
	)
	require.NoError(t, err)
	return notifier
}

func message() slack.Message {
	return slack.Message{Text: "deploy finished"}
}

func requireSlackError(t *testing.T, err error) *slack.Error {
	t.Helper()
	var slackErr *slack.Error
	require.ErrorAs(t, err, &slackErr)
	return slackErr
}

func TestPostsTheMessage(t *testing.T) {
	t.Parallel()

	fake, server := newFake(t, reply{status: http.StatusOK})
	receipt, err := newSlack(t, server.URL, nil).Post(t.Context(), message())
	require.NoError(t, err)
	require.Equal(t, testChannel, receipt.Channel)
	require.Equal(t, "1700000000.000100", receipt.TS)

	seen := fake.seen()
	require.Len(t, seen, 1)
	require.Equal(t, "Bearer "+testToken, seen[0].auth)
	require.Equal(t, "application/json; charset=utf-8", seen[0].contentType)
	require.Equal(t, "/chat.postMessage", seen[0].path)
	require.Equal(t, testChannel, seen[0].body["channel"])
	require.Equal(t, "deploy finished", seen[0].body["text"])
}

// The whole reason this package exists: Slack refuses with 200, so a client
// reading the status reports a refused message as delivered.
func TestOKFalseAtHTTP200IsAFailure(t *testing.T) {
	t.Parallel()

	fake, server := newFake(t, reply{
		status: http.StatusOK, body: notOK("channel_not_found"),
	})

	_, err := newSlack(t, server.URL, nil).Post(t.Context(), message())

	slackErr := requireSlackError(t, err)
	require.Equal(t, slack.CodeChannel, slackErr.Code)
	require.Equal(t, "channel_not_found", slackErr.Name)
	require.Equal(t, http.StatusOK, slackErr.Status)
	require.False(t, slackErr.Temporary)
	require.Len(
		t,
		fake.seen(),
		1,
		"a refusal that will not change is not retried",
	)
}

func TestClassifiesSlackErrorNames(t *testing.T) {
	t.Parallel()

	for name, want := range map[string]struct {
		code      string
		temporary bool
	}{
		"invalid_auth":      {slack.CodeAuth, false},
		"token_revoked":     {slack.CodeAuth, false},
		"account_inactive":  {slack.CodeAuth, false},
		"missing_scope":     {slack.CodeAuth, false},
		"channel_not_found": {slack.CodeChannel, false},
		"not_in_channel":    {slack.CodeChannel, false},
		"is_archived":       {slack.CodeChannel, false},
		"msg_too_long":      {slack.CodeInvalid, false},
		"invalid_blocks":    {slack.CodeInvalid, false},
		"internal_error":    {slack.CodeUnavailable, true},
		"service_unavailable": {
			slack.CodeUnavailable, true,
		},
		"something_new": {slack.CodeRejected, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, server := newFake(t, reply{
				status: http.StatusOK, body: notOK(name),
			})
			_, err := newSlack(t, server.URL, nil).Post(t.Context(), message())

			slackErr := requireSlackError(t, err)
			require.Equal(t, want.code, slackErr.Code)
			require.Equal(t, want.temporary, slackErr.Temporary)
		})
	}
}

func TestRetriesARateLimitAndThenSucceeds(t *testing.T) {
	t.Parallel()

	fake, server := newFake(
		t,
		reply{status: http.StatusTooManyRequests, retryAfter: "0"},
		reply{status: http.StatusOK},
	)

	receipt, err := newSlack(t, server.URL, nil).Post(t.Context(), message())
	require.NoError(t, err)
	require.Equal(t, "1700000000.000100", receipt.TS)
	require.Len(t, fake.seen(), 2)
}

// Waiting as long as a remote asks is a way for whoever runs it to hold this
// service's goroutines.
func TestRefusesARetryAfterBeyondTheCap(t *testing.T) {
	t.Parallel()

	fake, server := newFake(t, reply{
		status: http.StatusTooManyRequests, retryAfter: "600",
	})

	_, err := newSlack(t, server.URL, nil).Post(t.Context(), message())

	slackErr := requireSlackError(t, err)
	require.Equal(t, slack.CodeRateLimited, slackErr.Code)
	require.Equal(t, 10*time.Minute, slackErr.RetryAfter)
	require.Len(t, fake.seen(), 1, "the wait was refused, not slept")
}

func TestRetriesAServerFault(t *testing.T) {
	t.Parallel()

	fake, server := newFake(
		t,
		reply{status: http.StatusBadGateway},
		reply{status: http.StatusOK},
	)

	_, err := newSlack(t, server.URL, nil).Post(t.Context(), message())
	require.NoError(t, err)
	require.Len(t, fake.seen(), 2)
}

func TestRetriesADroppedConnection(t *testing.T) {
	t.Parallel()

	fake, server := newFake(
		t,
		reply{status: 0},
		reply{status: http.StatusOK},
	)

	_, err := newSlack(t, server.URL, nil).Post(t.Context(), message())
	require.NoError(t, err)
	require.Len(t, fake.seen(), 2)
}

func TestGivesUpAfterTheAttemptBudget(t *testing.T) {
	t.Parallel()

	fake, server := newFake(t, reply{status: http.StatusServiceUnavailable})

	_, err := newSlack(t, server.URL, nil).Post(t.Context(), message())

	slackErr := requireSlackError(t, err)
	require.Equal(t, slack.CodeUnavailable, slackErr.Code)
	require.True(t, slackErr.Temporary)
	require.Len(t, fake.seen(), 3, "MaxAttempts is 3")
}

// A token in a log line or an error string is a token that has leaked.
func TestTheTokenIsNeverLogged(t *testing.T) {
	t.Parallel()

	logs := &logBuffer{}
	_, server := newFake(t, reply{status: http.StatusServiceUnavailable})

	_, err := newSlack(t, server.URL, logs).Post(t.Context(), message())
	require.Error(t, err)

	require.NotContains(t, logs.String(), testToken)
	require.NotContains(t, err.Error(), testToken)
}

// Nothing a message carries may reach a log line.
func TestTheMessageTextIsNeverLogged(t *testing.T) {
	t.Parallel()

	logs := &logBuffer{}
	_, server := newFake(t, reply{status: http.StatusServiceUnavailable})

	msg := message()
	msg.Text = "database password rotated to hunter2"
	_, err := newSlack(t, server.URL, logs).Post(t.Context(), msg)
	require.Error(t, err)

	require.NotContains(t, logs.String(), "hunter2")
}

func TestSendsBlocksAndThreads(t *testing.T) {
	t.Parallel()

	fake, server := newFake(t, reply{status: http.StatusOK})

	msg := message()
	msg.Blocks = json.RawMessage(
		`[{"type":"section","text":{"type":"mrkdwn","text":"hi"}}]`,
	)
	msg.ThreadTS = "1699999999.000100"
	msg.ReplyBroadcast = true

	_, err := newSlack(t, server.URL, nil).Post(t.Context(), msg)
	require.NoError(t, err)

	body := fake.seen()[0].body
	require.Equal(t, "1699999999.000100", body["thread_ts"])
	require.Equal(t, true, body["reply_broadcast"])
	require.NotNil(t, body["blocks"])
}

// A field the caller did not set must not reach Slack, so Slack applies its
// own default rather than this package's zero value.
func TestLeavesUnsetFieldsOut(t *testing.T) {
	t.Parallel()

	fake, server := newFake(t, reply{status: http.StatusOK})
	_, err := newSlack(t, server.URL, nil).Post(t.Context(), message())
	require.NoError(t, err)

	body := fake.seen()[0].body
	require.NotContains(t, body, "thread_ts")
	require.NotContains(t, body, "reply_broadcast")
	require.NotContains(t, body, "blocks")
	require.NotContains(t, body, "username")
	require.NotContains(t, body, "icon_emoji")
}

func TestUsesTheDefaultChannel(t *testing.T) {
	t.Parallel()

	fake, server := newFake(t, reply{status: http.StatusOK})
	_, err := newSlack(t, server.URL, nil).Post(t.Context(), message())
	require.NoError(t, err)
	require.Equal(t, testChannel, fake.seen()[0].body["channel"])

	// An explicit channel wins over the default.
	msg := message()
	msg.Channel = "C9999999999"
	_, err = newSlack(t, server.URL, nil).Post(t.Context(), msg)
	require.NoError(t, err)
	require.Equal(t, "C9999999999", fake.seen()[1].body["channel"])
}

// A message that cannot be posted costs no request.
func TestValidatesBeforeSending(t *testing.T) {
	t.Parallel()

	tooManyBlocks, err := json.Marshal(
		make([]map[string]string, slack.MaxBlocks+1),
	)
	require.NoError(t, err)

	for name, msg := range map[string]slack.Message{
		"no text":       {Channel: testChannel},
		"text too long": {Channel: testChannel, Text: strings.Repeat("x", slack.MaxTextLen+1)},
		"blocks are not an array": {
			Channel: testChannel, Text: "hi",
			Blocks: json.RawMessage(`{"type":"section"}`),
		},
		"too many blocks": {
			Channel: testChannel, Text: "hi", Blocks: tooManyBlocks,
		},
		"broadcast without a thread": {
			Channel: testChannel, Text: "hi", ReplyBroadcast: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			fake, server := newFake(t, reply{status: http.StatusOK})
			_, err := newSlack(t, server.URL, nil).Post(t.Context(), msg)

			require.Equal(t, slack.CodeInvalid, requireSlackError(t, err).Code)
			require.Empty(t, fake.seen())
		})
	}
}

// With no default channel and none on the message, there is nowhere to post.
func TestRefusesAMessageWithNoChannel(t *testing.T) {
	t.Parallel()

	_, server := newFake(t, reply{status: http.StatusOK})
	notifier, err := slack.NewSlack(
		newLogger(&logBuffer{}), "", slackConfig(server.URL),
	)
	require.NoError(t, err)

	_, err = notifier.Post(t.Context(), message())
	require.Equal(t, slack.CodeInvalid, requireSlackError(t, err).Code)
}

func TestNewSlackNeedsAToken(t *testing.T) {
	t.Parallel()

	_, err := slack.NewSlack(
		newLogger(&logBuffer{}), testChannel, slack.SlackConfig{},
	)
	require.ErrorContains(t, err, "bot token")
}

func TestConfigValidate(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		config  slack.Config
		wantErr string
	}{
		"log provider needs nothing": {
			config: slack.Config{Provider: slack.ProviderLog},
		},
		"slack provider is complete": {
			config: slack.Config{
				Provider: slack.ProviderSlack,
				Slack:    slack.SlackConfig{BotToken: testToken},
			},
		},
		"slack provider without a token": {
			config:  slack.Config{Provider: slack.ProviderSlack},
			wantErr: "bot token",
		},
		"an unknown provider": {
			config:  slack.Config{Provider: "teams"},
			wantErr: `provider "teams"`,
		},
		"an empty provider is not inferred": {
			config:  slack.Config{},
			wantErr: "provider",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := tc.config.Validate()
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestLogProviderPostsNothing(t *testing.T) {
	t.Parallel()

	logs := &logBuffer{}
	notifier, err := slack.New(newLogger(logs), slack.Config{
		Provider:       slack.ProviderLog,
		DefaultChannel: testChannel,
	})
	require.NoError(t, err)

	msg := message()
	msg.Text = "the queue is backing up: hunter2"
	receipt, err := notifier.Post(t.Context(), msg)
	require.NoError(t, err)
	require.Equal(t, testChannel, receipt.Channel)
	require.True(t, strings.HasPrefix(receipt.TS, "log-"))

	require.Contains(t, logs.String(), testChannel)
	require.NotContains(t, logs.String(), "hunter2")
}

func TestLogProviderStillValidates(t *testing.T) {
	t.Parallel()

	notifier, err := slack.New(
		newLogger(&logBuffer{}), slack.Config{Provider: slack.ProviderLog},
	)
	require.NoError(t, err)

	_, err = notifier.Post(t.Context(), slack.Message{Text: "hi"})
	require.Equal(t, slack.CodeInvalid, requireSlackError(t, err).Code)
}

func TestRecorderRecords(t *testing.T) {
	t.Parallel()

	rec := &slacktest.Recorder{}

	_, err := rec.Post(t.Context(), slack.Message{
		Channel: testChannel, Text: "one",
	})
	require.NoError(t, err)
	_, err = rec.Post(t.Context(), slack.Message{
		Channel: "C9999999999", Text: "two",
	})
	require.NoError(t, err)

	require.Len(t, rec.Posted(), 2)
	require.Len(t, rec.PostedTo(testChannel), 1)
	require.Equal(t, "one", rec.PostedTo(testChannel)[0].Text)
}

func TestRecorderFailHook(t *testing.T) {
	t.Parallel()

	rec := &slacktest.Recorder{
		Fail: func(msg slack.Message) error {
			if msg.Channel == "C9999999999" {
				return &slack.Error{Code: slack.CodeChannel}
			}
			return nil
		},
	}

	_, err := rec.Post(t.Context(), slack.Message{
		Channel: "C9999999999", Text: "nope",
	})
	require.Equal(t, slack.CodeChannel, requireSlackError(t, err).Code)
	require.Empty(t, rec.Posted())
}
