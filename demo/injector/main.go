// Command injector seeds a Hippocampus instance with representative corporate events and
// memories through the HTTP/JSON gateway's ImportBatch endpoint (POST /v1/import/batch).
//
// The generated corpus mixes two shapes on purpose, so content search has something to find:
//
//   - Clustered events, drawn from a set of operational templates (service outages, deployments,
//     security patches, on-call handovers, ...). Many events share a template, so their memories
//     reuse the template's vocabulary and read as "similar to one another".
//
//   - Unique one-off events (an acquisition, an office move, a novel data-loss bug, ...), whose
//     memories use distinctive vocabulary that appears nowhere else.
//
// Usage:
//
//	go run ./demo/injector --url http://localhost:8080 --target-bytes 120000000
//
// It keeps generating until the cumulative memory-body byte count reaches --target-bytes.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"time"

	"github.com/spf13/pflag"
)

// int64 proto fields are rendered as JSON strings by protojson; the gateway accepts them as
// strings on input, so we emit them that way.
type jsonEvent struct {
	ID           string `json:"id"`
	TimeStart    string `json:"time_start,omitempty"`
	TimeEnd      string `json:"time_end,omitempty"`
	Significance int32  `json:"significance,omitempty"`
	Name         string `json:"name,omitempty"`
	Description  string `json:"description,omitempty"`
	Group        string `json:"group,omitempty"`
}

type jsonMemory struct {
	ID           string `json:"id"`
	TimeStamp    string `json:"time_stamp,omitempty"`
	Significance int32  `json:"significance,omitempty"`
	EventID      string `json:"event_id,omitempty"`
	Body         string `json:"body"`
	Group        string `json:"group,omitempty"`
}

type batchReq struct {
	Events   []jsonEvent  `json:"events,omitempty"`
	Memories []jsonMemory `json:"memories,omitempty"`
}

// template describes a family of similar events. Its phrase banks are combined to build both the
// event description and its memories' bodies, so every event minted from the same template shares
// vocabulary.
type template struct {
	group    string
	names    []string // "%s" is filled with a subject (service, region, ...)
	subjects []string
	openers  []string
	details  []string
	closers  []string
}

