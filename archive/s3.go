package archive

import (
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	log "github.com/sirupsen/logrus"
)

// ObjectStore is the object-storage contract Export and Import depend on: opaque byte streams
// keyed by name. Satisfied by S3Store; tests use an in-memory fake.
type ObjectStore interface {
	Put(ctx context.Context, key string, body io.Reader) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
}

// S3Config carries the object-store connection settings, read from viper in main.go. Credentials
// come from the standard AWS chain (environment, shared config, instance roles); Endpoint and
// UsePathStyle exist for S3-compatible stores such as MinIO.
type S3Config struct {
	Endpoint     string
	Region       string
	Bucket       string
	UsePathStyle bool
}

// S3Store is the AWS S3 (and S3-compatible) ObjectStore. Uploads stream through the transfer
// manager's multipart uploader, so an archive is never buffered whole in memory — the embedded
// deployments this exists for are the ones that can least afford that.
type S3Store struct {
	client   *s3.Client
	uploader *transfermanager.Client
	bucket   string
}

// NewS3Store builds the client from the default AWS configuration chain plus the given settings.
func NewS3Store(ctx context.Context, cfg S3Config) (*S3Store, error) {
	log.Trace("func() archive.NewS3Store")

	if cfg.Bucket == "" {
		return nil, fmt.Errorf("s3.bucket must be configured")
	}

	opts := []func(*awsconfig.LoadOptions) error{}
	if cfg.Region != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.Region))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS configuration: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}

		o.UsePathStyle = cfg.UsePathStyle
	})

	return &S3Store{
		client:   client,
		uploader: transfermanager.New(client),
		bucket:   cfg.Bucket,
	}, nil
}

func (s *S3Store) Put(ctx context.Context, key string, body io.Reader) error {
	log.Trace("func() archive.S3Store.Put")

	if _, err := s.uploader.UploadObject(ctx, &transfermanager.UploadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   body,
	}); err != nil {
		return fmt.Errorf("failed to upload object '%s': %w", key, err)
	}

	return nil
}

func (s *S3Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	log.Trace("func() archive.S3Store.Get")

	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch object '%s': %w", key, err)
	}

	return out.Body, nil
}

// Compile-time check that *S3Store satisfies ObjectStore.
var _ ObjectStore = (*S3Store)(nil)
