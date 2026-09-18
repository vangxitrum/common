package s3client_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/memory"
	"gitlab.internal/tuan.quang.tran/aioz-common/s3client"
)

func TestPutSmallObject(t *testing.T) {
	t.Parallel()

	srv := newServer(t, func(w http.ResponseWriter, r *http.Request, got recorded) {
		w.Header().Set("ETag", `"abc123"`)
		w.WriteHeader(http.StatusOK)
	})
	c := srv.client(t)

	info, err := c.Put(t.Context(), "video/master.m3u8",
		strings.NewReader("#EXTM3U\n"), s3client.PutParams{
			ContentType:  "application/vnd.apple.mpegurl",
			CacheControl: "max-age=60",
		})
	require.NoError(t, err)
	require.Equal(t, `"abc123"`, info.ETag)

	seen := srv.seen()
	require.Len(t, seen, 1)
	require.Equal(t, http.MethodPut, seen[0].Method)
	require.Equal(t, "/"+testBucket+"/video/master.m3u8", seen[0].Path)
	require.Equal(t, "#EXTM3U\n", string(seen[0].Body))
	require.Equal(t, "application/vnd.apple.mpegurl",
		seen[0].Header.Get("Content-Type"))
	require.Equal(t, "max-age=60", seen[0].Header.Get("Cache-Control"))
}

func TestPutLargeObjectGoesMultipartWithoutChecksums(t *testing.T) {
	t.Parallel()

	srv := newServer(t, func(w http.ResponseWriter, r *http.Request, got recorded) {
		switch {
		case r.Method == http.MethodPost && strings.Contains(got.Query, "uploads"):
			writeXML(w, http.StatusOK, `<InitiateMultipartUploadResult>`+
				`<Bucket>`+testBucket+`</Bucket><Key>big</Key>`+
				`<UploadId>upload-1</UploadId>`+
				`</InitiateMultipartUploadResult>`)
		case r.Method == http.MethodPut:
			part := r.URL.Query().Get("partNumber")
			w.Header().Set("ETag", `"part-`+part+`"`)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost:
			writeXML(w, http.StatusOK, `<CompleteMultipartUploadResult>`+
				`<ETag>&quot;complete&quot;</ETag>`+
				`</CompleteMultipartUploadResult>`)
		default:
			t.Errorf("unexpected %s %s?%s", r.Method, got.Path, got.Query)
		}
	})
	c := srv.client(t)

	// Two parts and a little: the part size in the test config is 5MiB.
	body := bytes.Repeat([]byte("x"), int(11*memory.MiB))
	info, err := c.Put(t.Context(), "big", bytes.NewReader(body),
		s3client.PutParams{ContentType: "video/mp4"})
	require.NoError(t, err)
	require.Equal(t, `"complete"`, info.ETag)

	var parts, uploaded int
	for _, got := range srv.seen() {
		if got.Method == http.MethodPut {
			parts++
			uploaded += len(got.Body)
		}
	}
	require.Equal(t, 3, parts, "an 11MiB body should be three 5MiB-capped parts")
	require.Equal(t, len(body), uploaded)
}

func TestDefaultPartSizeSplitsAtEightMiB(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		size      memory.Size
		wantParts int
	}{
		// The old client split at 100MB, so nothing under that was ever one
		// request; these bound the default from both sides.
		{name: "under the part size is one request", size: 7 * memory.MiB, wantParts: 1},
		{name: "over it is multipart", size: 9 * memory.MiB, wantParts: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := newServer(t, func(w http.ResponseWriter, r *http.Request, got recorded) {
				switch {
				case r.Method == http.MethodPost && strings.Contains(got.Query, "uploads"):
					writeXML(w, http.StatusOK, `<InitiateMultipartUploadResult>`+
						`<UploadId>upload-1</UploadId>`+
						`</InitiateMultipartUploadResult>`)
				case r.Method == http.MethodPost:
					writeXML(w, http.StatusOK, `<CompleteMultipartUploadResult>`+
						`<ETag>&quot;complete&quot;</ETag>`+
						`</CompleteMultipartUploadResult>`)
				default:
					w.Header().Set("ETag", `"part"`)
					w.WriteHeader(http.StatusOK)
				}
			})
			// PartSize and Concurrency left at zero, so the defaults apply.
			c, err := srv.clientWith(t, func(config *s3client.Config) {
				config.PartSize = 0
				config.Concurrency = 0
			})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, c.Close()) })

			body := bytes.Repeat([]byte("x"), int(tc.size))
			_, err = c.Put(t.Context(), "sized", bytes.NewReader(body),
				s3client.PutParams{})
			require.NoError(t, err)

			parts, uploaded := 0, 0
			for _, got := range srv.seen() {
				if got.Method == http.MethodPut {
					parts++
					uploaded += len(got.Body)
				}
			}
			require.Equal(t, tc.wantParts, parts)
			require.Equal(t, len(body), uploaded)
		})
	}
}

