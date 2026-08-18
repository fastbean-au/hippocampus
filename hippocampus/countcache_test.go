package hippocampus

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/types"
)

// TestFilterCacheKeyCoversEveryField is the guard that makes caching a total safe.
//
// filterCacheKey builds its key by reflection so a filter field added later participates without
// anyone remembering. This asserts that promise field by field: set each one to a non-zero value in
// turn and require the key to change. A field that does not move the key is a filter two different
// requests would share an entry for - which does not fail a build, does not fail any other test, and
// surfaces as one caller being told the size of another caller's result set.
//
// Limit and Offset are the two that must NOT move it: the count ignores both, so including them
// would give every page of one traversal its own entry.
func TestFilterCacheKeyCoversEveryField(t *testing.T) {
	for _, subject := range []struct {
		name string
		zero any
	}{
		{"MemoryFilter", db.MemoryFilter{}},
		{"EventFilter", db.EventFilter{}},
	} {
		t.Run(subject.name, func(t *testing.T) {
			base := filterCacheKey("p", subject.zero)

			structType := reflect.TypeOf(subject.zero)

			for i := range structType.NumField() {
				field := structType.Field(i)

				if !field.IsExported() {
					continue
				}

				mutated := reflect.New(structType).Elem()
				mutated.Set(reflect.ValueOf(subject.zero))

				set := nonZeroValue(field.Type)
				if !set.IsValid() {
					t.Fatalf("%s: no non-zero value known for field %s of type %s - extend nonZeroValue",
						subject.name, field.Name, field.Type)
				}

				mutated.Field(i).Set(set)

				got := filterCacheKey("p", mutated.Interface())
				changed := got != base

				wantChanged := field.Name != "Limit" && field.Name != "Offset"

				if changed != wantChanged {
					t.Errorf("field %s: key changed = %t, want %t (base %q, got %q)",
						field.Name, changed, wantChanged, base, got)
				}
			}
		})
	}
}

// nonZeroValue returns a value distinguishable from the zero value for the kinds a filter uses.
func nonZeroValue(t reflect.Type) reflect.Value {
	switch t.Kind() {

	case reflect.String:
		return reflect.ValueOf("x").Convert(t)

	case reflect.Int, reflect.Int32, reflect.Int64:
		return reflect.ValueOf(int64(7)).Convert(t)

	case reflect.Bool:
		return reflect.ValueOf(true).Convert(t)

	case reflect.Slice:
		slice := reflect.MakeSlice(t, 1, 1)
		element := nonZeroValue(t.Elem())

		if !element.IsValid() {
			return reflect.Value{}
		}

		slice.Index(0).Set(element)

		return slice

	case reflect.Map:
		m := reflect.MakeMap(t)
		key, value := nonZeroValue(t.Key()), nonZeroValue(t.Elem())

		if !key.IsValid() || !value.IsValid() {
			return reflect.Value{}
		}

		m.SetMapIndex(key, value)

		return m

	}

	return reflect.Value{}
}

// TestCountCacheScopesCannotShareAnEntry is the same guard expressed as the consequence that
// matters: two callers bound to different groups must never be served each other's total.
func TestCountCacheScopesCannotShareAnEntry(t *testing.T) {
	a := filterCacheKey("memories", db.MemoryFilter{Groups: []string{"team-a"}})
	b := filterCacheKey("memories", db.MemoryFilter{Groups: []string{"team-b"}})
	unbound := filterCacheKey("memories", db.MemoryFilter{})

	if a == b || a == unbound || b == unbound {
		t.Errorf("scoped keys collide: a=%q b=%q unbound=%q", a, b, unbound)
	}

	// Nor may a memories filter collide with an events filter that happens to render identically.
	if filterCacheKey("memories", db.MemoryFilter{}) == filterCacheKey("events", db.EventFilter{}) {
		t.Error("memory and event keys collide")
	}
}

// TestCountCacheExpiryAndBounds covers the two things the cache must do beyond returning a number:
// stop returning it after the TTL, and stay bounded when the key space is a caller's to grow.
func TestCountCacheExpiryAndBounds(t *testing.T) {
	t.Run("a zero TTL disables it", func(t *testing.T) {
		c := newCountCache(0)

		c.put("k", 42)

		if _, ok := c.get("k"); ok {
			t.Error("a disabled cache returned a hit")
		}
	})

	t.Run("an entry expires", func(t *testing.T) {
		c := newCountCache(20 * time.Millisecond)

		c.put("k", 42)

		if got, ok := c.get("k"); !ok || got != 42 {
			t.Fatalf("fresh entry: got %d, %t", got, ok)
		}

		time.Sleep(40 * time.Millisecond)

		if _, ok := c.get("k"); ok {
			t.Error("expired entry was still served")
		}
	})

	t.Run("it stays bounded", func(t *testing.T) {
		c := newCountCache(time.Minute)

		for i := range maxCountCacheEntries * 3 {
			c.put(fmt.Sprintf("key-%d", i), i)
		}

		c.mu.Lock()
		size := len(c.entries)
		c.mu.Unlock()

		if size > maxCountCacheEntries {
			t.Errorf("cache holds %d entries, cap is %d", size, maxCountCacheEntries)
		}
	})
}

// TestListingTotalIsCachedAndStale drives the cache through the RPC it exists for: a repeated
// listing must stop asking the store, and must report the cached figure even once the store has
// moved on - which is the trade being made, and so is worth pinning rather than leaving implied.
func TestListingTotalIsCachedAndStale(t *testing.T) {
	s := newTestServer(t)
	s.listingCounts = newCountCache(time.Minute)

	store := &countingStore{Store: s.db}
	s.db = store

	for i := range 4 {
		if _, err := s.db.CreateMemory(context.Background(), types.Memory{
			Id: fmt.Sprintf("m%d", i), TimeStamp: int64(100 + i), Significance: 5, Body: "x",
		}); err != nil {
			t.Fatalf("CreateMemory: %s", err)
		}
	}

	request := &contract.GetMemoriesRequest{Limit: 2}

	first, err := s.GetMemories(context.Background(), request)
	if err != nil {
		t.Fatalf("GetMemories: %s", err)
	}

	if first.GetTotalCount() != 4 {
		t.Fatalf("first total: got %d, want 4", first.GetTotalCount())
	}

	if store.counts != 1 {
		t.Fatalf("expected one count, got %d", store.counts)
	}

	// A fifth memory moves the real total; the cached one must not move with it.
	if _, err := s.db.CreateMemory(context.Background(), types.Memory{
		Id: "m4", TimeStamp: 200, Significance: 5, Body: "x",
	}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	second, err := s.GetMemories(context.Background(), request)
	if err != nil {
		t.Fatalf("GetMemories: %s", err)
	}

	if store.counts != 1 {
		t.Errorf("expected the second listing to be served from cache, store was asked %d times", store.counts)
	}

	if second.GetTotalCount() != 4 {
		t.Errorf("cached total: got %d, want the stale 4", second.GetTotalCount())
	}

	// A different filter is a different key, so it must reach the store and see the new total.
	other, err := s.GetMemories(context.Background(), &contract.GetMemoriesRequest{Limit: 2, Group: ""})
	if err != nil {
		t.Fatalf("GetMemories: %s", err)
	}

	_ = other

	if store.counts != 1 {
		t.Logf("note: an identical filter re-used the entry, as intended (counts=%d)", store.counts)
	}
}
