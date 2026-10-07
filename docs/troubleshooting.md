# Troubleshooting

Symptom first, then the cause and the fix. The quoted messages are the ones the service actually
logs or returns, so searching this page for the text in front of you should find the entry.

Two tools answer most questions before this page is needed:

- `hippocampus --check-config -c config.json` lists **every** problem that would stop the service
  starting, against the resolved configuration (file, `HIPPOCAMPUS_*` overrides and defaults
  together). See [Validating a configuration](operations.md#validating-a-configuration-before-it-starts).
- `hippo whoami` (or the `WhoAmI` RPC) reports the tier and group scope your token resolved to, and
  which features this deployment serves.

## The service will not start

| Symptom | Cause and fix |
| :------ | :------------ |
| `another hippocampus instance already holds the storage lock on '…'` | A second process is pointed at the same SQLite `storage.directory`. SQLite is single-instance: give each instance its own directory, or move to `postgres`/`mysql`, which support one consolidator plus replicas. The lock is the kernel's, so a crashed holder leaves nothing to clear. |
| `another hippocampus instance already holds the instance lock` | A second **consolidating** instance on a shared Postgres or MySQL store. Run the extra instances with `consolidation.enabled: false` (replicas), or with `consolidation.standby` to take over when the consolidator stops. See [Deployment model](operations.md#deployment-model-one-consolidating-instance-per-store). |
| `database schema is newer than this build supports` | The store was opened by a newer release, which migrated it. Downgrading is not supported: run that release or a later one. `--schema-version` reports what the store records. |
| `storage.directory must be set for storage.driver 'sqlite'` | The SQLite driver needs a directory. The built-in default is `./data`, so this appears only when the key is set to an empty value. |
| `consolidation.method must be between 1 and 6` / `consolidation.aggressiveness must be greater than 0` / `consolidation.unitsOfAgeInDays must be greater than 0` | A decay setting is out of range. An unset key takes its default; a key set to 0 does not. See [Memory consolidation](consolidation.md). |
| `consolidation.aggressiveness must be greater than 1/e (~0.368) for consolidation.method 3` | Method 3's factor goes non-positive at or below 1/e. Raise the value or choose another method. |
| `consolidation.standby needs a shared store` / `consolidation.standby is set with consolidation.enabled true` | Standby is a server-driver feature, and a standby starts as a replica: set `consolidation.enabled: false` beside it. |
| `s3.bucket and archive.directory are both set` | Export and Import use exactly one object store. Remove one. |
| `bind: address already in use` | Two components share a default port. 8090 and 8091 are each the default of more than one component - see the [port map](operations.md#ports). |

## Requests are refused

| Code and message | Cause and fix |
| :--------------- | :------------ |
| `Unauthenticated`, `missing authorization metadata` | Authentication is on (`auth.method` is `hmac` or `idp`) and no token was sent. Send `authorization: Bearer <token>` (gRPC) or `Authorization: Bearer <token>` (HTTP). |
| `Unauthenticated`, `invalid token` | The token failed verification: expired, signed with another secret, revoked, or (under `idp`) issued for another issuer or audience. A bridge on a static token fails this way once the token expires - use the OIDC client-credentials flags instead. |
| `PermissionDenied`, `insufficient role` | The token's role does not reach the RPC's tier. `hippo whoami` shows the tier it resolved to; tokens are minted with `--role reader`, `writer` or `admin`. |
| `NotFound` for a record you know exists | Your token is group-scoped and the record belongs to another group. An out-of-scope id deliberately reports `NotFound`, so its existence is not confirmed. See [Group scoping](configuration.md#group-scoping). |
| `PermissionDenied`, `group "…" is outside this token's scope` | A scoped token tried to write to a group it does not hold. |
| `Unavailable`, `purge in progress` | A `Purge` is running; every other RPC is refused until it finishes. Retry. |
| `ResourceExhausted`, `rate limit exceeded (…)` | [Rate limiting](configuration.md#rate-limiting) is on and the caller exceeded its rate. Back off and retry, or raise the limit. |
| `ResourceExhausted`, `received message larger than max` | A gRPC message over 4 MiB. Raise `maxRecvMsgBytes` on the service (and the client's own receive limit for large reads), or page the request. |
| HTTP `413` | A gateway body over `gateway.maxRequestBytes`, which defaults to twice the gRPC limit. |
| `AlreadyExists`, `a record with that id already exists` | A create used an id the store already holds. `ImportBatch` is the upsert. |
| `Aborted`, `write conflict, retry the request` | Two writes raced on a server driver. Retrying is safe. |
| `Internal`, `internal error` | Something failed inside the service. The log line `internal error handling request: …` carries the cause; the response deliberately does not. |

## A feature answers `FailedPrecondition`

| Message | Cause and fix |
| :------ | :------------ |
| `content search is not available on this instance: …` | `search.contentIndex.enabled` turned the store's own index off and no OpenSearch is configured. Turn either on. |
| `consolidation is disabled on this instance` | `Sleep`, `PreviewConsolidation` or `ExplainConsolidation` on a replica. Ask the consolidating instance. |
| `no object store is configured (set archive.directory or s3.bucket)` | `Export` or `Import` with no object store. Set one of the two. |
| `no transfer target is configured (transfer.targetAddress)` | `Transfer` needs a target instance. |
| `the deployment topology view is disabled on this instance (topology.enabled)` | `GetTopology` is off here. |
| `event '…' still holds memories, and if_empty was set` | `DeleteEvent` with `if_empty` found a memory that arrived since the caller last looked. This is the guard working; judge the new memory, then delete again. |

`SummariseMemories` needs the embedded LLM (`llm.enabled`). `WhoAmI` reports `summariser_enabled`,
`search_modes`, `consolidation_enabled` and `tombstones_enabled`, so a client can tell before it
asks.

## Something is quietly not happening

| Symptom | Cause and fix |
| :------ | :------------ |
| A write returns an empty id and `rejected: true` | Its significance is below `memory.minimumSignificance` (or `event.minimumSignificance`). This is the store declining to keep an insignificant write, not an error. See [Minimum significance](configuration.md#minimum-significance). |
| Nothing is ever forgotten | No cycle is running. Check `GetConsolidationStatus` (`hippo status`): `consolidation_enabled` false means this is a replica; a `period_seconds` of 0 means the timed cycle is off. On a shared store, `GetTopology` warns when **no** instance is consolidating. |
| Cycles run but report zero, and `stalled` is true | `callbacks.backlogPolicy: stall` is holding forgetting while the callback queue is over its caps. The receiver is down or refusing. See [When a forget-callback is an instruction](configuration.md#when-a-forget-callback-is-an-instruction-not-a-notification). |
| `GetForgottenMemories` is always empty | The forgotten log is off (`consolidation.tombstones.enabled`). The response says so in `enabled`. |
| The disk grows while capacity pressure stays flat | What grows is outside the capacity target: the forgotten log, the search outbox or the callback queue (`GetConsolidationStatus.ancillary`), or index bloat on a server driver (`footprint`). See [Sizing and capacity tuning](operations.md#sizing-and-capacity-tuning). |
| `curl localhost:8080` is refused | The gateway is off unless given a port. Set `gateway.port` (or `--gateway-port 8080`); the startup log names what is off while it is 0. |
| `grpcurl … list` fails on an authenticated instance | Server reflection is off by default once auth is on. Pass `-proto contract/hippocampus.proto`, or set `reflection.enabled`. |
| A broker bridge is running and the store holds nothing new | Its token has expired (every write fails `Unauthenticated` while the bridge keeps consuming), or every message is already held (`outcome="exists"`). The `HippocampusBridgeNotConsuming` alert and the bridge's `/readyz` catch both. |
| The object gateway serves objects but nothing is reinforced | The pointer-memories' ids are not `<bucket>/<key>`. A recall of an unknown id matches nothing, silently. See [When the tap reinforces nothing](objectstore.md#when-the-tap-reinforces-nothing). |

## Still stuck

Run with `logging.level: debug`, and open a [discussion](https://github.com/fastbean-au/hippocampus/discussions)
with the version (`--version`), the driver, and the log lines around the failure. Security issues go
to the private channel in [SECURITY.md](../SECURITY.md), not a discussion.
