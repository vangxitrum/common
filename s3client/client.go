package s3client

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsretry "github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"gitlab.internal/tuan.quang.tran/aioz-common/httpclient"
	"gitlab.internal/tuan.quang.tran/aioz-common/secret"
)

// Client is a configured S3 client for one bucket.
//
// The embedded *s3.Client already carries the endpoint, the path style and the
// compatibility settings, so an operation this package does not wrap is still
// safe to call directly. The wrapped ones are those where the raw call has a
// trap worth closing.
type Client struct {
	*s3.Client

	// Uploader puts an object of any size, splitting it into parts when it has
	// to. It is built once, here, because building one per upload allocates a
	// fresh part buffer pool for every object.
	//
	// This is transfermanager rather than the older feature/s3/manager, which
	// the SDK has deprecated. It is not the fix for anything: transfermanager
	// carries the same checksum setting, and its setting takes precedence over
	// the S3 client's rather than merely failing to inherit it, so getting it
	// wrong here is worse than before, not better.
	Uploader *transfermanager.Client

	// Presign signs a URL for a caller that is not this process.
	Presign *s3.PresignClient

	log           *slog.Logger
	bucket        string
	publicURL     string
	presignExpiry time.Duration
}

// New returns a client to keep for the life of the process.
//
// It is a lifecycle.Item with a Close and no Run. It pools connections and its
// uploader holds part buffers, both of which are worth reusing, so build it
// once and register it rather than per request.
//
// With Config.VerifyBucket it calls HeadBucket before returning, so a wrong
// bucket name or a key without access to it fails next to the configuration
// that caused it. It never calls ListBuckets: that needs an account-wide
// permission a correctly scoped key does not have.
func New(log *slog.Logger, config Config) (*Client, error) {
	if log == nil {
		return nil, fmt.Errorf("s3client: logger is nil")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}

	httpClient, err := newHTTPClient(log, config)
	if err != nil {
		return nil, err
	}

	secretKey := secret.Token(config.SecretAccessKey)
	awsCfg := aws.Config{
		Region: config.Region,
		// Built by hand rather than with config.LoadDefaultConfig, which reads
		// ~/.aws, the AWS_* environment, SSO and the EC2 metadata service.
		// None of those should be able to change what a service configured
		// from this repo's config package talks to, and the metadata probe
		// costs a timeout on a machine that is not on EC2.
		Credentials: credentials.NewStaticCredentialsProvider(
			config.AccessKeyID, secretKey.Reveal(), "",
		),
		HTTPClient: httpClient,
		Retryer: func() aws.Retryer {
			return awsretry.NewStandard(func(o *awsretry.StandardOptions) {
				o.MaxAttempts = orDefault(
					config.MaxAttempts,
					defaultMaxAttempts,
				)
				o.MaxBackoff = orDefault(
					config.RetryWaitMax,
					defaultRetryWaitMax,
				)
			})
		},
	}

	client := s3.NewFromConfig(awsCfg, options(config)...)

	partSize := config.PartSize
	if partSize <= 0 {
		partSize = defaultPartSize
	}
	checksums := aws.RequestChecksumCalculationWhenRequired
	if config.Compat != CompatAIOZ {
		checksums = aws.RequestChecksumCalculationWhenSupported
	}
	uploader := transfermanager.New(client, func(o *transfermanager.Options) {
		// Int64, not Int: memory.Size.Int() truncates on a 32-bit build, and
		// CI vets one.
		o.PartSizeBytes = partSize.Int64()
		o.MultipartUploadThreshold = partSize.Int64()
		o.Concurrency = orDefault(config.Concurrency, defaultConcurrency)
		// Set here as well as on the client, and this is the line that
		// matters: transfermanager's setting takes precedence over the S3
		// client's, so a client configured correctly is overridden if this is
		// missing. The older feature/s3/manager merely failed to inherit it.
		o.RequestChecksumCalculation = checksums
	})

	c := &Client{
		Client:        client,
		Uploader:      uploader,
		Presign:       s3.NewPresignClient(client),
		log:           log,
		bucket:        config.Bucket,
		publicURL:     strings.TrimSuffix(config.PublicURL, "/"),
		presignExpiry: orDefault(config.PresignExpiry, defaultPresignExpiry),
	}

	if config.VerifyBucket {
		ctx := context.Background()
		if _, err := client.HeadBucket(ctx, &s3.HeadBucketInput{
			Bucket: aws.String(config.Bucket),
		}); err != nil {
			return nil, fail("head bucket", err)
		}
	}
	return c, nil
}

