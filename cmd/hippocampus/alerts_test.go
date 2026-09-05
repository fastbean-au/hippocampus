package main

import (
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"
)

// The alert rules ship twice: once as a Prometheus rule file for a real deployment, and once as
// Grafana-managed rules provisioned into the bundled otel-lgtm stack. The second file exists only
// because Grafana provisions its own format and cannot read the first - the rules themselves are
// meant to be the same rules, and two copies of a PromQL expression are exactly the kind of thing
// that drifts silently, since neither file is executed by anything in this repo.
//
// These tests hold the pair together, and hold both to the metrics the service actually exports:
// a renamed instrument would otherwise leave an alert that can never fire and looks fine.

const (
	prometheusAlertsPath = "../../deploy/observability/prometheus-alerts.yaml"
	grafanaAlertsPath    = "../../deploy/compose/observability/alerting-rules.yaml"

	// claudeMdPath is checked because CLAUDE.md states how many rules ship, in prose, twice - and
	// prose is the one copy of a fact that nothing evaluates. See
	// TestClaudeMdRuleCountIsCurrent.
	claudeMdPath = "../../CLAUDE.md"

	// grafanaDatasourceUID is the uid otel-lgtm gives its Prometheus, and the one the provisioned
	// dashboard queries. A rule naming anything else provisions cleanly and then fails on every
	// evaluation, so it is worth asserting rather than discovering.
	grafanaDatasourceUID = "prometheus"

	// expressionDatasourceUID marks a server-side expression node rather than a datasource query.
	expressionDatasourceUID = "__expr__"
)

// metricSourceFiles are the files that declare an OTEL instrument. Every metric an alert names must
// come from one of them.
//
// The last three are outside this module - the shared client interceptor, and the two integrations
// whose processes publish the hippocampus-clients rule group's series. They are read as FILES, not
// imported, which is the whole reason a root-module test can hold rules about them: the root module
// deliberately does not depend on integrations/*, and this guard needs the instrument names rather
// than the instruments.
var metricSourceFiles = []string{
	"../../hippocampus/telemetry.go",
	"../../search/telemetry.go",
	"../../stats/stats.go",
	"interceptors.go",
	"ratelimit.go",
	"rpcmetrics.go",
	"../../observability/clientmetrics.go",
	"../../integrations/eventsource/bridge/telemetry.go",
	"../../integrations/ingestor/promoter/telemetry.go",
}

var (
	// instrumentPattern matches the name argument of an instrument constructor. Every declaration
	// in the files above passes the name as a literal, whether to one of the local newXxx helpers
	// or straight to a meter method.
	instrumentPattern = regexp.MustCompile(`"(hippocampus\.[a-z0-9_.]+)"`)

	// metricPattern finds the metrics an alert expression reads, in their Prometheus rendering.
	metricPattern = regexp.MustCompile(`\bhippocampus_[a-z0-9_]+\b`)

	// rangePattern finds the range selectors in an expression, so a Grafana rule's query window can
	// be checked against the widest one it needs.
	rangePattern = regexp.MustCompile(`\[(\d+)([smhd])\]`)
)

type prometheusRuleFile struct {
	Groups []struct {
		Name     string `yaml:"name"`
		Interval string `yaml:"interval"`
		Rules    []struct {
			Alert       string            `yaml:"alert"`
			Expr        string            `yaml:"expr"`
			For         string            `yaml:"for"`
			Labels      map[string]string `yaml:"labels"`
			Annotations map[string]string `yaml:"annotations"`
		} `yaml:"rules"`
	} `yaml:"groups"`
}

type grafanaRuleFile struct {
	APIVersion int `yaml:"apiVersion"`
	Groups     []struct {
		OrgID    int           `yaml:"orgId"`
		Name     string        `yaml:"name"`
		Folder   string        `yaml:"folder"`
		Interval string        `yaml:"interval"`
		Rules    []grafanaRule `yaml:"rules"`
	} `yaml:"groups"`
}

