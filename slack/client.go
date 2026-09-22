package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/spacemonkeygo/monkit/v3"

	"gitlab.internal/tuan.quang.tran/aioz-common/httpclient"
	"gitlab.internal/tuan.quang.tran/aioz-common/retry"
	"gitlab.internal/tuan.quang.tran/aioz-common/secret"
)

const (
	defaultBaseURL      = "https://slack.com/api"
	defaultMaxAttempts  = 4
	defaultRetryWaitMin = 500 * time.Millisecond
	defaultRetryWaitMax = 10 * time.Second
	defaultTimeout      = 30 * time.Second

	// maxResponseBody bounds how much of a response is read. Slack's
	// chat.postMessage answers are a few kilobytes at most.
	maxResponseBody = 1 << 20
)

// Slack posts messages through the Slack Web API.
//
// There is no vendor SDK here on purpose. slack-go carries socket mode, the
// events API and a websocket client, none of which this needs; chat.postMessage
// is one POST with a JSON body and a bearer token.
type Slack struct {
	log            *slog.Logger
	defaultChannel string
	token          secret.Token
	baseURL        string
	client         *http.Client
	backoff        *retry.Backoff
}

// NewSlack returns a Slack provider that posts to defaultChannel when a
// message names no channel.
func NewSlack(
	log *slog.Logger,
	defaultChannel string,
	config SlackConfig,
) (*Slack, error) {
	if log == nil {
		return nil, fmt.Errorf("slack: logger is nil")
	}
	token := secret.Token(strings.TrimSpace(config.BotToken))
	if token == "" {
		return nil, fmt.Errorf("slack: the slack provider needs a bot token")
	}

	clientConfig := config.Client
	if clientConfig.Name == "" {
		clientConfig.Name = "slack"
	}
	if clientConfig.Timeout == 0 {
		// A config built by hand rather than bound from flags has no backstop;
		// a post must not be able to hang for ever on a silent connection.
		clientConfig.Timeout = defaultTimeout
	}
	client, err := httpclient.New(log, clientConfig)
	if err != nil {
		return nil, err
	}

	baseURL := defaultBaseURL
	if config.BaseURL != "" {
		baseURL = strings.TrimSuffix(config.BaseURL, "/")
	}

	return &Slack{
		log:            log,
		defaultChannel: defaultChannel,
		token:          token,
		baseURL:        baseURL,
		client:         client,
		backoff: retry.New(retry.Config{
			Name:         "slack",
			MaxAttempts:  orDefault(config.MaxAttempts, defaultMaxAttempts),
			InitialDelay: orDefault(config.RetryWaitMin, defaultRetryWaitMin),
			MaxDelay:     orDefault(config.RetryWaitMax, defaultRetryWaitMax),
			Multiplier:   2,
			Jitter:       true,
		}),
	}, nil
}

func orDefault[T int | time.Duration](v, fallback T) T {
	if v <= 0 {
		return fallback
	}
	return v
}

// Post posts msg, retrying what can pass.
//
// There is no idempotency key: Slack offers none for chat.postMessage, so a
// retry after a response that was sent but not received can duplicate a
// message. Only failures that are known not to have posted are retried, which
// is why a 2xx that could not be read is not one of them.
func (s *Slack) Post(ctx context.Context, msg Message) (_ Receipt, err error) {
	defer mon.Task()(&ctx)(&err)

	if msg.Channel == "" {
		msg.Channel = s.defaultChannel
	}
	if err := msg.validate(); err != nil {
		return Receipt{}, err
	}

	body, err := json.Marshal(payloadOf(msg))
	if err != nil {
		return Receipt{}, invalid("the message could not be encoded")
	}

	loop := s.backoff.Loop()
	var last *Error

	for loop.Wait(ctx) {
		receipt, postErr := s.post(ctx, body)
		if postErr == nil {
			return receipt, nil
		}
		if ctx.Err() != nil {
			return Receipt{}, fmt.Errorf("slack: %w", ctx.Err())
		}

		last = postErr
		if !last.Temporary {
			return Receipt{}, failed(last)
		}
		if last.RetryAfter > httpclient.MaxRetryAfter {
			// Waiting as long as a remote asks is a way for whoever runs it to
			// hold this service's goroutines; the caller decides instead.
			return Receipt{}, failed(last)
		}
		if last.RetryAfter > 0 {
			loop.Override(last.RetryAfter)
		}

		s.log.WarnContext(ctx, "retrying a slack post",
			"code", last.Code, "status", last.Status, "slack_error", last.Name,
			"attempt", loop.Attempt()+2)
	}

	if ctx.Err() != nil {
		return Receipt{}, fmt.Errorf("slack: %w", ctx.Err())
	}
	if last == nil {
		return Receipt{}, fmt.Errorf("slack: %w", loop.Err())
	}
	return Receipt{}, failed(last)
}