func TestPutAbortsTheUploadWhenTheBodyFails(t *testing.T) {
	t.Parallel()

	var aborted atomic.Bool
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request, got recorded) {
		switch {
		case r.Method == http.MethodPost && strings.Contains(got.Query, "uploads"):
			writeXML(w, http.StatusOK, `<InitiateMultipartUploadResult>`+
				`<UploadId>upload-1</UploadId>`+
				`</InitiateMultipartUploadResult>`)
		case r.Method == http.MethodDelete:
			aborted.Store(true)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.Header().Set("ETag", `"part"`)
			w.WriteHeader(http.StatusOK)
		}
	})
	c := srv.client(t)

	// One good part, then a failure, so the upload is already multipart.
	body := io.MultiReader(
		bytes.NewReader(bytes.Repeat([]byte("x"), int(6*memory.MiB))),
		errReader{errors.New("the source went away")},
	)
	_, err := c.Put(t.Context(), "big", body, s3client.PutParams{})
	require.Error(t, err)
	require.True(t, aborted.Load(),
		"a multipart upload left open keeps paying for storage")
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func TestGetReturnsAClosableBody(t *testing.T) {
	t.Parallel()

	srv := newServer(t, func(w http.ResponseWriter, r *http.Request, got recorded) {
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Cache-Control", "max-age=30")
		w.Header().Set("Content-Length", "5")
		_, _ = w.Write([]byte("hello"))
	})
	c := srv.client(t)

	obj, err := c.Get(t.Context(), "greeting")
	require.NoError(t, err)
	t.Cleanup(func() { _ = obj.Close() })

	require.Equal(t, `"v1"`, obj.Info.ETag)
	require.Equal(t, int64(5), obj.Info.Size)
	require.Equal(t, "text/plain", obj.Info.ContentType)
	require.Equal(t, "max-age=30", obj.Info.CacheControl)

	body, err := io.ReadAll(obj.Body)
	require.NoError(t, err)
	require.Equal(t, "hello", string(body))
	require.NoError(t, obj.Close())
}

func TestGetMissingIsNotFound(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		write func(w http.ResponseWriter)
	}{
		{
			name: "NoSuchKey document",
			write: func(w http.ResponseWriter) {
				errorXML(w, http.StatusNotFound, "NoSuchKey", "no such key")
			},
		},
		{
			// Our service answers some paths with a bare 404 the SDK cannot
			// parse into a typed error.
			name: "bare 404",
			write: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusNotFound)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ recorded) {
				tc.write(w)
			})
			c := srv.client(t)

			_, err := c.Get(t.Context(), "gone")
			require.Error(t, err)
			require.True(t, s3client.IsNotFound(err), "got %v", err)
			require.Equal(t, s3client.CodeNotFound, s3client.Code(err))
		})
	}
}

func TestStatWithoutContentLengthReportsMissingSize(t *testing.T) {
	t.Parallel()

	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ recorded) {
		w.Header().Set("ETag", `"v1"`)
		w.WriteHeader(http.StatusOK)
	})
	c := srv.client(t)

	info, err := c.Stat(t.Context(), "sized/unknown")
	require.NoError(t, err)
	require.Equal(t, int64(-1), info.Size)
	require.Equal(t, `"v1"`, info.ETag)
}

func TestStatForbiddenIsNotAMissingObject(t *testing.T) {
	t.Parallel()

	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ recorded) {
		errorXML(w, http.StatusForbidden, "AccessDenied", "access denied")
	})
	c := srv.client(t)

	_, err := c.Stat(t.Context(), "private")
	require.Error(t, err)
	require.Equal(t, s3client.CodePermission, s3client.Code(err))
	require.False(t, s3client.IsNotFound(err),
		"an expired or wrong credential must never read as an absent object")
}

