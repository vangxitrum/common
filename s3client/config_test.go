package s3client_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/memory"
	"gitlab.internal/tuan.quang.tran/aioz-common/s3client"
)

func validConfig() s3client.Config {
	return s3client.Config{
		Compat:          s3client.CompatAIOZ,
		Endpoint:        "https://s3-storage.example.test",
		Region:          "us-east-1",
		Bucket:          "bucket",
		AccessKeyID:     testAccessKey,
		SecretAccessKey: testSecretKey,
	}
}

func TestConfigValidate(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		change  func(*s3client.Config)
		wantErr string
	}{
		{name: "the valid one", change: func(*s3client.Config) {}},
		{
			name:    "an unknown server profile",
			change:  func(c *s3client.Config) { c.Compat = "minio" },
			wantErr: `compat "minio" is neither "aioz" nor "aws"`,
		},
		{
			name:    "no bucket",
			change:  func(c *s3client.Config) { c.Bucket = "" },
			wantErr: "no bucket",
		},
		{
			name:    "no access key id",
			change:  func(c *s3client.Config) { c.AccessKeyID = "" },
			wantErr: "no access key id",
		},
		{
			name:    "no secret access key",
			change:  func(c *s3client.Config) { c.SecretAccessKey = "" },
			wantErr: "no secret access key",
		},
		{
			name:    "no region",
			change:  func(c *s3client.Config) { c.Region = "" },
			wantErr: "no region",
		},
		{
			name:    "a part size S3 would reject on the second part",
			change:  func(c *s3client.Config) { c.PartSize = 1 * memory.MiB },
			wantErr: "under the 5.0 MiB S3 allows",
		},
		{
			name:    "an endpoint with no scheme",
			change:  func(c *s3client.Config) { c.Endpoint = "s3.example.test" },
			wantErr: "endpoint needs an http or https scheme",
		},
		{
			name:    "a public url with no scheme",
			change:  func(c *s3client.Config) { c.PublicURL = "cdn.example.test" },
			wantErr: "public url needs an http or https scheme",
		},
		{
			name:    "negative concurrency",
			change:  func(c *s3client.Config) { c.Concurrency = -1 },
			wantErr: "concurrency must not be negative",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			config := validConfig()
			tc.change(&config)

			err := config.Validate()
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestNewRefusesANilLogger(t *testing.T) {
	t.Parallel()

	_, err := s3client.New(nil, validConfig())
	require.ErrorContains(t, err, "logger is nil")
}

func TestNewValidatesBeforeDialing(t *testing.T) {
	t.Parallel()

	config := validConfig()
	config.Bucket = ""
	config.VerifyBucket = true

	_, err := s3client.New(discard(), config)
	require.ErrorContains(t, err, "no bucket")
}

func TestZeroSizedTuningTakesTheDefaults(t *testing.T) {
	t.Parallel()

	// A Config built by hand rather than bound from flags has zeroes where the
	// tags would have put defaults; those must not become a zero part size or
	// a zero-length presigned URL.
	config := validConfig()
	require.NoError(t, config.Validate())

	c, err := s3client.New(discard(), config)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, c.Close()) })

	require.Equal(t, "bucket", c.Bucket())
	require.NotNil(t, c.Uploader)
	require.NotNil(t, c.Presign)
	// The part size and concurrency the defaults produce are asserted from the
	// outside, in TestDefaultPartSizeSplitsAtEightMiB: transfermanager keeps
	// its options unexported, and what matters is the request it makes anyway.
}
