// Package e2e wires the OWASP controls together the way a service does, with
// nothing but net/http, and checks them from the outside.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/clientip"
	"gitlab.internal/tuan.quang.tran/aioz-common/httpsec"
	"gitlab.internal/tuan.quang.tran/aioz-common/logging"
	"gitlab.internal/tuan.quang.tran/aioz-common/mail"
	"gitlab.internal/tuan.quang.tran/aioz-common/password"
	"gitlab.internal/tuan.quang.tran/aioz-common/postgres"
	"gitlab.internal/tuan.quang.tran/aioz-common/ratelimit"
	"gitlab.internal/tuan.quang.tran/aioz-common/time2"
	"gitlab.internal/tuan.quang.tran/aioz-common/upload"
)

const (
	good    = "correct horse battery staple"
	appSite = "https://app.example.com"
	// htpasswd -bnBC 10 "" "correct horse battery staple": a hash from
	// before the password package.
	legacyHash = "$2y$10$v.x1YHl0vq0Ow8PmXaQiIu5iJTcXYgmUHLJLzd5Rd2HLBpGJxTZpi"
)

var (
	loginIP = ratelimit.Rule{
		Name: "login-ip",
		Rates: ratelimit.Rates{
			{Count: 20, Period: time.Minute},
			{Count: 200, Period: time.Hour},
		},
	}
	loginAccountIP = ratelimit.Rule{
		Name: "login-account-ip",
		Rates: ratelimit.Rates{
			{Count: 5, Period: time.Minute},
			{Count: 20, Period: time.Hour},
		},
	}
	loginAccount = ratelimit.Rule{
		Name:  "login-account",
		Rates: ratelimit.Rates{{Count: 100, Period: time.Hour}},
	}

	avatars = upload.Policy{
		MaxBytes: 1 << 20,
		Types:    []upload.Type{upload.PNG, upload.JPEG, upload.JPG},
	}
	mediaSort = postgres.SortColumns{
		"title":   {"media", "title"},
		"created": {"media", "created_at"},
	}
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// service is a login, an avatar upload and a sorted list, behind the security
// headers and CORS.
type service struct {
	handler http.Handler
	logs    *lockedBuffer

	mu    sync.Mutex
	users map[string]string // canonical email -> stored hash
}

func newService(t *testing.T) *service {
	t.Helper()
	headers, err := httpsec.NewHeaders(httpsec.HeadersConfig{})
	require.NoError(t, err)
	cors, err := httpsec.NewCORS(
		httpsec.CORSConfig{
			AllowedOrigins:   []string{appSite},
			AllowCredentials: true,
		},
	)
	require.NoError(t, err)
	resolver, err := clientip.New(
		clientip.Config{TrustedProxies: []string{"10.0.0.0/8"}},
	)
	require.NoError(t, err)
	limiter, err := ratelimit.New(
		slog.New(slog.DiscardHandler),
		ratelimit.Config{},
	)
	require.NoError(t, err)
	hasher, err := password.New(password.Config{})
	require.NoError(t, err)

	s := &service{
		logs:  &lockedBuffer{},
		users: map[string]string{"alice@example.com": legacyHash},
	}
	log := slog.New(
		slog.NewJSONHandler(
			s.logs,
			&slog.HandlerOptions{ReplaceAttr: logging.RedactAttr()},
		),
	)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		var form struct{ Email, Password string }
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).
			Decode(&form); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		addr, err := mail.ParseAddress(form.Email)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		email := addr.Canonical()
		ip := resolver.ClientIP(r)

		// Before the user lookup, so the answer is the same for an account
		// that does not exist.
		account := loginAccount.Key(email)
		accountIP := loginAccountIP.Key(email + " " + ip.String())
		result, err := limiter.Allow(
			r.Context(),
			loginIP.IPKey(ip),
			accountIP,
			account,
		)
		if err != nil {
			http.Error(w, "try again later", http.StatusServiceUnavailable)
			return
		}
		if !result.Allowed {
			w.Header().
				Set("Retry-After", strconv.FormatInt(result.RetryAfterSeconds(), 10))
			http.Error(w, "too many attempts", http.StatusTooManyRequests)
			return
		}

		// A careless log line; the redaction is the safety net under it.
		log.Info("login attempt", "email", email, "password", form.Password)

		s.mu.Lock()
		stored := s.users[email]
		s.mu.Unlock()
		verified, err := hasher.Verify(r.Context(), form.Password, stored)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !verified.OK {
			http.Error(w, "wrong email or password", http.StatusUnauthorized)
			return
		}
		if verified.NeedsRehash {
			if fresh, err := hasher.Hash(
				r.Context(),
				form.Password,
			); err == nil {
				s.mu.Lock()
				if s.users[email] == stored { // compare-and-swap
					s.users[email] = fresh
				}
				s.mu.Unlock()
			}
		}
		_ = limiter.Reset(r.Context(), accountIP, account)
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc(
		"POST /avatar",
		func(w http.ResponseWriter, r *http.Request) {
			upload.LimitRequest(w, r, 2<<20)
			_, fh, err := r.FormFile("avatar")
			var tooLarge *http.MaxBytesError
			switch {
			case errors.As(err, &tooLarge):
				http.Error(w, "too large", http.StatusRequestEntityTooLarge)
				return
			case err != nil:
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			f, err := avatars.Open(fh)
			switch {
			case errors.Is(err, upload.ErrTooLarge):
				http.Error(w, "too large", http.StatusRequestEntityTooLarge)
				return
			case errors.Is(err, upload.ErrRefused):
				http.Error(
					w,
					"unsupported file",
					http.StatusUnsupportedMediaType,
				)
				return
			case err != nil:
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			defer func() { _ = f.Close() }()
			if _, err := io.Copy(io.Discard, f); err != nil {
				http.Error(w, "too large", http.StatusRequestEntityTooLarge)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, f.Type.MIME)
		},
	)

	mux.HandleFunc("GET /media", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		orderBy, err := mediaSort.OrderBy(q.Get("sort"), q.Get("order"))
		if errors.Is(err, postgres.ErrInvalidSort) {
			http.Error(w, "invalid sort", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(
			w,
			"SELECT id, title FROM media ORDER BY "+orderBy+", media.id",
		)
	})

	s.handler = headers.Wrap(cors.Wrap(mux))
	return s
}

// frozen is a context whose clock only moves when the test says so, so a
// Retry-After is exact however slow the machine is.
func frozen(t *testing.T) context.Context {
	t.Helper()
	ctx, _ := time2.WithNewMachine(t.Context())
	return ctx
}

func (s *service) login(
	ctx context.Context,
	peer, forwardedFor, email, pw string,
) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"email": email, "password": pw})
	r := httptest.NewRequestWithContext(
		ctx,
		http.MethodPost,
		"/login",
		bytes.NewReader(body),
	)
	r.RemoteAddr = peer
	if forwardedFor != "" {
		r.Header.Set("X-Forwarded-For", forwardedFor)
	}
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, r)
	return rec
}

func requireOWASPHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	h := rec.Header()
	require.Equal(t, "nosniff", h.Get("X-Content-Type-Options"), rec.Code)
	require.Equal(t, "DENY", h.Get("X-Frame-Options"), rec.Code)
	require.Equal(t, "no-store", h.Get("Cache-Control"), rec.Code)
	// HSTS is opt-in until every host under ours serves HTTPS.
	require.Empty(t, h.Get("Strict-Transport-Security"), rec.Code)
	require.NotEmpty(t, h.Get("Content-Security-Policy"), rec.Code)
}

// Five wrong passwords, then a 429 - and the same 429 for an account that
// does not exist, so the limit cannot be used to find out which do.
func TestTheSixthLoginIsLimitedWhetherOrNotTheAccountExists(t *testing.T) {
	t.Parallel()
	ctx := frozen(t)
	s := newService(t)

	retryAfter := make([]string, 0, 2)
	for _, email := range []string{"alice@example.com", "nobody@example.com"} {
		for i := range 5 {
			rec := s.login(
				ctx,
				"198.51.100.7:40000",
				"",
				email,
				"wrong password "+strconv.Itoa(i),
			)
			require.Equal(
				t,
				http.StatusUnauthorized,
				rec.Code,
				"%s attempt %d",
				email,
				i+1,
			)
			requireOWASPHeaders(t, rec)
		}
		rec := s.login(ctx, "198.51.100.7:40000", "", email, "wrong password 6")
		require.Equal(t, http.StatusTooManyRequests, rec.Code, email)
		requireOWASPHeaders(t, rec)
		retryAfter = append(retryAfter, rec.Header().Get("Retry-After"))
	}
	require.Equal(t, []string{"12", "12"}, retryAfter)
}

// X-Forwarded-For from a client that is not our proxy changes nothing: every
// attempt still counts against the peer's address.
func TestAForgedForwardedForDoesNotEscapeTheLimit(t *testing.T) {
	t.Parallel()
	ctx := frozen(t)
	s := newService(t)

	for i := range 5 {
		rec := s.login(
			ctx,
			"198.51.100.7:40000",
			"203.0.113."+strconv.Itoa(i+1),
			"alice@example.com",
			"wrong",
		)
		require.Equal(t, http.StatusUnauthorized, rec.Code)
	}
	rec := s.login(
		ctx,
		"198.51.100.7:40000",
		"203.0.113.99",
		"alice@example.com",
		"wrong",
	)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
}

