# Commands and tooling

This is the design record moved out of CLAUDE.md verbatim (TODO-3 item 176): why the code is the way it
is, the alternatives that were rejected, and the incidents behind the guards. CLAUDE.md keeps the short
version and links here.

## Commands, with their design notes

- Build: `go build ./...`
- Run: `go run ./cmd/hippocampus -c config.json` (the `-c`/`--config_file` flag defaults to `./config.json`)
- Run the MCP server (separate module — run from its directory):
  `cd integrations/mcp && go run . --address localhost:50051` (a standalone MCP
  bridge that dials a running service; stdio by default, `--transport http` for streamable HTTP;
  see `docs/mcp.md`)
- Run the `hippo` CLI (separate module — run from its directory):
  `cd integrations/cli && go run . whoami` (a stateless command-line client exposing the full RPC
  surface; gRPC by default, `--transport http` for the `/v1` gateway; `go test ./...` in that dir;
  see `docs/cli.md`)
- Test the Python client (its own project, not a Go module — run from its directory):
  `cd integrations/python && pip install -e '.[dev]' && python -m pytest` (the `hippocampus-client`
  package - built and verified per release but **not yet uploaded to PyPI** (`vars.PUBLISH_PYPI`
  unset; TODO-2 item 123.1), so the docs give the git+tag install; the gRPC stubs are generated from
  `contract/hippocampus.proto` at build time and are **not committed** — the build hook runs on an
  editable install too, so there is no separate setup step, and `python scripts/generate_stubs.py`
  regenerates them after a contract edit without reinstalling; see `docs/python.md`)
- Run an event-sourcing bridge (separate module — run from its directory):
  `cd integrations/eventsource && go run ./cmd/nats --subject 'events.>' --address localhost:50051`
  (one `cmd/<broker>` each for `nats`/`mqtt`/`rabbitmq`/`kafka`/`bluesky`; consumes from the broker
  and stores each message as a memory; `go test ./...` in that dir, with `HIPPOCAMPUS_TEST_MQTT_BROKER`/
  `HIPPOCAMPUS_TEST_RABBITMQ_URL`/`HIPPOCAMPUS_TEST_JETSTREAM` set to run the integration tests; see
  `docs/eventsource.md`)
- Run the ingestor (separate module — run from its directory):
  `cd integrations/ingestor && go run ./cmd/ingestor --source-address localhost:50051 --target-address central:50051 --rules rules.json`
  (promotes completed events from an edge instance into a central one under a CEL rules file;
  `--check-rules` compiles the rules and exits, `--dry-run` judges without moving anything;
  `--health-port` (8090) serves `/healthz`+`/readyz` and `--metrics` exports OTLP;
  `go test ./...` in that dir needs no service; see `docs/ingestor.md`)
- Run the object-storage agents (separate module — run from its directory):
  `cd integrations/objectstore && go run ./cmd/object-gateway --bucket payloads --auth-token dev`
  (the tap: fronts a bucket, and every object it serves reinforces the pointer-memory behind it) and
  `go run ./cmd/object-reaper --bucket payloads --sweep-now` (the actuator: deletes the object behind
  a forgotten memory — **shadow mode unless `--delete`**, and `--sweep-now` is one pass and out).
  A pointer-memory's id must be `<bucket>/<key>`, which is what makes both agents stateless;
  `go test ./...` in that dir needs neither a bucket nor a service; see `docs/objectstore.md`
- Test: `go test ./...` (single test: `go test ./hippocampus -run TestName`)
- Benchmarks: `go test ./db -bench . -run XXX` (`db/bench_test.go`; run on demand — deliberately
  not CI-gated — and compare with benchstat when touching `hippocampus/sleep.go`, the db scans,
  or the schema; they pin that the consolidation scans never read memory bodies, eviction's
  scan+sort cost, and `UsedBytes` on all three drivers — the Postgres/MySQL ones need
  `HIPPOCAMPUS_TEST_POSTGRES_DSN`/`HIPPOCAMPUS_TEST_MYSQL_DSN`)
