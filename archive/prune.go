package archive

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	log "github.com/sirupsen/logrus"
)

// Both stores this package ships can prune; asserted here so a change to either fails the build
// rather than quietly turning the scheduled export's pruning off.
var (
	_ Pruner = (*FileStore)(nil)
	_ Pruner = (*S3Store)(nil)
)

// Pruner is implemented by an object store that can enumerate and remove what it holds. It is
// separate from ObjectStore because only the scheduled export needs it - to keep its last N archives
// and delete the rest (TODO-3 item 158) - and every other use of a store, including the tests' fakes
// of it, needs nothing but Put and Get. Both stores this package ships implement it.
type Pruner interface {
	// List returns the keys under prefix, sorted.
	List(ctx context.Context, prefix string) ([]string, error)

	// Delete removes one object. Deleting a key that does not exist is not an error.
	Delete(ctx context.Context, key string) error
}

// List walks the archive directory for keys under prefix. The temporary files an in-flight Put
// writes are skipped, since they are not archives and one is renamed into place or removed the
// moment its Put returns.
func (f *FileStore) List(ctx context.Context, prefix string) ([]string, error) {
	log.Trace("func() archive.FileStore.List")

	var keys []string

	err := filepath.WalkDir(f.dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".archive-") {
			return nil
		}

		relative, err := filepath.Rel(f.dir, path)
		if err != nil {
			return err
		}

		key := filepath.ToSlash(relative)

		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list the archive directory: %w", err)
	}

	sort.Strings(keys)

	return keys, nil
}

// Delete removes one archive, through the same key resolution Put and Get use, so a key that would
// leave the directory is refused rather than followed.
func (f *FileStore) Delete(ctx context.Context, key string) error {
	log.Trace("func() archive.FileStore.Delete")

	target, err := f.resolve(key)
	if err != nil {
		return err
	}

	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("failed to delete object '%s': %w", key, err)
	}

	return nil
}

// List pages through the bucket for keys under prefix.
func (s *S3Store) List(ctx context.Context, prefix string) ([]string, error) {
	log.Trace("func() archive.S3Store.List")

	var keys []string

	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list s3://%s/%s: %w", s.bucket, prefix, err)
		}

		for _, object := range page.Contents {
			keys = append(keys, aws.ToString(object.Key))
		}
	}

	sort.Strings(keys)

	return keys, nil
}

// Delete removes one object. S3 reports success for a key that does not exist, which is the
// contract Pruner asks for.
func (s *S3Store) Delete(ctx context.Context, key string) error {
	log.Trace("func() archive.S3Store.Delete")

	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}); err != nil {
		return fmt.Errorf("failed to delete s3://%s/%s: %w", s.bucket, key, err)
	}

	return nil
}
