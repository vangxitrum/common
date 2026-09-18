package s3client

import (
	"fmt"
	"net/url"
	"time"

	"github.com/spacemonkeygo/monkit/v3"

	"gitlab.internal/tuan.quang.tran/aioz-common/httpclient"
	"gitlab.internal/tuan.quang.tran/aioz-common/memory"
)

var mon = monkit.Package()

// The servers [New] knows how to talk to.
const (
	// CompatAIOZ applies the workarounds our own S3-compatible service needs:
	// path-style addressing, checksums only where the protocol requires one,
	// and a Content-MD5 on DeleteObjects.
	CompatAIOZ = "aioz"

	// CompatAWS takes the SDK's defaults, for real AWS S3 or anything else
	// current enough to accept the flexible checksums it now sends.
	CompatAWS = "aws"
)

// The smallest part S3 accepts for a multipart upload. Anything under it is
// rejected on the second part, after the first has already been sent.
const minPartSize = 5 * memory.MiB

const (
	defaultPartSize      = 8 * memory.MiB
	defaultConcurrency   = 4
	defaultPresignExpiry = 15 * time.Minute
	defaultMaxAttempts   = 4
	defaultRetryWaitMax  = 10 * time.Second
)

// Config configures the client.
//
// One Config is one bucket. Every service we have uses exactly one, and making
// the bucket a per-call argument instead would put it in ten signatures to
// serve nobody.
type Config struct {
	Compat string `help:"which server this talks to: aioz applies our own service's compatibility workarounds; aws takes the SDK defaults" default:"aioz"`

	Endpoint string `help:"S3 endpoint, e.g. https://s3-storage.attoaioz.cyou; empty talks to AWS for the region" default:""`
	Region   string `help:"region sent with every request; our own service ignores it, but SigV4 needs one"      default:"us-east-1"`
	Bucket   string `help:"bucket every key is relative to; required"                                            default:""`

	AccessKeyID     string `help:"access key id; required"     secret:"optional"`
	SecretAccessKey string `help:"secret access key; required" secret:"optional"`

	PublicURL string `help:"base URL the objects are already readable at; set it and PresignGet returns <public-url>/<key> and signs nothing" default:""`

	PartSize      memory.Size   `help:"multipart part size; 5MiB is the smallest S3 allows, and part-size times concurrency is what one upload holds in memory" default:"8MiB"`
	Concurrency   int           `help:"parts uploaded in parallel per object"                                                                                  default:"4"`
	PresignExpiry time.Duration `help:"how long a presigned URL is valid when the caller asks for no particular lifetime"                                       default:"15m"`

	VerifyBucket bool `help:"HeadBucket at startup, so a wrong bucket or a key without access to it fails next to its configuration" default:"true"`

	MaxAttempts  int           `help:"attempts per request, the first included" default:"4"`
	RetryWaitMax time.Duration `help:"longest wait between attempts"            default:"10s"`

	Client httpclient.Config
}

// Validate reports a configuration that cannot work, at startup rather than at
// the first request.
func (config Config) Validate() error {
	switch config.Compat {
	case CompatAIOZ, CompatAWS:
	default:
		return fmt.Errorf("s3client: compat %q is neither %q nor %q",
			config.Compat, CompatAIOZ, CompatAWS)
	}

	if config.Bucket == "" {
		return fmt.Errorf("s3client: no bucket")
	}
	if config.AccessKeyID == "" {
		return fmt.Errorf("s3client: no access key id")
	}
	if config.SecretAccessKey == "" {
		return fmt.Errorf("s3client: no secret access key")
	}
	if config.Region == "" {
		return fmt.Errorf("s3client: no region; SigV4 signs with one even where the service ignores it")
	}

	if config.Concurrency < 0 {
		return fmt.Errorf("s3client: concurrency must not be negative")
	}
	if config.PartSize < 0 {
		return fmt.Errorf("s3client: part size must not be negative")
	}
	if config.PartSize > 0 && config.PartSize < minPartSize {
		return fmt.Errorf(
			"s3client: part size %s is under the %s S3 allows",
			config.PartSize, minPartSize,
		)
	}
	if config.MaxAttempts < 0 {
		return fmt.Errorf("s3client: max attempts must not be negative")
	}

	if err := validateURL("endpoint", config.Endpoint); err != nil {
		return err
	}
	return validateURL("public url", config.PublicURL)
}

// validateURL accepts an empty value and otherwise insists on an absolute
// http or https URL, because "s3-storage.attoaioz.cyou" without a scheme
// parses fine and then signs against an empty host.
func validateURL(name, raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("s3client: %s is not a URL: %w", name, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf(
			"s3client: %s needs an http or https scheme, got %q", name, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("s3client: %s has no host", name)
	}
	return nil
}