- Fuzz one target: `go test ./hippocampus -run '^$' -fuzz '^FuzzRedactEndpoint$' -fuzztime 2m` (the
  seeds, and any failing input saved under `testdata/fuzz`, run in plain `go test`; the nightly
  `Fuzz` workflow fuzzes every target, and `fuzz_workflow_test.go` holds its matrix to the `func Fuzz*`
  declarations in both directions)
- Vulnerability scan: `scripts/govulncheck.sh` (all six Go modules; needs `govulncheck` and `jq`;
  fails on a reachable finding not on its reviewed-exception list, and on an exception no longer
  reported; CI runs it as the `govulncheck` job)
- Lint: `trunk check` (config in `.trunk/trunk.yaml`: golangci-lint, gofmt, markdownlint, etc.)
- Regenerate protobuf/gRPC/gateway code after editing `contract/hippocampus.proto`:
  `go generate ./contract` (the `//go:generate` directive lives in `contract/generate.go`)
  (requires `protoc` plus the `protoc-gen-go`, `protoc-gen-go-grpc`, `protoc-gen-grpc-gateway`,
  and `protoc-gen-openapiv2` plugins, all `go install`-able; the `google/api` proto dependencies
  the gateway needs are vendored under `contract/google/api/`)
- Check the contract for breaking changes: `cd contract && buf breaking --against
'../.git#tag=<previous tag>,subdir=contract' --path hippocampus.proto` (config and rationale in
  `contract/buf.yaml`; CI runs it as the `proto-breaking` job against the last release tag, which
  always reports but only **fails** the build where semver does not already permit the break — it
  stands down against a pre-1.0 baseline, and for a major increment declared as
  `## [Unreleased] (v2.0.0)` in `CHANGELOG.md`; see `RELEASE.md`). The
  proto package is `hippocampus.v1`, so every gRPC method is `/hippocampus.v1.Hippocampus/<Method>`
  — three packages keep a hand-written copy of that prefix (`auth/grpc.go`,
  `cmd/hippocampus/rpcmetrics.go`, `hippocampus/server.go`), each held to the generated descriptor
  by a `TestServicePrefixMatchesDescriptor`, because a stale copy fails **open** in the auth
  interceptor and the purge gate. `buf lint` is deliberately not wired up — see `contract/buf.yaml`
- Provision the shipped Grafana alert rules into a real Grafana: `scripts/check-grafana-alerts.sh`
  (docker or podman; boots `grafana/otel-lgtm` with `deploy/compose/observability/alerting-rules.yaml`
  mounted as the stacks mount it, and fails unless Grafana starts, holds exactly the rules
  `prometheus-alerts.yaml` declares, and evaluates each cleanly). CI runs it as the `grafana-alerts`
  job. It exists because `alerts_test.go` only checks what somebody thought to check, and a
  provisioning file Grafana refuses takes the **whole server** down, which is what 0.49.0's
  41-character uid did (TODO-2 item 132.1)
- Demo/soak test: `./demo/run.sh` (builds and launches the service plus a load generator; see
  `demo/README.md`). By default it also launches a `grafana/otel-lgtm` collector (docker or
  podman) with the provisioned dashboard and ships metrics/traces to it (Grafana on `:3000`); set
  `OBSERVABILITY=0` to skip it. The env overrides are exported by `run.sh`, not baked into
  `demo/config.json`
