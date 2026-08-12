# Event sourcing — broker bridges

`integrations/eventsource` bridges a message broker into Hippocampus: it consumes from a broker and
stores every message as a **memory**, so a stream of events decays and consolidates under the same
significance/recall dynamics as everything else in the store. Routine, low-significance events fade;
the ones that matter (or that you keep recalling) survive.

There is one bridge for each of five sources:

| Source   | Command        | Delivery semantics                               |
| -------- | -------------- | ------------------------------------------------ |
| NATS     | `cmd/nats`     | at-most-once (core NATS has no per-message ack)  |
| MQTT     | `cmd/mqtt`     | at-least-once (QoS ≥ 1, manual ack)              |
| RabbitMQ | `cmd/rabbitmq` | at-least-once (manual ack, nack-with-requeue)    |
| Kafka    | `cmd/kafka`    | at-least-once (offset committed after store)     |
| Bluesky  | `cmd/bluesky`  | at-least-once, cursor-gated (see [Bluesky](#bluesky-the-firehose-bridge)) |

Like the [OpenTelemetry exporter](../integrations/otel/hippocampusexporter/README.md), this is its
own Go module (`github.com/fastbean-au/hippocampus/integrations/eventsource`), separate from the root
so its broker-client dependencies never reach the main service build. Each bridge is a normal gRPC
client, so it works against any deployment topology — an embedded per-tenant SQLite instance, a
centralised Postgres/MySQL store, or a read/write replica behind a load balancer.

> For turning application **logs** into memories instead of broker messages, see the
> [OpenTelemetry log ingestion](../integrations/otel/collector/README.md) integration; for an LLM
> host, see the [MCP server](mcp.md).

## How it works

Every bridge is the same two pieces: a broker **adapter** (the consume loop) on top of a shared
**`bridge` core** (the transform-and-store logic). The adapter normalises its native delivery onto a
broker-agnostic `bridge.Message`, and the core turns that into one or more memories via a
`Transformer` and writes them over gRPC.

```text
broker ─▶ adapter (nats/mqtt/rabbitmq/kafka/bluesky) ─▶ bridge.Store ─▶ Transformer ─▶ StoreMemory RPC ─▶ Hippocampus
```

A delivery that fails to store (a transform error or a gRPC transport failure) is treated as failed
so the adapter can redeliver it — NATS drops it (no ack exists), MQTT leaves it unacked, RabbitMQ
nacks with requeue, and Kafka leaves the offset uncommitted. A memory dropped for significance below
the service's threshold is a _success_, not a failure.

## Install

Grab a pre-built binary for your platform from the
[releases page](https://github.com/fastbean-au/hippocampus/releases) — each release attaches
`hippocampus-<broker>-bridge` archives for Linux, macOS, and Windows on amd64/arm64, with a
`checksums.txt` to verify them.

Or build from source (the bridges are a separate module, so build from its directory):

```sh
cd integrations/eventsource
go build -o hippocampus-nats-bridge ./cmd/nats
# ...and ./cmd/mqtt, ./cmd/rabbitmq, ./cmd/kafka, ./cmd/bluesky
```

### Container image

Each tagged release publishes one image per broker to GHCR, so you can run a bridge without a Go
toolchain:

```sh
docker run --rm ghcr.io/fastbean-au/hippocampus-nats-bridge:latest \
  --nats-url nats://nats:4222 --subject 'events.>' --address hippocampus:50051
```

Images: `hippocampus-nats-bridge`, `hippocampus-mqtt-bridge`, `hippocampus-rabbitmq-bridge`,
`hippocampus-kafka-bridge` (tagged with the release version, the rolling `major.minor`, and
`latest`; `linux/amd64` and `linux/arm64`). A bridge is an outbound client — it dials the broker and
the Hippocampus service and listens on no port — so the image exposes nothing and takes the bridge's
flags after the image name. It must be able to reach both endpoints: on a shared compose/Kubernetes
network use the service names (as above); with `docker run` on the host, `--network host` (Linux) or
`host.docker.internal` for the broker/service addresses (Docker Desktop). All four are built from the
one parameterised `integrations/eventsource/Dockerfile` (the `BROKER` build-arg):

```sh
docker build -f integrations/eventsource/Dockerfile --build-arg BROKER=kafka \
  -t hippocampus-kafka-bridge .    # built from the repo root (the module's replace reaches it)
```

## Running

Each command shares a common set of flags — how to reach the Hippocampus service (`--address`,
`--token` or the `--oidc-*` client-credentials flags, and the `--tls*` trust options) and how the
default transformer shapes each message — plus its own broker flags. Secrets can be injected via
`HIPPOCAMPUS_<BROKER>_*` environment variables (e.g. `HIPPOCAMPUS_NATS_TOKEN`,
`HIPPOCAMPUS_KAFKA_OIDC_CLIENT_SECRET`) instead of argv. Run `--help` on any command for the full
list, or `--version` to print the build version.

### Authenticating to the service

Two shapes, and the choice matters more than it looks:

| | Use it when |
| --- | --- |
| `--token` | A hand run, or a deployment minting long-lived HMAC tokens itself. |
| `--oidc-client-id/-secret` + `--oidc-issuer` | Anything long-running against an IdP-backed service. |

A **static token eventually expires**, and when it does the bridge does not stop — it keeps consuming
and fails every write with `Unauthenticated`, silently, for as long as it is left running. That is
the worst failure shape a daemon has, so a bridge deployed beside a service using `auth.method: idp`
should use the client-credentials grant: the bridge mints its own access tokens and refreshes them
30 seconds before expiry.

```sh
go run ./cmd/kafka --brokers localhost:9092 --topic events --consumer-group hippocampus \
  --oidc-issuer https://auth.example/realms/hippocampus \
  --oidc-client-id hippocampus-bridge \
  --oidc-client-secret "$HIPPOCAMPUS_KAFKA_OIDC_CLIENT_SECRET"
```

Setting `--oidc-client-id` selects this over `--token`; passing both is not an error, because a
deployment mid-migration will carry both for a while and the refreshing source is unambiguously the
one it meant. `--oidc-token-url` skips discovery when the provider's metadata is not reachable, and
`--oidc-audience` is needed by providers whose access token is opaque without one (Auth0's API
identifier; Keycloak ignores it).

Two deliberate behaviours:

- **Configuration is validated at startup, but no network call is made there.** A typo'd flag fails
  immediately; an IdP that happens to be unreachable does not stop the bridge coming up. Discovery
  and the first token fetch happen on the first RPC, so a supervised bridge rides out an IdP blip
  instead of exiting.
- **A token that cannot be obtained fails the RPC** rather than sending none. On an at-least-once
  broker the message is therefore not acked and is redelivered, which is the right outcome for a
  transient outage.

The bridge's token needs the **writer** tier for `StoreMemory`; the Bluesky bridge additionally needs
it to be unscoped and writer-tier for reinforcement to work at all (see below).

```sh
# NATS: store everything published on "events.>" as memories grouped by subject
go run ./cmd/nats --nats-url nats://localhost:4222 --subject 'events.>' --address localhost:50051

# MQTT: sensor readings, QoS 1, significance 3
go run ./cmd/mqtt --broker tcp://localhost:1883 --topic 'sensors/#' --significance 3

# RabbitMQ: consume a queue with manual ack
go run ./cmd/rabbitmq --amqp-url amqp://guest:guest@localhost:5672/ --queue events

# Kafka: consume a topic as a group member
go run ./cmd/kafka --brokers localhost:9092 --topic events --consumer-group hippocampus

# Bluesky: the public firehose, with likes and reposts reinforcing the posts they name
go run ./cmd/bluesky --address localhost:50051 --group bluesky --group-from-subject=false
```

### Shaping messages into memories

The default transformer maps one message to one memory. Its behaviour is controlled by the shared
flags:

| Flag                       | Effect                                                                                           |
| -------------------------- | ------------------------------------------------------------------------------------------------ |
| `--significance`           | Significance stamped on each memory (default 1).                                                 |
| `--significance-header`    | Message header whose integer value overrides `--significance` per message.                       |
| `--group`                  | Group label for every memory.                                                                    |
| `--group-from-subject`     | When `--group` is empty, use the subject/topic as the group (default on).                        |
| `--group-header`           | Message header whose value overrides the group per message.                                      |
| `--binary`                 | Base64-encode the payload and mark the memory `is_binary` (never content-indexed).               |
| `--max-body-bytes`         | Truncate an over-long payload before it becomes a body (0 = unlimited).                          |
| `--metadata`               | Metadata label as `key=value`, stamped on every memory (repeatable).                             |
| `--metadata-header`        | Message header to copy onto each memory's metadata (repeatable).                                 |
| `--metadata-header-prefix` | Copy every header carrying this prefix onto the metadata, with the prefix stripped from the key. |
| `--subject-metadata-key`   | Record the subject/topic as a metadata label under this key, as well as (or instead of) as the group. |

Header selection is an **allowlist or a prefix, never "copy every header"**. Broker headers are
unbounded and mostly machinery — trace context, delivery counts, redelivery flags — so copying them
all would fill each memory's metadata budget with noise, and the keys would be infrastructure's
rather than the operator's choice.

> **If the bridge's token is [group-scoped](configuration.md#group-scoping), turn
> `--group-from-subject` off.** It is on by default, so each memory would be stamped with its
> subject as the group — and a scoped token may only write the groups it holds, so **every message
> would be refused** with `PermissionDenied`.
>
> ```bash
> go run ./cmd/nats --subject 'events.>' --token "$SCOPED_TOKEN" \
>   --group team-alpha --group-from-subject=false --subject-metadata-key subject
> ```
>
> `--group` names the label the token carries (or leave it unset and let the server stamp the
> token's sole group), and `--subject-metadata-key` keeps the subject as **metadata** — which is
> where multi-dimensional classification belongs now that `group` can also be an access boundary.
> Nothing is lost: filter on it with `?metadata=subject%3Devents.orders.created` exactly as you
> would have filtered on the group. An unscoped token is unaffected and the default stands.

Selected header names are normalised to the service's metadata key charset (lowercased, anything
outside `[A-Za-z0-9._:/-]` replaced with `_`), since header names routinely contain spaces and
capitals the service would reject. Anything that still will not fit — an over-long value, or a
selection past the 32-key or 4 KiB caps — is **dropped with a warning rather than failing the
delivery**: the message is not at fault, and on an at-least-once broker a nack would redeliver it
forever.

```sh
hippocampus-nats-bridge --subject 'events.>' \
  --metadata source=nats --metadata env=prod \
  --metadata-header-prefix 'hippo-'
```

The broker-provided message timestamp is used when available (a future timestamp is clamped to now so
the service's clock-skew guard never rejects the write), otherwise the current time.

## End-to-end walkthrough (NATS)

With a local SQLite Hippocampus running (gRPC `:50051`, gateway `:8080` — see
[getting started](getting-started.md)):

1. Start a broker — for NATS:

   ```sh
   docker run --rm -p 4222:4222 nats:latest
   ```

2. Run the bridge, storing everything on `events.>` as memories grouped by subject, at
   significance 5:

   ```sh
   cd integrations/eventsource
   go run ./cmd/nats --nats-url nats://localhost:4222 --subject 'events.>' \
     --address localhost:50051 --significance 5
   ```

   It logs `NATS bridge subscribed`.

3. Publish a message (any NATS client; here the `nats` CLI):

   ```sh
   nats pub events.orders.created 'order 42 created for acme'
   ```

4. Confirm it landed, via the gateway:

   ```sh
   curl -s http://localhost:8080/v1/memories | jq '.memories[] | {body, significance, group}'
   # { "body": "order 42 created for acme", "significance": 5, "group": "events.orders.created" }
   ```

The memory now decays and consolidates like any other: publish a stream of events, trigger a
consolidation cycle (`curl -s -X POST http://localhost:8080/v1/sleep -d '{}'`), and the
lower-significance ones are forgotten first while the ones you keep recalling survive.

## Bluesky: the firehose bridge

`cmd/bluesky` is the odd one out, and deliberately so: it is the only bridge that **reinforces** as
well as writes. It consumes [Jetstream](https://github.com/bluesky-social/jetstream) — Bluesky's JSON
projection of the atproto firehose — and maps it onto both of the store's dynamics at once:

- a **post** becomes a memory, and
- the **engagement** that follows it (a like, a repost, a reply) becomes a `RecallMemories` call
  against that post, resetting its decay clock and raising its effective significance.

So every post arrives with the same significance and what survives is only what people came back to.

### Why it needs no state

A memory's id **is** the post's `at://` URI, and a like names its target by that same URI. So
reinforcing is a call, not a lookup: the bridge holds no map, keeps no cursor table, and never asks
whether an id exists. A like for a post it never ingested, or one the store has already forgotten,
costs one `UPDATE` that matches no rows and is reported as the `missing` outcome above.

Two consequences worth knowing:

- **The token must be unscoped and at least writer tier.** A group-scoped token makes the service
  scope-check ids *before* recalling them, which turns an id it does not hold into `NotFound` for the
  whole batch; and a reader-tier token gets a plain, non-reinforcing read unless the deployment sets
  `auth.readerRecallReinforces`. The bridge absorbs `NotFound` either way, so a misconfigured token
  degrades to "reinforcement quietly stops working" rather than "the bridge stops consuming" — worth
  checking `hippocampus.bridge.recalls` if reinforcement seems to do nothing.
- **An unlike does not un-reinforce.** There is no such operation and there should not be:
  reinforcement is a fact about the past, not a mutable count.

### Delivery semantics

Jetstream has no per-message ack; the resume point is a cursor. Writes are **at-least-once,
cursor-gated**: the cursor advances only after a frame is fully handled, a store failure retries in
place (`--max-retries`, `--error-backoff-seconds`) and then drops the connection so the socket
reopens at the last good cursor and replays from the failure. Replay is safe because the id is the
URI — a duplicate write returns `AlreadyExists`, which the bridge counts as success. Jetstream's own
delivery is documented at-least-once, so duplicates arrive on the happy path too and one rule covers
both.

**Batched recalls are best-effort.** Likes arrive at hundreds a second, so ids are buffered
(`--recall-batch-size`, `--recall-batch-window-ms`) and flushed together — a frame counts as handled
once its id is buffered, so a crash inside the window loses at most one window of reinforcement. That
is a deliberate trade: a lost like is a memory that decays slightly sooner, not one that is wrong.
`--recall-batch-size 0` restores synchronous, at-least-once recalls.

**Gaps are silent.** Jetstream's cursor lookback is bounded (36 hours at the time of writing) and
`/subscribe` clamps an older cursor without saying so, so a bridge down for longer resumes at the
window's edge. This is a stream of the present, not a ledger.

### Curated feeds instead of the firehose

`--feed at://…` takes **posts** from an atproto feed generator — a ranked list someone maintains,
read over HTTP — instead of the firehose. Engagement still comes from Jetstream, and that is the
whole trick: the feed decides what is worth storing, the firehose reports what people did with it,
and the two meet by `at://` URI with no correlation state to keep. It is the same statelessness the
recall path already relies on, paying off a second time.

```sh
go run ./cmd/bluesky --address localhost:50051 --group news --group-from-subject=false \
  --feed 'at://did:plc:kkf4naxqmweop7dv4l2iqqf5/app.bsky.feed.generator/news-2-0' \
  --collections app.bsky.feed.post,app.bsky.feed.like,app.bsky.feed.repost
```

The trade against the firehose is **volume for legibility**. A curated feed delivers tens of posts an
hour rather than tens a second, so the store is small and the decay clock wants to run in hours
rather than minutes — but every memory in it is something a person can read. That makes it the right
source for anything hosted and looked at once, and the wrong one for a demo you want to watch turn
over in a sitting.

Keep `app.bsky.feed.post` in `--collections` even in feed mode: the firehose is where **deletions**
are reported, and dropping it means an upstream withdrawal is never honoured. A post arriving on the
firehose is not stored in feed mode, but a *reply* still reinforces its parent — that is engagement,
and the parent is very likely one of the feed's posts.

**Backfill and seeding.** `--feed-backfill` (on by default) reads the whole feed once at startup, so
the store is populated immediately rather than after hours of trickle. Those posts' likes are all in
the past, though, and the firehose will never report them — so without `--feed-seed-recalls` (also on
by default) several hundred headlines all arrive looking equally untouched, and the model appears to
be doing nothing. Seeding carries each post's observed engagement across as a **damped** recall
count, `round(log1p(likes + reposts))`:

| likes + reposts | seeded recall count |
|---|---|
| 0 | 0 |
| 1 | 1 |
| 6 | 2 |
| 50 | 4 |
| 5,658 | 9 |

The damping is load-bearing, not decoration: effective significance rises *linearly* with recall
count, so passing five thousand likes through raw would give one post an effective significance in
the tens of thousands and it would never be forgotten — which is not a demonstration of a decay
model. log1p compresses four orders of magnitude into single digits, still ranking posts correctly
against each other while leaving even the biggest of them mortal. Reposts count alongside likes
because both mean "someone came back to it"; replies are excluded, since a reply is its own post and
on a news feed is as often disagreement as endorsement.

Seeding is the **only** write that uses `ImportBatch` (the one RPC that can carry recall history),
and it happens once. Polling uses `StoreMemory` and treats `AlreadyExists` as "already have it" —
which is what lets re-reading the same feed page need no bookmark, and, crucially, leaves an existing
memory's accumulated reinforcement alone. An upsert on every poll would silently roll live
reinforcement back to whatever the feed last reported.

### Threads

`--events thread` opens an event per thread root, so a reply becomes a memory of the root's event
(`--events none`, the default, leaves every post standalone and costs one RPC per post instead of
two). A top-level post opens its event and stores its own memory in a single `StoreEvent`; a reply
whose root the bridge has not seen creates that root's event first.

Note that threads are **sparse on the open firehose**: sampling the whole network means you rarely
see both a root and its replies, so most events end up holding one memory. `--dids` is where
threading gets interesting, because following a handful of accounts yields whole conversations — and
in feed mode, `--capture-replies` below.

### Capturing more than the feed picked

In feed mode a firehose post is reinforcement and nothing else. Two flags widen that, and they are
independent — each answers "store this post anyway" for a different reason.

`--capture-replies` stores a post that **replies to a thread this bridge holds**. That is what makes
`--events thread` produce actual conversations rather than lone posts: the feed's post opens the
event, and the public's replies to it become memories in that event. A reply is matched on its thread
**root** first, so it matches however deep in the conversation it sits, and on its parent after that.
It still reinforces its parent exactly as before — capture adds a memory, it does not replace the
engagement.

`--feed-authors` stores a post by **any account the feed has surfaced**, so the accounts a feed is
made of are followed rather than only the posts that feed chose. The DIDs are derived from the feed
itself on every read (a feed hands back `post.author.did`), so there is no account list to maintain
and the set follows the feed's editorial choices as they change.

Both are bounded, in-memory and best-effort, like the topic index: `--capture-index-size` (5,000
stored feed posts) and `--feed-authors-max` (500 authors) cap them, losing an entry means one post is
not captured, and a restart starts empty. The capture index holds only what the **feed** produced,
never the replies captured through it, so one busy thread cannot evict the posts every other thread
is matched on.

**Rank a capture below the feed.** `--capture-significance` gives captured posts a significance of
their own — a reply is worth keeping without being worth as much as the post it answers, and without
it they arrive at `--significance` like everything else and compete for the same capacity. With
`--significance 10 --capture-significance 3` a headline outlives its replies by more than three times
(a method-1 lifetime is roughly `significance / deletionThreshold` age units), so a thread thins back
to its head rather than going all at once. It is delivered through the transformer's per-message
override, so it cannot be combined with `--significance-header`: the bridge refuses that pair at
startup rather than letting one of them silently win.

**Captured posts are deliberately not topic-linked.** `--topic-links` takes terms from a post's
link-card URL and falls back to its body; a reply carries no card, and its body is conversation rather
than an editorially written slug, so relating on it relates posts that merely argue alike. The feed's
own posts carry cards and keep being related exactly as before.

Two things neither flag can do, both for the same reason — Jetstream's `wantedDids` selects on the
repository a record was written **in**, and a like, repost or reply lives in the *engager's*
repository:

- `--feed-authors` does **not** bring in replies to those accounts. Only `--capture-replies` reaches
  those, and only on an unfiltered subscription.
- Neither works under `--dids`, which is also why `--dids` beside `--feed` receives **no engagement
  at all**: the likes and replies your feed's posts collect are written by other people. The bridge
  warns at startup about each of these combinations, since all of them present as a feed nobody
  appears to be interacting with rather than as anything failing.

### Relating posts to each other

`--topic-links` relates posts that are about the same story, using no NLP at all.

A news post's link card carries the article URL, and a news URL's path is a **slug someone wrote by
hand**: `/politics/2026/08/samuel-alito-ethics-conflicts-interest-fossil`. That is a keyword list,
already tokenised on hyphens and chosen editorially rather than grammatically — which is what
relatedness actually wants, and is better here than extracting nouns from the headline would be. A
part-of-speech tagger would cost a dependency, model data and per-post CPU to do the same job worse.

Two posts are related when they share at least `--topic-min-shared` terms (2 by default — one shared
term relates half a news corpus, three relates almost nothing), ignoring terms carried by more than
`--topic-max-frequency-percent` of the indexed memories, which is the cheap stand-in for IDF and is
what stops a section name relating everything to everything. Posts without a link card fall back to
their own text.

Measured against a live news feed that relates about a quarter of posts, and the matches are
**cross-outlet** — one paper's *"Alito made up to $2.9 million from fossil fuel assets"* against
another's *"Supreme Court justice Samuel Alito gained up to $2.9 million from…"*.

**Why it is worth having.** Links are what make `consolidation.linkRecallPropagation` do anything: with
them, a like on one outlet's coverage pulls the others back from the threshold too, and
`linkSignificanceWeight` makes a cluster of coverage more durable than a lone post. That is a real
claim about news, demonstrated rather than asserted. Both settings live on the **service**, not the
bridge — the bridge only creates the edges.

**Two things to know.** The term index is the one genuinely stateful thing in this bridge (bounded by
`--topic-index-size`); unlike the thread-root cache it is not merely an optimisation, so losing it
stops links being made — accepted deliberately, since linking is best-effort enrichment. And links
are issued **after** the write, not attached to it: a link target must exist, and in a store whose
whole job is forgetting, attaching them to the create would let a neighbour consolidated a minute ago
fail the write itself. A backfill is the exception and attaches them to its `ImportBatch`, whose
second pass resolves intra-batch targets without a call each.

### Deletions

`--honour-deletes` (**on by default**) maps an upstream record deletion onto a memory deletion.
Decay and deletion answer different questions — decay is about significance, deletion is about
consent — and on Bluesky deleting a post is the only withdrawal a person has. A bridge that keeps the
copy has quietly turned a public post into a permanent private archive.

### Flags

| Flag | Default | Effect |
| --- | --- | --- |
| `--jetstream-url` | `wss://jetstream2.us-east.bsky.network/subscribe` | endpoint (Jetstream is self-hostable) |
| `--collections` | post, like, repost | Jetstream `wantedCollections` (max 100) |
| `--dids` | *(all)* | restrict to these repositories — the "follow a few accounts" flag |
| `--cursor` | `0` | resume point; 0 starts at the live tip |
| `--feed` | *(off)* | at:// URI of a feed generator to take posts from instead of the firehose |
| `--feed-appview` | `https://public.api.bsky.app` | AppView serving `getFeed` |
| `--feed-poll-seconds` | `60` | how often the feed is re-read |
| `--feed-backfill` | `true` | read the whole feed once at startup |
| `--feed-seed-recalls` | `true` | carry a backfilled post's engagement across as a damped recall count |
| `--capture-replies` | `false` | also store a firehose post replying to a thread this bridge holds |
| `--capture-index-size` | `5000` | stored feed posts a reply is matched against |
| `--capture-significance` | `0` | significance a captured post arrives with (0 = same as `--significance`) |
| `--feed-authors` | `false` | also store firehose posts by any account the feed has surfaced |
| `--feed-authors-max` | `500` | feed authors remembered |
| `--topic-links` | `false` | relate posts sharing topic terms |
| `--topic-index-size` | `5000` | memories the term index remembers |
| `--topic-min-shared` | `2` | terms two posts must share to be related |
| `--topic-max-links` | `8` | links given to one post |
| `--topic-max-frequency-percent` | `2` | ignore terms carried by more than this share of the index |
| `--topic-link-significance` | `50` | significance carried by each topic link |
| `--events` | `none` | `none` or `thread` |
| `--recall` | `true` | reinforce a post when it is liked, reposted or replied to |
| `--recall-batch-size` | `256` | ids per `RecallMemories` call (0 = one RPC per engagement) |
| `--recall-batch-window-ms` | `250` | how long ids buffer before a partial flush |
| `--honour-deletes` | `true` | delete a memory when its post is deleted upstream |
| `--langs` | *(all)* | keep only posts declaring one of these languages |
| `--min-text-bytes` | `1` | drop posts shorter than this |
| `--root-cache-size` | `8192` | cache of known thread roots (an optimisation only) |

### Jetstream is a convenience service, not the protocol

Jetstream is operated by Bluesky: single-node, unauthenticated, and it could be rate-limited or
withdrawn. It is still the right choice here — the canonical firehose
(`com.atproto.sync.subscribeRepos`) is DAG-CBOR-encoded Merkle Search Tree blocks in CAR files, and
consuming it would mean an MST implementation, a CAR reader and a CBOR codec, three substantial
dependencies in a module whose whole premise is that client trees stay small. A `subscribeRepos`
consumer would be a *different adapter* rather than a rewrite of this one; `bridge.Message` is
exactly where that substitution happens.

### Privacy

This bridge stores other people's public speech, keyed by their DID. Deletions are honoured by
default, `--dids` and `--langs` narrow what is taken, and whoever runs it is subject to whatever
data-protection regime they sit under. A demo of it is in
[`demo/bluesky.sh`](../demo/README.md#the-bluesky-firehose-demo-demobluesky-sh), which keeps its
store local and gitignored.

## Delivery semantics and scaling

- **NATS** core delivery is at-most-once; a failed store is logged and dropped. Run several bridges
  sharing a `--queue` group to load-balance a subject. Front the bridge with JetStream for durable
  replay.
- **MQTT** uses manual acknowledgement (`AutoAckDisabled`): the PUBACK is sent only after the store
  succeeds, so with `--qos 1` (or `2`) and a persistent session (`--clean-session=false`, the
  default, plus a stable `--client-id`) an unstored message is redelivered on reconnect.
- **RabbitMQ** acks on success and nacks on failure — with requeue by default, or
  `--requeue-on-error=false` to dead-letter instead of risking a hot redelivery loop on a poison
  message. `--prefetch` bounds in-flight deliveries; keep it at 1 for strict ordering. Scale by
  running multiple bridges on the same queue.
- **Kafka** commits the offset only after a successful store. Run multiple bridges sharing
  `--consumer-group` to split a topic's partitions between them; a store failure backs off
  (`--error-backoff-seconds`) and re-reads rather than skipping.
- **Bluesky** advances its cursor only after a frame is fully handled, so a failure replays from
  there on reconnect; see [Bluesky](#bluesky-the-firehose-bridge) for the batched-recall caveat and
  the bounded cursor lookback. It does **not** scale by running several instances: they would each
  consume the whole subscription and duplicate every write (harmless, since the id is the URI, but
  pointless). Split by `--dids` or `--collections` instead.

## Observability

Every bridge serves `/healthz` and `/readyz` on `--health-port` (**8090 by default**; 0 disables) and
exports OTEL metrics over OTLP/gRPC with `--metrics`.

`/healthz` is process liveness and never fails while the process runs; `/readyz` reports whether the
Hippocampus instance the bridge writes to can actually serve:

```json
{"component":"hippocampus-nats-bridge","dependencies":{"hippocampus":"ok"},"status":"ready"}
```

**The broker is deliberately not part of readiness.** Both of its failure modes are already handled:
a broker unreachable at startup exits the process before the consume loop begins — the supervisor's
problem, and visible as a restart — while a mid-run disconnect is the adapter's own to retry. What
nothing else would notice is the Hippocampus end going away, because a bridge with no traffic and a
bridge that cannot write look identical from outside. That is the gap the probe closes.

| Metric | Type | Attributes |
| --- | --- | --- |
| `hippocampus.bridge.messages` | counter | `broker`, `outcome` (stored/rejected/filtered/failed) |
| `hippocampus.bridge.memories` | counter | `broker`, `outcome` |
| `hippocampus.bridge.message.duration` | histogram (s) | `broker`, `outcome` |
| `hippocampus.bridge.body_bytes` | histogram | `broker` |
| `hippocampus.bridge.recalls` | counter | `broker`, `outcome` (reinforced/missing/failed) |
| `hippocampus.bridge.events` | counter | `broker`, `outcome` (created/exists/rejected/failed) |
| `hippocampus.bridge.recall.batch_size` | histogram | `broker` |
| `hippocampus.client.rpc.requests` | counter | `endpoint`, `rpc`, `code`, `outcome` |
| `hippocampus.client.rpc.duration` | histogram (s) | as above |

`outcome` is four-valued rather than a success bool, because the three non-failures are
operationally different and an SLO has to separate them: a memory the **service** declined for
insignificance (`rejected`) is the decay model working, a message a Transformer chose to yield
nothing for (`filtered`) was dropped on purpose, and only `failed` is the bridge not doing its job. A
message yielding several memories reports the **worst** of their outcomes, since the adapter is about
to redeliver the whole message if any of them failed.

The **recall** outcomes are a separate closed enum, and `missing` is the one worth understanding: an
id an engagement stream asked to reinforce that the store no longer holds is not a failure and not a
filter — it is the decay model having already done its job. The ratio of `missing` to `reinforced` is
the single most informative number a reinforcing bridge produces:

```promql
sum(rate(hippocampus_bridge_recalls_total{outcome="reinforced"}[5m]))
  / sum(rate(hippocampus_bridge_recalls_total[5m]))
```

**Tenancy** is `--metrics-group`, defaulting to `--group` when that is set. It is a **per-process**
label, never the per-message group — with `--group-from-subject` (the default) that value is the
message subject, so on a wildcard subscription it would be one metric stream per subject. Set once
per process it costs no extra cardinality at all.

```promql
sum by (broker, outcome) (rate(hippocampus_bridge_messages_total[5m]))
```

Running several bridges on one host means giving each its own `--health-port`.

## Custom transformations

The `Transformer` is the extension point for anything beyond one-message-one-memory:

```go
type Transformer interface {
    Transform(msg bridge.Message) ([]*contract.Memory, error)
}
```

A program can embed an adapter and supply its own transform — parse a JSON envelope, derive
significance from a field, split a batch message into several memories, or drop messages that don't
match a filter (return an empty slice). Wire a `bridge.TransformerFunc` into `bridge.NewStore` and
hand that store to the adapter's `New`:

```go
store := bridge.NewStore(client, bridge.TransformerFunc(func(msg bridge.Message) ([]*contract.Memory, error) {
    // ...shape msg.Data into one or more *contract.Memory...
}), callTimeout)

b := nats.New(nats.Config{URL: url, Subject: "events.>"}, store)
_ = b.Run(ctx)
```

## Testing

```sh
cd integrations/eventsource
go test ./...        # unit tests (transform, store, and each adapter's message/ack routing)
go test -race ./...
```

The unit tests cover the pure logic without a live broker. Two adapters additionally have
integration tests that exercise the real connect path; they skip unless the matching environment
variable points at a broker:

- NATS runs an embedded in-process server, so it always runs — no external broker needed.
- MQTT: set `HIPPOCAMPUS_TEST_MQTT_BROKER` (e.g. `tcp://localhost:1883`).
- RabbitMQ: set `HIPPOCAMPUS_TEST_RABBITMQ_URL` (e.g. `amqp://guest:guest@localhost:5672/`).
- Bluesky: set `HIPPOCAMPUS_TEST_JETSTREAM` (e.g.
  `wss://jetstream2.us-east.bsky.network/subscribe`). Unlike the other two this needs **no
  container** — Jetstream is public and unauthenticated — so it costs one variable and no service
  definition. It stores nothing; what it covers is the real dial and the real wire, which the fakes
  cannot.

CI starts mosquitto and RabbitMQ and runs the full suite with those variables set, so the adapters'
real-connect paths are exercised on every push. The Jetstream variable is set only on pushes to the
default branch, so a fork's pull request never reaches out to Bluesky's infrastructure and a
third-party outage cannot turn a PR red.
