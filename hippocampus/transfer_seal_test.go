package hippocampus

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/archive"
	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/seal"
	"github.com/fastbean-au/hippocampus/types"
)

// sealTestRing builds a ring sealing under a fresh key, and returns the key.
func sealTestRing(t *testing.T) (*seal.Keyring, []byte) {
	t.Helper()

	key := make([]byte, seal.KeyBytes)

	if _, err := rand.Read(key); err != nil {
		t.Fatalf("drawing a key: %s", err)
	}

	ring, err := seal.NewKeyring(key, nil)
	if err != nil {
		t.Fatalf("building a ring: %s", err)
	}

	return ring, key
}

// exportToDirectory exports source's store into a fresh archive directory and returns the archive's
// bytes on disk, the object store and the key.
func exportToDirectory(t *testing.T, source *Server) ([]byte, *archive.FileStore, string) {
	t.Helper()

	directory := filepath.Join(t.TempDir(), "archives")

	objects, err := archive.NewFileStore(directory)
	if err != nil {
		t.Fatalf("NewFileStore: %s", err)
	}

	source.objects = objects

	exported, err := source.Export(context.Background(), &contract.ExportRequest{})
	if err != nil {
		t.Fatalf("Export: %s", err)
	}

	data, err := os.ReadFile(filepath.Join(directory, filepath.FromSlash(exported.GetObjectKey())))
	if err != nil {
		t.Fatalf("reading the archive: %s", err)
	}

	return data, objects, exported.GetObjectKey()
}

// TestExportSealsTheArchive is the archive half of TODO-3 item 195: with a ring, the archive on disk
// is a sealed stream that does not carry the bodies, and it imports into an instance holding the key
// with every body intact.
func TestExportSealsTheArchive(t *testing.T) {
	t.Parallel()

	ring, key := sealTestRing(t)

	source := newTransferTestServer(t, nil)
	source.keyring = ring

	if _, err := source.db.CreateMemory(context.Background(), types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, Body: "the launch code is swordfish"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	data, objects, objectKey := exportToDirectory(t, source)

	if !seal.IsSealedStream(data) {
		t.Fatal("the exported archive is not a sealed stream")
	}

	if bytes.Contains(data, []byte("swordfish")) {
		t.Error("the sealed archive carries a body in the clear")
	}

	// A different instance holding the key only as retired - the state after a rotation.
	readOnly, err := seal.NewKeyring(nil, [][]byte{key})
	if err != nil {
		t.Fatalf("building the read-only ring: %s", err)
	}

	target := newTransferTestServer(t, nil)
	target.objects = objects
	target.keyring = readOnly

	imported, err := target.Import(context.Background(), &contract.ImportRequest{ObjectKey: objectKey})
	if err != nil {
		t.Fatalf("Import: %s", err)
	}

	if imported.GetMemoriesImported() != 1 {
		t.Fatalf("imported %d memories, want 1", imported.GetMemoriesImported())
	}

	memories, err := target.db.GetMemoriesByIds(context.Background(), []string{"m1"})
	if err != nil || len(*memories) != 1 || (*memories)[0].Body != "the launch code is swordfish" {
		t.Fatalf("the imported memory reads %+v, %v", memories, err)
	}
}

// TestImportRefusesAnArchiveUnderAnUnknownKey is the failure an operator can act on: an instance
// without the archive's key says so, as a precondition, rather than as an internal error.
func TestImportRefusesAnArchiveUnderAnUnknownKey(t *testing.T) {
	t.Parallel()

	ring, _ := sealTestRing(t)

	source := newTransferTestServer(t, nil)
	source.keyring = ring

	seedTransferFixture(t, source)

	_, objects, objectKey := exportToDirectory(t, source)

	for name, keyring := range map[string]*seal.Keyring{"no ring": nil, "another key": func() *seal.Keyring {
		other, _ := sealTestRing(t)

		return other
	}()} {
		target := newTransferTestServer(t, nil)
		target.objects = objects
		target.keyring = keyring

		_, err := target.Import(context.Background(), &contract.ImportRequest{ObjectKey: objectKey})
		if status.Code(err) != codes.FailedPrecondition {
			t.Errorf("%s: got %v, want FailedPrecondition", name, err)
		}
	}
}

// TestUnsealedArchiveImportsWithEncryptionOn is the upgrade case: an archive written before
// encryption was enabled still imports into an instance that seals, and its bodies are sealed as
// they land.
func TestUnsealedArchiveImportsWithEncryptionOn(t *testing.T) {
	t.Parallel()

	source := newTransferTestServer(t, nil)

	seedTransferFixture(t, source)

	data, objects, objectKey := exportToDirectory(t, source)

	if seal.IsSealedStream(data) {
		t.Fatal("an instance without a ring wrote a sealed archive")
	}

	ring, _ := sealTestRing(t)

	target := newTransferTestServer(t, nil)
	target.objects = objects
	target.keyring = ring

	imported, err := target.Import(context.Background(), &contract.ImportRequest{ObjectKey: objectKey})
	if err != nil {
		t.Fatalf("Import: %s", err)
	}

	if imported.GetEventsImported() != 2 || imported.GetMemoriesImported() != 3 {
		t.Errorf("expected 2 events and 3 memories imported, got %v", imported)
	}
}

// TestImportRefusesATruncatedSealedArchive covers an archive that announces itself as sealed and
// ends inside its header: the import fails, and not as the FailedPrecondition a missing key gets,
// since adding a key would not help.
func TestImportRefusesATruncatedSealedArchive(t *testing.T) {
	t.Parallel()

	ring, _ := sealTestRing(t)

	objects, err := archive.NewFileStore(filepath.Join(t.TempDir(), "archives"))
	if err != nil {
		t.Fatalf("NewFileStore: %s", err)
	}

	if err := objects.Put(context.Background(), "truncated.hca", bytes.NewReader([]byte("HIPSEAL\x01"))); err != nil {
		t.Fatalf("Put: %s", err)
	}

	target := newTransferTestServer(t, nil)
	target.objects = objects
	target.keyring = ring

	_, err = target.Import(context.Background(), &contract.ImportRequest{ObjectKey: "truncated.hca"})
	if err == nil || status.Code(err) == codes.FailedPrecondition {
		t.Errorf("got %v, want a failure other than FailedPrecondition", err)
	}
}