- Docker: every compose file publishes its ports on `${PUBLISH_ADDRESS:-127.0.0.1}` (loopback unless
  that is set; `compose_ports_test.go` refuses a bare `"N:N"`, since published ports bypass a host
  firewall - TODO-3 item 168). `docker compose up --build` (SQLite), `docker compose -f deploy/compose/docker-compose.postgres.yaml
up --build` (PostgreSQL), `docker compose -f deploy/compose/docker-compose.mysql.yaml up --build` (MySQL), or
  `docker compose -f deploy/compose/docker-compose.opensearch.yaml up --build` (SQLite + OpenSearch content
  search, security disabled — demo only) or `docker compose -f
deploy/compose/docker-compose.opensearch-secured.yaml up --build` (the same with the OpenSearch security
  plugin enabled: HTTPS + basic auth, Hippocampus connecting over TLS via the `opensearch.tls`
  config block, credentials injected as `OPENSEARCH_ADMIN_PASSWORD`); container configs in
  `deploy/compose/`, image config baked from `deploy/compose/config.sqlite.json`. The `Dockerfile` is multi-stage:
  one build stage compiles both binaries, then an `mcp` stage (the `hippocampus-mcp` image) precedes
  the default `hippocampus` stage — the mcp stage is placed first so a no-`target` build still selects
  hippocampus, keeping every existing compose file unchanged. The event-sourcing broker bridges have
  their own `integrations/eventsource/Dockerfile` (parameterised by a `BROKER` build-arg, built from
  the repo root): `docker build -f integrations/eventsource/Dockerfile --build-arg BROKER=nats -t
hippocampus-nats-bridge .` — the release publishes one image per broker to
  `ghcr.io/fastbean-au/hippocampus-<broker>-bridge`
- Browser API explorer (SQLite compose only): `docker compose --profile swagger up --build` adds an
  opt-in `swagger-ui` service on `:8082`, a browser form over the `/v1` JSON API. Two things make it
  work and both are load-bearing: it is pointed at the **running gateway** (`SWAGGER_JSON_URL`, the
  host-published address — the browser resolves it, and the generated document declares no `host`,
  so Swagger UI addresses every "Try it out" call at whichever origin served the spec — a mounted
  copy of the file would make them all 404 against the container), and the gateway lists its origin
  in `gateway.corsOrigins`. Unauthenticated like the rest of that demo stack; against a secured
  instance the Authorize box works, since the contract declares a bearer `securityDefinition`. See
  `docs/clients.md`
- MCP-over-HTTP endpoint (SQLite compose only): `docker compose --profile mcp up --build` adds an
  opt-in `mcp` service (streamable-HTTP transport, `Dockerfile` `target: mcp`) that dials the
  `hippocampus` service over the compose network and publishes the MCP endpoint on the host's
  loopback `127.0.0.1:8090`; off by default (behind the `mcp` profile), unauthenticated unless
  `MCP_HTTP_TOKEN` is set. The bridge itself listens on loopback by default and **refuses** a
  non-loopback `--http-address` with no `--http-token` unless `--allow-unauthenticated-http`,
  because whoever reaches it acts with its writer token (TODO-3 item 160). The common
  local pattern is instead the stdio transport, spawned by the MCP host against the published
  `:50051` — no container. See `docs/mcp.md`
- Observability stack (any compose file): `OBSERVABILITY=true docker compose --profile observability
up --build` adds an all-in-one `grafana/otel-lgtm` service (Grafana `:3000`, OTLP `:4317`) behind a
  compose `observability` profile — off by default. The `hippocampus` service sets
  `HIPPOCAMPUS_OBSERVABILITY_*` env overrides (metrics/traces on from `${OBSERVABILITY:-false}`,
  endpoint `otel-lgtm:4317`), so metrics stay off (and never log an export failure) unless the
  collector is up. A Hippocampus overview dashboard (`deploy/compose/observability/`) is bind-mounted into
  Grafana's provisioning tree and set as the home page (`GF_DASHBOARDS_DEFAULT_HOME_DASHBOARD_PATH`),
  alongside `alerting-rules.yaml` (the shipped alerts as Grafana-managed rules) — see the alerting
  note under Architecture
