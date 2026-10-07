# Observability

This is the design record moved out of CLAUDE.md verbatim (TODO-3 item 176): why the code is the way it
is, the alternatives that were rejected, and the incidents behind the guards. CLAUDE.md keeps the short
version and links here.

## `observability/`

`observability/` — the shared OTEL bootstrap and probe endpoints, in the root module so the
service, the ingestor and the four broker bridges use one implementation (it began as
`cmd/hippocampus/observability.go` and was promoted, not copied; the integration modules already
depend on the root module for the contract, and the root already carried the OTEL dependencies, so
sharing costs neither side anything). Four pieces. `Init` installs the global tracer/meter
providers and returns a flush func, unchanged from the service's version apart from a
configurable `service.name`. Metrics leave the process by either or both of **two readers** on one
meter provider - the OTLP/gRPC push exporter (`MetricsEnabled`) and a **Prometheus scrape reader**
(`PrometheusEnabled`, `prometheus.go`), independent in both directions because push and scrape are
different collection models rather than encodings of one decision: a kube-prometheus-stack cluster
scrapes, and before item 105 it had to run a collector purely as a protocol adapter in front of
this service. Two decisions there are load-bearing rather than incidental. The name-**translation
strategy is pinned** to `UnderscoreEscapingWithSuffixes`: the exporter's default follows
`prometheus/common`'s global name-validation scheme, which under `utf8` leaves the dots in the
instrument names, and every expression in `deploy/observability/` is written against the
underscored form - `TestScrapeNamesMatchTheAlertRules` holds the served names against the shipped
rules in both directions. And the exporter gets **its own registry** rather than
`prometheus.DefaultRegisterer`, whose Go-runtime and process collectors nothing here declares or
alerts on (`runtime.go` already publishes the runtime figures this project does claim) and which
would make a second `Init` in one process fail on a duplicate registration. `PrometheusHandler`
publishes the handler for a caller to mount; `MetricsServer` is the **service's** standalone
listener for it, deliberately not the gateway - a scrape endpoint on the public API surface would
hand every caller the store's size, capacity pressure and RPC rates, while gating it behind a
token would put it out of reach of most scrapers, and keeping it off the gateway is also what
keeps a scrape out of `hippocampus.rpc.*`'s denominator for free. The client daemons take the
other route (`HealthConfig.MetricsHandler`, `/metrics` on the probe port they already bind), since
that port is already their private operational surface. `HealthServer` serves `/healthz` (liveness) and `/readyz` (a named
map of dependency checks, cached so a probe cannot become its own load), with `GRPCHealthCheck`
probing `grpc.health.v1.Health` — chosen because it is exempt from the auth interceptor, touches
no data, and is driven on the service side by its own database readiness, so "ready" means the far
end can serve rather than that a socket opened. `UnaryClientMetricsInterceptor` records the
client-side RED metrics (`hippocampus.client.rpc.requests`/`.duration`) for everything that dials
the service, classifying `outcome` exactly as `rpcmetrics.go` does so the error rate means one
thing on both sides. **Tenancy** (`WithGroup`, `GroupAttribute`) is a per-process label set once at
`Init` and stamped on both the resource and each metric: the duplication exists because the
OTLP→Prometheus translation puts resource attributes in `target_info`, and it is affordable only
because the value is static — reading a group off each record would be unbounded (a bridge's group
defaults to the message subject), which is why that shape is refused. No **service** metric carries
a group; this is the client side only, and item 60.1 records why the service side stayed out.
