# The web console

Every instance serves a browser console at **`/ui`**, embedded in the binary. It is not a separate
deployment, an npm build, or an optional image: it is four files compiled into the service, and it
drives the same `/v1` endpoints [the CLI](cli.md) and any other client do.

It exists because the premise of this store — that it is finite and forgets — is invisible from a
list of rows. The console's landing view is that premise made live: how much is held, what the last
cycle forgot, and how long until the next one runs.

## Reaching it

The console is served by the HTTP gateway, so it needs `gateway.port` set — a zero port disables
the gateway and the console with it (along with the OpenAPI document and the HTTP probes; the
startup log says so explicitly).

```json
{ "gateway": { "port": 8080 } }
```

Then open [`localhost:8080/ui`](http://localhost:8080/ui). There is nothing else to install, and
the page makes **no external requests at all** — it is served under a Content-Security-Policy with
no `unsafe-inline` and no remote origins, so it works on an air-gapped host and cannot leak a memory
body to a third party by loading a font.

## Signing in

What the console offers depends on the instance's `auth.method` (see
[Authentication](configuration.md#authentication)):

| `auth.method` | What the console shows                                                                                              |
| ------------- | ------------------------------------------------------------------------------------------------------------------- |
| `none`        | No sign-in at all — the console opens straight onto the store.                                                      |
| `hmac`        | A **bearer token** box. Mint one with `--mint-token`; it is held in the browser only.                               |
| `idp`         | A **Sign in** button, either the in-browser PKCE flow (`auth.ui`) or the service-hosted OIDC login (`auth.oauth2`). |

The console reveals itself only once `GET /v1/whoami` answers, and it adapts to the tier that call
reports: a `reader` is shown no write controls, and the Decay tab's dry-run panel appears only for
an `admin`. **Those hidden controls are a convenience, not a boundary** — the server enforces every
tier on every request, and a hidden button is simply one nobody is invited to press. See
[The web console](security.md#the-web-console-ui) for what that boundary actually is.

## The tabs

| Tab            | What it answers                                                                                           |
| -------------- | --------------------------------------------------------------------------------------------------------- |
| **Now**        | What is this store holding, when does it next forget, and what did it forget last?                        |
| **Search**     | Which memories match this text? (Or, with an event id and no query, what is in this event?)               |
| **Memories**   | Browse, filter, create, edit, link.                                                                       |
| **Events**     | The same for events, plus the open event's memories and the summarisation candidates the last scan found. |
| **Decay**      | Where does a memory stand against the threshold, and what would the next cycle take?                      |
| **Deployment** | What is this instance attached to, and is each part of it healthy?                                        |

**Now** is the landing view and reads from three RPCs: `GetConsolidationStatus` for the schedule and
the last cycle's counts, `ExplainConsolidation` for the capacity figures, and `GetForgottenMemories`
for the feed of what has just gone. That feed is empty unless the
[forgotten log](operations.md#what-was-forgotten--the-forgotten-log) is enabled — nothing else in the
service can speak about a memory that no longer exists.

**Decay** is the client side of the same transparency set: a per-row value column in the memory and
search tables, the current capacity pressure and the threshold it scales, an inline-SVG decay curve
for the configured algorithm, and — for an `admin` — a dry run over
[`PreviewConsolidation`](operations.md#previewing-what-would-be-forgotten) that deletes nothing. The
tab is hidden on a replica, which runs no cycle of its own.

**Deployment** renders [`GetTopology`](configuration.md#deployment-topology): this instance, whatever
it dials, its peers on a shared store, the components declared under `topology.components`, and the
clients observed calling it. Each node carries where it came from and when it was last probed, so a
sparse diagram reads as "nothing declared" rather than "nothing running". The tab is hidden both
when the instance does not serve the view (`topology.enabled` false) and when the caller's tier is
below `topology.minimumTier` — a control that would always be refused is worse than no control.

## Three things worth knowing

- **The console computes no decay maths of its own.** Every value, threshold, projection and curve
  point on the Decay tab is served by `ExplainConsolidation`, computed by the same code that decides
  what to delete. A second implementation in JavaScript would eventually disagree with the first,
  and the tab whose whole purpose is to be trusted is the wrong place for an approximation.
- **Its filters are a deliberate subset of the RPCs'.** The Memories tab does not offer
  `is_summary`, `is_binary` or recall-count bounds: it is a browse, and those are questions asked
  from a script. [`hippo memory list`](cli.md) has all of them.
- **It is not covered by the version number.** Like the demo stack and the Grafana dashboard, the
  console is excluded from the compatibility promises in [CHANGELOG.md](../CHANGELOG.md) — the RPCs
  it calls are not.