- Kubernetes: `kubectl apply -k deploy/k8s/overlays/sqlite` (embedded SQLite: one `StatefulSet` +
  a PVC) or `kubectl apply -k deploy/k8s/overlays/postgres` (centralised: one consolidator
  `Deployment` + N replica `Deployment`s over a shared Postgres, mirroring the horizontal-scaling
  model; `overlays/mysql` is the same over MySQL). The base carries a default-deny `NetworkPolicy`,
  every overlay pins the image tag, and `examples/` holds an `ExternalSecret` and an `Ingress`. Kustomize base+overlays under `deploy/k8s/` — no Helm; a shared `base/` (namespace,
  token-less ServiceAccount, Service) plus per-overlay `config.json` wired through a
  `configMapGenerator` (content-hashed → auto-rolls on edit). Secrets (DSN, signing key) and the
  consolidator/replica split are injected as `HIPPOCAMPUS_*` env overrides, not baked into the
  ConfigMap; probes hit `/healthz`/`/readyz`; pods run non-root/read-only-rootfs. See
  `deploy/k8s/README.md`
- CI: `.github/workflows/ci.yaml` — build/vet/gofmt/tests (with postgres and mysql service
  containers so the `db/postgres_test.go` and `db/mysql_test.go` integration tests run instead
  of skipping) plus compose-stack smoke tests. Postgres/MySQL integration tests run locally with
  `HIPPOCAMPUS_TEST_POSTGRES_DSN=<dsn>`/`HIPPOCAMPUS_TEST_MYSQL_DSN=<dsn>` `go test ./db`
  against any disposable database. The `proto-breaking` job gates the contract (above). The `k8s` job builds every Kustomize
  overlay and validates it with kubeconform, and `k8s_test.go` holds every overlay's image pin
  (never `latest`) to the newest released version, which `scripts/pin-k8s-image.sh` moves from
  `release.sh`. The `race`
  job runs the root module under `-race` (SQLite only, a separate job because `db` alone takes over
  four minutes under it), and every integration module's job tests with `-race` too
- Run the `db` suite against a server dialect: `HIPPOCAMPUS_TEST_DIALECT=postgres go test ./db`
  (or `mysql`), with that dialect's DSN set. It re-points `newTestDB` (`db/conformance_test.go`), so
  the **same ~190 shared tests** execute there rather than on SQLite alone; CI runs all three. This
  is the cross-dialect conformance suite, and it is the safety net any change under `db/` wants:
  before it existed only 18 of the 74 `db.Store` methods had **any** server-driver coverage, and its
  first run found spreading activation silently inert on Postgres (a float bound into an
  integer-inferred parameter arriving as 0). A test that legitimately cannot run on a server dialect
  calls `requireSQLite`; one that opens its own store instead of `newTestDB` is refused by
  `TestSharedSuiteOpensThroughNewTestDB` unless its file is on that guard's allow-list. The same
  variable reruns the **service layer**: `HIPPOCAMPUS_TEST_DIALECT=postgres go test ./hippocampus`
  points `newTestServer` at `db/dbtest`, which gives every test a scratch schema (Postgres, from
  `HIPPOCAMPUS_TEST_POSTGRES_DSN`) or database (MySQL, from `HIPPOCAMPUS_TEST_MYSQL_ADMIN_DSN`) of
  its own, dropped afterwards - isolated rather than shared and emptied, because emptying from
  outside the package would need an exported "delete everything" on the production type. CI runs
  both packages this way and merges the three coverage profiles
- Cut a release: `scripts/release.sh --minor` (or `--patch`/`--major`/`--version X.Y.Z`) — runs the
  pre-flight, rolls `[Unreleased]` into a dated version section, rewrites both link references,
  commits and tags. It deliberately does **not** push (that is what starts the release workflow) and
  it **refuses** on an empty `[Unreleased]`, a dirty tree, a non-`main` branch, a `HEAD` that is not
  `origin/main`, or — the one that matters — a changelog whose newest version heading is not the
  current tag, which is how seventeen releases once shipped with their entries still under
  `[Unreleased]`