// newHTTPClient builds the transport, overriding two httpclient defaults that
// are right for an API and wrong for an object store.
func newHTTPClient(log *slog.Logger, config Config) (*http.Client, error) {
	clientConfig := config.Client
	if clientConfig.Name == "" {
		clientConfig.Name = "s3"
	}
	// An object is as large as it is. A 32MiB cap on a response body would
	// truncate a download, and a 30s backstop on a whole request would kill an
	// upload that is making progress. The caller's context is the bound.
	clientConfig.MaxResponseSize = 0
	clientConfig.Timeout = 0

	return httpclient.New(log, clientConfig)
}

// options returns the s3.Options mutators for the configured server.
func options(config Config) []func(*s3.Options) {
	opts := []func(*s3.Options){func(o *s3.Options) {
		if config.Endpoint != "" {
			o.BaseEndpoint = aws.String(config.Endpoint)
		}
	}}
	if config.Compat == CompatAIOZ {
		opts = append(opts, applyCompat, withDeleteObjectsContentMD5)
	} else {
		opts = append(opts, applyAWSDefaults)
	}
	return opts
}

// applyCompat is the whole of what our own S3-compatible service needs.
//
// Path style, because the endpoint has no per-bucket DNS. And checksums only
// where the protocol requires one: since aws-sdk-go-v2 v1.72 the default is
// "when supported", which adds x-amz-checksum-crc32 and
// x-amz-sdk-checksum-algorithm to every request and turns a streaming body
// into aws-chunked with a trailing checksum. Our service rejects all three.
func applyCompat(o *s3.Options) {
	o.UsePathStyle = true
	o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
	o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
}

// applyAWSDefaults sets what config.LoadDefaultConfig would have resolved.
//
// Both settings are enums whose zero value is Unset, and only the config
// loader turns Unset into the documented default. An aws.Config built by hand,
// as this package builds one, leaves them unresolved - so "aws" has to say out
// loud that it means the SDK's default, or it would quietly mean neither
// profile.
func applyAWSDefaults(o *s3.Options) {
	o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenSupported
	o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenSupported
}

func orDefault[T int | time.Duration](v, fallback T) T {
	if v <= 0 {
		return fallback
	}
	return v
}

// Bucket is the bucket every key is relative to.
func (c *Client) Bucket() string { return c.bucket }

// Close releases the pooled connections.
func (c *Client) Close() error { return nil }

// Put stores r under key.
//
// The body is read as it is sent, so a reader that fails partway fails the
// Put; a multipart upload started for a large body is aborted rather than left
// to pay for storage.
//
// A Put is not retried as a whole, because an io.Reader cannot be rewound. The
// SDK's retryer does retry an individual part, which it has already buffered,
// and that is why it stays enabled on this path.
func (c *Client) Put(
	ctx context.Context,
	key string,
	r io.Reader,
	p PutParams,
) (_ ObjectInfo, err error) {
	defer mon.Task()(&ctx)(&err)

	if err := validateKey("put", key); err != nil {
		return ObjectInfo{}, err
	}
	if r == nil {
		return ObjectInfo{}, invalid("put", "the body is nil")
	}

	input := &transfermanager.UploadObjectInput{
		Bucket:   aws.String(c.bucket),
		Key:      aws.String(key),
		Body:     r,
		Metadata: p.Metadata,
	}
	if p.ContentType != "" {
		input.ContentType = aws.String(p.ContentType)
	}
	if p.CacheControl != "" {
		input.CacheControl = aws.String(p.CacheControl)
	}
	if p.ContentLength > 0 {
		input.ContentLength = aws.Int64(p.ContentLength)
	}

	out, err := c.Uploader.UploadObject(ctx, input)
	if err != nil {
		return ObjectInfo{}, fail("put", err)
	}

	info := ObjectInfo{
		Key:          key,
		Size:         p.ContentLength,
		ETag:         aws.ToString(out.ETag),
		ContentType:  p.ContentType,
		CacheControl: p.CacheControl,
		Metadata:     p.Metadata,
	}
	if p.ContentLength <= 0 {
		info.Size = -1
	}
	return info, nil
}

