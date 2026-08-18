# Security policy

## Reporting a vulnerability

**Please report privately, not in a public issue.**

Use GitHub's private vulnerability reporting: the **Security** tab on
[this repository](https://github.com/fastbean-au/hippocampus/security) → **Report a vulnerability**.
That opens a private advisory visible only to the maintainer, and it is the preferred channel. If it
is unavailable to you, open a public issue asking for a private channel — **without any detail of
the finding** — and you will be sent one.

Helpful things to include, in rough order of usefulness:

- The version (`hippocampus --version`) or commit, and the storage driver.
- The relevant configuration, **with secrets redacted** — particularly `auth.method`, `tls.enabled`,
  whether the gateway is exposed, and whether tokens are group-scoped.
- What an attacker gains, and what access they need to start: an unauthenticated caller, a valid
  `reader` token, and a host-local operator are three very different findings.
- The smallest reproduction you have. A failing test against a disposable store is ideal; a curl or
  `hippo` invocation is plenty.

Hippocampus is maintained by one person, in their own time. There is no response-time commitment.
You will get an acknowledgement, an assessment of whether it is in scope, and — where a fix is
warranted — a release and a credit in the [changelog](CHANGELOG.md) unless you would rather not be
named. Please give a fix a reasonable window before disclosing publicly.

## Supported versions

Hippocampus is **pre-1.0**. Fixes land on `main` and ship in the next release; there are no
backported patch branches, so **the latest release is the supported one**. See
[Compatibility](CHANGELOG.md#compatibility) for what a version number covers.

## Scope

**In scope** — the service (`cmd/hippocampus`), the storage and search layers, the auth package, the
embedded web console, the [configuration wizard](docs/config-wizard.md), and the components under
`integrations/` (the `hippo` CLI, the MCP bridge, the event-source bridges, the ingestor, the OTEL
exporter). Anything that lets a caller read, write, or destroy records beyond what their token
permits — or that leaks credentials, another group's data, or memory content — is worth reporting.

**Out of scope** — the public demo at `hippocampus-demo.com` and the stacks that run it. They exist
to be poked at: the consoles take a published `demo`/`demo` sign-in, their stores are generated data
that is meant to be forgotten, and their decay clocks are deliberately unrealistic. Please do not
attempt denial of service against them; if you find something there that would also affect a real
deployment, report the underlying issue here.

## Behaviours that are not vulnerabilities

Each of these is deliberate, documented, and reported often enough to be worth stating up front.
They are still worth a report if you can show the documented boundary being *crossed*.

- **Authentication, TLS, and rate limiting are off by default.** The default install is an embedded
  store on localhost with no dependencies. See
  [Security · the defaults](docs/security.md#the-defaults-and-what-to-turn-on) for what to turn on,
  and note that the service warns at startup when auth is enabled without TLS.
- **The Compose stacks under `deploy/compose/` are demonstrations**, and several run OpenSearch with
  its security plugin disabled and no authentication at all. `docker-compose.opensearch-secured.yaml`
  is the one that shows the secured shape.
- **Group scoping is a soft partition**, not isolation: records are scoped, but decay dynamics stay
  store-global and `link_significance` crosses the boundary. Hard isolation is one instance per
  tenant — [the trust boundary](docs/security.md#group-scoping-and-the-trust-boundary) says exactly
  what it does and does not give you.
- **`Import`/`ImportBatch` bypass write-path validation** on purpose, so an archive restores
  faithfully. They are `writer`-tier; grant that only to loaders you trust.
- **The web console keeps its token in `localStorage`**, and hides controls a role may not use. The
  hiding is a convenience — the server enforces every tier on every RPC — so a hidden control being
  reachable is not itself a finding; a *server* accepting the call would be.
- **The service does not encrypt data at rest**, verify client certificates on its listeners, or
  keep an audit log separate from its request log. See
  [what the service does not do](docs/security.md#what-the-service-does-not-do).

## Hardening a deployment

If you are here because you are about to expose an instance rather than because you found something,
the guide is **[docs/security.md](docs/security.md)**, which ends in a hardening checklist.