- Reclaim disk after builds/tests/soaks: `scripts/cleanup.sh` (add `--dry-run` to see it first).
  By default it removes only what costs nothing to recreate — `demo/bin`/`demo/data`/
  `demo/data-bluesky`/`demo/soak-runs`, stray module binaries, coverage profiles, dangling image
  layers, and orphaned **anonymous** container volumes — then trims the podman VM disk.
  `--images`/`--build-cache`/`--trunk` (or `--all`) additionally clear things that cost a
  re-download or a cold rebuild, which is why they are opt-in. Two things carry it. (1) **The trim
  is the point**: on macOS the podman machine's disk is a sparse file, so pruning frees space
  inside the guest and returns none of it to the host — `podman system df` reports gigabytes
  reclaimed while `df` does not move, and only `fstrim` inside the VM punches the holes back out.
  (2) Volumes are selected by `dangling=true` **and** a 64-hex name, not by letting the engine
  refuse the in-use ones: the first filter protects the test databases (which hang off _stopped_
  containers, and that is how they survive a reboot), the second protects named compose state, and
  relying on the refusal instead would make `--dry-run` overstate what it is about to delete. It
  never touches `~/.hippocampus` (a personal instance's real store) or the Go module cache
- Administer the **family of repositories** (the service plus `hippocampus-demo-site`,
  `-gen`, `-llamaindex`, `-obsidian`, `-otel-collector` and `homebrew-tap`), all from this
  repository because it is the hub the other six are satellites of. Four scripts — the first three
  idempotent and each taking `--dry-run`, the fourth read-only:
  - `scripts/apply-rulesets.sh` — the `protect-main` branch ruleset on every repository: `deletion`
    and `non_fast_forward`, no bypass actors, targeting `~DEFAULT_BRANCH`. **What it leaves out is
    the design**: required pull requests, required status checks and required linear history would
    all refuse the direct pushes these repositories are actually developed with, and the predictable
    response — a bypass actor for the only person who pushes — protects nothing. Tags are
    deliberately untargeted too, since a rebase of already-tagged work has to re-point them.
  - `scripts/setup-discussions.sh` — enables Discussions and seeds one welcome post per repository.
    The satellites' posts point at the service's Discussions for anything about the service, because
    conversation split seven ways across six thin satellites is six tabs holding one thread each.
  - `scripts/social-preview.py` — renders each repository's social-preview card (Pillow; the family
    mark from `docs/go-hippocampus.png` over the demo site's palette) to `.github/social-preview.png`
    in that repository's clone. Every card carries a human title rather than the repository name,
    because GitHub's fallback card is the NAME over the owner's avatar and six names all beginning
    `hippocampus-` are indistinguishable at the size a link preview renders. **Uploading is not
    scriptable** — GitHub exposes no API field for a repository's social preview — so the PNG is
    committed and set by hand under Settings → General.
  - `scripts/family-status.py` — reports, per repository, the latest release, the service version
    pinned **at that release** and on `main`, and how many commits no release carries; `--check`
    exits non-zero when anything is outstanding, `--verbose` lists the commits. It exists because
    the dispatch above ended at a merged pull request: nothing tagged, nothing published, and
    nothing said so, which left all three satellites carrying two unreleased service bumps at
    0.49.0. Two things carry it. (1) **The `pin@release` column is the point** — it reads the pin
    out of the release tag itself, because the two satellites whose version line continues this
    one's had both tagged before their bump merged (`v0.48.0` pinning `v0.47.0` in each), and a
    number claiming a correspondence it does not have is worse than no number. (2) It is **read-only
    and needs no token** (every repository is public; one only raises the rate limit), so it is safe
    in the release checklist and in the weekly `family-status` workflow, which keeps one standing
    issue open — reopened and closed, never one issue per run — while anything is outstanding.
    (3) A repository on its **own** version line is judged on what a release would actually **ship**
    (`SHIPS`), not on commit count: `hippocampus-obsidian` receives a re-vendored contract every
    service release and ships `main.js` built from `src/`, so counting those would leave it
    permanently red — and a report that is always red stops being read, which is the same failure by
    the other door. Only a complete comparison can prove nothing shipped, so a truncated file list,
    an empty one, or a `package.json` whose runtime `dependencies` are bundled into the release all
    report rather than go quiet. `hippocampus-llamaindex` and `hippocampus-otel-collector` now tag
    themselves from the pin when a bump lands (`release-on-bump` in each, dispatching their own
    `Release` because a `GITHUB_TOKEN`-created tag triggers no `push: tags:` event);
    `hippocampus-obsidian` is deliberately manual, its tag being a user-facing plugin version.
    (4) A pin is stale only when something the satellite **consumes** moved — its `SURFACE` entry,
    path prefixes in this repository — not when its number is lower, and `--dispatch-targets` asks
    the same question for the release workflow's `notify-satellites`, so a patch changing no
    satellite's surface dispatches nothing and the report and the dispatch cannot disagree. A
    misspelt prefix would match nothing and silently stop that satellite being told, which is why
    `family_test.go` requires every prefix to exist.
    [`cmd/hippocampus/family_test.go`](cmd/hippocampus/family_test.go) holds this script's
    repository table against the release workflow's dispatch loop in both directions — a repository
    in the loop and not the table is one whose staleness nobody is told about, which was
    `hippocampus-gen`'s condition until it gained a bump workflow of its own (it had sat eleven
    releases behind on v0.36.1). It is dispatched to and has **no release line**: its five generator
    images publish from `main` on push, so merging its bump is the release, and that bump is also
    the only build the new pin gets, there being no CI workflow there.
    The two shell scripts need `GITHUB_TOKEN` with `administration: write` (and `discussions: write`
    for the second)
