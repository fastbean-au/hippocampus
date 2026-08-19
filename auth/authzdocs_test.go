package auth

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/fastbean-au/hippocampus/contract"
)

// The tier table in docs/configuration.md is a second copy of the policies table above it in
// authz.go, and it is the copy an operator reads before deciding which token to hand out. Its
// failure mode is omission rather than contradiction: a row listing eleven of thirteen reader RPCs
// reads as complete, so the two the table lost - GetMemoryLinks and GetEventLinks, and with them
// the four link writes - looked like RPCs with no policy at all rather than RPCs whose policy was
// simply not written down.
//
// TestEveryRPCHasAPolicy already holds `policies` to the service descriptor. This holds the
// documentation to `policies`, so the chain runs from the contract to the page.

// tierRowPattern matches one row of that table: the tier in inline code, then the RPCs it may call.
var tierRowPattern = regexp.MustCompile("^\\|\\s*`(reader|writer|admin)`\\s*\\|(.*)\\|")

// documentedRPCPattern picks the RPC names out of a row.
var documentedRPCPattern = regexp.MustCompile("`([A-Z][A-Za-z]+)`")

func TestTierTableMatchesThePolicies(t *testing.T) {
	documented := documentedTiers(t)

	for rpc, policy := range policies {
		tier, present := documented[rpc]

		switch {

		case !present:
			t.Errorf("RPC '%s' is missing from the tier table in docs/configuration.md; its "+
				"policy is '%s'", rpc, policy.tier)

		case tier != policy.tier:
			t.Errorf("RPC '%s' is documented as '%s' but its policy is '%s'", rpc, tier, policy.tier)

		}
	}

	for rpc := range documented {
		if _, present := policies[rpc]; !present {
			t.Errorf("the tier table in docs/configuration.md carries '%s', which has no policy - "+
				"it is either misspelled or no longer an RPC", rpc)
		}
	}
}

// documentedTiers reads the table, resolving each row against the ones above it: the writer and
// admin rows say "everything `reader` can, plus ...", so a tier's set is its own row plus every
// lower row's.
func documentedTiers(t *testing.T) map[string]Tier {
	t.Helper()

	source, err := os.ReadFile(filepath.Join("..", "docs", "configuration.md"))
	if err != nil {
		t.Fatalf("failed to read the configuration guide: %s", err.Error())
	}

	tiers := map[string]Tier{}
	names := map[string]Tier{"reader": TierReader, "writer": TierWriter, "admin": TierAdmin}

	for _, line := range strings.Split(string(source), "\n") {
		row := tierRowPattern.FindStringSubmatch(line)
		if row == nil {
			continue
		}

		tier := names[row[1]]

		for _, match := range documentedRPCPattern.FindAllStringSubmatch(row[2], -1) {
			// The service descriptor is what decides whether a capitalised word in the row is an
			// RPC: the prose in these cells names types and codes as well.
			if !isRPC(match[1]) {
				continue
			}

			tiers[match[1]] = tier
		}
	}

	if len(tiers) == 0 {
		t.Fatal("found no tier table rows in docs/configuration.md - the table's shape changed")
	}

	return tiers
}

// isRPC answers whether a name is a method of the service, so the reader can tell an RPC in a table
// cell from any other capitalised word in inline code.
func isRPC(name string) bool {
	for _, method := range contract.Hippocampus_ServiceDesc.Methods {
		if method.MethodName == name {
			return true
		}
	}

	return false
}