type grafanaRule struct {
	UID          string            `yaml:"uid"`
	Title        string            `yaml:"title"`
	Condition    string            `yaml:"condition"`
	For          string            `yaml:"for"`
	NoDataState  string            `yaml:"noDataState"`
	ExecErrState string            `yaml:"execErrState"`
	Labels       map[string]string `yaml:"labels"`
	Annotations  map[string]string `yaml:"annotations"`
	Data         []struct {
		RefID             string `yaml:"refId"`
		DatasourceUID     string `yaml:"datasourceUid"`
		RelativeTimeRange struct {
			From int `yaml:"from"`
			To   int `yaml:"to"`
		} `yaml:"relativeTimeRange"`
		Model struct {
			RefID      string `yaml:"refId"`
			Expr       string `yaml:"expr"`
			Instant    bool   `yaml:"instant"`
			Type       string `yaml:"type"`
			Expression string `yaml:"expression"`
		} `yaml:"model"`
	} `yaml:"data"`
}

// alert is the part of a rule the two files must agree on: what it is called, what it asks, how
// long it must hold, and how it is labelled and described.
type alert struct {
	expr        string
	forDuration string
	labels      map[string]string
	annotations map[string]string
}

func readPrometheusAlerts(t *testing.T) map[string]alert {
	t.Helper()

	source, err := os.ReadFile(prometheusAlertsPath)
	if err != nil {
		t.Fatalf("failed to read the prometheus alert rules: %s", err.Error())
	}

	var file prometheusRuleFile
	if err := yaml.Unmarshal(source, &file); err != nil {
		t.Fatalf("failed to parse the prometheus alert rules: %s", err.Error())
	}

	alerts := make(map[string]alert)

	for _, group := range file.Groups {
		for _, rule := range group.Rules {
			if _, duplicate := alerts[rule.Alert]; duplicate {
				t.Errorf("alert '%s' is declared twice in the prometheus rules", rule.Alert)
			}

			alerts[rule.Alert] = alert{
				expr:        rule.Expr,
				forDuration: rule.For,
				labels:      rule.Labels,
				annotations: rule.Annotations,
			}
		}
	}

	if len(alerts) == 0 {
		t.Fatal("found no alerts in the prometheus rules - the file no longer parses as expected")
	}

	return alerts
}

func readGrafanaAlerts(t *testing.T) (map[string]alert, map[string]grafanaRule) {
	t.Helper()

	source, err := os.ReadFile(grafanaAlertsPath)
	if err != nil {
		t.Fatalf("failed to read the grafana alert rules: %s", err.Error())
	}

	var file grafanaRuleFile
	if err := yaml.Unmarshal(source, &file); err != nil {
		t.Fatalf("failed to parse the grafana alert rules: %s", err.Error())
	}

	if file.APIVersion != 1 {
		t.Errorf("grafana provisioning apiVersion is %d, expected 1", file.APIVersion)
	}

	alerts := make(map[string]alert)
	rules := make(map[string]grafanaRule)

	for _, group := range file.Groups {
		for _, rule := range group.Rules {
			if _, duplicate := alerts[rule.Title]; duplicate {
				t.Errorf("alert '%s' is declared twice in the grafana rules", rule.Title)
			}

			alerts[rule.Title] = alert{
				expr:        queryExpression(rule),
				forDuration: rule.For,
				labels:      rule.Labels,
				annotations: rule.Annotations,
			}
			rules[rule.Title] = rule
		}
	}

	if len(alerts) == 0 {
		t.Fatal("found no alerts in the grafana rules - the file no longer parses as expected")
	}

	return alerts, rules
}

// queryExpression returns the PromQL of a Grafana rule's datasource query, which is the part that
// must match the Prometheus rule's expr.
func queryExpression(rule grafanaRule) string {
	for _, node := range rule.Data {
		if node.DatasourceUID == expressionDatasourceUID {
			continue
		}

		return node.Model.Expr
	}

	return ""
}

// TestAlertRulesMatchAcrossFiles is the drift guard proper: the two files must describe the same
// alerts, asking the same question over the same window, described the same way.
func TestAlertRulesMatchAcrossFiles(t *testing.T) {
	prometheusAlerts := readPrometheusAlerts(t)
	grafanaAlerts, _ := readGrafanaAlerts(t)

	for name := range prometheusAlerts {
		if _, found := grafanaAlerts[name]; !found {
			t.Errorf("alert '%s' is in the prometheus rules but not the grafana ones", name)
		}
	}

	for name := range grafanaAlerts {
		if _, found := prometheusAlerts[name]; !found {
			t.Errorf("alert '%s' is in the grafana rules but not the prometheus ones", name)
		}
	}

	for name, expected := range prometheusAlerts {
		actual, found := grafanaAlerts[name]
		if !found {
			continue
		}

		if actual.expr != expected.expr {
			t.Errorf("alert '%s' asks a different question in each file:\n  prometheus: %s\n  grafana:    %s",
				name,
				expected.expr,
				actual.expr,
			)
		}

		if actual.forDuration != expected.forDuration {
			t.Errorf("alert '%s' holds for '%s' in the prometheus rules and '%s' in the grafana ones",
				name,
				expected.forDuration,
				actual.forDuration,
			)
		}

		compareStringMaps(t, name, "labels", expected.labels, actual.labels)
		compareStringMaps(t, name, "annotations", expected.annotations, actual.annotations)
	}
}