// post makes one attempt.
func (s *Slack) post(ctx context.Context, body []byte) (Receipt, *Error) {
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, s.baseURL+"/chat.postMessage",
		bytes.NewReader(body),
	)
	if err != nil {
		return Receipt{}, invalid("the request could not be built")
	}
	req.Header.Set("Authorization", "Bearer "+s.token.Reveal())
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := s.client.Do(req)
	if err != nil {
		return Receipt{}, &Error{
			Code: CodeTransport, Temporary: true,
			detail: err.Error(), cause: err,
		}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return Receipt{}, &Error{
			Code: CodeTransport, Status: resp.StatusCode,
			Temporary: true, detail: "the response could not be read",
			cause: err,
		}
	}

	retryAfter, _ := httpclient.ParseRetryAfter(resp)

	// 429 carries no useful body, and every other non-2xx is Slack's edge
	// rather than the API, so the status decides those.
	if resp.StatusCode != http.StatusOK {
		return Receipt{}, statusError(resp.StatusCode, retryAfter)
	}

	var answer struct {
		OK      bool   `json:"ok"`
		Error   string `json:"error"`
		Channel string `json:"channel"`
		TS      string `json:"ts"`
		Warning string `json:"warning"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		// A 200 that is not the JSON documented. It may well have posted, so
		// this is not retried.
		return Receipt{}, &Error{
			Code: CodeUnavailable, Status: resp.StatusCode,
			detail: "the response was not the documented JSON",
			cause:  err,
		}
	}

	if !answer.OK {
		return Receipt{}, namedError(answer.Error, resp.StatusCode, retryAfter)
	}
	return Receipt{Channel: answer.Channel, TS: answer.TS}, nil
}

// payloadOf builds the request body, leaving out every field the caller did
// not set so Slack applies its own defaults.
func payloadOf(msg Message) map[string]any {
	payload := map[string]any{
		"channel": msg.Channel,
		"text":    msg.Text,
	}
	if len(msg.Blocks) > 0 {
		payload["blocks"] = msg.Blocks
	}
	if msg.ThreadTS != "" {
		payload["thread_ts"] = msg.ThreadTS
		if msg.ReplyBroadcast {
			payload["reply_broadcast"] = true
		}
	}
	if msg.Username != "" {
		payload["username"] = msg.Username
	}
	if msg.IconEmoji != "" {
		payload["icon_emoji"] = msg.IconEmoji
	}
	return payload
}

// failed counts a post that gave up.
func failed(e *Error) *Error {
	mon.Event("slack_post_failed", monkit.NewSeriesTag("code", e.Code))
	return e
}

// statusError classifies a non-200 response.
func statusError(status int, retryAfter time.Duration) *Error {
	e := &Error{Status: status, RetryAfter: retryAfter}
	switch {
	case status == http.StatusTooManyRequests:
		e.Code, e.Temporary, e.Name = CodeRateLimited, true, "ratelimited"
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		e.Code = CodeAuth
	case status >= http.StatusInternalServerError:
		e.Code, e.Temporary = CodeUnavailable, true
	default:
		e.Code = CodeRejected
	}
	return e
}

// namedError classifies a 200 whose body said ok:false, by Slack's error
// string (https://api.slack.com/methods/chat.postMessage#errors).
//
// The name decides, not the status, because every one of these arrives as 200.
func namedError(name string, status int, retryAfter time.Duration) *Error {
	e := &Error{Status: status, Name: name, RetryAfter: retryAfter}

	switch name {
	case "ratelimited", "rate_limited":
		e.Code, e.Temporary = CodeRateLimited, true
	case "invalid_auth", "not_authed", "token_revoked", "token_expired",
		"account_inactive", "no_permission", "missing_scope",
		"not_allowed_token_type":
		e.Code = CodeAuth
	case "channel_not_found", "not_in_channel", "is_archived",
		"cannot_reply_to_message":
		e.Code = CodeChannel
	case "msg_too_long", "no_text", "invalid_blocks", "invalid_blocks_format",
		"messages_tab_disabled", "invalid_arguments", "invalid_arg_name":
		e.Code = CodeInvalid
	case "internal_error", "fatal_error", "service_unavailable",
		"request_timeout":
		e.Code, e.Temporary = CodeUnavailable, true
	default:
		e.Code = CodeRejected
	}
	return e
}
