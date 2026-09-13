package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	netmail "net/mail"
	"net/url"
	"strings"
	"time"

	"github.com/resend/resend-go/v3"
	"github.com/spacemonkeygo/monkit/v3"

	"gitlab.internal/tuan.quang.tran/aioz-common/httpclient"
	"gitlab.internal/tuan.quang.tran/aioz-common/random"
	"gitlab.internal/tuan.quang.tran/aioz-common/retry"
	"gitlab.internal/tuan.quang.tran/aioz-common/secret"
)

const (
	defaultMaxAttempts  = 4
	defaultRetryWaitMin = 500 * time.Millisecond
	defaultRetryWaitMax = 10 * time.Second
	defaultTimeout      = 30 * time.Second

	// generatedKeyLen is the length of an idempotency key made for a send
	// that brought none: 32 base62 characters, about 190 bits.
	generatedKeyLen = 32

	// maxErrorBody bounds how much of an error response is read to find its
	// name. Resend's error bodies are a few hundred bytes.
	maxErrorBody = 64 << 10
)

// Resend sends mail through the Resend API, with the resend-go SDK.
//
// The SDK does not retry, and it reports every failure but a 429 as a string,
// with the status and Resend's error name thrown away - and only the name tells
// a rate limit worth waiting out from a quota no retry will fix. So the HTTP
// client it is given records both from each response, and Send decides from
// those.
type Resend struct {
	log     *slog.Logger
	from    string
	client  *resend.Client
	backoff *retry.Backoff
}