var clusteredTemplates = []template{
	{
		group:    "sre",
		names:    []string{"%s outage", "%s degradation", "%s latency spike", "%s error-rate alert"},
		subjects: []string{"payments-api", "checkout-service", "auth-gateway", "search-cluster", "notification-worker", "inventory-service", "recommendation-engine", "media-transcoder"},
		openers:  []string{"PagerDuty alert fired at %02d:%02d UTC for the %s.", "On-call paged: %s reported elevated 5xx responses.", "Synthetic monitor detected the %s failing health checks."},
		details:  []string{"Error rate climbed to %d%% and p99 latency crossed %dms.", "Circuit breaker tripped after the upstream connection pool saturated.", "Retries backed up the queue; consumer lag reached %d thousand messages.", "A recent config change to the connection timeout was implicated.", "Memory pressure on node-%d triggered repeated OOM restarts."},
		closers:  []string{"Mitigated by rolling back the last deploy and draining the affected node.", "Failed over to the standby region while the primary recovered.", "Scaled the pool out by %d replicas to absorb the load; error rate returned to baseline.", "Root cause traced to an exhausted database connection pool."},
	},
	{
		group:    "platform",
		names:    []string{"Deploy %s", "Release %s", "Rollout %s"},
		subjects: []string{"payments-api v3.%d.%d", "checkout-service v2.%d.%d", "auth-gateway v5.%d.%d", "web-frontend v8.%d.%d", "mobile-bff v1.%d.%d", "data-pipeline v4.%d.%d"},
		openers:  []string{"Canary at %d%% for %d minutes with no regression in the golden signals.", "Progressive rollout to the %s fleet started at %02d:%02d.", "Blue/green cutover completed; traffic shifted to the new version."},
		details:  []string{"Migration %04d applied cleanly across %d shards.", "Feature flag `%s` enabled for internal users only.", "Bundle size dropped %d%% after tree-shaking the vendor chunk.", "Added structured logging around the checkout funnel."},
		closers:  []string{"No rollback needed; dashboards green after %d minutes.", "Post-deploy smoke tests passed in every region.", "Held the rollout at 50%% overnight as a precaution."},
	},
	{
		group:    "security",
		names:    []string{"Patch %s", "Remediate %s", "Harden %s"},
		subjects: []string{"CVE-2026-%04d", "CVE-2025-%04d", "the TLS config", "the IAM policy", "the container base image"},
		openers:  []string{"Severity rated %s by the scanner; SLA is %d days.", "Dependabot opened a PR bumping the affected library.", "Reported via the coordinated-disclosure inbox."},
		details:  []string{"Affected %d services across the %s and %s namespaces.", "Exploitability required an authenticated session, lowering the real-world risk.", "Rotated the leaked credential and invalidated %d active tokens.", "Added a WAF rule as a compensating control while the fix ships."},
		closers:  []string{"Verified fixed by re-running the scan; zero findings remain.", "Backported the patch to the %d.x maintenance branch.", "Filed a follow-up to add a regression test."},
	},
	{
		group:    "support",
		names:    []string{"Escalation: %s", "Ticket: %s", "Customer issue: %s"},
		subjects: []string{"Northwind Traders", "Contoso Ltd", "Globex Corp", "Initech", "Umbrella Retail", "Wayne Industries", "Stark Labs"},
		openers:  []string{"P%d ticket opened; customer reports %s.", "Account manager flagged churn risk on the %s account.", "Reproduced the issue on a staging tenant."},
		details:  []string{"Failed invoices totalled %d over the billing cycle.", "The customer's webhook endpoint returned 401 for %d hours.", "Data export stalled at %d%% for tenants over %d GB.", "Root cause was a stale API key after their rotation."},
		closers:  []string{"Issued a credit and shipped a targeted fix within the SLA.", "Provided a workaround; permanent fix tracked in JIRA-%d.", "Customer confirmed resolution and closed the ticket."},
	},
	{
		group:    "data",
		names:    []string{"Nightly ETL: %s", "Backfill: %s", "Data quality: %s"},
		subjects: []string{"orders warehouse", "clickstream ingest", "billing reconciliation", "user dimension", "inventory snapshot", "ML feature store"},
		openers:  []string{"Job ran for %d minutes processing %d million rows.", "Airflow DAG `%s` completed with %d retries.", "Freshness SLA breached by %d minutes."},
		details:  []string{"Null rate on the %s column rose to %d%%, above the %d%% threshold.", "Deduplicated %d thousand duplicate keys introduced upstream.", "Partition for %04d-%02d was reprocessed from source.", "Schema drift detected: a new nullable column appeared."},
		closers:  []string{"Downstream dashboards refreshed and reconciled to the source of truth.", "Quarantined the bad partition and alerted the producing team.", "Added a dbt test to catch the regression next time."},
	},
	{
		group:    "sre",
		names:    []string{"On-call handover %s", "Weekly ops review %s", "Capacity review %s"},
		subjects: []string{"week %d", "Q%d", "for the payments domain", "for the platform team"},
		openers:  []string{"%d alerts fired this rotation, %d actionable.", "Toil down %d%% after automating the certificate renewal.", "Reviewed the top %d noisy alerts for tuning."},
		details:  []string{"Error budget for the checkout SLO is %d%% consumed.", "Disk usage on the metrics cluster trending to full in %d weeks.", "%d runbooks reviewed and %d marked stale.", "Load test showed headroom of %dx over peak."},
		closers:  []string{"Action items assigned; follow-ups tracked in the ops board.", "No carry-over incidents into the next rotation.", "Agreed to raise the autoscaling ceiling ahead of the sale."},
	},
}

