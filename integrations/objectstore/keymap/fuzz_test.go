package keymap

import "testing"

// FuzzMemoryIdRoundTrips (TODO-3 item 167): the id IS the contract between the tap, the reaper and
// the producer - the reaper turns an id back into the object to delete - so every id MemoryId issues
// must map back to exactly the bucket and key it came from.
func FuzzMemoryIdRoundTrips(f *testing.F) {
	f.Add("payloads", "a/b.json")
	f.Add("payloads", "with space/é.bin")
	f.Add("b", "/leading")
	f.Add("bucket.with.dots", "trailing/")

	f.Fuzz(func(t *testing.T, bucket string, key string) {
		id, err := MemoryId(bucket, key)
		if err != nil {
			return
		}

		gotBucket, gotKey, err := Object(id)
		if err != nil {
			t.Fatalf("Object(%q) refused an id MemoryId issued: %s", id, err)
		}

		if gotBucket != bucket || gotKey != key {
			t.Fatalf("(%q, %q) -> %q -> (%q, %q)", bucket, key, id, gotBucket, gotKey)
		}
	})
}
