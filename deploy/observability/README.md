# Alert rules

`prometheus-alerts.yaml` is the shipped alert set for a Hippocampus deployment, in Prometheus
rule-file format. It is deployment-agnostic: point a Prometheus at it with `rule_files:`, lift its
`groups:` block into a prometheus-operator `PrometheusRule` `spec:`, or load it into Mimir/Grafana
Cloud with `mimirtool rules load`.

```yaml
# prometheus.yml
rule_files:
  - /etc/prometheus/hippocampus-alerts.yaml
```

Sixteen rules in two groups. `hippocampus` is the service itself — ten rules covering what actually
goes wrong:

| Alert                              | Fires when                                                   | Severity |
| ---------------------------------- | ------------------------------------------------------------ | -------- |
| `HippocampusServerErrorRateHigh`   | >1% of RPCs return a server-fault code for 10m               | critical |
| `HippocampusRequestLatencyHigh`    | p95 of the interactive RPCs is above 1s for 15m              | warning  |
| `HippocampusSleepCycleFailing`     | sleep cycles have been failing for 15m                       | critical |
| `HippocampusConsolidatorAbsent`    | no successful sleep cycle anywhere for an hour               | critical |
| `HippocampusCapacityPressureHigh`  | capacity pressure sustained near or above the target for 30m | warning  |
| `HippocampusStoreOverCapacity`     | used bytes above `capacityBytes` for an hour                 | warning  |
| `HippocampusRetentionNearCapacity` | retained bytes exceed 90% of `capacityBytes` for 30m         | critical |
| `HippocampusRateLimitRejecting`    | a rate limit is refusing >1 request/second for 10m           | warning  |
| `HippocampusSearchIndexDropping`   | index operations are being dropped for 10m                   | warning  |
| `HippocampusPanicsRecovered`       | a handler panicked and was recovered                         | warning  |

`hippocampus-clients` is the six rules for the components that _dial_ a Hippocampus instance — the
[broker bridges](../../docs/eventsource.md) and the [ingestor](../../docs/ingestor.md), separate
processes publishing their own metrics:

| Alert                              | Fires when                                                        | Severity |
| ---------------------------------- | ----------------------------------------------------------------- | -------- |
| `HippocampusBridgeDuplicateStream` | >80% of a bridge's messages are records the store already had     | warning  |
| `HippocampusBridgeNotConsuming`    | a bridge is publishing metrics and handling no messages at all    | warning  |
| `HippocampusBridgeWriteFailing`    | >5% of a bridge's messages fail to transform or store for 10m     | critical |
| `HippocampusClientTokenRejected`   | a client's calls are refused `Unauthenticated`/`PermissionDenied` | critical |
| `HippocampusIngestorPassStale`     | no ingestor pass has completed in 15 minutes                      | critical |
| `HippocampusIngestorRuleErrors`    | an ingestor rule is erroring rather than matching, for 15m        | warning  |

That group exists because of an outage. A Bluesky bridge on the public demo spent hours
re-presenting one record it could never store, at a cursor that never moved: the process was up, its
`/healthz` answered, the store kept filling from a second goroutine, and nothing was being
reinforced. The first two rules are the two shapes that failure has in metrics — a stream that is
all duplicates, or no stream at all — and the third and fourth are the ways a bridge that _is_
consuming can still be writing nothing.

Each rule carries a `summary`, a `description` saying what to do about it, and a `runbook_url` into
`docs/operations.md`.

Four properties worth knowing before you deploy them:

- **Thresholds are starting points.** The latency budget and the error ratio are whatever your SLO
  says they are; the file says so at each rule.
- **A rule for an unconfigured feature stays silent, not broken.** The capacity, retention, and
  search rules read metrics the service only publishes when the corresponding setting is on, and an
  expression over an absent metric returns nothing. The one rule that is _about_ absence
  (`HippocampusConsolidatorAbsent`, which is how the instance-lock keepalive exiting the
  consolidator becomes visible) says so in PromQL with `absent_over_time` rather than relying on an
  alerting engine's no-data policy — so it behaves the same in Prometheus and in Grafana.
- **Every expression aggregates over the whole datasource**, which is right for the
  one-instance-per-store deployment model. If one Prometheus holds several Hippocampus deployments,
  add `by (job)` — or whatever label separates them — to each expression. The bridge rules already
  aggregate `by (broker)` so one wedged bridge is not diluted by a healthy one; if you run more than
  one bridge of the _same_ broker, add the `hippocampus_group` label (their `--metrics-group`) too.
- **A bridge that has _exited_ is not caught here**, and cannot be: a process that publishes nothing
  is indistinguishable from a deployment that runs no bridge at all, so `HippocampusBridgeNotConsuming`
  only fires while one is up and idle. Declare the bridge under the service's `topology.components`
  to have its `/readyz` probed instead — that is the half of this that notices a process being gone.

## The Grafana copy

`../compose/observability/alerting-rules.yaml` is the same eighteen rules as Grafana-managed rules,
provisioned into the bundled `grafana/otel-lgtm` stack (every compose file's `observability` profile,
and `demo/run.sh`) so the demo stack alerts as well as draws. It exists as a second file only
because Grafana provisions its own rule format and cannot read a Prometheus rule file.

The PromQL and the `for:` durations are byte-identical between the two files, and
`cmd/hippocampus/alerts_test.go` fails if they drift — or if either file names a metric no
instrument in this repo declares, the integrations' included.

One consequence of the Grafana copy is worth knowing before writing a rule: each of its rules is an
instant query thresholded at `gt 0`, so **an expression must return a positive number while it
should be firing**. A rule whose firing value is zero is correct in Prometheus and silent in
Grafana, which is why `HippocampusBridgeNotConsuming` is written as `count(… == 0) > 0` rather than
the plain `… == 0` it wants to be.

No contact point or notification policy is provisioned in either file: where alerts should be
delivered is deployment-specific. In the demo stack, firing rules are visible in Grafana under
**Alerting → Alert rules**.
