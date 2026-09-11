// Package objects is the object-storage half of this integration: the handful of operations the
// gateway and the reaper need, behind an interface small enough to fake in a test.
//
// It is deliberately not archive.ObjectStore. That interface is Put/Get over opaque byte streams -
// what an export needs - while this one presigns, lists and deletes, and never writes. Sharing the
// name would have meant one interface serving two components that have no operation in common.
package objects

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	log "github.com/sirupsen/logrus"
)

// ErrNotFound reports an object the store does not hold. Delete never returns it - see S3Store.
var ErrNotFound = errors.New("object not found")

// Object is one enumerated object: what the sweep needs to judge it, and nothing else.
type Object struct {
	Key      string
	Size     int64
	Modified time.Time
}

// Reader is an object's content plus the response metadata the gateway forwards when it is
// proxying rather than redirecting.
//
// ContentRange is set only for a ranged read, and its presence is what tells the gateway to answer
// 206 rather than 200 - the status is derived from what the store actually returned rather than
// from what the client asked for, since a store may legitimately ignore a range it cannot satisfy
// the way it was asked.
type Reader struct {
	Body          io.ReadCloser
	ContentType   string
	ContentLength int64
	ContentRange  string
	ETag          string
	LastModified  time.Time
}

// GetOptions carries the parts of the client's request that have to reach the store.
//
// Range exists because proxy mode would otherwise break every client that asks for part of an
// object: answering a range request with the whole body and a 200 is not a degraded response, it is
// a wrong one, and media players and resumable downloads are exactly the traffic somebody puts a
// bucket behind a gateway for. In redirect mode the question never arises - the client asks the
// store directly.
type GetOptions struct {
	Range string
}

// Store is the object-storage surface this integration uses.
//
// Delete must be IDEMPOTENT: deleting an object that is not there is a success, not an error. That
// is what makes at-least-once delivery of a forget-instruction correct, and it is why the callback
// receiver can return 500 and have the whole delivery replayed without a second thought.
type Store interface {
	// Bucket names the bucket, which is half of every memory id this integration mints.
	Bucket() string

	// Presign returns a URL granting a GET of one object for ttl.
	Presign(ctx context.Context, key string, ttl time.Duration) (string, error)

	// Get opens an object for reading. The caller closes Reader.Body.
	Get(ctx context.Context, key string, opts GetOptions) (*Reader, error)

	// Delete removes an object. An absent object is a success.
	Delete(ctx context.Context, key string) error

	// List calls fn for every object under prefix, in whatever order the store enumerates. An error
	// from fn stops the walk and is returned.
	List(ctx context.Context, prefix string, fn func(Object) error) error

	// Ping reports whether the bucket is reachable, for the readiness probe.
	Ping(ctx context.Context) error
}

// Config carries the connection settings, mirroring the service's own s3.* keys so an operator
// configures a bucket the same way here as there. Credentials come from the standard AWS chain
// (environment, shared config, instance roles); Endpoint and UsePathStyle exist for S3-compatible
// stores such as MinIO.
type Config struct {
	Endpoint     string
	Region       string
	Bucket       string
	UsePathStyle bool
}

// S3Store is the AWS S3 (and S3-compatible) implementation.
type S3Store struct {
	client   *s3.Client
	presign  *s3.PresignClient
	bucket   string
	pageSize int32
}

// listPageSize is the S3 maximum, and the sweep wants it: every page is a round trip, and the
// per-page cost on this side is one ExplainConsolidation call per 200 ids either way.
const listPageSize = 1000

// NewS3Store builds the client from the default AWS configuration chain plus cfg.
func NewS3Store(ctx context.Context, cfg Config) (*S3Store, error) {
	log.Trace("func() objects.NewS3Store")

	if cfg.Bucket == "" {
		return nil, fmt.Errorf("a bucket must be configured")
	}

	opts := []func(*awsconfig.LoadOptions) error{}
	if cfg.Region != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.Region))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("loading AWS configuration: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}

		o.UsePathStyle = cfg.UsePathStyle
	})

	return &S3Store{
		client:   client,
		presign:  s3.NewPresignClient(client),
		bucket:   cfg.Bucket,
		pageSize: listPageSize,
	}, nil
}

