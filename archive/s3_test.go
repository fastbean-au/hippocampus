package archive

import (
	"context"
	"testing"
)

// TestNewS3Store_RequiresBucket verifies construction fails fast without a bucket, since every
// Put/Get is bucket-scoped.
func TestNewS3Store_RequiresBucket(t *testing.T) {
	if _, err := NewS3Store(context.Background(), S3Config{}); err == nil {
		t.Error("expected an error when s3.bucket is unset")
	}
}

// TestNewS3Store_BuildsClient verifies a configured store constructs (the default AWS config chain
// does not reach the network) and satisfies the ObjectStore contract. Region is set so the config
// chain never blocks looking one up.
func TestNewS3Store_BuildsClient(t *testing.T) {
	store, err := NewS3Store(context.Background(), S3Config{
		Bucket:       "archive-bucket",
		Region:       "us-east-1",
		Endpoint:     "http://localhost:9000",
		UsePathStyle: true,
	})
	if err != nil {
		t.Fatalf("NewS3Store: %s", err)
	}

	if store.bucket != "archive-bucket" {
		t.Errorf("expected bucket 'archive-bucket', got %q", store.bucket)
	}

	var _ ObjectStore = store
}
