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

Nine rules, covering what actually goes wrong:

| Alert                              | Fires when                                                   | Severity |
| ---------------------------------- | ------------------------------------------------------------ | -------- |
| `HippocampusServerErrorRateHigh`   | >1% of RPCs return a server-fault code for 10m               | critical |
| `HippocampusRequestLatencyHigh`    | p95 of the interactive RPCs is above 1s for 15m              | warning  |
| `HippocampusSleepCycleFailing`     | sleep cycles have been failing for 15m                       | critical |
| `HippocampusConsolidatorAbsent`    | no successful sleep cycle anywhere for an hour               | critical |
| `HippocampusCapacityPressureHigh`  | capacity pressure sustained near or above the target for 30m | warning  |
| `HippocampusStoreOverCapacity`     | used bytes above `capacityBytes` for an hour                 | warning  |
| `HippocampusRetentionNearCapacity` | retained bytes exceed 90% of `capacityBytes` for 30m         | critical |
| `HippocampusSearchIndexDropping`   | index operations are being dropped for 10m                   | warning  |
| `HippocampusPanicsRecovered`       | a handler panicked and was recovered                         | warning  |

Each rule carries a `summary`, a `description` saying what to do about it, and a `runbook_url` into
`docs/operations.md`.

Three properties worth knowing before you deploy them:

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
  add `by (job)` — or whatever label separates them — to each expression.

## The Grafana copy

`../compose/observability/alerting-rules.yaml` is the same nine rules as Grafana-managed rules,
provisioned into the bundled `grafana/otel-lgtm` stack (every compose file's `observability` profile,
and `demo/run.sh`) so the demo stack alerts as well as draws. It exists as a second file only
because Grafana provisions its own rule format and cannot read a Prometheus rule file.

The PromQL and the `for:` durations are byte-identical between the two files, and
`cmd/hippocampus/alerts_test.go` fails if they drift — or if either file names a metric the service
does not export.

No contact point or notification policy is provisioned in either file: where alerts should be
delivered is deployment-specific. In the demo stack, firing rules are visible in Grafana under
**Alerting → Alert rules**.
