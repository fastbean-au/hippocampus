package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/proto"

	"github.com/fastbean-au/hippocampus/contract"
)

// Two of the documentation's tables are copies of a table that lives in the code - the instruments
// the service exports, and the gateway route each RPC answers on. Nothing executes either copy, so
// both drift in the one way that is invisible to a reader: silently and plausibly. A metric name
// with an underscore where the instrument has a dot (docs/operations.md carried five) returns no
// series rather than an error, and an RPC missing from the route table is a feature nobody can
// reach over HTTP because nothing said it was there.
//
// The guards below hold both to their source. They are the same shape as the configuration-key
// guards in configkeys_test.go and the alert-rule guards in alerts_test.go: read what the code
// declares, read what the documentation claims, and require the two to agree in both directions.
// The reverse direction is the half that matters most here - a name only the documentation carries
// is one a reader will type into a query and get nothing back from.

var (
	// A metric name as the documentation writes it: inline code, dotted, OTLP form.
	documentedMetricPattern = regexp.MustCompile("`(hippocampus\\.[a-z0-9_.]+)`")

	// The same name as a PromQL expression carries it, which is how the documentation's example
	// queries and the shipped alert rules name it.
	promQLMetricPattern = regexp.MustCompile(`\bhippocampus_[a-z0-9_]+\b`)

	// A row of the RPC-to-route table in docs/configuration.md.
	routeTableRowPattern = regexp.MustCompile("^\\|\\s*`(\\w+)`\\s*\\|\\s*(\\w+)\\s*\\|\\s*`([^`]+)`\\s*\\|")
)

// clientInstrumentFiles declare instruments that are exported by something other than the service
// itself, and documented on that component's page rather than in the operations guide. They are
// separate from metricSourceFiles because the alert rules deliberately cover the service alone.
var clientInstrumentFiles = []string{
	"../../observability/clientmetrics.go",
	"../../integrations/eventsource/bridge/telemetry.go",
	"../../integrations/ingestor/promoter/telemetry.go",
}

// nonInstrumentNames are `hippocampus.*` names the documentation carries that are not instruments,
// with the reason, in whichever of the two spellings the documentation writes them. A file name is
// excluded by its extension rather than by an entry here; what is left is the handful of names that
// read exactly like a metric and are not.
var nonInstrumentNames = map[string]string{
	"hippocampus.group":    "the tenancy resource attribute stamped by observability.WithGroup, not an instrument",
	"hippocampus_group":    "the same attribute, as a PromQL label",
	"hippocampus.v1":       "the proto package, as it appears in a fully qualified method name",
	"hippocampus_pb2":      "a generated Python module in the client-codegen walkthrough",
	"hippocampus_pb2_grpc": "as above",
}

// fileNameSuffixes are the extensions that make a dotted `hippocampus.x` token a file rather than a
// metric - hippocampus.db, hippocampus.service, hippocampus.yaml and friends.
var fileNameSuffixes = map[string]bool{
	"db": true, "lock": true, "proto": true, "env": true, "service": true, "yaml": true,
	"yml": true, "json": true, "png": true, "plist": true, "sh": true, "md": true, "go": true,
	"ts": true, "js": true, "css": true, "html": true, "sock": true, "log": true,
}

// TestEveryInstrumentIsDocumented requires each declared instrument to be named somewhere in
// docs/. An instrument nobody wrote down is a series an operator meets for the first time on a
// dashboard, with no way to learn what it counts or which setting has to be on for it to appear.
func TestEveryInstrumentIsDocumented(t *testing.T) {
	documentation := documentationText(t)

	for _, name := range declaredInstruments(t) {
		if !strings.Contains(documentation, name) {
			t.Errorf("instrument '%s' is not named anywhere in docs/ - add it to the instrument "+
				"list in docs/operations.md, or to the component's own page", name)
		}
	}
}

