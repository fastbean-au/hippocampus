package contract_test

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/fastbean-au/hippocampus/contract"
)

// undocumentedContract names the messages and fields of hippocampus.proto that still carry no
// comment (TODO-3 item 174). The comments are what a client author reads - protoc copies them into
// every generated client and into the OpenAPI document - so the list may only shrink: a new message
// or field must arrive with its comment, and an entry here that has gained one must be removed.
var undocumentedContract = map[string]bool{}

var (
	protoScopeOpen = regexp.MustCompile(`^\s*(message|enum|oneof|service)\s+(\w+)\s*\{`)
	protoField     = regexp.MustCompile(`^\s*(?:repeated\s+|optional\s+)?(?:map<[^>]+>|[\w.]+)\s+(\w+)\s*=\s*\d+\s*(?:\[[^\]]*\])?\s*;(.*)$`)
)

// protoComments scans hippocampus.proto and reports, for every message and field, whether it is
// documented: by a // comment block directly above it, or by a trailing // comment on its line. It
// is a line scanner rather than a parser, which is why TestContractCommentScanSeesTheDescriptor
// holds what it finds to the compiled descriptor - a scanner that missed a field would otherwise
// pass it silently.
func protoComments(t *testing.T) map[string]bool {
	t.Helper()

	source, err := os.ReadFile("hippocampus.proto")
	if err != nil {
		t.Fatalf("reading hippocampus.proto: %s", err)
	}

	type scope struct {
		kind string
		name string
	}

	documented := map[string]bool{}

	var scopes []scope

	commented := false

	for _, line := range strings.Split(string(source), "\n") {
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "//") {
			commented = true

			continue
		}

		if match := protoScopeOpen.FindStringSubmatch(line); match != nil {
			kind, name := match[1], match[2]

			if kind == "message" {
				var path []string

				for _, s := range scopes {
					if s.kind == "message" {
						path = append(path, s.name)
					}
				}

				documented["hippocampus.v1."+strings.Join(append(path, name), ".")] = commented
			}

			scopes = append(scopes, scope{kind: kind, name: name})
			commented = false

			continue
		}

		if strings.HasPrefix(trimmed, "}") && len(scopes) > 0 {
			scopes = scopes[:len(scopes)-1]
			commented = false

			continue
		}

		if len(scopes) > 0 && scopes[len(scopes)-1].kind != "enum" && scopes[len(scopes)-1].kind != "service" {
			if match := protoField.FindStringSubmatch(line); match != nil {
				var path []string

				for _, s := range scopes {
					if s.kind == "message" {
						path = append(path, s.name)
					}
				}

				name := "hippocampus.v1." + strings.Join(append(path, match[1]), ".")
				documented[name] = commented || strings.Contains(match[2], "//")
			}
		}

		commented = false
	}

	return documented
}

// descriptorNames is every message and field the compiled contract declares, by full name.
func descriptorNames() map[string]bool {
	names := map[string]bool{}

	var walk func(messages protoreflect.MessageDescriptors)

	walk = func(messages protoreflect.MessageDescriptors) {
		for i := range messages.Len() {
			message := messages.Get(i)

			if message.IsMapEntry() {
				continue
			}

			names[string(message.FullName())] = true

			for j := range message.Fields().Len() {
				names[string(message.Fields().Get(j).FullName())] = true
			}

			walk(message.Messages())
		}
	}

	walk(contract.File_hippocampus_proto.Messages())

	return names
}

// TestContractCommentScanSeesTheDescriptor is what makes the line scanner trustworthy: it must find
// exactly the messages and fields the compiled descriptor has, no more and no fewer.
func TestContractCommentScanSeesTheDescriptor(t *testing.T) {
	scanned := protoComments(t)
	compiled := descriptorNames()

	for name := range compiled {
		if _, ok := scanned[name]; !ok {
			t.Errorf("the descriptor declares %s, which the comment scan did not find", name)
		}
	}

	for name := range scanned {
		if !compiled[name] {
			t.Errorf("the comment scan found %s, which the descriptor does not declare", name)
		}
	}
}

// TestContractIsCommented fails on a message or field with no comment that is not on the
// allow-list, and on an allow-list entry that has since been commented or removed.
func TestContractIsCommented(t *testing.T) {
	scanned := protoComments(t)

	var missing []string

	for name, documented := range scanned {
		if !documented && !undocumentedContract[name] {
			missing = append(missing, name)
		}
	}

	sort.Strings(missing)

	for _, name := range missing {
		t.Errorf("%s has no comment: document it in hippocampus.proto", name)
	}

	for name := range undocumentedContract {
		documented, ok := scanned[name]

		switch {

		case !ok:
			t.Errorf("%s is on the undocumented allow-list but no longer exists - remove it", name)

		case documented:
			t.Errorf("%s is on the undocumented allow-list but is now commented - remove it", name)

		}
	}
}

// TestContractSaysAuthorisation holds the contract's comments to the project's spelling. The comments
// reach every generated client and the OpenAPI document; the Authorization HEADER keeps its name,
// being a protocol term, so only the prose phrase is refused.
func TestContractSaysAuthorisation(t *testing.T) {
	source, err := os.ReadFile("hippocampus.proto")
	if err != nil {
		t.Fatalf("reading hippocampus.proto: %s", err)
	}

	for i, line := range strings.Split(string(source), "\n") {
		if strings.Contains(strings.ToLower(line), "authorization tier") {
			t.Errorf("hippocampus.proto:%d says %q: the project spells it authorisation", i+1, strings.TrimSpace(line))
		}
	}
}
