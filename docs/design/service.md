# The service binary

This is the design record moved out of CLAUDE.md verbatim (TODO-3 item 176): why the code is the way it
is, the alternatives that were rejected, and the incidents behind the guards. CLAUDE.md keeps the short
version and links here.

## `cmd/hippocampus/`

`cmd/hippocampus/` — the `package main` entrypoint (`main.go` plus `backfill.go`,
`interceptors.go`, `logging.go`, `ratelimit.go`, `readiness.go`, `rpcmetrics.go`, and the
`webui.go`/`webui/` embedded
console — four embedded files, no build step and no bundler: `index.html`, `styles.css`, `app.js`
(the DOM, the network, the state) and `lib.js` (the pure logic, an ES module `app.js` imports).
That split is what lets the page be served under a **CSP with no `unsafe-inline`**
(`webUISecurityHeaders`) — every handler is a `data-act`/`data-change` attribute routed through one
`ACTIONS` table, and every dynamic style goes through the CSSOM — and what lets ~3,000 lines of
JavaScript be tested at all: `cmd/hippocampus/webuitest/` runs `lib.js` under `node --test` with
**zero dependencies**, plus drift guards pairing every control with a handler and every embedded
asset with a route and both middleware allow-lists. Those guards scan the assets as text, so their
comment stripper is load-bearing and deliberately **line-based**: it can never remove more than the
line it matched, where the obvious whole-file `/*…*/` regex would delete from any `/*` in a string
to the next `*/` and leave the tests reporting the deleted code as clean. JS block comments are
therefore refused by a test of their own rather than half-handled, and every scan reads the
stripped text — including the `data-act` sets, or a control commented out would keep its handler
looking reachable. Its landing tab is **Now** — the store's
premise made live: memories held, what the last cycle forgot, a countdown to the next
(`GetConsolidationStatus`), a capacity meter, and a feed off the forgotten log — and its **Decay**
tab is the client side of `ExplainConsolidation`: a per-row value column in the memory/search
tables, the current capacity pressure and threshold, an inline-SVG decay curve, and an
`admin`-gated dry-run panel over `PreviewConsolidation`. The **Deployment** tab also carries the
outbound callback queue (item 102.2) - depth, oldest delivery, worst attempt count - gated on
`admin` AND on `WhoAmI.callbacks_enabled`, because with no sink configured `GetCallbackQueue`
answers with an empty page rather than refusing, which on screen is a queue that is keeping up;
there is deliberately no Clear button, an abandoned delivery being a notification nobody will
ever receive. It computes **no** decay maths of its own
— every number and every curve point is served — which is the whole reason those RPCs report what
they do. A **guided tour** (item 93; `TOUR_STEPS`/`tourSteps`/`tourPlacement` in `lib.js`, the DOM
layer at the foot of `app.js`) walks a first-time reader through the RELATIONSHIPS between those
numbers rather than the tabs, since none of them is visible in any one panel. Four things carry it.
It runs over the **live** console, not a seeded story — each stop opens its tab, primes the table
it points at through the `TOUR_PRIMERS` allow-list, and quotes figures read at the moment the stop
opens, every body still reading as a complete thought when a figure has not arrived. It is
**filtered by capability**, because pointing at a panel a replica or a reader does not have teaches
that the console is broken. It is **offered once and always reachable** (unprompted on a first
visit behind one `localStorage` key, plus a header control), because most visitors to a public demo
arrive exactly once. And the popover is repositioned by a **ResizeObserver** as well as on scroll:
the tour is offered as soon as capabilities resolve, which is before the Now tab's fetches answer,
so the card a stop points at is routinely half its final height when it is measured). `main.go` —
bootstrap only: reads the JSON config file into viper (**optional on the default path** — an
absent `./config.json` starts the service on `setStartupDefaults`' built-in defaults with a Warn
line naming them, while a `--config_file` given explicitly must exist; `setStartupDefaults` is a
function rather than inline statements so a test can assert the defaults alone form a valid
configuration, and it defaults the four keys `validateConfig` refuses at zero —
`consolidation.method`/`aggressiveness`/`unitsOfAgeInDays` and `storage.directory` — without
relaxing item 19.1, since viper falls back to a default only for an _unset_ key and a configured
0 still fails validation), initialises logging
(logrus, `logging.go`; `logging.level` selects severity — default `info` — and `logging.json`
toggles JSON-vs-text output to stdout) and observability (the shared `observability` package, which
`cmd/hippocampus/observability.go` was promoted into: optional OTEL tracing/metrics over
OTLP/gRPC, no-op when disabled), opens the DB, wires the gRPC server with interceptors (plus the
`otelgrpc` stats handler when observability is enabled), starts stats, and on SIGINT/SIGTERM
flushes observability then closes the DB. The build version (`version.go`,
`runtime/debug.ReadBuildInfo`) is logged in the startup lines, returned in the `/healthz` body,
and set as the OTEL `service.version` resource attribute; `--version` prints it and exits before
the config file is read. `--mint-token` (with `--client-id`, `--ttl`,
`--signing-secret`) is a separate CLI mode: it prints a signed `auth.MintToken` token to stdout
and exits before the database, observability, or server are touched at all; it refuses under
`auth.method: idp` (the IdP issues tokens there). `auth.method` selects the auth scheme —
`none` (default), `hmac` (`auth.NewHMACVerifier` from `auth.signingSecret`), or `idp`
(`auth.NewJWKSVerifier`: RS256 against an IdP's JWKS, from `auth.jwksUrl` or OIDC discovery
via `auth.issuer`); the legacy boolean `auth.enabled` is a deprecated alias consulted only
when `auth.method` is unset (`true` → `hmac` plus a warning). Whichever verifier is built,
`auth.UnaryServerInterceptor` is prepended to the gRPC interceptor chain (ahead of
`InterceptorBlockWhenPurgeInProgress`/`InterceptorLogger`, so unauthenticated requests are
rejected before any other interceptor runs; on success it stashes the verified `auth.Claims` in
the request context (`auth.ContextWithClaims`) so downstream interceptors can attribute the call.
`InterceptorLogger` keeps its Trace entry/exit lines
but also logs a failing RPC at Warn — Info for client-fault codes — so failures are visible at the
default log level, adding a `client_id` field from the stashed claims when present; when `tls.enabled`,
the `*tls.Config` `loadServerTLS` builds is added via `grpc.Creds` and shared with the gateway. Auth without
`tls.enabled` only logs a warning — TLS may be terminated upstream instead. That config is also
where **mutual TLS** lives (item 101): `tls.clientCaFile` populates `ClientCAs` and sets
`VerifyClientCertIfGiven`, `tls.requireClientCert` raises it to `RequireAndVerifyClientCert`. Three
things carry it. It exists because six client-side TLS blocks in this repo (`opensearch.tls`,
`transfer.tls`, `callbacks.tls`, the MCP bridge, the `hippo` CLI, the event-source bridges) have
always carried a `certFile`/`keyFile` pair while no listener ever requested one — and that failure
is **silent**, since a client presenting a certificate to a server that does not ask completes the
handshake and never sends it, which is why the test drives a real handshake rather than asserting on
the config. `requireClientCert` without a CA file is **refused**, because Go verifies against the
system roots when `ClientCAs` is nil, so "required" would admit every certificate any public CA has
ever issued. And **nothing is derived from the certificate** — no client id, no role, no group scope:
a certificate says which process is connecting, a token says which client and at what tier, and
authorisation keeps one source. Requiring a certificate covers the gateway's `/healthz`/`/readyz`
too (the handshake precedes the request), which is warned about at startup. gRPC **server
reflection** is registered beside `RegisterHippocampusServer` when `reflectionSetting` says so —
`reflection.enabled` when set, otherwise derived from `auth.method` (on under `none`, off under
`hmac`/`idp`), with the choice and its reason logged. It cannot be gated by the auth interceptor
instead: reflection is a **streaming** RPC and every interceptor in the chain is unary, so not
registering it is the only thing that keeps it off an authenticated instance. Optional gRPC
hardening server options are appended when their keys are positive: `maxRecvMsgBytes`
(`grpc.MaxRecvMsgSize`), `maxConcurrentStreams` (`grpc.MaxConcurrentStreams`), and a keepalive
enforcement policy from `keepalive.minTimeSeconds`/`keepalive.permitWithoutStream`
(`grpc.KeepaliveEnforcementPolicy`); each defaults to grpc-go's own default when unset. Both
listeners bind all interfaces unless `bindAddress` (gRPC) / `gateway.bindAddress` (HTTP) restrict
the interface — e.g. `127.0.0.1` behind a TLS-terminating sidecar/mesh. A zero `gateway.port` is
logged at Info naming what goes with it (console, OpenAPI doc, HTTP probes) and how to enable it,
since binding nothing was previously indistinguishable from binding something that failed. When
`gateway.port`
is positive it also registers `contract.RegisterHippocampusHandlerServer` (the generated
`hippocampus.pb.gw.go` reverse proxy) on a `runtime.NewServeMux()` and serves it over HTTP (TLS
via `ListenAndServeTLS` when `tls.enabled`) — calling straight into the same `hipo` server
instance, not dialing back over gRPC — alongside a static `/v1/openapi.json` (the embedded
`contract.SwaggerJSON`) and an unauthenticated `/healthz`. Because the gateway calls `hipo`
directly and never runs the gRPC interceptor chain, the mux is always wrapped in
`hipo.HTTPMiddlewareBlockWhenPurgeInProgress` (the HTTP counterpart to
`InterceptorBlockWhenPurgeInProgress`; open paths `/healthz` and `/v1/openapi.json`, else 503
while a purge runs), which is in turn wrapped in `httpLoggingMiddleware` (the gateway's counterpart
to `InterceptorLogger`, since the gateway never runs the gRPC chain — logs 5xx at Warn, else at
Debug, via an intercepting status recorder, and adds the `client_id` from the request context
when present); when `auth.enabled`, that is in turn wrapped in
`auth.HTTPMiddleware` (outermost, so unauthenticated requests are rejected first — and, like the
gRPC interceptor, stashing the verified claims on the request context on success) except
`/healthz`. The gateway is shut down before the gRPC
server on SIGINT/SIGTERM; `shutdown.timeoutSeconds` (default 10) bounds each phase (gateway drain,
gRPC graceful stop, observability flush). All configuration flows through viper keys matching `config.json`
structure. Instrumentation elsewhere uses the global OTEL providers
(`hippocampus/telemetry.go`, `stats/stats.go`), so it stays no-op-safe whether or not
observability is enabled. The domain metrics defined in `hippocampus/telemetry.go` (counters,
the `sleep.duration` and `memory.body_bytes` histograms, and the `capacity_pressure`/`used_bytes`
gauges) keep every attribute low-cardinality (bool or small enum), so it is safe to add attributes
only within that constraint; an optional `grafana/otel-lgtm` compose profile with a provisioned
dashboard (`deploy/compose/observability/`) exists for local viewing. The **RED metrics** —
request rate, errors, duration — are separate, in `rpcmetrics.go`, because they belong to the
transport boundary rather than the domain: `hippocampus.rpc.requests` and
`hippocampus.rpc.duration` share one attribute set (`transport`, `rpc`, `code`, `outcome`) and are
recorded by `InterceptorMetrics` on gRPC and `httpMetricsMiddleware` on the gateway, both installed
inside panic recovery but **outside** authentication so a rejected request still appears in the
error rate. Four things carry the design. (1) `outcome` is three-valued
(`ok`/`client_error`/`server_error`, gRPC classifying via the existing `isClientFaultCode`) rather
than a success bool, so an SLO can alert on the service failing without also firing on clients
sending bad requests. (2) `rpc` names the same thing on both transports — the gateway resolves it
via `auth.RouteRPC`, an inversion of `auth/authz.go`'s already-drift-guarded `policies` table —
and **never** from the request path, which carries ids. (3) The gateway therefore needs _two_
middlewares: only a post-routing `runtime.Middleware` (`gatewayRouteMiddleware`, registered first
so it runs ahead of the authoriser) knows the matched route, but only the outer handler sees
requests rejected before routing, so the former fills in a `routeCapture` the latter placed on the
context, and an unrouted request is counted as `rpc="unknown"`. (4) Neither recording is deferred:
panic recovery sits outside both, so a deferred record would count a panicking call as a success —
panics stay `hippocampus.panics_recovered`'s to report. Both are scoped to the RPC surface (the
`/hippocampus.v1.Hippocampus/` prefix; `/v1` minus the OpenAPI doc), keeping probe, console and login
traffic out of the error-rate denominator. **The alert rules those metrics exist for are shipped
too**, and deliberately twice: `deploy/observability/prometheus-alerts.yaml` (a portable
Prometheus rule file — the artefact a real deployment loads) and
`deploy/compose/observability/alerting-rules.yaml` (the same thirty-one rules as Grafana-managed rules,
provisioned into every compose file's `observability` profile and `demo/run.sh`, because Grafana
provisions its own format and cannot read a Prometheus rule file). Two copies of a PromQL
expression that nothing in the repo executes is exactly what drifts, so the drift guard
(`cmd/hippocampus/alerts_test.go`) fails if the two disagree on any expression, `for:`, label or
annotation, if either
names a metric no instrument declares (matching the queried series name back to the instrument
through the OTLP `_total`/`_bucket`/`_seconds` suffixes), or if a Grafana rule's wiring is wrong in
a way that provisions cleanly and then fails every evaluation (dangling `condition`, wrong
datasource uid, a query window narrower than its own range selector). There is a **third** copy —
`deploy/observability/README.md`, the page an operator reads before deploying the rules, which
tables every rule and states the counts in prose — and the same guard now holds it too: a rule
with no row, a row naming no rule, a severity that disagrees, an order that does not match the
file, or a stale count all fail. It was added because that page had drifted by six rules, which
is what a documentation table that copies a table in the code does when nothing executes it. There
is a **fourth** copy of the count in `docs/operations.md`, guarded the same way since it was found
eleven rules out of date. Three
decisions carry the
pair: the comparison lives in the **PromQL** on both sides (Grafana adds only a `gt 0` threshold
over an instant query, hence `noDataState: OK` on every rule), so the two engines behave
identically; absence — a consolidator that has exited publishes no counter to alert on — is
asked inside the expression rather than through a no-data policy, for the
same reason (and asked **twice**, since `absent_over_time` only sees the metric leave the whole
deployment and cannot see a consolidator that is up but wedged, whose counter is still there but
no longer advancing); and every expression aggregates **`by (service_name)`**, which is
`observability.serviceName` and so names the STORE rather than the process — replicas of one store
share a name and re-aggregate into one alert, while separate stores sharing a datasource stay
apart. That grouping replaced a bare `sum()`/`max()` plus a header telling the reader to add
`by (job)` themselves: the advice went untaken even on this project's own demo, where five stores
share a collector, and the result was not the dilution it was described as but genuine
cross-wiring — `HippocampusStoreDiskFarAboveEstimate` computing one store's disk over another
store's estimate. Neither file provisions a contact point. The `gt 0` threshold has a consequence worth
knowing before writing a rule: an expression must return a **positive** number while it should be
firing, so a rule whose firing value is zero is correct in Prometheus and silent in Grafana —
hence `HippocampusBridgeNotConsuming`'s `count(… == 0) > 0`. Nine of the thirty-one are a second group,
`hippocampus-clients`, and are **not about the service**: they cover the processes that dial it (the
broker bridges, the ingestor, the object-storage agents) and read instruments declared in
`integrations/*`, which
`metricSourceFiles` reaches as FILES rather than imports — the root module deliberately does not
depend on those modules, and the guard needs the instrument names, not the instruments. That group
is item 73's outage written down: a bridge whose stream is all `outcome="exists"` is running and
adding nothing, and one that is up and consuming nothing is the same fault's other shape. What it
cannot catch is a bridge that has **exited** — that publishes nothing, which is indistinguishable
from a deployment running no bridge, so the other half is declaring the bridge in
`topology.components` and letting the prober watch its `/readyz` (`demo/config.bluesky.json` does,
and `demo/bluesky.sh`'s `HEALTH_PORT` is coupled to it).

## `stats/`

`stats/` — logs event/memory counts every `stats.intervalSeconds` (default 300; 0 disables the
log line) and registers the count gauges. The log ticker and the gauge callback share one
`countCache`, so the full-table `CountEvents`/`CountMemories` run at most once per interval
regardless of the metric export frequency, rather than once per ticker tick plus once per export.