// TestClaudeMdRuleCountIsCurrent holds the rule count CLAUDE.md states, in words, to the number of
// rules that actually ship.
//
// The paragraph it guards makes exactly this argument about the two YAML files - "two copies of a
// PromQL expression that nothing in the repo executes is exactly what drifts" - and then states the
// count in prose, which is a third copy that nothing executes either. It had drifted by four before
// anybody noticed, in the sentence explaining why drift guards exist.
//
// Both figures are checked: the total, and the size of the hippocampus-clients group, since that one
// reads as "N of the M" and a change to either number invalidates the sentence.
func TestClaudeMdRuleCountIsCurrent(t *testing.T) {
	source, err := os.ReadFile(claudeMdPath)
	if err != nil {
		t.Fatalf("failed to read CLAUDE.md: %s", err.Error())
	}

	claudeMd := string(source)

	total, clients := ruleCounts(t)
	totalWord := numberWord(t, total)
	clientsWord := numberWord(t, clients)

	// Both sentences are quoted with enough of their surroundings that only the real one can match.
	// A bare word would not do: "six" appears eight times in CLAUDE.md and all but one are about
	// something else entirely, so the clients-group check would pass however far it had drifted -
	// a guard that cannot fail being the failure mode this whole file exists to avoid.
	for _, want := range []string{
		"the same " + totalWord + " rules as Grafana-managed rules",
		capitalise(clientsWord) + " of the " + totalWord + " are a second group",
	} {
		if !strings.Contains(claudeMd, want) {
			t.Errorf("CLAUDE.md's alert-rules paragraph does not say %q, but %d rules ship, %d of them in the %s group",
				want,
				total,
				clients,
				clientsGroupName,
			)
		}
	}
}

// numberWord spells a count out, failing loudly rather than silently matching nothing when the count
// grows past the table.
func numberWord(t *testing.T, count int) string {
	t.Helper()

	word, ok := numberWords[count]
	if !ok {
		t.Fatalf("no spelled-out form for %d - extend numberWords", count)
	}

	return word
}

// capitalise upper-cases the first letter, since one of the two sentences starts with its number.
func capitalise(in string) string {
	if in == "" {
		return in
	}

	return strings.ToUpper(in[:1]) + in[1:]
}

// ruleCounts returns how many alert rules ship in total, and how many of them are in the
// hippocampus-clients group. It reads the prometheus file, which TestAlertRulesMatchAcrossFiles
// already holds the grafana one to name for name.
func ruleCounts(t *testing.T) (int, int) {
	t.Helper()

	source, err := os.ReadFile(prometheusAlertsPath)
	if err != nil {
		t.Fatalf("failed to read the prometheus alert rules: %s", err.Error())
	}

	var file prometheusRuleFile
	if err := yaml.Unmarshal(source, &file); err != nil {
		t.Fatalf("failed to parse the prometheus alert rules: %s", err.Error())
	}

	var total, clients int

	for _, group := range file.Groups {
		total += len(group.Rules)

		if group.Name == clientsGroupName {
			clients += len(group.Rules)
		}
	}

	if total == 0 {
		t.Fatal("found no alert rules at all, so the count guard is not guarding anything")
	}

	return total, clients
}

// clientsGroupName is the second rule group: the alerts about the processes that dial the service
// rather than about the service itself.
const clientsGroupName = "hippocampus-clients"

