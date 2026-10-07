# Contributing

Hippocampus is one Go service plus a set of clients and bridges that dial it. This page covers
setting up, the tests that guard the project, and the checklists for the two changes people most
often make. Questions are welcome in [Discussions](https://github.com/fastbean-au/hippocampus/discussions).
Security issues go to the private channel in [SECURITY.md](SECURITY.md).

## Setting up

You need Go 1.27 or later. `go.mod` names the exact toolchain, and `GOTOOLCHAIN=auto` fetches it.

```sh
git clone https://github.com/fastbean-au/hippocampus.git
cd hippocampus
git config core.hooksPath hooks     # once per clone: runs hooks/pre-commit before each commit
go build ./...
go test ./...
```

`hooks/pre-commit` runs `go mod tidy`, `gofmt`, `go vet`, `golangci-lint` and the root module's
tests with coverage. Any failure stops the commit. If `golangci-lint` is missing, install the
version CI uses: `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2`.

### The modules

The repository holds several Go modules, so a dependency of one integration never reaches the
service's build. Each of these is tested from its own directory:

| Directory                  | What it is                                         |
| :------------------------- | :------------------------------------------------- |
| `.` (root)                 | the service, the storage layer, the shared packages |
| `integrations/cli`         | the `hippo` command-line client                    |
| `integrations/mcp`         | the MCP bridge                                     |
| `integrations/eventsource` | the broker bridges                                 |
| `integrations/ingestor`    | the edge-to-central ingestor                       |
| `integrations/objectstore` | the object-storage agents                          |

```sh
(cd integrations/eventsource && go test ./...)
```

The pre-commit hook runs the root module only, so run the module you changed by hand. The Python
client (`integrations/python`) is its own project: `pip install -e '.[dev]' && python -m pytest`.

### The three storage drivers

`go test ./...` runs against SQLite. The same shared suites run against PostgreSQL and MySQL when
pointed at a disposable database, and CI runs all three. Run them yourself after any change under
`db/`, or to anything that composes storage calls:

```sh
HIPPOCAMPUS_TEST_POSTGRES_DSN='postgres://user:pass@localhost:5432/test?sslmode=disable' \
HIPPOCAMPUS_TEST_DIALECT=postgres go test ./db ./hippocampus

HIPPOCAMPUS_TEST_MYSQL_DSN='user:pass@tcp(localhost:3306)/test' \
HIPPOCAMPUS_TEST_MYSQL_ADMIN_DSN='root:pass@tcp(localhost:3306)/test' \
HIPPOCAMPUS_TEST_DIALECT=mysql go test ./db ./hippocampus
```

MySQL must be 8.0.20 or later.

## Design record

[docs/design/](docs/design/README.md) explains why each subsystem is built the way it is. Read the
page for a subsystem before changing it: most of its guards exist because of an incident it
describes.

## Drift guards

Much of the test suite checks that two copies of something still agree: the contract and the API
reference, the config keys and their documentation, the alert rules and the instruments they read,
each RPC and its authorisation and scope. When one of these fails, read its message. It names the
place to update, and usually explains why the duplicate exists. Updating the other copy is almost
always the fix. Loosening the check is almost never the fix.

## Adding an RPC

1. Add it to `contract/hippocampus.proto` with a `google.api.http` annotation, and a comment on the
   RPC and on every new message and field (`contract/comments_test.go` refuses one without). Run
   `go generate ./contract`.
2. Give it a minimum tier in the `policies` table in `auth/authz.go`. An admin-tier RPC that is not
   a `GET` joins the audit trail through `auth.AdminMutations`, which is derived from that table.
3. Declare how it honours a caller's group scope in the `scopes` table in `hippocampus/scope.go`, and
   add a subtest to `hippocampus/scope_isolation_test.go`.
4. Implement it on `hippocampus.Server`, with tests.
5. Add its route to the table in `docs/api.md` (`TestRouteTableMatchesTheContract`).
6. Expose it in the `hippo` CLI and list it in `docs/cli.md` (`TestEveryCommandIsDocumented`), and
   in the Python client (`tests/test_contract_coverage.py`).
7. Decide whether the MCP bridge offers it. The tool set is held exact by `TestServer_EndToEnd` and
   documented in `docs/mcp.md`. Destructive, bulk and enumerating RPCs stay off it.
8. Check the change against the last release with `buf breaking` (see [RELEASE.md](RELEASE.md)), and
   add a CHANGELOG entry.

## Adding a configuration key

1. Read it with viper in `cmd/hippocampus`. A key the service package acts on gets a field in
   `hippocampus.Config` and is read in `serverconfig.go` (`TestServerConfigFillsEveryField` fails if
   the field is never assigned); the `hippocampus` package itself never reads configuration. Give
   the key a default in `setStartupDefaults` or with `viper.SetDefault`. Do not use a constant for a
   default.
2. If some values must stop the service starting, refuse them in `configProblems`, which also backs
   `--check-config`.
3. Offer it in the configuration wizard (`cmd/config-wizard/wizard/app.js`). Record its service
   default as `svc`. If the wizard deliberately leaves it out, add it to `unmanagedConfigKeys` with
   the reason.
4. Document it in the section of `docs/configuration.md` (or `docs/consolidation.md`) where it
   belongs. Then regenerate the key index:

   ```sh
   HIPPOCAMPUS_UPDATE_CONFIG_INDEX=1 go test ./cmd/hippocampus -run TestConfigIndex
   ```

5. Add a CHANGELOG entry. Narrowing the values an existing key accepts is a breaking change, even
   though the key itself is untouched.

## Conventions

- **Spelling** is Australian English everywhere, identifiers and config keys included
  (`authorisation`, `summarise`). Protocol and standard-library names keep their own spelling
  (`Authorization` header, `codes.Canceled`).
- **Logging** is logrus. Metric, trace and log attributes are snake_case. Metric attributes are
  never high-cardinality: no ids, no group names.
- **Errors** carry context with `fmt.Errorf` and `%w`. Errors that reach a caller go through
  `mapError`, which passes a gRPC status through and masks everything else as `Internal`.
- **Bug fixes** start with a test that fails without the fix.
- **CHANGELOG entries** are a few lines: what changed, for whom, and a link to the documentation
  that explains it. The reasoning belongs in that documentation, not in the changelog.

## Releasing

Maintainers cut releases with `scripts/release.sh`. [RELEASE.md](RELEASE.md) has the pre-flight
checklist and what a deliberate breaking change requires.