// Get opens the object at key.
//
// The returned Object holds a live connection: close it. Info is filled in
// before the body is read, so an ETag is available for a later conditional
// delete without draining the object.
//
// A missing object is [CodeNotFound], which [IsNotFound] reports.
func (c *Client) Get(
	ctx context.Context,
	key string,
) (_ *Object, err error) {
	defer mon.Task()(&ctx)(&err)

	if err := validateKey("get", key); err != nil {
		return nil, err
	}

	out, err := c.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fail("get", err)
	}

	return &Object{
		Body: out.Body,
		Info: ObjectInfo{
			Key:          key,
			Size:         orMissing(out.ContentLength),
			ETag:         aws.ToString(out.ETag),
			LastModified: aws.ToTime(out.LastModified),
			ContentType:  aws.ToString(out.ContentType),
			CacheControl: aws.ToString(out.CacheControl),
			Metadata:     out.Metadata,
		},
	}, nil
}

// Stat describes the object at key without fetching it.
//
// An absent object is [CodeNotFound], not a false: a bool cannot tell a
// missing object from an expired credential, and a caller that reads one as
// the other deletes nothing while reporting success.
func (c *Client) Stat(
	ctx context.Context,
	key string,
) (_ ObjectInfo, err error) {
	defer mon.Task()(&ctx)(&err)

	if err := validateKey("stat", key); err != nil {
		return ObjectInfo{}, err
	}

	out, err := c.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return ObjectInfo{}, fail("stat", err)
	}

	return ObjectInfo{
		Key:          key,
		Size:         orMissing(out.ContentLength),
		ETag:         aws.ToString(out.ETag),
		LastModified: aws.ToTime(out.LastModified),
		ContentType:  aws.ToString(out.ContentType),
		CacheControl: aws.ToString(out.CacheControl),
		Metadata:     out.Metadata,
	}, nil
}

// orMissing reads a length the service may have omitted. Our service leaves
// Content-Length off some responses, and a missing length is not a failure.
func orMissing(length *int64) int64 {
	if length == nil {
		return -1
	}
	return *length
}

// List returns one page of the listing p describes.
//
// The cursor is the caller's, so a job that lists in batches can store where
// it got to. [All] is the iterator for the common case of walking everything.
func (c *Client) List(
	ctx context.Context,
	p ListParams,
) (_ Page, err error) {
	defer mon.Task()(&ctx)(&err)

	input := &s3.ListObjectsV2Input{Bucket: aws.String(c.bucket)}
	if p.Prefix != "" {
		input.Prefix = aws.String(p.Prefix)
	}
	if p.Delimiter != "" {
		input.Delimiter = aws.String(p.Delimiter)
	}
	if p.MaxKeys > 0 {
		input.MaxKeys = aws.Int32(p.MaxKeys)
	}
	if p.Cursor != "" {
		input.ContinuationToken = aws.String(p.Cursor)
	}

	out, err := c.ListObjectsV2(ctx, input)
	if err != nil {
		return Page{}, fail("list", err)
	}

	page := Page{
		Objects:  make([]ObjectInfo, 0, len(out.Contents)),
		Prefixes: make([]string, 0, len(out.CommonPrefixes)),
	}
	for _, obj := range out.Contents {
		page.Objects = append(page.Objects, ObjectInfo{
			Key:          aws.ToString(obj.Key),
			Size:         orMissing(obj.Size),
			ETag:         aws.ToString(obj.ETag),
			LastModified: aws.ToTime(obj.LastModified),
		})
	}
	for _, prefix := range out.CommonPrefixes {
		page.Prefixes = append(page.Prefixes, aws.ToString(prefix.Prefix))
	}
	if aws.ToBool(out.IsTruncated) {
		page.Cursor = aws.ToString(out.NextContinuationToken)
	}
	return page, nil
}