func TestListPagesAndRollsUpPrefixes(t *testing.T) {
	t.Parallel()

	srv := newServer(t, func(w http.ResponseWriter, r *http.Request, _ recorded) {
		query := r.URL.Query()
		if query.Get("delimiter") == "/" {
			writeXML(w, http.StatusOK, `<ListBucketResult>`+
				`<IsTruncated>false</IsTruncated>`+
				`<CommonPrefixes><Prefix>video/a/</Prefix></CommonPrefixes>`+
				`<CommonPrefixes><Prefix>video/b/</Prefix></CommonPrefixes>`+
				`</ListBucketResult>`)
			return
		}
		if query.Get("continuation-token") == "" {
			writeXML(w, http.StatusOK, `<ListBucketResult>`+
				`<IsTruncated>true</IsTruncated>`+
				`<NextContinuationToken>page-2</NextContinuationToken>`+
				`<Contents><Key>video/one</Key><Size>10</Size>`+
				`<ETag>&quot;e1&quot;</ETag></Contents>`+
				`</ListBucketResult>`)
			return
		}
		writeXML(w, http.StatusOK, `<ListBucketResult>`+
			`<IsTruncated>false</IsTruncated>`+
			`<Contents><Key>video/two</Key><Size>20</Size>`+
			`<ETag>&quot;e2&quot;</ETag></Contents>`+
			`</ListBucketResult>`)
	})
	c := srv.client(t)

	first, err := c.List(t.Context(), s3client.ListParams{Prefix: "video/"})
	require.NoError(t, err)
	require.Equal(t, "page-2", first.Cursor)
	require.Len(t, first.Objects, 1)
	require.Equal(t, "video/one", first.Objects[0].Key)
	require.Equal(t, int64(10), first.Objects[0].Size)

	second, err := c.List(t.Context(), s3client.ListParams{
		Prefix: "video/", Cursor: first.Cursor,
	})
	require.NoError(t, err)
	require.Empty(t, second.Cursor)
	require.Equal(t, "video/two", second.Objects[0].Key)

	rolled, err := c.List(t.Context(), s3client.ListParams{
		Prefix: "video/", Delimiter: "/",
	})
	require.NoError(t, err)
	require.Equal(t, []string{"video/a/", "video/b/"}, rolled.Prefixes)
	require.Empty(t, rolled.Objects)

	var keys []string
	for info, err := range s3client.All(t.Context(), c,
		s3client.ListParams{Prefix: "video/"}) {
		require.NoError(t, err)
		keys = append(keys, info.Key)
	}
	require.Equal(t, []string{"video/one", "video/two"}, keys)
}

func TestDeleteIfMatchLosingTheRaceIsNotRetried(t *testing.T) {
	t.Parallel()

	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ recorded) {
		errorXML(w, http.StatusPreconditionFailed,
			"PreconditionFailed", "the etag moved on")
	})
	c := srv.client(t)

	err := c.Delete(t.Context(), "racy", s3client.DeleteParams{IfMatch: `"old"`})
	require.Error(t, err)
	require.True(t, s3client.IsPrecondition(err), "got %v", err)
	require.False(t, s3client.IsTemporary(err))
	require.Equal(t, 1, srv.count(),
		"a lost race is not a temporary failure and must not be retried")

	require.Equal(t, `"old"`, srv.seen()[0].Header.Get("If-Match"))
}

func TestDeletePrefixBatchesAtTheProtocolLimit(t *testing.T) {
	t.Parallel()

	const total = 2500

	var listed atomic.Int32
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request, got recorded) {
		if r.Method == http.MethodPost {
			writeXML(w, http.StatusOK, `<DeleteResult></DeleteResult>`)
			return
		}
		// Two list pages of 1000 and one of 500.
		page := listed.Add(1)
		var contents strings.Builder
		count, truncated := 1000, true
		if page == 3 {
			count, truncated = 500, false
		}
		for i := range count {
			fmt.Fprintf(&contents, `<Contents><Key>old/%d-%d</Key>`+
				`<Size>1</Size></Contents>`, page, i)
		}
		writeXML(w, http.StatusOK, `<ListBucketResult>`+
			`<IsTruncated>`+strconv.FormatBool(truncated)+`</IsTruncated>`+
			`<NextContinuationToken>page-`+strconv.Itoa(int(page)+1)+
			`</NextContinuationToken>`+contents.String()+`</ListBucketResult>`)
	})
	c := srv.client(t)

	deleted, err := c.DeletePrefix(t.Context(), "old/")
	require.NoError(t, err)
	require.Equal(t, total, deleted)

	var batches []int
	for _, got := range srv.seen() {
		if got.Method != http.MethodPost {
			continue
		}
		batches = append(batches, strings.Count(string(got.Body), "<Key>"))
		require.Contains(t, string(got.Body), "<Quiet>true</Quiet>")
	}
	require.Equal(t, []int{1000, 1000, 500}, batches)
}

