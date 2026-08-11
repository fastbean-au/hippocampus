# Hippocampus ingestor

Promotes completed events from an edge ("source") Hippocampus instance into a central ("target")
one, under a [CEL](https://cel.dev) rules file that decides — per event — whether the data is worth
keeping at all.

It is a **separate Go module** (`replace github.com/fastbean-au/hippocampus => ../..`), so its rules
engine never enters the service build, and a **client of two instances** holding no state of its
own. The edge is a stock, unmodified `hippocampus` binary.

Full guide: [`docs/ingestor.md`](../../docs/ingestor.md).

## Run

```bash
cd integrations/ingestor
go run ./cmd/ingestor \
  --source-address localhost:50051 \
  --target-address central.internal:50051 \
  --target-token "$TOKEN" \
  --rules rules.json
```

Check a rules file without connecting to anything:

```bash
go run ./cmd/ingestor --rules rules.json --check-rules
```

See what a ruleset would do without moving or deleting anything:

```bash
go run ./cmd/ingestor --source-address ... --target-address ... --rules rules.json --dry-run
```

## Rules

```json
{
  "defaultAction": "drop",
  "rules": [
    {
      "name": "keep-errors",
      "expr": "event.metadata[?'severity'].orValue('') == 'error'",
      "action": "promote"
    },
    {
      "name": "sample-chatter",
      "expr": "event.group == 'chatter' && event.memory_count >= 50",
      "action": "promote",
      "reduce": { "keepTopN": 10 }
    },
    {
      "name": "condense-long-sessions",
      "expr": "event.duration_seconds > 3600 && memories.exists(m, m.body.contains('checkout'))",
      "action": "promote",
      "reduce": { "summarise": true }
    }
  ]
}
```

First match wins; `defaultAction` is required. The file is re-read when its mtime changes — a broken
initial load fails startup, a broken reload keeps the last good ruleset.

A `promote` rule may also **rewrite what crosses**, which is how an edge re-ranks what it admits
rather than only admitting it:

```json
{
  "name": "escalate-production-errors",
  "expr": "event.metadata[?'severity'].orValue('') == 'error'",
  "action": "promote",
  "set": {
    "event": { "significance": "math.least(100, event.significance * 4)", "group": "'incidents'" },
    "memory": { "significance": "memory.body.contains('panic') ? 90 : memory.significance" }
  }
}
```

Only the promoted copy is touched, never the edge; the mutation runs before any `reduce`, so
`keepTopN` ranks by the score the rule set. See
[docs/ingestor.md](../../docs/ingestor.md#setting-fields-on-promotion).

## Observability

`/healthz` and `/readyz` on `--health-port` (8090 by default, 0 disables); `/readyz` names whichever
end is unreachable. `--metrics` exports OTEL metrics over OTLP/gRPC, and `--metrics-group` stamps a
per-process tenancy label on them. See
[docs/ingestor.md](../../docs/ingestor.md#observability) for the metric list and what each is for.

## Test

```bash
go test ./...
```

The promoter is driven against two in-memory fake instances, so no service or broker is needed.