// NewResend returns a Resend provider that sends as from when a message sets
// no sender.
func NewResend(
	log *slog.Logger,
	from string,
	config ResendConfig,
) (*Resend, error) {
	if log == nil {
		return nil, fmt.Errorf("mail: logger is nil")
	}
	key := secret.Token(strings.TrimSpace(config.APIKey))
	if key == "" {
		return nil, fmt.Errorf("mail: the resend provider needs an API key")
	}
	if from == "" {
		return nil, fmt.Errorf("mail: the resend provider needs a sender")
	}
	if _, err := netmail.ParseAddress(from); err != nil {
		return nil, fmt.Errorf("mail: sender %q is not an address", from)
	}

	clientConfig := config.Client
	if clientConfig.Name == "" {
		clientConfig.Name = "resend"
	}
	if clientConfig.Timeout == 0 {
		// A config built by hand rather than bound from flags has no backstop;
		// a send must not be able to hang for ever on a silent connection.
		clientConfig.Timeout = defaultTimeout
	}
	httpClient, err := httpclient.New(log, clientConfig)
	if err != nil {
		return nil, err
	}
	httpClient.Transport = &outcomeTransport{next: httpClient.Transport}

	client := resend.NewCustomClient(httpClient, key.Reveal())
	if config.BaseURL != "" {
		base, err := url.Parse(strings.TrimSuffix(config.BaseURL, "/") + "/")
		if err != nil {
			return nil, fmt.Errorf("mail: resend base URL: %w", err)
		}
		client.BaseURL = base
	}

	return &Resend{
		log:    log,
		from:   from,
		client: client,
		backoff: retry.New(retry.Config{
			Name:         "mail",
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

// Send sends msg, retrying what can pass, with one idempotency key across
// every attempt.
func (r *Resend) Send(ctx context.Context, msg Message) (_ Receipt, err error) {
	defer mon.Task()(&ctx)(&err)

	if err := msg.validate(r.from, true); err != nil {
		return Receipt{}, err
	}

	from := msg.From
	if from == "" {
		from = r.from
	}
	key := msg.IdempotencyKey
	if key == "" {
		key = random.String(generatedKeyLen, random.Base62)
	}

	request := &resend.SendEmailRequest{
		From:    from,
		To:      msg.To,
		Cc:      msg.Cc,
		Bcc:     msg.Bcc,
		ReplyTo: msg.ReplyTo,
		Subject: msg.Subject,
		Html:    msg.HTML,
		Text:    msg.Text,
		Headers: msg.Headers,
	}
	options := &resend.SendEmailOptions{IdempotencyKey: key}

	loop := r.backoff.Loop()
	var last *Error
	for loop.Wait(ctx) {
		result := &outcome{}
		sent, sendErr := r.client.Emails.SendWithOptions(
			withOutcome(ctx, result),
			request,
			options,
		)
		if sendErr == nil {
			return Receipt{ID: sent.Id}, nil
		}
		if ctx.Err() != nil {
			return Receipt{}, fmt.Errorf("mail: %w", ctx.Err())
		}

		last = result.classify()
		if !last.Temporary {
			return Receipt{}, failed(last)
		}
		if last.RetryAfter > httpclient.MaxRetryAfter {
			// Waiting as long as a remote asks is a way for whoever runs it
			// to hold this service's goroutines; the caller decides instead.
			return Receipt{}, failed(last)
		}
		if last.RetryAfter > 0 {
			loop.Override(last.RetryAfter)
		}

		r.log.WarnContext(
			ctx,
			"retrying a mail send",
			"code",
			last.Code,
			"status",
			last.Status,
			"attempt",
			loop.Attempt()+2,
			"recipients",
			msg.recipients(),
		)
	}

	if ctx.Err() != nil {
		return Receipt{}, fmt.Errorf("mail: %w", ctx.Err())
	}
	if last == nil {
		return Receipt{}, fmt.Errorf("mail: %w", loop.Err())
	}
	return Receipt{}, failed(last)
}

// failed counts a send that gave up.
func failed(e *Error) *Error {
	mon.Event("mail_send_failed", monkit.NewSeriesTag("code", e.Code))
	return e
}

// outcome is what one attempt's HTTP response said, recorded by
// [outcomeTransport] because the SDK does not pass it on.
type outcome struct {
	status     int
	name       string
	retryAfter time.Duration
}

type outcomeKey struct{}

func withOutcome(ctx context.Context, o *outcome) context.Context {
	return context.WithValue(ctx, outcomeKey{}, o)
}

// classify turns an attempt's outcome into an [*Error], by Resend's error
// name first and its status second
// (https://resend.com/docs/api-reference/errors).
func (o *outcome) classify() *Error {
	e := &Error{Status: o.status, Name: o.name, RetryAfter: o.retryAfter}

	switch {
	case o.status == 0:
		e.Code, e.Temporary = CodeTransport, true
	case o.name == "daily_quota_exceeded" || o.name == "monthly_quota_exceeded":
		e.Code = CodeQuota
	case o.status == http.StatusTooManyRequests:
		e.Code, e.Temporary = CodeRateLimited, true
	case o.name == "concurrent_idempotent_requests" || o.name == "resource_locked":
		e.Code, e.Temporary = CodeConflict, true
	case o.status == http.StatusConflict:
		e.Code = CodeConflict // invalid_idempotent_request: same key, another body
	case o.status == http.StatusUnauthorized:
		e.Code = CodeAuth
	case o.status == http.StatusBadRequest || o.status == http.StatusUnprocessableEntity:
		e.Code = CodeInvalid
	case o.status >= http.StatusInternalServerError || o.status < http.StatusMultipleChoices:
		// A 5xx, or a 2xx the SDK could not read: the key makes a retry safe.
		e.Code, e.Temporary = CodeUnavailable, true
	default:
		e.Code = CodeRejected
	}
	return e
}

// outcomeTransport records each response's status, Retry-After and Resend
// error name into the [outcome] carried by the request's context.
//
// An error body is read (bounded) to find the name and then put back, so the
// SDK still decodes it as it always has.
type outcomeTransport struct {
	next http.RoundTripper
}

func (t *outcomeTransport) RoundTrip(
	req *http.Request,
) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)

	result, _ := req.Context().Value(outcomeKey{}).(*outcome)
	if result == nil || err != nil || resp == nil {
		return resp, err
	}

	result.status = resp.StatusCode
	if wait, ok := httpclient.ParseRetryAfter(resp); ok {
		result.retryAfter = wait
	}

	if resp.StatusCode >= http.StatusMultipleChoices && resp.Body != nil {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}

		var named struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(body, &named)
		result.name = named.Name

		resp.Body = io.NopCloser(bytes.NewReader(body))
	}
	return resp, nil
}
