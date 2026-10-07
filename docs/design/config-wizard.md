# The configuration wizard

This is the design record moved out of CLAUDE.md verbatim (TODO-3 item 176): why the code is the way it
is, the alternatives that were rejected, and the incidents behind the guards. CLAUDE.md keeps the short
version and links here.

## `cmd/config-wizard/`

`cmd/config-wizard/` — the configuration and deployment wizard: a second `package main` in the
root module (`main.go` plus the embedded `wizard/` assets). The Go side is a static file server
and nothing else — embedded `index.html`/`app.js`/`styles.css` behind a strict CSP (no
`unsafe-inline`, `connect-src 'none'`), plus `/healthz`. All the work is in `wizard/app.js`: a
schema (`STEPS` → cards → fields, indexed into `FIELDS`) that drives the form, the validation, and
the generated artefacts from one source. It builds a `config.json` plus an `HIPPOCAMPUS_*`
environment file for the secret-typed keys, and a Compose file / Kubernetes manifests / systemd
unit / launchd plist / `DEPLOY.md` runbook per deployment target, all in the browser (nothing is
transmitted, and secrets are kept out of `localStorage`). Validation mirrors `validateConfig` and
the driver switch in `cmd/hippocampus/main.go`; the decay preview mirrors `calculateValue` in
`hippocampus/sleep.go`. Beyond mirroring the startup refusals, `validate()` carries a third class
of check the service itself has no place for: **combinations in which every value is individually
valid and the whole does not do what it looks like it does** — a cluster answering searches the
store already answers, sleep-cycle settings on a replica, a store with nothing to trigger a cycle,
a loopback bind inside a container. Three things carry them. (1) They are **advice, not
refusals**: each names the case where the pairing is deliberate, because most of them are
somebody's real deployment. (2) They read conditional keys through `active`/`on`/`num` rather than
`val`, since a field whose `when` is false never reaches the generated config and a check reading
one would judge a value nobody set — the stale answer left behind when the key that revealed it
was turned off again. (3) A mistyped key here fails **silently and in the wrong direction** (an
unknown key reads as `undefined`, every comparison answers no, and the check simply never fires),
so `validate_test.go` holds every dotted key literal in `validate()` against the declared field
set and every issue's step id against the declared steps. Each field records `def` (what the wizard suggests) and, where the service
has one of its own, `svc` (the `viper.SetDefault` value) — that distinction is what makes the
"minimal" config safe, since a key the service does not default reads as zero and several of those
are fatal; `defaults_test.go` cross-checks the two files so they cannot drift. The
**starting points** (`PRESETS`) are use cases, not deployment shapes (the target tiles already
choose those): each names its `docs/use-cases.md` section, which names it back, and declares what
only the operator can `supply` and which warnings it `accepts` on purpose. `wizardtest/` (run with
`node --test`, nothing to install, a step in the `webui` CI job) is the only test that
**executes** `app.js` — under `node:vm`, with the trailing `init()` cut off — and runs `validate()`
over every starting point, because a preset that trips the wizard's own checks is the wizard
advising against its own suggestion, which three of the original five did. Ships as its own
image (`Dockerfile` `target: config-wizard` → `ghcr.io/fastbean-au/hippocampus-config-wizard`) and
a per-OS/arch release binary; the hosted copy is `config-builder.hippocampus-demo.com`, a service
in the separate demo-site repo's combined showcase stack. See `docs/config-wizard.md`.
