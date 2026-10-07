# Conventions

This is the design record moved out of CLAUDE.md verbatim (TODO-3 item 176): why the code is the way it
is, the alternatives that were rejected, and the incidents behind the guards. CLAUDE.md keeps the short
version and links here.

## Conventions in this repo

- Logging is **logrus** (not zerolog), typically with a `log.Trace("func() ...")` entry line at the
  top of functions — match this existing style rather than global preferences.
- Errors are logged where they occur and returned unwrapped with `fmt.Errorf`.
- Exactly one instance may consolidate a given store, and on the server drivers a replica with
  `consolidation.standby` takes over when it wins the lock (`hippocampus/standby.go`; the role is
  read through `consolidating()`, and the consolidator-only workers start behind
  `runsConsolidatorWork()`, because `promote` must start them before the role it publishes is
  visible to an RPC - TODO-3 item 169). Administrative mutations are audited by
  `hippocampus.Audited`, the decorator both transports serve (`auth.AdminMutations` is the set,
  and a test drives every member of it through the decorator). SQLite is single-instance (embedded DB),
  enforced by the `hippocampus.lock` file lock described above; on
  the `postgres`/`mysql` drivers a shared database can have one consolidating instance
  (`consolidation.enabled: true`, holds the lock) plus read/write replicas
  (`consolidation.enabled: false`, skip the lock, reject the `Sleep` RPC) — horizontal scaling.
  Authentication (JWT bearer tokens) and TLS
  are both optional and disabled by default; see [Authentication](docs/configuration.md#authentication)
  and [TLS](docs/configuration.md#tls).
- **A shared store is a shared trust domain unless tokens are group-scoped.** `group` is a label
  with no access-control meaning of its own; binding it to a token's `groups` claim
  ([Group scoping](docs/configuration.md#group-scoping)) makes it a **soft** partition — records are
  scoped, but the decay dynamics stay store-global, so a busy group still influences what another
  forgets. Hard isolation is still one instance per tenant, which is what item 9 and item 60.1 both
  concluded. Anything new that reads or writes stored records must therefore decide how it honours a
  caller's scope, and say so in `hippocampus/scope.go` — the drift guard will not let a new RPC
  through without it.