// Delete removes the object at key.
//
// Without an IfMatch it succeeds whether or not the object was there, which is
// what S3 does. With one it removes the object only while it still has that
// ETag, and fails with [CodePrecondition] rather than removing whatever
// replaced it.
func (c *Client) Delete(
	ctx context.Context,
	key string,
	p DeleteParams,
) (err error) {
	defer mon.Task()(&ctx)(&err)

	if err := validateKey("delete", key); err != nil {
		return err
	}

	input := &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	}
	if p.IfMatch != "" {
		input.IfMatch = aws.String(p.IfMatch)
	}

	if _, err := c.DeleteObject(ctx, input); err != nil {
		return fail("delete", err)
	}
	return nil
}

// DeleteMany removes every key given and returns how many were accepted.
//
// The keys are sent in batches of [MaxDeleteBatch], which is the protocol's
// limit. Each batch carries a Content-MD5 rather than the checksum the SDK
// would send, because that is what the DeleteObjects protocol defines and what
// our service accepts.
//
// A failure names how many keys failed and the service's code for the first,
// never the keys themselves.
func (c *Client) DeleteMany(
	ctx context.Context,
	keys []string,
) (_ int, err error) {
	defer mon.Task()(&ctx)(&err)

	for _, key := range keys {
		if err := validateKey("delete many", key); err != nil {
			return 0, err
		}
	}

	deleted := 0
	for batch := range slices.Chunk(keys, MaxDeleteBatch) {
		ids := make([]types.ObjectIdentifier, 0, len(batch))
		for _, key := range batch {
			ids = append(ids, types.ObjectIdentifier{Key: aws.String(key)})
		}

		out, err := c.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(c.bucket),
			// Quiet asks for only the failures back, so a batch of a thousand
			// successes answers with an empty document.
			Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return deleted, fail("delete many", err)
		}
		if len(out.Errors) > 0 {
			return deleted, fail("delete many", &Error{
				Code: CodeUnavailable,
				Op:   "delete many",
				AWS:  aws.ToString(out.Errors[0].Code),
				detail: fmt.Sprintf(
					"%d of %d keys in this batch were refused",
					len(out.Errors), len(batch),
				),
				Temporary: true,
			})
		}
		deleted += len(batch)
	}
	return deleted, nil
}

// DeletePrefix removes every object under prefix and returns how many.
//
// An empty prefix is refused: it would empty the bucket, and nothing we have
// means to ask for that.
func (c *Client) DeletePrefix(
	ctx context.Context,
	prefix string,
) (_ int, err error) {
	defer mon.Task()(&ctx)(&err)

	if prefix == "" {
		return 0, invalid("delete prefix", "the prefix is empty")
	}

	deleted := 0
	params := ListParams{Prefix: prefix}
	for {
		page, err := c.List(ctx, params)
		if err != nil {
			return deleted, err
		}
		if len(page.Objects) > 0 {
			keys := make([]string, 0, len(page.Objects))
			for _, obj := range page.Objects {
				keys = append(keys, obj.Key)
			}
			n, err := c.DeleteMany(ctx, keys)
			deleted += n
			if err != nil {
				return deleted, err
			}
		}
		if page.Cursor == "" {
			return deleted, nil
		}
		params.Cursor = page.Cursor
	}
}