- Release compatibility: `CHANGELOG.md` is the curated record (the GitHub release notes are a commit
  list); its **Compatibility** section states what a version number covers — contract, config keys
  **and the values they accept**, stored schema — and what is exempt. `RELEASE.md` carries the
  process, including the changelog step in the pre-flight and what a deliberate break requires.
  Pre-1.0, a breaking change goes in a minor release. Narrowing a key's accepted range is a break
  even though the key is untouched, and it is the only class with **no** mechanical gate behind it —
  `buf breaking` covers the contract, the version ledger and upgrade fixtures cover the schema, and
  `TestShippedConfigsAreValid` only ever sees the configurations in this tree — so the pre-flight
  asks it as a question (step 6) instead. 0.41.0 is the case: a tightened
  `consolidation.deletionThreshold` check, filed under **Fixed**, that stopped two demo instances
  from starting
- Run the configuration wizard (root module, second binary):
  `go run ./cmd/config-wizard` (serves the browser-based config/deployment builder on `:8091`;
  static assets only, no service connection; `--port`/`--bind-address`/`--log-level`, all
  `HIPPOCAMPUS_WIZARD_*` overridable; see `docs/config-wizard.md`)
- Print the build version: `go run ./cmd/hippocampus --version` (module + VCS revision/time from
  `runtime/debug.ReadBuildInfo`; prints and exits before the config is read — see `version.go`)