func (s *S3Store) Bucket() string {
	return s.bucket
}

// Presign returns a URL that grants a GET of one object until ttl elapses.
//
// Presigning is what lets the gateway be a chokepoint without being a bottleneck: the reader is
// redirected straight at the store, so no payload byte passes through this process, while the
// request that asked for the URL is still a read this integration saw and can reinforce.
func (s *S3Store) Presign(ctx context.Context, key string, ttl time.Duration) (string, error) {
	log.Trace("func() objects.S3Store.Presign")

	req, err := s.presign.PresignGetObject(ctx,
		&s3.GetObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(key),
		},
		s3.WithPresignExpires(ttl),
	)
	if err != nil {
		return "", fmt.Errorf("presigning %q: %w", key, err)
	}

	return req.URL, nil
}

func (s *S3Store) Get(ctx context.Context, key string, opts GetOptions) (*Reader, error) {
	log.Trace("func() objects.S3Store.Get")

	input := &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}

	if opts.Range != "" {
		input.Range = aws.String(opts.Range)
	}

	out, err := s.client.GetObject(ctx, input)
	if err != nil {
		if absent(err) {
			return nil, fmt.Errorf("%w: %q", ErrNotFound, key)
		}

		return nil, fmt.Errorf("reading %q: %w", key, err)
	}

	reader := &Reader{
		Body:          out.Body,
		ContentLength: aws.ToInt64(out.ContentLength),
		ContentType:   aws.ToString(out.ContentType),
		ContentRange:  aws.ToString(out.ContentRange),
		ETag:          aws.ToString(out.ETag),
	}

	if out.LastModified != nil {
		reader.LastModified = *out.LastModified
	}

	return reader, nil
}

// Delete removes one object, reporting an absent one as a success.
//
// S3 answers a DELETE of a key it does not hold with a 204 exactly as it answers one it did, so
// this is idempotent without any effort on our part - which is the property the whole delivery
// model rests on. The absent() guard is kept for the S3-compatible stores that are less faithful.
func (s *S3Store) Delete(ctx context.Context, key string) error {
	log.Trace("func() objects.S3Store.Delete")

	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}); err != nil {
		if absent(err) {
			return nil
		}

		return fmt.Errorf("deleting %q: %w", key, err)
	}

	return nil
}

func (s *S3Store) List(ctx context.Context, prefix string, fn func(Object) error) error {
	log.Trace("func() objects.S3Store.List")

	input := &s3.ListObjectsV2Input{
		Bucket:  aws.String(s.bucket),
		MaxKeys: aws.Int32(s.pageSize),
	}

	if prefix != "" {
		input.Prefix = aws.String(prefix)
	}

	pager := s3.NewListObjectsV2Paginator(s.client, input)

	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("listing %q: %w", s.bucket, err)
		}

		for _, v := range page.Contents {
			object := Object{
				Key:  aws.ToString(v.Key),
				Size: aws.ToInt64(v.Size),
			}

			if v.LastModified != nil {
				object.Modified = *v.LastModified
			}

			if err := fn(object); err != nil {
				return err
			}
		}
	}

	return nil
}

// Ping reports whether the bucket answers. HeadBucket rather than a list: it is the cheapest call
// that proves both reachability and access, and a readiness probe must not enumerate anything.
func (s *S3Store) Ping(ctx context.Context) error {
	if _, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)}); err != nil {
		return fmt.Errorf("reaching bucket %q: %w", s.bucket, err)
	}

	return nil
}

// absent reports whether err is the store saying it does not hold the object.
func absent(err error) bool {
	var noSuchKey *types.NoSuchKey
	if errors.As(err, &noSuchKey) {
		return true
	}

	var notFound *types.NotFound

	return errors.As(err, &notFound)
}

// Compile-time check that S3Store satisfies Store.
var _ Store = (*S3Store)(nil)