// uniqueEvents are one-off, distinctive events; each gets memories built from its own detail
// pool, so their vocabulary does not overlap with the clustered templates.
var uniqueEvents = []struct {
	group   string
	name    string
	desc    string
	details []string
}{
	{"corp", "Acquisition of Meridian Analytics closed", "Legal and finance completed the Meridian Analytics acquisition.",
		[]string{"Integration of the Meridian data-science team begins next quarter under a dedicated cost centre.", "Meridian's proprietary forecasting model will be evaluated against our in-house baseline.", "Retention packages were offered to twelve key Meridian engineers.", "Their Frankfurt office lease transfers to us in the new fiscal year."}},
	{"corp", "Headquarters relocation to the Riverside campus", "The company moved its primary office to the Riverside campus.",
		[]string{"Desk allocation for the Riverside campus was finalised for eight hundred staff.", "The old Harbourgate lease was surrendered three months early.", "Loading-dock access at Riverside is restricted to weekday mornings.", "The new campus runs entirely on the renewable-energy tariff."}},
	{"platform", "Great Antarctic data-loss near-miss", "A snapshot pruning bug nearly deleted the cold-tier archive.",
		[]string{"A misconfigured lifecycle rule targeted the wrong glacier bucket prefix.", "Object-lock retention saved four petabytes of archival snapshots from deletion.", "The pruning cron was disabled globally pending a full audit.", "This near-miss prompted a company-wide review of every destructive automation."}},
	{"security", "Coordinated red-team exercise 'Nightjar'", "An external red team ran a two-week authorised engagement.",
		[]string{"The Nightjar team phished a staging credential but was stopped by hardware MFA.", "Lateral movement into the billing VLAN was blocked by network segmentation.", "A forgotten debug endpoint on an internal tool was the most serious finding.", "Every Nightjar finding was triaged and assigned within the debrief."}},
	{"data", "One-time GDPR erasure sweep", "A regulatory request triggered a bulk right-to-erasure operation.",
		[]string{"Personal data for the requesting cohort was purged across seventeen systems.", "Tombstone records were written so re-ingestion cannot resurrect the erased rows.", "The erasure was witnessed and signed off by the data protection officer.", "Downstream search indices were reconciled to confirm no residual documents."}},
	{"finance", "Unbudgeted cloud spend anomaly investigation", "A sudden spike in egress charges triggered a finance review.",
		[]string{"An accidental cross-region replication doubled the monthly egress bill.", "The anomaly was traced to a debugging flag left enabled after an incident.", "A budget alert threshold was added at eighty percent of forecast.", "Reserved-capacity commitments were renegotiated to recover the overrun."}},
}

// phrase banks shared across paragraphs to pad body length and add filler realism.
var filler = []string{
	"Timeline reconstructed from the incident channel and the metrics backend.",
	"Stakeholders were kept updated on the status page throughout.",
	"A blameless retrospective is scheduled to capture the learnings.",
	"Relevant dashboards and query links are attached to the ticket.",
	"The change was reviewed by two engineers before merge.",
	"Observability gaps surfaced during triage were logged for follow-up.",
	"Communication with the affected customers followed the standard template.",
	"No customer data was exposed at any point during the event.",
	"The runbook was updated to reflect the steps taken here.",
	"Cross-team coordination happened over the shared operations bridge.",
}

// injectResult summarises one runInjection call, for the final "done" line and for tests.
type injectResult struct {
	eventCount     int
	memoryCount    int
	totalBodyBytes int64
}