// TestDocumentedMetricsAreDeclared is the reverse, and the direction that caught the sweep's
// finding: a name the documentation carries that no instrument declares. It checks both spellings
// the documentation uses - the dotted OTLP name in prose, and the underscored Prometheus rendering
// in an example query.
func TestDocumentedMetricsAreDeclared(t *testing.T) {
	declared := make(map[string]bool)
	underscored := make(map[string]bool)

	for _, name := range declaredInstruments(t) {
		declared[name] = true
		underscored[strings.ReplaceAll(name, ".", "_")] = true
	}

	for path, source := range documentationFiles(t) {
		for _, match := range documentedMetricPattern.FindAllStringSubmatch(source, -1) {
			name := match[1]

			segments := strings.Split(name, ".")
			if fileNameSuffixes[segments[len(segments)-1]] {
				continue
			}

			if _, allowed := nonInstrumentNames[name]; allowed {
				continue
			}

			if !declared[name] {
				t.Errorf("%s names the metric '%s', which no instrument declares - check the "+
					"spelling against the declaration (a dot is not an underscore)",
					filepath.Base(path), name)
			}
		}

		for _, match := range promQLMetricPattern.FindAllString(source, -1) {
			if _, allowed := nonInstrumentNames[match]; allowed {
				continue
			}

			if !declaresSeries(underscored, match) {
				t.Errorf("%s queries the series '%s', which no instrument declares",
					filepath.Base(path), match)
			}
		}
	}
}

// declaresSeries answers whether a series name in an example query belongs to a declared
// instrument. The OTLP-to-Prometheus translation renames on ingest, and a document may name either
// the base series or one of its renderings, so each suffix the translation adds is peeled off in
// turn rather than guessed at: hippocampus.sleeps is queried as hippocampus_sleeps_total, and
// hippocampus.sleep.duration as hippocampus_sleep_duration_seconds (or _bucket, _sum, _count).
func declaresSeries(declared map[string]bool, series string) bool {
	candidates := []string{series, instrumentName(series)}

	for _, suffix := range []string{"_total", "_bucket", "_sum", "_count"} {
		candidates = append(candidates, strings.TrimSuffix(series, suffix))
	}

	for _, candidate := range candidates {
		if declared[candidate] || declared[strings.TrimSuffix(candidate, "_seconds")] {
			return true
		}
	}

	return false
}

// TestNoStaleNonInstrumentNames rejects an exception naming something the documentation no longer
// mentions, so the list cannot outlive its subjects.
func TestNoStaleNonInstrumentNames(t *testing.T) {
	documentation := documentationText(t)

	for name, reason := range nonInstrumentNames {
		if !strings.Contains(documentation, name) {
			t.Errorf("'%s' is excused from the instrument check ('%s') but appears nowhere in "+
				"docs/ - remove the exception", name, reason)
		}
	}
}

// TestRouteTableMatchesTheContract holds docs/configuration.md's RPC-to-route table to the
// google.api.http annotations the gateway is actually generated from. The table is how a reader
// discovers the HTTP surface at all - the OpenAPI document is served by a running instance, which
// is no help to somebody deciding whether to run one - so an RPC missing from it is a route that
// exists and cannot be found.
func TestRouteTableMatchesTheContract(t *testing.T) {
	documented := documentedRoutes(t)

	for rpc, route := range contractRoutes(t) {
		switch actual, present := documented[rpc]; {

		case !present:
			t.Errorf("RPC '%s' is missing from the route table in docs/configuration.md (it "+
				"answers %s %s)", rpc, route.method, route.path)

		case actual != route:
			t.Errorf("RPC '%s' is documented as %s %s but the contract maps it to %s %s",
				rpc, actual.method, actual.path, route.method, route.path)

		}
	}

	for rpc := range documented {
		if _, present := contractRoutes(t)[rpc]; !present {
			t.Errorf("the route table in docs/configuration.md carries '%s', which is not an RPC "+
				"of the service", rpc)
		}
	}
}

type route struct {
	method string
	path   string
}

