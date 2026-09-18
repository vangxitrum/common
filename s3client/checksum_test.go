package s3client_test

import (
	"crypto/md5" // #nosec G501 -- verifying the checksum S3 defines for DeleteObjects.
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/s3client"
)

// TestDeleteObjectsIsSignedWithContentMD5 is the canary for the one workaround
// that cannot be seen from a stack trace.
//
// The SignedHeaders assertion is the point of it: it is what fails if the
// middleware is ever added to the stack rather than swapped into the
// flexible-checksum middleware's place, because added it would run after SigV4
// and the header would not be signed. Do not skip this test.
func TestDeleteObjectsIsSignedWithContentMD5(t *testing.T) {
	t.Parallel()

	srv := newServer(t, func(w http.ResponseWriter, r *http.Request, got recorded) {
		if r.Method != http.MethodPost {
			writeXML(w, http.StatusOK, `<ListBucketResult>`+
				`<IsTruncated>false</IsTruncated>`+
				`<Contents><Key>old/a</Key><Size>1</Size></Contents>`+
				`<Contents><Key>old/b</Key><Size>1</Size></Contents>`+
				`</ListBucketResult>`)
			return
		}

		sum := md5.Sum(got.Body) // #nosec G401 -- the S3 protocol checksum.
		want := base64.StdEncoding.EncodeToString(sum[:])

		require.Equal(t, want, got.Header.Get("Content-MD5"),
			"DeleteObjects must carry a Content-MD5 of the body sent")
		require.Empty(t, got.Header.Get("X-Amz-Checksum-Crc32"),
			"the flexible checksum must be gone, not sent alongside")
		require.Empty(t, got.Header.Get("X-Amz-Sdk-Checksum-Algorithm"))
		require.Contains(t,
			strings.ToLower(got.Header.Get("Authorization")), "content-md5",
			"Content-MD5 must be signed, which only happens when the "+
				"middleware is swapped in before SigV4 rather than added after")

		writeXML(w, http.StatusOK, `<DeleteResult></DeleteResult>`)
	})
	c := srv.client(t)

	deleted, err := c.DeletePrefix(t.Context(), "old/")
	require.NoError(t, err)
	require.Equal(t, 2, deleted)
}

// TestOtherOperationsKeepTheirOwnChecksums proves the middleware is scoped to
// DeleteObjects: registered on the client without the operation guard, it
// would strip the checksum from every operation that legitimately needs one.
func TestOtherOperationsKeepTheirOwnChecksums(t *testing.T) {
	t.Parallel()

	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ recorded) {
		w.Header().Set("ETag", `"v1"`)
		w.WriteHeader(http.StatusOK)
	})
	c := srv.client(t)

	_, err := c.Put(t.Context(), "object", strings.NewReader("hello"),
		s3client.PutParams{})
	require.NoError(t, err)

	seen := srv.seen()
	require.Len(t, seen, 1)
	require.Empty(t, seen[0].Header.Get("Content-MD5"),
		"a PutObject must not be given the DeleteObjects checksum")
}