- Mint an auth token: `go run ./cmd/hippocampus --mint-token --client-id <id> --role writer --ttl 24h -c config.json` (prints the token and exits; see [Authentication](docs/configuration.md#authentication))
- Backfill/rebuild the OpenSearch index: `go run ./cmd/hippocampus --backfill-search [--reindex] -c config.json`
  (CLI mode in `backfill.go`, exits when done; requires `opensearch.enabled`; safe beside a live
  instance; see [Backfill and reindex](docs/configuration.md#backfill-and-reindex))
- Read a store's schema version: `go run ./cmd/hippocampus --schema-version [--output json] -c config.json`
  (CLI mode in `schema.go` over `db.InspectSchema`; prints the recorded version, the applied
  migrations and what an upgrade would do, then exits non-zero if the store is newer than this
  build). Both renderings derive their verdict from one `schemaStatus`, so the word in the text and
  the `status` value in the JSON cannot disagree; the JSON is a projection rather than tags on
  `db.SchemaReport`, on the same rule the MCP bridge follows — the wire shape is the command's
  contract, and the storage layer should not acquire one by being marshalled. The mode points
  logging at **stderr** before rendering, because stdout is a data channel: one log line on it makes
  the JSON unparseable, and it is the `ahead` path — the one a deployment script gates on — that
  both renders and then fatals. It
  deliberately does **not** go through the read-only constructors: those apply the version gate, so
  against the store an operator most needs this for — one a refused downgrade has left unopenable —
  they refuse and there is nothing left to report. Takes no lock and runs no DDL, so it is safe
  beside a live instance. It is a flag on this binary rather than a `hippo` subcommand because the
  CLI is a network client and a stopped store has nothing to dial
- Back up a SQLite store: `go run ./cmd/hippocampus --backup <path> -c config.json` (CLI mode in
  `backup.go` over `db.BackupTo`: `VACUUM INTO` through a read-only open, so it is safe beside a
  live instance and takes no lock; refuses an existing destination, and refuses the server drivers
  with the tool to use instead). Its scheduled counterpart is `archive.scheduledExport.*`
  (`hippocampus/scheduledexport.go`): the consolidating instance exports on an interval, prunes only
  its own `scheduled/` prefix to `keep`, reads its schedule back from the archives already there, and
  publishes the gauges `HippocampusScheduledExportStale` alerts on (TODO-3 item 158)
- Validate a configuration without starting: `go run ./cmd/hippocampus --check-config [--output json] -c config.json`
  (CLI mode in `checkconfig.go`, exits non-zero when the service would refuse to start; touches no
  store and no network, so it is safe in a build step or beside a live instance). It checks the
  **resolved** configuration, not the file — the same read, `HIPPOCAMPUS_*` overrides and defaults a
  real start applies — because viper's precedence means a sound `config.json` can still be invalid
  once a container's env injection lands. It reports **every** problem rather than the first, which
  is why `validateConfig` is now a thin wrapper over `configProblems() []error`: a pre-flight tool
  surfacing one fault per run sends an operator around the restart loop once per mistake, and
  startup gains the same list for free. Its dispatch sits **above** the "no configuration file"
  Warn line (unlike every other CLI mode's) for the reason `--schema-version` documents about its
  own output — logging writes to stdout, so a Warn line emitted before the mode can redirect it
  corrupts the JSON; the same reasoning moved that warning below `--schema-version` too, where it
  had been silently breaking `--output json` on a host with no config file.
  `TestShippedConfigsAreValid` is the other half: it runs `configProblems` over every configuration
  file in the repo (glob-based, so a new one is covered without anybody remembering), with
  `TestShippedConfigsCoverEveryDriver` pinning that all three drivers stay represented. Both exist
  because 0.41.0 shipped a rule that no configuration here violated and two on the public demo did

## `demo/`

`demo/` — a long-running load generator (`demo/generator`, its own `main` package) plus a
launch script (`run.sh`) and a demo-tuned config. Bursty/slow/event-less writers, query and
recall workers, and a mutator exercise every RPC; a watcher pauses writes while the database
is at its size cap (default 1 GiB, `MAX_BYTES` env var overrides). The demo config compresses
the decay clock (`unitsOfAgeInDays` 0.002 ≈ one age unit per 3 minutes) so forgetting,
recall reinforcement, and the byte capacity target all play out within a session instead of
over real days.