// runInjection drives the generate-batch-flush loop: it mints events/memories via generateEvent
// until totalBodyBytes reaches targetBytes, flushing a batch through post whenever it reaches
// batchMemories (and once more at the end for the remainder). progress, when non-nil, is called
// after every mid-loop flush under the same "roughly every 20000 memories" gate the CLI output
// used inline before this was extracted, so callers (main, or a test) can observe/log without
// runInjection depending on stdout directly. post/progress are injected so this loop is testable
// without a real network client; main wires them to postBatch and fmt.Printf respectively.
func runInjection(
	rng *rand.Rand,
	now time.Time,
	targetBytes int64,
	batchMemories int,
	post func(*batchReq) error,
	progress func(eventCount int, memoryCount int, totalBodyBytes int64),
) (injectResult, error) {
	var (
		totalBodyBytes int64
		eventCount     int
		memoryCount    int
		batch          batchReq
	)

	flush := func() error {
		if len(batch.Memories) == 0 && len(batch.Events) == 0 {
			return nil
		}

		if err := post(&batch); err != nil {
			return err
		}

		batch = batchReq{}

		return nil
	}

	for totalBodyBytes < targetBytes {
		ev, mems := generateEvent(rng, now)
		batch.Events = append(batch.Events, ev)
		eventCount++

		for i := range mems {
			batch.Memories = append(batch.Memories, mems[i])
			memoryCount++
			totalBodyBytes += int64(len(mems[i].Body))

			if len(batch.Memories) >= batchMemories {
				if err := flush(); err != nil {
					return injectResult{eventCount, memoryCount, totalBodyBytes}, err
				}

				if progress != nil && memoryCount%20000 < batchMemories {
					progress(eventCount, memoryCount, totalBodyBytes)
				}
			}
		}
	}

	if err := flush(); err != nil {
		return injectResult{eventCount, memoryCount, totalBodyBytes}, err
	}

	return injectResult{eventCount, memoryCount, totalBodyBytes}, nil
}