// numberWords spells out the counts CLAUDE.md writes as words. Deliberately a small fixed table
// rather than a general speller - the range that matters is "how many alert rules are there", and a
// count outside it fails loudly above rather than silently matching nothing.
var numberWords = map[int]string{
	1: "one", 2: "two", 3: "three", 4: "four", 5: "five", 6: "six", 7: "seven",
	8: "eight", 9: "nine", 10: "ten", 11: "eleven", 12: "twelve", 13: "thirteen",
	14: "fourteen", 15: "fifteen", 16: "sixteen", 17: "seventeen", 18: "eighteen",
	19: "nineteen", 20: "twenty", 21: "twenty-one", 22: "twenty-two", 23: "twenty-three",
	24: "twenty-four", 25: "twenty-five", 26: "twenty-six", 27: "twenty-seven",
	28: "twenty-eight", 29: "twenty-nine", 30: "thirty", 31: "thirty-one", 32: "thirty-two",
	33: "thirty-three", 34: "thirty-four", 35: "thirty-five", 36: "thirty-six",
	37: "thirty-seven", 38: "thirty-eight", 39: "thirty-nine", 40: "forty",
}

func compareStringMaps(t *testing.T, alertName string, what string, expected map[string]string, actual map[string]string) {
	t.Helper()

	for k, v := range expected {
		if actual[k] != v {
			t.Errorf("alert '%s' %s['%s'] differ:\n  prometheus: %s\n  grafana:    %s", alertName, what, k, v, actual[k])
		}
	}

	for k := range actual {
		if _, found := expected[k]; !found {
			t.Errorf("alert '%s' has %s['%s'] in the grafana rules only", alertName, what, k)
		}
	}
}

// TestAlertRulesReferenceExportedMetrics holds the rules to the instruments the service declares.
// An alert naming a metric that has been renamed is worse than no alert at all: it evaluates
// cleanly, never fires, and reads as coverage.
func TestAlertRulesReferenceExportedMetrics(t *testing.T) {
	exported := exportedMetrics(t)

	prometheusAlerts := readPrometheusAlerts(t)
	grafanaAlerts, _ := readGrafanaAlerts(t)

	for name, rule := range prometheusAlerts {
		checkMetrics(t, "prometheus", name, rule.expr, exported)
	}

	for name, rule := range grafanaAlerts {
		checkMetrics(t, "grafana", name, rule.expr, exported)
	}
}

func checkMetrics(t *testing.T, file string, alertName string, expr string, exported map[string]bool) {
	t.Helper()

	referenced := metricPattern.FindAllString(expr, -1)
	if len(referenced) == 0 {
		t.Errorf("the %s rules' alert '%s' reads no hippocampus metric at all: %s", file, alertName, expr)

		return
	}

	for _, metric := range referenced {
		if !exported[instrumentName(metric)] {
			t.Errorf("the %s rules' alert '%s' reads '%s', which no exported instrument produces (instrument names: %s)",
				file,
				alertName,
				metric,
				strings.Join(sortedKeys(exported), ", "),
			)
		}
	}
}

// instrumentName strips the suffixes the OTLP-to-Prometheus translation adds - `_total` on a
// counter, `_bucket`/`_sum`/`_count` on a histogram, and the unit before those - so a queried
// series name can be matched against the instrument that produces it.
func instrumentName(metric string) string {
	name := metric

	switch {

	case strings.HasSuffix(name, "_total"):
		name = strings.TrimSuffix(name, "_total")

	case strings.HasSuffix(name, "_bucket"), strings.HasSuffix(name, "_sum"):
		name = strings.TrimSuffix(strings.TrimSuffix(name, "_bucket"), "_sum")
		name = strings.TrimSuffix(name, "_seconds")

	}

	return name
}

// exportedMetrics reads the instrument names the service declares, in the underscored form
// Prometheus sees them under.
func exportedMetrics(t *testing.T) map[string]bool {
	t.Helper()

	metrics := make(map[string]bool)

	for _, path := range metricSourceFiles {
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("failed to read the instrument declarations in %s: %s", path, err.Error())
		}

		for _, match := range instrumentPattern.FindAllStringSubmatch(string(source), -1) {
			metrics[strings.ReplaceAll(match[1], ".", "_")] = true
		}
	}

	if len(metrics) == 0 {
		t.Fatal("found no instrument declarations - the pattern no longer matches the telemetry")
	}

	return metrics
}