func TestDeleteManyPartialFailureNamesNoKey(t *testing.T) {
	t.Parallel()

	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ recorded) {
		writeXML(w, http.StatusOK, `<DeleteResult><Error>`+
			`<Key>secret/user-42/passport.jpg</Key>`+
			`<Code>AccessDenied</Code><Message>nope</Message>`+
			`</Error></DeleteResult>`)
	})
	c := srv.client(t)

	_, err := c.DeleteMany(t.Context(), []string{
		"secret/user-42/passport.jpg", "secret/user-42/id.jpg",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "1 of 2")
	require.Contains(t, err.Error(), "AccessDenied")
	require.NotContains(t, err.Error(), "passport",
		"a key names a user and their file; it must not reach an error string")
}

func TestCopyReplacingMetadata(t *testing.T) {
	t.Parallel()

	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ recorded) {
		writeXML(w, http.StatusOK, `<CopyObjectResult>`+
			`<ETag>&quot;copied&quot;</ETag></CopyObjectResult>`)
	})
	c := srv.client(t)

	info, err := c.Copy(t.Context(), "from/a.mp4", "to/b.mp4", s3client.CopyParams{
		ReplaceMetadata: true,
		ContentType:     "video/mp4",
	})
	require.NoError(t, err)
	require.Equal(t, `"copied"`, info.ETag)

	seen := srv.seen()
	require.Len(t, seen, 1)
	require.Equal(t, "/"+testBucket+"/to/b.mp4", seen[0].Path)
	require.Equal(t, url.PathEscape(testBucket+"/from/a.mp4"),
		seen[0].Header.Get("X-Amz-Copy-Source"))
	require.Equal(t, "REPLACE", seen[0].Header.Get("X-Amz-Metadata-Directive"))
	require.Equal(t, "video/mp4", seen[0].Header.Get("Content-Type"))
}

func TestPresignSignsWithoutCallingTheService(t *testing.T) {
	t.Parallel()

	srv := newServer(t, func(w http.ResponseWriter, r *http.Request, got recorded) {
		t.Errorf("presigning must not call the service, got %s %s",
			got.Method, got.Path)
	})
	c := srv.client(t)

	for _, tc := range []struct {
		name string
		sign func() (string, error)
	}{
		{"get", func() (string, error) {
			return c.PresignGet(t.Context(), "video/a.mp4", 5*time.Minute)
		}},
		{"put", func() (string, error) {
			return c.PresignPut(t.Context(), "video/a.mp4", 5*time.Minute,
				s3client.PutParams{ContentType: "video/mp4"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			raw, err := tc.sign()
			require.NoError(t, err)

			u, err := url.Parse(raw)
			require.NoError(t, err)
			require.Equal(t, "/"+testBucket+"/video/a.mp4", u.Path)
			require.NotEmpty(t, u.Query().Get("X-Amz-Signature"))
			require.Equal(t, "300", u.Query().Get("X-Amz-Expires"))
		})
	}
	require.Zero(t, srv.count())
}

func TestPresignGetWithAPublicURLSignsNothing(t *testing.T) {
	t.Parallel()

	srv := newServer(t, func(w http.ResponseWriter, r *http.Request, got recorded) {
		t.Errorf("a public URL must not call the service, got %s %s",
			got.Method, got.Path)
	})
	c, err := srv.clientWith(t, func(config *s3client.Config) {
		config.PublicURL = "https://cdn.example.test/"
	})
	require.NoError(t, err)

	got, err := c.PresignGet(t.Context(), "video/a b.mp4", time.Minute)
	require.NoError(t, err)
	require.Equal(t, "https://cdn.example.test/video/a%20b.mp4", got)
	require.Zero(t, srv.count())
}

func TestRetries(t *testing.T) {
	t.Parallel()

	t.Run("a temporary failure is retried", func(t *testing.T) {
		t.Parallel()

		var attempts atomic.Int32
		srv := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ recorded) {
			if attempts.Add(1) < 3 {
				errorXML(w, http.StatusServiceUnavailable,
					"ServiceUnavailable", "try later")
				return
			}
			w.Header().Set("ETag", `"v1"`)
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(http.StatusOK)
		})
		c := srv.client(t)

		_, err := c.Stat(t.Context(), "flaky")
		require.NoError(t, err)
		require.Equal(t, 3, srv.count())
	})

	t.Run("a permanent failure is not", func(t *testing.T) {
		t.Parallel()

		srv := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ recorded) {
			errorXML(w, http.StatusForbidden, "AccessDenied", "no")
		})
		c := srv.client(t)

		_, err := c.Stat(t.Context(), "private")
		require.Error(t, err)
		require.Equal(t, 1, srv.count())
	})

	t.Run("a canceled context stops", func(t *testing.T) {
		t.Parallel()

		srv := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ recorded) {
			errorXML(w, http.StatusServiceUnavailable,
				"ServiceUnavailable", "try later")
		})
		c := srv.client(t)

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err := c.Stat(ctx, "flaky")
		require.Error(t, err)
		require.Equal(t, s3client.CodeTransport, s3client.Code(err))
	})
}

