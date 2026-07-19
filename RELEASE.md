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

6. Land all changes on `main` (PR merged, or pushed) — the tag should point at the commit you intend
   to release.

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
   `?branch=main` badge stale; the CI workflow reports coverage to Coveralls on push to `main`.
2. **`publish` job** (gated on `release` succeeding, so a red build publishes nothing) — builds the
   image and pushes it to **`ghcr.io/fastbean-au/hippocampus`**, tagged with the full version
   (`1.2.3`), the rolling `major.minor` (`1.2`), and `latest` for non-prerelease tags. The tag is
   passed as `--build-arg VERSION=v1.2.3`, so the published binary reports the release version.

## After the release

- Verify the GitHub release page and its generated notes.
- Verify the image: `docker pull ghcr.io/fastbean-au/hippocampus:1.2.3` and
  `docker run --rm ghcr.io/fastbean-au/hippocampus:1.2.3 --version` should print `v1.2.3`.
- Confirm the coverage update on Coveralls — it lands from the CI run for the merge to `main`, not
  from the tag push.

## How the version reaches the binary

A git tag only flows into `debug.BuildInfo.Main.Version` when the module is resolved through the Go
proxy — never for a working-tree or Docker build, which would otherwise report `(devel)`/`unknown`.
So the tag is injected explicitly: the `Dockerfile` builds with
`-ldflags "-X main.buildVersion=${VERSION}"`, and `cmd/hippocampus/version.go` prefers that value
over the embedded module version when set. For local `go build`/`go run` without the flag the binary
reports `unknown` plus the VCS revision, which is expected outside a release build.
