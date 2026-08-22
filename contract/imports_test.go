package contract_test

import (
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/fastbean-au/hippocampus/contract"
)

// vendoredImports are the proto files hippocampus.proto may import: the google/api annotations that
// generate the HTTP gateway, the protoc-gen-openapiv2 options that carry the OpenAPI document's
// securityDefinitions, and the files those two in turn import. Each is vendored beside the contract
// at the path given here; an empty path means the file is bundled with protoc and every language's
// protobuf runtime (everything under google/protobuf/), so it needs no vendoring.
var vendoredImports = map[string]string{
	"google/api/annotations.proto":                   "google/api/annotations.proto",
	"google/api/http.proto":                          "google/api/http.proto",
	"protoc-gen-openapiv2/options/annotations.proto": "protoc-gen-openapiv2/options/annotations.proto",
	"protoc-gen-openapiv2/options/openapiv2.proto":   "protoc-gen-openapiv2/options/openapiv2.proto",
	"google/protobuf/descriptor.proto":               "",
	"google/protobuf/struct.proto":                   "",
}

// TestContractImportsStayVendored holds docs/clients.md's client-generation recipe to the contract.
// That recipe tells a client author in any language that pointing an include path at contract/ is
// enough to compile hippocampus.proto, because its only import is google/api/annotations.proto and
// the googleapis files that reach are vendored beside it. A new import of some other googleapis
// file - google/type/date.proto, say - would break every documented recipe at the generation step,
// in a way nothing else in the repo notices: the Go build keeps working, since the Go module
// resolves those imports through its own dependencies.
//
// Adding an import is a legitimate change; it just has to come with the file vendored beside the
// contract and listed here - or, for one every protobuf runtime bundles (anything under
// google/protobuf/), listed here with no vendored path.
func TestContractImportsStayVendored(t *testing.T) {
	imports := contract.File_hippocampus_proto.Imports()

	for i := 0; i < imports.Len(); i++ {
		imported := imports.Get(i)

		assertVendored(t, imported.FileDescriptor)
	}
}

// assertVendored checks one imported file, then recurses into its own imports - a vendored file is
// only usable if whatever it imports resolves too.
func assertVendored(t *testing.T, file protoreflect.FileDescriptor) {
	t.Helper()

	path := file.Path()

	vendored, known := vendoredImports[path]
	if !known {
		t.Errorf("hippocampus.proto (transitively) imports %q, which docs/clients.md's generation "+
			"recipe does not account for: vendor it beside the contract (under contract/google/ or "+
			"contract/protoc-gen-openapiv2/) and amend the recipe, or add it here if the client "+
			"toolchains all bundle it", path)

		return
	}

	if vendored != "" {
		if _, err := os.Stat(filepath.FromSlash(vendored)); err != nil {
			t.Errorf("%q is imported but not vendored at contract/%s: %v", path, vendored, err)
		}
	}

	imports := file.Imports()

	for i := 0; i < imports.Len(); i++ {
		assertVendored(t, imports.Get(i).FileDescriptor)
	}
}
