package objects

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"
)

// Memory is an in-memory Store, for tests and for reading: it is the shortest possible statement of
// what the interface promises, including the one behaviour that is easy to get wrong - Delete
// reports an absent object as a success.
//
// It lives in the package rather than in a _test.go file because both the gateway and the reaper
// need one, and a test double copied into two packages is a test double that drifts.
type Memory struct {
	mu     sync.Mutex
	bucket string
	items  map[string]memoryItem

	// The error fields let a test drive the failure paths, which for this integration are most of
	// the interesting ones.
	PresignErr error
	GetErr     error
	DeleteErr  error
	ListErr    error

	// Deleted records every key Delete was called with, in order, so a test can assert on what an
	// armed reaper actually did as well as on its counts.
	Deleted []string
}

type memoryItem struct {
	body        []byte
	contentType string
	modified    time.Time
}

// NewMemory builds an empty in-memory bucket.
func NewMemory(bucket string) *Memory {
	return &Memory{
		bucket: bucket,
		items:  map[string]memoryItem{},
	}
}

// Put adds or replaces an object.
func (m *Memory) Put(key string, body []byte, modified time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.items[key] = memoryItem{
		body:        body,
		contentType: "application/octet-stream",
		modified:    modified,
	}
}

// Has reports whether the object is still there.
func (m *Memory) Has(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	_, ok := m.items[key]

	return ok
}

// Len reports how many objects remain.
func (m *Memory) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return len(m.items)
}

func (m *Memory) Bucket() string {
	return m.bucket
}

func (m *Memory) Presign(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if m.PresignErr != nil {
		return "", m.PresignErr
	}

	return fmt.Sprintf("https://example.invalid/%s/%s?expires=%d", m.bucket, key, int64(ttl.Seconds())), nil
}

func (m *Memory) Get(ctx context.Context, key string, opts GetOptions) (*Reader, error) {
	if m.GetErr != nil {
		return nil, m.GetErr
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	item, ok := m.items[key]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, key)
	}

	reader := &Reader{
		Body:          io.NopCloser(bytes.NewReader(item.body)),
		ContentLength: int64(len(item.body)),
		ContentType:   item.contentType,
		LastModified:  item.modified,
	}

	// Just enough range handling to prove the gateway forwards one: a "bytes=N-" suffix request,
	// which is what a resumed download sends.
	if opts.Range != "" {
		var start int

		if _, err := fmt.Sscanf(opts.Range, "bytes=%d-", &start); err != nil || start > len(item.body) {
			return nil, fmt.Errorf("unsatisfiable range %q", opts.Range)
		}

		reader.Body = io.NopCloser(bytes.NewReader(item.body[start:]))
		reader.ContentLength = int64(len(item.body) - start)
		reader.ContentRange = fmt.Sprintf("bytes %d-%d/%d", start, len(item.body)-1, len(item.body))
	}

	return reader, nil
}

// Delete removes an object, reporting an absent one as a success - which is the property that makes
// at-least-once delivery of a forget-instruction correct.
func (m *Memory) Delete(ctx context.Context, key string) error {
	if m.DeleteErr != nil {
		return m.DeleteErr
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.Deleted = append(m.Deleted, key)

	delete(m.items, key)

	return nil
}

func (m *Memory) List(ctx context.Context, prefix string, fn func(Object) error) error {
	if m.ListErr != nil {
		return m.ListErr
	}

	m.mu.Lock()

	objects := make([]Object, 0, len(m.items))

	for k, v := range m.items {
		if prefix != "" && !bytes.HasPrefix([]byte(k), []byte(prefix)) {
			continue
		}

		objects = append(objects, Object{
			Key:      k,
			Size:     int64(len(v.body)),
			Modified: v.modified,
		})
	}

	m.mu.Unlock()

	// Sorted so a test's expectations are stable; a real store promises no order.
	sort.Slice(objects, func(i int, j int) bool { return objects[i].Key < objects[j].Key })

	for _, v := range objects {
		if err := fn(v); err != nil {
			return err
		}
	}

	return nil
}

func (m *Memory) Ping(ctx context.Context) error {
	return nil
}

// Compile-time check that Memory satisfies Store.
var _ Store = (*Memory)(nil)
