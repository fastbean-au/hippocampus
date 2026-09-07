package archive

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	log "github.com/sirupsen/logrus"
)

// FileStore is the filesystem ObjectStore: an archive is a file under a configured directory.
//
// It exists because the archive format is the only thing that preserves a store's full state -
// timestamps, recall history, groups, summary flags, links - and until it landed, reaching that
// format at all required standing up a bucket or a MinIO. That is a disproportionate prerequisite
// on a product whose default deployment is one static binary and a directory, and it left the
// offline backup, the air-gapped move, and the archive somebody keeps *because the store is
// designed to forget* all behind infrastructure.
//
// Two things it does that S3 gets for free.
//
// A write is atomic. Put streams into a temporary file beside its destination and renames it into
// place, so an export interrupted half way leaves nothing rather than a truncated file - which
// Import would otherwise accept as an archive and fail part way through, having already upserted
// whatever it managed to read.
//
// A key cannot escape the directory. Over S3 a key is an object name and a caller-supplied one is
// merely rude; over a filesystem it is a path, and Import takes its object_key straight from the
// request. Keys are therefore validated before they are joined (see resolve), which is what makes
// "../../etc/passwd" a request that is refused rather than one that is quietly answered about a
// different file. The directory itself is assumed to be server-owned: containment is lexical, so a
// symlink planted inside it by something else would still be followed.
type FileStore struct {
	dir string
}

// NewFileStore prepares the directory and returns the store. The directory is created if it is
// missing - the same courtesy the storage directory gets - so a first export does not fail on a
// path the operator has only named.
func NewFileStore(dir string) (*FileStore, error) {
	log.Trace("func() archive.NewFileStore")

	if dir == "" {
		return nil, fmt.Errorf("archive.directory must be configured")
	}

	absolute, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve the archive directory '%s': %w", dir, err)
	}

	if err := os.MkdirAll(absolute, 0o750); err != nil {
		return nil, fmt.Errorf("failed to create the archive directory '%s': %w", absolute, err)
	}

	info, err := os.Stat(absolute)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect the archive directory '%s': %w", absolute, err)
	}

	if !info.IsDir() {
		return nil, fmt.Errorf("the archive directory '%s' is not a directory", absolute)
	}

	return &FileStore{dir: absolute}, nil
}

// Directory reports where archives are written, for the startup log and the topology view.
func (f *FileStore) Directory() string {
	return f.dir
}

func (f *FileStore) Put(ctx context.Context, key string, body io.Reader) error {
	log.Trace("func() archive.FileStore.Put")

	target, err := f.resolve(key)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return fmt.Errorf("failed to create the directory for object '%s': %w", key, err)
	}

	temporary, err := os.CreateTemp(filepath.Dir(target), ".archive-*.partial")
	if err != nil {
		return fmt.Errorf("failed to create a temporary file for object '%s': %w", key, err)
	}

	// Every failure below this point has to remove the temporary file, and the successful path
	// renames it away, so removing it unconditionally is both correct and the only way to be sure
	// a failed export leaves nothing behind.
	defer func() {
		_ = os.Remove(temporary.Name())
	}()

	if _, err := io.Copy(temporary, &contextReader{ctx: ctx, reader: body}); err != nil {
		_ = temporary.Close()

		return fmt.Errorf("failed to write object '%s': %w", key, err)
	}

	// The archive is only durable once the bytes are on the device: a rename is atomic with
	// respect to readers, not with respect to power loss.
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()

		return fmt.Errorf("failed to flush object '%s': %w", key, err)
	}

	if err := temporary.Close(); err != nil {
		return fmt.Errorf("failed to close object '%s': %w", key, err)
	}

	if err := os.Chmod(temporary.Name(), 0o640); err != nil {
		return fmt.Errorf("failed to set permissions on object '%s': %w", key, err)
	}

	if err := os.Rename(temporary.Name(), target); err != nil {
		return fmt.Errorf("failed to store object '%s': %w", key, err)
	}

	return nil
}

func (f *FileStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	log.Trace("func() archive.FileStore.Get")

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	target, err := f.resolve(key)
	if err != nil {
		return nil, err
	}

	file, err := os.Open(target)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch object '%s': %w", key, err)
	}

	return file, nil
}

// Ping reports whether the archive directory is still there and still a directory, for the
// deployment topology view. It is deliberately a read: the alternative - writing a probe file
// every probe interval to prove the directory is writable - would be the only probe in the set
// that mutates what it is probing, and a directory that has become read-only is a rarer fault
// than one that has been unmounted underneath the process.
func (f *FileStore) Ping(ctx context.Context) error {
	log.Trace("func() archive.FileStore.Ping")

	if err := ctx.Err(); err != nil {
		return err
	}

	info, err := os.Stat(f.dir)
	if err != nil {
		return fmt.Errorf("failed to reach the archive directory '%s': %w", f.dir, err)
	}

	if !info.IsDir() {
		return fmt.Errorf("the archive directory '%s' is no longer a directory", f.dir)
	}

	return nil
}

// resolve turns an object key into a path inside the directory, or refuses it.
//
// It refuses rather than sanitises, which is the decision worth recording. Anchoring the key and
// cleaning it - the usual trick - is safe, in that "../../etc/passwd" then names a file called
// "etc/passwd" inside the archive directory and nothing escapes. But it answers a request to read
// /etc/passwd with the contents of some other file, and answers a request to write there by
// writing somewhere else, both silently. A key that was never going to name the object its author
// meant should fail, and be visible in the log as a refusal.
//
// The strictness is also what keeps the two backends agreeing: over S3 a leading slash makes a
// different key, and a segment of ".." is a literal segment rather than a traversal, so any key
// this accepts names the same object on either store.
func (f *FileStore) resolve(key string) (string, error) {
	if key == "" {
		return "", fmt.Errorf("object key must not be empty")
	}

	// A backslash is an ordinary character to a POSIX filesystem and a separator to a Windows one,
	// so a key carrying one would mean two different things; a NUL cannot reach a syscall at all.
	if strings.ContainsAny(key, "\\\x00") {
		return "", fmt.Errorf("object key '%s' contains a character that is not allowed in a key", key)
	}

	if strings.HasPrefix(key, "/") || filepath.IsAbs(key) || filepath.VolumeName(key) != "" {
		return "", fmt.Errorf("object key '%s' must be relative to the archive directory", key)
	}

	for _, segment := range strings.Split(key, "/") {
		switch segment {

		case "", ".", "..":
			return "", fmt.Errorf("object key '%s' is not a well-formed key", key)

		}
	}

	target := filepath.Join(f.dir, filepath.FromSlash(key))

	// The segment check above already makes this unreachable. It stays because the thing being
	// asserted is a security property, and an assertion is cheaper than the next reader having to
	// re-derive that the loop above is exhaustive.
	if !strings.HasPrefix(target, f.dir+string(filepath.Separator)) {
		return "", fmt.Errorf("object key '%s' resolves outside the archive directory", key)
	}

	return target, nil
}

// contextReader lets a long Put honour the calling RPC's deadline: io.Copy has no context, and an
// export of a large store is exactly the operation whose caller has given up before it finishes.
type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (c *contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}

	return c.reader.Read(p)
}

// Compile-time check that *FileStore satisfies ObjectStore.
var _ ObjectStore = (*FileStore)(nil)