// TestPrometheusAlertRulesAreActionable checks the parts of a rule an operator reads at 3am. An
// alert with no description or runbook is a page with no next step.
func TestPrometheusAlertRulesAreActionable(t *testing.T) {
	for name, rule := range readPrometheusAlerts(t) {
		if rule.forDuration == "" {
			t.Errorf("alert '%s' has no 'for' duration, so a single scrape can page", name)
		}

		switch rule.labels["severity"] {

		case "critical", "warning":

		default:
			t.Errorf("alert '%s' has severity '%s', expected critical or warning", name, rule.labels["severity"])

		}

		if rule.labels["service"] != "hippocampus" {
			t.Errorf("alert '%s' is not labelled service=hippocampus", name)
		}

		for _, annotation := range []string{"summary", "description", "runbook_url"} {
			if rule.annotations[annotation] == "" {
				t.Errorf("alert '%s' has no '%s' annotation", name, annotation)
			}
		}

		if !strings.Contains(rule.annotations["runbook_url"], "/docs/operations.md#") {
			t.Errorf("alert '%s' points its runbook_url somewhere other than an operations.md section: %s",
				name,
				rule.annotations["runbook_url"],
			)
		}
	}
}

// TestGrafanaAlertRulesAreWellFormed checks the wiring Grafana needs and this repo cannot exercise:
// a provisioning file with a dangling condition or the wrong datasource uid loads without complaint
// and then fails on every evaluation.
func TestGrafanaAlertRulesAreWellFormed(t *testing.T) {
	_, rules := readGrafanaAlerts(t)

	uids := make(map[string]string)

	for name, rule := range rules {
		if rule.UID == "" {
			t.Errorf("grafana rule '%s' has no uid, so re-provisioning cannot update it in place", name)
		}
		if existing, duplicate := uids[rule.UID]; duplicate {
			t.Errorf("grafana rules '%s' and '%s' share the uid '%s'", existing, name, rule.UID)
		}

		uids[rule.UID] = name

		// The query returns a value only while the alert should fire (the comparison is in the
		// PromQL, as Prometheus evaluates it), so no data is the healthy state and must read as OK.
		if rule.NoDataState != "OK" {
			t.Errorf("grafana rule '%s' has noDataState '%s': the comparison lives in the PromQL, so an empty result is healthy",
				name,
				rule.NoDataState,
			)
		}

		var (
			query     = -1
			condition = -1
		)

		for i, node := range rule.Data {
			switch node.DatasourceUID {

			case expressionDatasourceUID:
				if node.RefID == rule.Condition {
					condition = i
				}

			case grafanaDatasourceUID:
				query = i

			default:
				t.Errorf("grafana rule '%s' queries datasource uid '%s', expected '%s' (the uid otel-lgtm gives its Prometheus)",
					name,
					node.DatasourceUID,
					grafanaDatasourceUID,
				)

			}

			if node.RefID != node.Model.RefID {
				t.Errorf("grafana rule '%s' node '%s' carries model refId '%s'", name, node.RefID, node.Model.RefID)
			}
		}

		if query < 0 {
			t.Errorf("grafana rule '%s' has no prometheus query node", name)

			continue
		}

		if condition < 0 {
			t.Errorf("grafana rule '%s' names condition '%s', which no expression node provides", name, rule.Condition)

			continue
		}

		if rule.Data[condition].Model.Expression != rule.Data[query].RefID {
			t.Errorf("grafana rule '%s' evaluates its condition over '%s' rather than the query '%s'",
				name,
				rule.Data[condition].Model.Expression,
				rule.Data[query].RefID,
			)
		}

		// A range query would hand the threshold a series rather than a value, so every rule asks
		// for an instant one - and its window has to cover the widest range selector in the
		// expression, or the query has nothing to compute over.
		if !rule.Data[query].Model.Instant {
			t.Errorf("grafana rule '%s' does not use an instant query", name)
		}

		widest := widestRange(t, rule.Data[query].Model.Expr)
		if from := rule.Data[query].RelativeTimeRange.From; from < widest {
			t.Errorf("grafana rule '%s' looks back %ds but its expression needs %ds", name, from, widest)
		}
	}
}

// widestRange returns the largest range selector in an expression, in seconds.
func widestRange(t *testing.T, expr string) int {
	t.Helper()

	units := map[string]int{"s": 1, "m": 60, "h": 3600, "d": 86400}

	widest := 0

	for _, match := range rangePattern.FindAllStringSubmatch(expr, -1) {
		value, err := strconv.Atoi(match[1])
		if err != nil {
			t.Fatalf("failed to read the range selector '%s': %s", match[0], err.Error())
		}

		if seconds := value * units[match[2]]; seconds > widest {
			widest = seconds
		}
	}

	return widest
}

func sortedKeys(in map[string]bool) []string {
	out := make([]string, 0, len(in))

	for k := range in {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}