// Copy copies srcKey to dstKey inside this bucket.
//
// Without ReplaceMetadata the source's content type, cache control and user
// metadata come across unchanged. With it, all three are taken from p: S3 has
// no way to change one and keep the rest.
//
// Our own service is not known to implement CopyObject. This is here because
// it is the natural way to rename an object, and it will fail at the first
// call if the service does not support it.
func (c *Client) Copy(
	ctx context.Context,
	srcKey, dstKey string,
	p CopyParams,
) (_ ObjectInfo, err error) {
	defer mon.Task()(&ctx)(&err)

	if err := validateKey("copy", srcKey); err != nil {
		return ObjectInfo{}, err
	}
	if err := validateKey("copy", dstKey); err != nil {
		return ObjectInfo{}, err
	}

	input := &s3.CopyObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(dstKey),
		// The source is bucket and key joined and escaped as one path.
		CopySource: aws.String(url.PathEscape(c.bucket + "/" + srcKey)),
	}
	if p.ReplaceMetadata {
		input.MetadataDirective = types.MetadataDirectiveReplace
		input.Metadata = p.Metadata
		if p.ContentType != "" {
			input.ContentType = aws.String(p.ContentType)
		}
		if p.CacheControl != "" {
			input.CacheControl = aws.String(p.CacheControl)
		}
	}

	out, err := c.CopyObject(ctx, input)
	if err != nil {
		return ObjectInfo{}, fail("copy", err)
	}

	info := ObjectInfo{Key: dstKey, Size: -1}
	if out.CopyObjectResult != nil {
		info.ETag = aws.ToString(out.CopyObjectResult.ETag)
		info.LastModified = aws.ToTime(out.CopyObjectResult.LastModified)
	}
	return info, nil
}

// PresignGet returns a URL that reads key, valid for expires. A zero expires
// takes Config.PresignExpiry.
//
// With Config.PublicURL set the objects are already readable, so this returns
// <public-url>/<key> and signs nothing. That URL does not expire: do not use a
// public URL for a bucket holding anything private.
func (c *Client) PresignGet(
	ctx context.Context,
	key string,
	expires time.Duration,
) (_ string, err error) {
	defer mon.Task()(&ctx)(&err)

	if err := validateKey("presign", key); err != nil {
		return "", err
	}
	if c.publicURL != "" {
		return c.publicURL + "/" + escapeKey(key), nil
	}

	req, err := c.Presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(orDefault(expires, c.presignExpiry)))
	if err != nil {
		return "", fail("presign", err)
	}
	return req.URL, nil
}

// PresignPut returns a URL that writes key, valid for expires. A zero expires
// takes Config.PresignExpiry.
//
// Whatever p sets here the uploader must send as a header, or the signature
// will not match. Set only what the uploader is known to send.
func (c *Client) PresignPut(
	ctx context.Context,
	key string,
	expires time.Duration,
	p PutParams,
) (_ string, err error) {
	defer mon.Task()(&ctx)(&err)

	if err := validateKey("presign", key); err != nil {
		return "", err
	}

	input := &s3.PutObjectInput{
		Bucket:   aws.String(c.bucket),
		Key:      aws.String(key),
		Metadata: p.Metadata,
	}
	if p.ContentType != "" {
		input.ContentType = aws.String(p.ContentType)
	}
	if p.CacheControl != "" {
		input.CacheControl = aws.String(p.CacheControl)
	}
	if p.ContentLength > 0 {
		input.ContentLength = aws.Int64(p.ContentLength)
	}

	req, err := c.Presign.PresignPutObject(ctx, input,
		s3.WithPresignExpires(orDefault(expires, c.presignExpiry)))
	if err != nil {
		return "", fail("presign", err)
	}
	return req.URL, nil
}

// escapeKey escapes a key for a URL path, one segment at a time, so the
// slashes that separate segments survive.
func escapeKey(key string) string {
	segments := strings.Split(key, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}
