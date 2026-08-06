# Releasing Hippocampus

Releases are driven entirely by **semver git tags**. A tag such as `v1.2.3` is simultaneously the
release label, the Go module version, and — stamped into the binary at build time — the version the
running service reports through `--version`, the `/healthz` body, and the OTEL `service.version`
attribute. Because all of these derive from the one tag, they can never drift out of lockstep.

Pushing the tag triggers `.github/workflows/release.yaml`, which runs the test suite as a gate,
creates the GitHub release, and publishes the container image to GHCR. Coverage is reported to
Coveralls separately by the CI workflow on push to `main` (see below). The steps below are the local
pre-flight plus the one command that starts all of that.

## One-time setup

- **`COVERALLS_TOKEN` repository secret** — the CI workflow reports coverage to Coveralls with it on
  every push to `main`. Set it under **Settings → Secrets and variables → Actions** (or
  `gh secret set COVERALLS_TOKEN --repo fastbean-au/hippocampus`). No token is ever committed.
- **`HOMEBREW_TAP_TOKEN` repository secret** — a cross-repo credential with write access to
  [`fastbean-au/homebrew-tap`](https://github.com/fastbean-au/homebrew-tap), used by the
  `bump-homebrew` job to push the updated formulae. The built-in `GITHUB_TOKEN` cannot push to
  another repository, so this is required for the tap to auto-update. Its only actions are cloning
  the tap and pushing a commit, so it needs exactly one permission — **repository contents,
  read/write** — and nothing more:
  - **Fine-grained PAT (recommended):** resource owner `fastbean-au`, *Only select repositories* →
    `homebrew-tap`, permissions **Contents: Read and write** (Metadata: Read-only is mandatory and
    auto-added). Grant nothing else — in particular **not** _Workflows_: the bump only edits
    `Formula/*.rb`, never `.github/workflows/`, and a fine-grained token is rejected on a push that
    touches workflow files, so omitting it is both sufficient and a guard. Set an expiry and rotate
    it. (If `fastbean-au` is an org, fine-grained PATs may need org approval.)
  - **Classic PAT:** `public_repo` scope suffices for the public tap (`repo` if it is ever private) —
    broader, since classic tokens cannot be scoped to a single repository.
  - **Deploy key alternative:** a per-repo SSH key with write access added to the tap's *Settings →
    Deploy keys* is the tightest scope, but the `bump-homebrew` checkout step must then use
    `ssh-key:` instead of `token:`.

  **Optional** — when the secret is absent the job self-skips (it does not fail the release), so a
  fork or a repo without the tap configured is unaffected. If the tap's `main` is branch-protected to
  require PRs, exempt this credential or the direct push will fail.
- **Git hooks** — point git at the tracked hooks once per clone: `git config core.hooksPath hooks`.
- GHCR publishing needs no secret: the workflow authenticates with the built-in `GITHUB_TOKEN`.

## Local pre-flight

Run these before cutting a tag. Most of the mechanical checks are also enforced by
`hooks/pre-commit`, but the benchmark and coverage review are manual judgement calls.

1. `go mod tidy` — no unexpected `go.mod`/`go.sum` churn.
2. `go vet ./...`
3. `golangci-lint run`
4. Benchmarks (on demand — not CI-gated):
   `go test -bench=. -timeout 300s ./db -run XXX > bench.out`, then update any tables/graphs and
   compare with `benchstat` when `hippocampus/sleep.go`, the db scans, or the schema changed.
5. Coverage review:

   ```sh
   go test -coverprofile=coverage.out $(go list ./... | grep -v '/cmd/')
   go tool cover -html=coverage.out -o coverage.html
   ```

   Open `coverage.html` and confirm nothing important regressed. (`cmd/` is excluded here and in the
   release workflow — the main-package wiring is covered by the docker smoke tests in CI, not unit
   coverage.)

6. Update [`CHANGELOG.md`](CHANGELOG.md): rename the `[Unreleased]` heading to the version and its
   date, open a fresh empty `[Unreleased]`, and add the two link references at the foot of the file.
   Anything under **Breaking** must also be reflected in the release notes people actually read —
   see [Compatibility](#compatibility) below.
7. Land all changes on `main` (PR merged, or pushed) — the tag should point at the commit you intend
   to release.

## Compatibility

What a version number promises, and what is exempt, is stated once in
[`CHANGELOG.md`](CHANGELOG.md#compatibility) rather than repeated here. Two parts of it are the
release process's business:

- **The contract is gated, not merely documented.** The `proto-breaking` CI job runs `buf breaking`
  on `contract/hippocampus.proto` against the most recent release tag reachable from the commit, so
  an accidental field renumbering or a removed RPC fails the build before it can ship. Configuration
  and rationale live in [`contract/buf.yaml`](contract/buf.yaml).
- **A deliberate break needs three things**, in this order: agreement that it is worth doing *now*
  (pre-1.0 is far cheaper than after — every generated client is a copy of the contract, and there
  are more of them each release); a **Breaking** entry in the changelog saying what a caller must
  change, not merely what changed; and a major version bump, or — while the leading version
  component is 0 — a minor one, which semver permits and this project has used. The gate does not
  stand in the way: it compares against the previous release, so the first tag carrying the new
  contract becomes the new baseline automatically.

## Cut the release

```sh
git tag v1.2.3        # semver, v-prefixed
git push --tags
```

That's the whole manual release action. Choose the version with normal semver rules: patch for
fixes, minor for backward-compatible features, major for breaking changes.

## What the workflow does

`.github/workflows/release.yaml`, on any `v*` tag:

1. **`release` job** — runs `go test` against real Postgres, MySQL, and OpenSearch service
   containers (so the gate reflects the integration tests, not just the SQLite paths) as a gate,
   then `gh release create` builds the GitHub release with auto-generated notes from the tag.
   Coverage is not reported here — a tag-triggered run would file it under the tag ref, leaving the
   `?branch=main` badge stale; the CI workflow reports coverage to Coveralls on push to `main`. The
   generated notes are a commit list, not the compatibility record; that is `CHANGELOG.md`, written
   in the pre-flight above and already on the tagged commit.
2. **`binaries` job** (gated on `release`) — cross-compiles `hippocampus`, the `hippo` command-line
   client, and `hippocampus-mcp` (plus the four event-sourcing bridges) for `linux`, `darwin`, and
   `windows` on `amd64` and `arm64` (pure Go, CGO disabled, so the whole matrix builds on one
   runner), archives each with `LICENSE` (`.tar.gz`, or `.zip` for Windows), then repackages the
   Linux `hippocampus` binary into `.deb`/`.rpm` packages (via nfpm, `amd64`/`arm64` — carrying the
   systemd unit and a default config, see [`deploy/nfpm/`](deploy/nfpm/)), and attaches every archive
   and package plus a `checksums.txt` (generated last, so it covers the packages too) to the release.
   This is what lets someone run the CLI or the MCP bridge — which an MCP host spawns locally over
   stdio — without a Go toolchain, and install the service natively without a container.
3. **`publish` job** (gated on `release` succeeding, so a red build publishes nothing) — builds two
   images and pushes them to GHCR, each tagged with the full version (`1.2.3`), the rolling
   `major.minor` (`1.2`), and `latest` for non-prerelease tags: the service image at
   **`ghcr.io/fastbean-au/hippocampus`** and the MCP-bridge image (Dockerfile `target: mcp`) at
   **`ghcr.io/fastbean-au/hippocampus-mcp`**. The tag is passed as `--build-arg VERSION=v1.2.3`, so
   each published binary reports the release version.
4. **`bump-homebrew` job** (gated on `binaries`, so the assets and `checksums.txt` exist first) —
   updates the [`fastbean-au/homebrew-tap`](https://github.com/fastbean-au/homebrew-tap) formulae to
   the new release: it downloads the release's `checksums.txt`, runs
   [`deploy/homebrew/bump-formulae.py`](deploy/homebrew/bump-formulae.py) to rewrite the version and
   per-arch `sha256`s in `hippocampus`, `hippocampus-cli`, and `hippocampus-mcp`, then commits and
   pushes to the tap. Requires the `HOMEBREW_TAP_TOKEN` secret above; **self-skips** when it is
   absent (so the release still succeeds). The parallel `publish-otel-collector` and
   `publish-eventsource-bridges` jobs (also gated on `release`) publish the collector and the four
   per-broker bridge images to GHCR.

## After the release

- Verify the GitHub release page, its generated notes, and the attached binary archives, `.deb`/
  `.rpm` packages, and `checksums.txt`.
- Verify the service image: `docker pull ghcr.io/fastbean-au/hippocampus:1.2.3` and
  `docker run --rm ghcr.io/fastbean-au/hippocampus:1.2.3 --version` should print `v1.2.3`.
- Verify the MCP image: `docker run --rm ghcr.io/fastbean-au/hippocampus-mcp:1.2.3 --version` should
  print `v1.2.3`.
- Verify the Homebrew tap updated: the `bump-homebrew` job should have pushed a `hippocampus v1.2.3`
  commit to [`fastbean-au/homebrew-tap`](https://github.com/fastbean-au/homebrew-tap), and
  `brew update && brew install fastbean-au/tap/hippocampus-cli` should install `v1.2.3` (the tap's
  own CI also re-runs `brew style`/`brew audit` on that push). Skip this check if
  `HOMEBREW_TAP_TOKEN` is not configured.
- Confirm the coverage update on Coveralls — it lands from the CI run for the merge to `main`, not
  from the tag push.

## How the version reaches the binary

A git tag only flows into `debug.BuildInfo.Main.Version` when the module is resolved through the Go
proxy — never for a working-tree or Docker build, which would otherwise report `(devel)`/`unknown`.
So the tag is injected explicitly: the `Dockerfile` builds with
`-ldflags "-X main.buildVersion=${VERSION}"`, and `cmd/hippocampus/version.go` prefers that value
over the embedded module version when set. For local `go build`/`go run` without the flag the binary
reports `unknown` plus the VCS revision, which is expected outside a release build.