func main() {
	url := pflag.String("url", "http://localhost:8080", "base URL of the Hippocampus HTTP gateway")
	targetBytes := pflag.Int64("target-bytes", 120_000_000, "stop once this many bytes of memory body have been generated")
	batchMemories := pflag.Int("batch-memories", 400, "memories per ImportBatch request")
	seed := pflag.Int64("seed", 42, "PRNG seed for reproducibility")
	pflag.Parse()

	rng := rand.New(rand.NewSource(*seed))
	client := &http.Client{Timeout: 60 * time.Second}
	now := time.Now()
	start := time.Now()

	fmt.Printf("injecting into %s until ~%d MB of memory bodies...\n", *url, *targetBytes/1_000_000)

	post := func(batch *batchReq) error {
		return postBatch(client, *url, batch)
	}

	progress := func(eventCount int, memoryCount int, totalBodyBytes int64) {
		fmt.Printf("  %d events, %d memories, %d MB (%.0fs)\n",
			eventCount, memoryCount, totalBodyBytes/1_000_000, time.Since(start).Seconds())
	}

	result, err := runInjection(rng, now, *targetBytes, *batchMemories, post, progress)
	if err != nil {
		fmt.Fprintf(os.Stderr, "batch failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("done: %d events, %d memories, %d MB of body in %.0fs\n",
		result.eventCount, result.memoryCount, result.totalBodyBytes/1_000_000, time.Since(start).Seconds())
}

func postBatch(client *http.Client, base string, batch *batchReq) error {
	buf, err := json.Marshal(batch)
	if err != nil {
		return err
	}

	resp, err := client.Post(base+"/v1/import/batch", "application/json", bytes.NewReader(buf))
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode/100 != 2 {
		body := make([]byte, 512)
		n, _ := resp.Body.Read(body)

		return fmt.Errorf("status %d: %s", resp.StatusCode, string(body[:n]))
	}

	return nil
}

var idCounter int

func nextID(prefix string) string {
	idCounter++

	return fmt.Sprintf("%s-%08d", prefix, idCounter)
}

// generateEvent mints one event and its memories. ~82% of events are drawn from a clustered
// template (similar to their siblings); the rest are unique one-offs.
func generateEvent(rng *rand.Rand, now time.Time) (jsonEvent, []jsonMemory) {
	// Spread creation times over the last 200 days for a realistic aged store.
	ageDays := rng.Intn(200)
	when := now.Add(-time.Duration(ageDays)*24*time.Hour - time.Duration(rng.Intn(86400))*time.Second)
	whenNano := fmt.Sprintf("%d", when.UnixNano())

	if rng.Float64() < 0.18 {
		u := uniqueEvents[rng.Intn(len(uniqueEvents))]
		eid := nextID("evt")

		ev := jsonEvent{
			ID:           eid,
			TimeStart:    whenNano,
			Significance: int32(60 + rng.Intn(41)), // unique events skew significant
			Name:         u.name,
			Description:  u.desc,
			Group:        u.group,
		}

		n := 6 + rng.Intn(10)
		mems := make([]jsonMemory, n)

		for i := 0; i < n; i++ {
			mems[i] = jsonMemory{
				ID:           nextID("mem"),
				TimeStamp:    memNano(rng, when),
				Significance: int32(40 + rng.Intn(61)),
				EventID:      eid,
				Group:        u.group,
				Body:         buildBody(rng, u.desc, u.details),
			}
		}

		return ev, mems
	}

	t := clusteredTemplates[rng.Intn(len(clusteredTemplates))]
	subject := fillNumbers(rng, t.subjects[rng.Intn(len(t.subjects))])
	name := fmt.Sprintf(t.names[rng.Intn(len(t.names))], subject)
	eid := nextID("evt")

	ev := jsonEvent{
		ID:           eid,
		TimeStart:    whenNano,
		Significance: int32(20 + rng.Intn(70)),
		Name:         name,
		Description:  fmt.Sprintf("%s — %s", name, fillNumbers(rng, pick(rng, t.openers))),
		Group:        t.group,
	}

	// Variable fan-out so some events are large and some small.
	n := 8 + rng.Intn(48)
	mems := make([]jsonMemory, n)

	for i := 0; i < n; i++ {
		mems[i] = jsonMemory{
			ID:           nextID("mem"),
			TimeStamp:    memNano(rng, when),
			Significance: int32(1 + rng.Intn(99)),
			EventID:      eid,
			Group:        t.group,
			Body:         buildTemplateBody(rng, subject, t),
		}
	}

	return ev, mems
}

func memNano(rng *rand.Rand, base time.Time) string {
	// Memories land within a few hours of their event.
	t := base.Add(time.Duration(rng.Intn(6*3600)) * time.Second)

	return fmt.Sprintf("%d", t.UnixNano())
}

// buildTemplateBody assembles a multi-sentence memory body from a template's phrase banks plus
// shared filler, padded to a realistic paragraph length (~1-3 KB).
func buildTemplateBody(rng *rand.Rand, subject string, t template) string {
	var b bytes.Buffer

	fmt.Fprintf(&b, "[%s] ", subject)
	b.WriteString(fillNumbers(rng, pick(rng, t.openers)))
	b.WriteByte(' ')

	sentences := 3 + rng.Intn(6)

	for i := 0; i < sentences; i++ {
		switch rng.Intn(3) {

		case 0:
			b.WriteString(fillNumbers(rng, pick(rng, t.details)))

		case 1:
			b.WriteString(fillNumbers(rng, pick(rng, t.closers)))

		default:
			b.WriteString(pick(rng, filler))
		}

		b.WriteByte(' ')
	}

	return b.String()
}

func buildBody(rng *rand.Rand, opener string, details []string) string {
	var b bytes.Buffer

	b.WriteString(opener)
	b.WriteByte(' ')

	sentences := 4 + rng.Intn(6)

	for i := 0; i < sentences; i++ {
		if rng.Intn(3) == 0 {
			b.WriteString(pick(rng, filler))
		} else {
			b.WriteString(pick(rng, details))
		}

		b.WriteByte(' ')
	}

	return b.String()
}

func pick(rng *rand.Rand, xs []string) string {
	return xs[rng.Intn(len(xs))]
}

// fillNumbers substitutes any %-verbs in a phrase with plausible random values, so no two
// sibling memories read identically even when they share a template.
func fillNumbers(rng *rand.Rand, s string) string {
	var args []any

	for i := 0; i < len(s)-1; i++ {
		if s[i] != '%' {
			continue
		}

		switch s[i+1] {

		case 'd':
			args = append(args, rng.Intn(900)+1)

		case '0':
			// %02d / %04d
			if i+3 < len(s) && s[i+3] == 'd' {
				args = append(args, rng.Intn(60))
			}

		case 's':
			args = append(args, pick(rng, []string{"payments", "checkout", "prod-eu", "prod-us", "ledger", "cache-tier", "staging", "primary"}))
		}
	}

	// Guard against a mismatch between placeholders and args (some %0Nd are counted above).
	defer func() { _ = recover() }()

	return fmt.Sprintf(s, args...)
}