// From our own proxy, the forwarded address is the client, and two clients
// are two buckets.
func TestATrustedProxysForwardedForIsTheClient(t *testing.T) {
	t.Parallel()
	ctx := frozen(t)
	s := newService(t)

	for i := range 8 {
		rec := s.login(
			ctx,
			"10.0.0.5:40000",
			"203.0.113."+strconv.Itoa(i+1),
			"alice@example.com",
			"wrong",
		)
		require.Equal(
			t,
			http.StatusUnauthorized,
			rec.Code,
			"client %d has its own login-account-ip bucket",
			i+1,
		)
	}
}

// A hash from before the password package lets its owner in and is replaced
// by an argon2id hash on the spot.
func TestALegacyHashIsUpgradedOnLogin(t *testing.T) {
	t.Parallel()
	ctx := frozen(t)
	s := newService(t)

	require.Equal(
		t,
		http.StatusNoContent,
		s.login(ctx, "198.51.100.7:40000", "", "Alice@Example.com", good).Code,
	)
	s.mu.Lock()
	upgraded := s.users["alice@example.com"]
	s.mu.Unlock()
	require.True(
		t,
		strings.HasPrefix(upgraded, "$argon2id$v=19$m=19456,t=2,p=1$"),
		upgraded,
	)

	require.Equal(
		t,
		http.StatusNoContent,
		s.login(ctx, "198.51.100.7:40000", "", "alice@example.com", good).Code,
	)
	require.Equal(
		t,
		http.StatusUnauthorized,
		s.login(
			ctx,
			"198.51.100.7:40000",
			"",
			"alice@example.com",
			"wrong",
		).Code,
	)
}

func TestThePasswordNeverReachesTheLog(t *testing.T) {
	t.Parallel()
	ctx := frozen(t)
	s := newService(t)

	s.login(
		ctx,
		"198.51.100.7:40000",
		"",
		"alice@example.com",
		"hunter2-hunter2",
	)

	require.NotContains(t, s.logs.String(), "hunter2-hunter2")
	require.Contains(t, s.logs.String(), logging.Redacted)
	require.Contains(t, s.logs.String(), "alice@example.com")
}

func TestCORSPreflights(t *testing.T) {
	t.Parallel()
	s := newService(t)

	for origin, allowed := range map[string]bool{appSite: true, "https://evil.example.net": false} {
		r := httptest.NewRequest(http.MethodOptions, "/login", nil)
		r.Header.Set("Origin", origin)
		r.Header.Set("Access-Control-Request-Method", http.MethodPost)
		rec := httptest.NewRecorder()
		s.handler.ServeHTTP(rec, r)

		require.Equal(t, http.StatusNoContent, rec.Code, origin)
		requireOWASPHeaders(t, rec)
		if allowed {
			require.Equal(
				t,
				origin,
				rec.Header().Get("Access-Control-Allow-Origin"),
			)
			require.Equal(
				t,
				"true",
				rec.Header().Get("Access-Control-Allow-Credentials"),
			)
			require.Contains(
				t,
				rec.Header().Get("Access-Control-Allow-Methods"),
				http.MethodPost,
			)
		} else {
			require.Empty(
				t,
				rec.Header().Values("Access-Control-Allow-Origin"),
				origin,
			)
		}
	}
}

func (s *service) upload(
	t *testing.T,
	name string,
	content []byte,
) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("avatar", name)
	require.NoError(t, err)
	_, err = part.Write(content)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	r := httptest.NewRequest(http.MethodPost, "/avatar", &body)
	r.Header.Set("Content-Type", w.FormDataContentType())
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, r)
	return rec
}

func TestUploads(t *testing.T) {
	t.Parallel()
	s := newService(t)

	png := append(
		[]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR"),
		make([]byte, 600)...,
	)

	rec := s.upload(t, "cat.png", png)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	require.Equal(t, "image/png", rec.Body.String())
	requireOWASPHeaders(t, rec)

	rec = s.upload(t, "shell.jpg", []byte("<?php system($_GET['c']); ?>"))
	require.Equal(t, http.StatusUnsupportedMediaType, rec.Code)

	rec = s.upload(
		t,
		"huge.png",
		append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 3<<20)...),
	)
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}

func TestASortParameterIsAnAllowlistedColumn(t *testing.T) {
	t.Parallel()
	s := newService(t)

	get := func(query string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.handler.ServeHTTP(
			rec,
			httptest.NewRequest(http.MethodGet, "/media?"+query, nil),
		)
		return rec
	}

	rec := get("sort=title&order=desc")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(
		t,
		`SELECT id, title FROM media ORDER BY "media"."title" DESC, media.id`,
		rec.Body.String(),
	)

	for _, query := range []string{"sort=title;DROP+TABLE+media", "sort=id", "sort=title&order=sideways"} {
		require.Equal(t, http.StatusBadRequest, get(query).Code, query)
	}
}