// contractRoutes reads each RPC's google.api.http annotation from the generated descriptor, which
// is the same source the gateway's own routing is generated from.
func contractRoutes(t *testing.T) map[string]route {
	t.Helper()

	services := contract.File_hippocampus_proto.Services()
	routes := make(map[string]route)

	for i := range services.Len() {
		methods := services.Get(i).Methods()

		for j := range methods.Len() {
			method := methods.Get(j)

			rule, ok := proto.GetExtension(method.Options(), annotations.E_Http).(*annotations.HttpRule)
			if !ok || rule == nil {
				continue
			}

			verb, path := httpRule(rule)
			if verb == "" {
				continue
			}

			routes[string(method.Name())] = route{method: verb, path: path}
		}
	}

	if len(routes) == 0 {
		t.Fatal("found no google.api.http annotations - the contract or the extension changed")
	}

	return routes
}

// httpRule unpacks the one pattern an HttpRule carries.
func httpRule(rule *annotations.HttpRule) (string, string) {
	switch pattern := rule.GetPattern().(type) {

	case *annotations.HttpRule_Get:
		return "GET", pattern.Get

	case *annotations.HttpRule_Post:
		return "POST", pattern.Post

	case *annotations.HttpRule_Put:
		return "PUT", pattern.Put

	case *annotations.HttpRule_Patch:
		return "PATCH", pattern.Patch

	case *annotations.HttpRule_Delete:
		return "DELETE", pattern.Delete

	}

	return "", ""
}

// documentedRoutes reads the RPC-to-route table. The table is identified by its rows rather than by
// its heading: every row names an RPC in inline code, a bare verb, and a path, which no other table
// in the file does.
func documentedRoutes(t *testing.T) map[string]route {
	t.Helper()

	source, err := os.ReadFile(filepath.Join("..", "..", "docs", "configuration.md"))
	if err != nil {
		t.Fatalf("failed to read the configuration guide: %s", err.Error())
	}

	routes := make(map[string]route)

	for _, line := range strings.Split(string(source), "\n") {
		match := routeTableRowPattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}

		if !strings.HasPrefix(match[3], "/v1") {
			continue
		}

		routes[match[1]] = route{method: match[2], path: match[3]}
	}

	if len(routes) == 0 {
		t.Fatal("found no route table rows in docs/configuration.md - the table's shape changed")
	}

	return routes
}

// declaredInstruments reads every instrument name the repository declares, service-side and
// client-side alike, in the dotted form OTLP carries.
func declaredInstruments(t *testing.T) []string {
	t.Helper()

	names := make(map[string]bool)

	for _, path := range append(append([]string{}, metricSourceFiles...), clientInstrumentFiles...) {
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("failed to read the instrument declarations in %s: %s", path, err.Error())
		}

		for _, match := range instrumentPattern.FindAllStringSubmatch(string(source), -1) {
			names[match[1]] = true
		}
	}

	if len(names) == 0 {
		t.Fatal("found no instrument declarations - the pattern no longer matches the telemetry")
	}

	declared := make([]string, 0, len(names))
	for name := range names {
		declared = append(declared, name)
	}

	return declared
}

// documentationFiles reads docs/ plus the two documents at the root that describe behaviour.
func documentationFiles(t *testing.T) map[string]string {
	t.Helper()

	paths, err := filepath.Glob(filepath.Join("..", "..", "docs", "*.md"))
	if err != nil {
		t.Fatalf("failed to list the documentation: %s", err.Error())
	}

	paths = append(paths, filepath.Join("..", "..", "README.md"))
	sources := make(map[string]string, len(paths))

	for _, path := range paths {
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("failed to read %s: %s", path, err.Error())
		}

		sources[path] = string(source)
	}

	return sources
}

// documentationText is the same corpus as one string, for a plain containment check.
func documentationText(t *testing.T) string {
	t.Helper()

	var builder strings.Builder

	for _, source := range documentationFiles(t) {
		builder.WriteString(source)
	}

	return builder.String()
}