func TestNothingLogsOrReportsTheSecret(t *testing.T) {
	t.Parallel()

	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ recorded) {
		errorXML(w, http.StatusForbidden, "AccessDenied", "no")
	})

	var buf bytes.Buffer
	config := srv.config()
	c, err := s3client.New(intoBuffer(&buf), config)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, c.Close()) })

	_, putErr := c.Put(t.Context(), "denied", strings.NewReader("x"),
		s3client.PutParams{})
	require.Error(t, putErr)

	require.NotContains(t, buf.String(), testSecretKey)
	require.NotContains(t, putErr.Error(), testSecretKey)
	require.NotContains(t, buf.String(), testAccessKey)
}

func TestKeyValidationCostsNoRequest(t *testing.T) {
	t.Parallel()

	srv := newServer(t, func(w http.ResponseWriter, r *http.Request, got recorded) {
		t.Errorf("a refused key must not reach the service, got %s %s",
			got.Method, got.Path)
	})
	c := srv.client(t)

	for _, tc := range []struct{ name, key string }{
		{"empty", ""},
		{"leading slash", "/video/a.mp4"},
		{"dot dot segment", "video/../../etc/passwd"},
		{"too long", strings.Repeat("k", s3client.MaxKeyLen+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := c.Stat(t.Context(), tc.key)
			require.Error(t, err)
			require.Equal(t, s3client.CodeInvalid, s3client.Code(err))
		})
	}
	require.Zero(t, srv.count())
}

func TestVerifyBucketUsesHeadNotList(t *testing.T) {
	t.Parallel()

	t.Run("a bucket that is not there fails construction", func(t *testing.T) {
		t.Parallel()

		srv := newServer(t, func(w http.ResponseWriter, r *http.Request, got recorded) {
			if got.Path == "/" {
				t.Error("ListBuckets needs an account-wide permission; " +
					"a bucket-scoped key must never need it")
			}
			w.WriteHeader(http.StatusNotFound)
		})

		_, err := srv.clientWith(t, func(config *s3client.Config) {
			config.VerifyBucket = true
		})
		require.Error(t, err)
		require.True(t, s3client.IsNotFound(err), "got %v", err)
		require.Equal(t, http.MethodHead, srv.seen()[0].Method)
	})

	t.Run("a bucket that is there succeeds", func(t *testing.T) {
		t.Parallel()

		srv := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ recorded) {
			w.WriteHeader(http.StatusOK)
		})

		c, err := srv.clientWith(t, func(config *s3client.Config) {
			config.VerifyBucket = true
		})
		require.NoError(t, err)
		require.NoError(t, c.Close())
		require.Equal(t, 1, srv.count())
	})
}

func TestCompatAWSSendsWhatOurServiceRejects(t *testing.T) {
	t.Parallel()

	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ recorded) {
		w.Header().Set("ETag", `"v1"`)
		w.WriteHeader(http.StatusOK)
	})
	// The whole point of this test is the headers the other tests refuse.
	srv.skipCompat = true

	c, err := srv.clientWith(t, func(config *s3client.Config) {
		config.Compat = s3client.CompatAWS
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, c.Close()) })

	_, err = c.Put(t.Context(), "aws/object", strings.NewReader("hello"),
		s3client.PutParams{})
	require.NoError(t, err)

	seen := srv.seen()
	require.Len(t, seen, 1)
	var checksums []string
	for header := range seen[0].Header {
		if strings.HasPrefix(strings.ToLower(header), "x-amz-checksum-") {
			checksums = append(checksums, header)
		}
	}
	require.NotEmpty(t, checksums,
		"the aws profile takes the SDK defaults, checksums included")
}

func writeXML(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>`+body)
}
