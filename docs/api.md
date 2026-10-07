# API reference

How every RPC maps onto the JSON/HTTP gateway under `/v1`, and what the listing and filter fields
mean on either transport. The gRPC contract itself is [`contract/hippocampus.proto`](../contract/hippocampus.proto),
whose field comments are the authoritative per-field documentation, and the generated OpenAPI
document is served at `/v1/openapi.json`. How the gateway itself is configured - its port, CORS,
body limits - is in [Configuration](configuration.md#http-gateway).

## Routes

Each RPC maps onto a REST-ish path under `/v1`; path segments in `{braces}` come from the URL,
`GET`/`DELETE` take their remaining fields as query parameters, and `POST`/`PATCH` take them as
a JSON body:

| RPC                          | Method | Path                              |
| ---------------------------- | ------ | --------------------------------- |
| `StoreEvent`                 | POST   | `/v1/events`                      |
| `GetEvents`                  | GET    | `/v1/events`                      |
| `GetEventById`               | GET    | `/v1/events/{id}`                 |
| `DeleteEvent`                | DELETE | `/v1/events/{id}`                 |
| `UpdateEvent`                | PATCH  | `/v1/events/{id}`                 |
| `EndEvent`                   | POST   | `/v1/events/{id}/end`             |
| `UpdateEventSignificance`    | PATCH  | `/v1/events/{id}/significance`    |
| `MergeEvents`                | POST   | `/v1/events/merge`                |
| `ReplaceMemoriesWithSummary` | POST   | `/v1/events/{event_id}/summary`   |
| `StoreMemory`                | POST   | `/v1/memories`                    |
| `StoreMemories`              | POST   | `/v1/memories/batch`              |
| `UpdateMemory`               | PATCH  | `/v1/memories/{id}`               |
| `GetMemories`                | GET    | `/v1/memories`                    |
| `DeleteMemories`             | POST   | `/v1/memories/delete`             |
| `DeleteMemoriesByFilter`     | POST   | `/v1/memories/delete-by-filter`   |
| `DeleteEventsByFilter`       | POST   | `/v1/events/delete-by-filter`     |
| `RecallMemories`             | POST   | `/v1/memories/recall`             |
| `SearchMemories`             | POST   | `/v1/memories/search`             |
| `LinkMemories`               | POST   | `/v1/memories/{id}/links`         |
| `UnlinkMemories`             | POST   | `/v1/memories/{id}/links/delete`  |
| `GetMemoryLinks`             | GET    | `/v1/memories/{id}/links`         |
| `LinkEvents`                 | POST   | `/v1/events/{id}/links`           |
| `UnlinkEvents`               | POST   | `/v1/events/{id}/links/delete`    |
| `GetEventLinks`              | GET    | `/v1/events/{id}/links`           |
| `GetSummarisationCandidates` | GET    | `/v1/summarisation/candidates`    |
| `GetSignificanceLevels`      | GET    | `/v1/significance/levels`         |
| `SummariseMemories`          | POST   | `/v1/events/{event_id}/summarise` |
| `Export`                     | POST   | `/v1/export`                      |
| `Import`                     | POST   | `/v1/import`                      |
| `ImportBatch`                | POST   | `/v1/import/batch`                |
| `Transfer`                   | POST   | `/v1/transfer`                    |
| `Clear`                      | POST   | `/v1/clear`                       |
| `Sleep`                      | POST   | `/v1/sleep`                       |
| `PreviewConsolidation`       | GET    | `/v1/sleep/preview`               |
| `ExplainConsolidation`       | POST   | `/v1/consolidation/explain`       |
| `GetConsolidationStatus`     | GET    | `/v1/consolidation/status`        |
| `GetForgottenMemories`       | GET    | `/v1/memories/forgotten`          |
| `DeleteForgottenMemories`    | POST   | `/v1/memories/forgotten/delete`   |
| `GetCallbackQueue`           | GET    | `/v1/callbacks/queue`             |
| `DeleteCallbackQueue`        | POST   | `/v1/callbacks/queue/delete`      |
| `Purge`                      | POST   | `/v1/purge`                       |
| `WhoAmI`                     | GET    | `/v1/whoami`                      |
| `GetTopology`                | GET    | `/v1/topology`                    |

**An id in a `{braces}` segment must be percent-encoded**, including its slashes (`/` → `%2F`) —
ids are caller-chosen and routinely contain them (an `at://` URI is what the event-source bridges
write). Encode the whole id as one path segment, as `encodeURIComponent` and Go's `url.PathEscape`
do; an unencoded slash splits the id across segments and no route matches it.

`ReplaceMemoriesWithSummary`'s body maps directly to its `summary` field (a `Memory`), rather
than the whole request, so a client posts a plain memory object to
`/v1/events/{event_id}/summary` without a wrapper. `/healthz` and `/readyz` are always reachable
without authentication, for liveness/readiness probes (see [Health and readiness](#health-and-readiness)); every other path, including
`/v1/openapi.json`, is subject to [Authentication](#authentication) when it is enabled.

The `GetEvents` and `GetMemories` list endpoints additionally accept a `significance_extremum`
query parameter (`SIGNIFICANCE_EXTREMUM_HIGHEST` or `SIGNIFICANCE_EXTREMUM_LOWEST`): in place of a
`significance_min`/`significance_max` range, it returns only the events/memories tied at the single
highest (or lowest) significance value among those matching the other filters (time range, group,
metadata, recall state) — computed dynamically, not against a caller-supplied bound. It is mutually
exclusive with `significance_min`/`significance_max`; supplying both is rejected with
`InvalidArgument`. The
lowest-significance set is exactly what the next sleep cycle forgets first, which makes it a handy
lens on consolidation (see [Demonstrations](demonstrations.md)). The full field-level request and
response schema for every endpoint lives in the OpenAPI description at `/v1/openapi.json`.

## Metadata filters

Memories and events carry a `metadata` map (see [Metadata](#metadata)), and `GetMemories`,
`GetEvents`, and `SearchMemories` all accept a `metadata` filter that restricts results to items
carrying **every** one of the given pairs — a conjunction, matched exactly.

Over HTTP the filter is a **repeated `key=value` query parameter**, not a map:

```
GET /v1/memories?metadata=source%3Dslack&metadata=project%3Dapollo
```

It is a repeated string rather than a map because grpc-gateway cannot bind a map field from a URL
query string, and these are `GET` routes. The pair is split on the **first** `=`, so a value may
itself contain one; a key may not, which is what keeps the packing unambiguous. A key that does not
match the metadata charset is rejected with `InvalidArgument` rather than reaching the database.

On `SearchMemories` the filter is applied **inside the search index**, alongside `group` — so it
narrows the candidates that ranking sees, and `limit` still returns a full page when one exists.

Metadata predicates are **unindexed**, exactly as the `group` filter is: the only index on the
memories table is the consolidation covering index. On a large store a metadata-filtered list is a
scan, and is best combined with a time range or a page size.

## Reading by id

`GetMemories` takes `ids` — up to 200, repeated as `?ids=a&ids=b` over the gateway — to read
particular memories **without reinforcing them**. `RecallMemories` is the other by-id read, and it
resets the decay clock of everything it returns, so an inspection (an operator checking a record, a
subject-access lookup, a sync job checking what is held) would otherwise make what it looked at
more durable. An id the store does not hold, or one outside the caller's group scope, is simply
missing from the page rather than refused, so the answer never says which. `ids` composes with every
other filter; with `linked_to` the result is the intersection.

## Recall-state filters

`GetMemories` also filters on the columns the store already maintained but never exposed:

| Parameter                                 | Meaning                                                                       |
| ----------------------------------------- | ----------------------------------------------------------------------------- |
| `recalled`                                | `FALSE` for memories never recalled, `TRUE` for those recalled at least once  |
| `recall_count_min` / `recall_count_max`   | inclusive bounds on the recall count; `0` means no bound                      |
| `time_recalled_min` / `time_recalled_max` | inclusive UnixNano bounds on the last recall                                  |
| `is_summary`                              | `TRUE` for summary memories only, `FALSE` to exclude them                     |
| `is_binary`                               | `TRUE` for binary memories only, `FALSE` to exclude them                      |
| `event_id`                                | restrict to one event's memories; empty means no restriction                  |
| `has_event`                               | `FALSE` for memories belonging to no event, `TRUE` for those belonging to one |

`event_id` is the **paged** way to read an event's memories. `GetEventById` with `memories: true`
returns every one of them in a single message, which overruns the receive frame on a large event;
this composes with `limit`/`offset` and every other filter. `has_event` exists beside it for the
same reason `recalled` exists beside the count range: an event-less memory stores an empty
`event_id`, which is also `event_id`'s "no bound" value, so one field cannot ask both questions.

`recalled`, `has_event`, `is_summary` and `is_binary` are the tri-state `Bool` (`UNSPECIFIED`/`FALSE`/`TRUE`)
rather than plain booleans, because an unset proto3 `bool` and an explicit `false` are the same
value on the wire — so "only the ones that are false" would otherwise be unaskable.

That is also why `recalled` exists alongside the count range. Every numeric bound in this API treats
`0` as _no bound_, so `recall_count_max=0` reads as unbounded and cannot mean "never recalled" —
which is the question worth asking, being the closest thing to "what is about to be forgotten"
short of `ExplainConsolidation`:

```
GET /v1/memories?recalled=FALSE&order_by=significance
```

`time_recalled_min`/`time_recalled_max` ask only about memories that **have** been recalled. A
never-recalled memory has `time_recalled` of `0`, so an upper bound would otherwise sweep in every
memory that was never recalled at all — "recalled before Tuesday" answering with memories that were
never recalled would be a trap rather than a filter.

## Event filters

`GetEvents` carries the same shape of filter for the questions events raise:

| Parameter                       | Meaning                                                                                   |
| ------------------------------- | ----------------------------------------------------------------------------------------- |
| `ended`                         | `FALSE` for events that have not ended, `TRUE` for those that have                        |
| `time_end_min` / `time_end_max` | inclusive UnixNano bounds on the end time; both ask only about events that **have** ended |
| `name_contains`                 | restrict to events whose name contains this substring, case-insensitively                 |
| `linked_to`                     | restrict to the events one hop from this event id, in either direction                    |
| `links`                         | when true, populate each returned event's outbound links                                  |

`ended` is the tri-state `Bool`, for the reason `recalled` is one on the memory side: an event that
has not ended stores `time_end` of `0`, which is also every numeric bound's "no bound" value, so one
field cannot ask both questions.

`time_end_max` **excludes open events**, which is the same treatment `time_recalled_max` gives the
never-recalled and a **change in behaviour** from releases before 0.42.0, where "ended before
Friday" returned every event still running. Use `ended: FALSE` to ask about those.

`name_contains` is a substring match, not a content search: neither search backend indexes events at
all (see [Content search](#content-search)), so an event's `name` and `description` are reachable
only through this filter. It is matched case-insensitively on every driver, and `%` and `_` in the
value are literal characters rather than wildcards. Like `group` and `metadata` it is
**unindexed**, so on a large store it is best combined with a time range or a page size.

`linked_to` and `links` mirror `GetMemories`' two link parameters exactly, including that `links`
lists an event's **outbound** edges only — ask `GetEventLinks` for both directions.

## Editing an event

`UpdateEvent` (`PATCH /v1/events/{id}`) applies a partial update: every field carrying a value —
`time_start`, `time_end`, `significance`/`placement`, `name`, `description`, `group`, `metadata` —
overwrites the stored row, with `clear_group` and `clear_metadata` to unset the two whose own zero
value cannot say it. An unknown id is `NotFound` rather than a create. Nested `memories` and `links`
are ignored: memories are a `StoreEvent` input, and links are edited through
`LinkEvents`/`UnlinkEvents`, which report what became of each one.

`EndEvent` and `UpdateEventSignificance` remain, and are the shorter route where they fit:
`EndEvent` defaults `time_end` to now, and `UpdateEventSignificance` is the significance change on
its own.

A `group` on an update **moves** the event, exactly as it does on `UpdateMemory` — see
[Group scoping](#group-scoping) for what that means for a scoped caller.

## The significance registry

`GetSignificanceLevels` (`GET /v1/significance/levels`) lists the distinct significance values
currently in use, ascending, bounded by `significance_min`/`significance_max` and paged by
`limit`/`offset` (default 200, capped at 1,000).

It exists for `SignificancePlacement`: positioning a new item "just above the 5s" or "between 5 and
6" means naming anchors, and until this RPC there was no way to see what the anchors were. Two
**adjacent** values in the list have no room between them, which is precisely the situation
placement opens a gap for.

One registry ranks memories and events alike, so a value says nothing about which records carry it —
which is why it is `reader` tier and why a group-scoped caller is answered in full rather than shown
a partition. There is no per-group significance scale, and the decay maths a scoped caller's
memories are subject to runs on this one.

### The registry forgets too

The registry gains a row for every distinct significance value ever written, and for a long time
lost one only to a `Purge`. That made it the one table in the store that grew with the store's
**history** rather than its contents: a producer writing varied significance left a level behind for
every value it had ever used, long after the last memory carrying that value had been forgotten. One
live deployment held 29,001 levels against 211,657 memories.

```json
"consolidation": {
    "significanceLevels": {
        "unusedRetentionInDays": 7
    }
}
```

The sleep cycle now reaps levels that nothing carries any more. A level is not removed on the cycle
that first finds it unused: that cycle marks it, and only a later cycle, finding it still carried by
nothing and marked for longer than `unusedRetentionInDays`, deletes it. Anything that hands the
level out again in the meantime clears the mark, and the clock starts over the next time it falls
idle.

**The window is there because the registry is a scale, not a log.** A value carried by nothing today
is not necessarily one a client has finished positioning against, so the reap waits out a week
before assuming so; a client that is actually using `SignificancePlacement` reuses its anchors far
more often than that, and a producer writing arbitrary values never reads the list at all. Set
`unusedRetentionInDays` to **0** to keep every value the store has ever seen, which is what earlier
versions did — the registry's size is still reported either way (`hippocampus.significance_levels`).

Removing a level changes nothing about the records that were ranked by it, because by definition
there are none. What it does change is the answer `GetSignificanceLevels` gives, which is the whole
reason the window is configurable.

## Sorting

`GetMemories` and `GetEvents` both take an `order_by` field naming the column to sort on, and an
`order_dir` (`SORT_DIRECTION_ASC`/`SORT_DIRECTION_DESC`) reversing it. Both default to
`timestamp`, descending — most recent first, and the one ordering an index can serve (see below).

| `order_by`          | `GetMemories`                           | `GetEvents`                            | Natural direction |
| ------------------- | --------------------------------------- | -------------------------------------- | ----------------- |
| `significance`      | yes                                     | yes                                    | descending        |
| `timestamp`         | the memory's `time_stamp` (the default) | the event's `time_start` (the default) | descending        |
| `time_recalled`     | yes                                     | —                                      | descending        |
| `recall_count`      | yes                                     | —                                      | descending        |
| `time_end`          | —                                       | yes                                    | descending        |
| `name`              | —                                       | yes                                    | ascending         |
| `link_significance` | yes                                     | yes                                    | descending        |
| `group`             | yes                                     | yes                                    | ascending         |
| `id`                | yes                                     | yes                                    | ascending         |

An omitted `order_dir` means each field's **natural** direction rather than ascending: the magnitude
and time fields read most-significant/most-recent first, which is what a listing is nearly always
asked for, while the lexical ones (`id`, `group`, an event's `name`) read alphabetically. Setting it
explicitly overrides that either way, and it applies to the whole ordering including the tiebreakers
— so `ASC` returns exactly the reverse of `DESC` rather than a differently-tied version of it. Every
ordering ends in an id tiebreaker, so `limit`/`offset` paging stays stable across pages.

Two orderings include rows a filter would exclude, deliberately. A never-recalled memory stores
`time_recalled` of `0` and so sorts as the least recently recalled rather than dropping out of the
page (use `recalled` to ask about those); an event that has not ended stores `time_end` of `0` and
sorts as the oldest-ended (use `time_end_min` to exclude those). An ordering that silently filtered
would be the more surprising of the two behaviours.

An `order_by` naming anything else is rejected with `InvalidArgument` listing the accepted values.

**Only `timestamp` is backed by an index** (`idx_memories_listing_v1` / `idx_events_listing_v1`,
whose columns _and directions_ match that ordering clause exactly), so it reads a page by walking as
many index entries as the page holds, whatever the store's size. Every other ordering — including
`significance` — costs a scan and a sort of everything the filter matched, growing with the store,
so it is worth knowing before wiring one into a hot path. `significance` cannot be indexed at all:
it is the significance registry's rank, reached through a join, and so is not a column of the
memories or events table.

Two things are **not** sortable, because neither is a stored column: a memory's computed decay value
(it is derived per request from the configuration and the store's current capacity pressure — ask
`ExplainConsolidation` for it) and an event's `memory_count` (an aggregate over another table).

## Metadata

Every memory and event carries an optional `metadata` map of string keys to string values: the
multi-dimensional classification the single freeform `group` label cannot express (`source=slack`,
`project=apollo`, `author=…` all at once, rather than one of them or a delimited string).

> Metadata is generally the right place for classification, leaving `group` free to be the access
> boundary — see [Group scoping](#group-scoping). Note that **`group` is not a security boundary
> unless tokens are scoped to it**: without a `groups` claim it is a label like any other, and any
> writer can read and delete every group's records.

It is opaque to the server — filterable, never interpreted — and bounded, because unbounded metadata
would be a body by another name:

| Bound            | Value                                                  |
| ---------------- | ------------------------------------------------------ |
| Keys per item    | 32                                                     |
| Key length       | 64 bytes, matching `[A-Za-z0-9][A-Za-z0-9._:/-]{0,63}` |
| Value length     | 512 bytes                                              |
| Total serialised | 4096 bytes                                             |

The serialised size **counts toward [`memory.limit.sizeBytes`](configuration.md#memory-size-limit)** alongside the body, so metadata
cannot be used to escape that limit; the caps above are what bound it when the limit is unset (the
default). Metadata bytes are also counted by the store's byte accounting, so they contribute to
capacity pressure and to what eviction reclaims.

The key charset is narrow for two concrete reasons: excluding `=` keeps the `key=value` filter
packing unambiguous, and excluding `"`, `$` and `[` keeps a key from escaping the JSON path it is
bound into, so a filter key is always a bound parameter and never string-concatenated into SQL.

On `UpdateMemory`/`UpdateEvent` a non-empty map **replaces** the stored map wholesale — there is no
per-key merge — and an absent or empty map leaves it unchanged, following the same
non-zero-means-change rule as every other updatable field. Because an absent map and an explicitly
empty one are indistinguishable on the wire, removing metadata needs the write-only
`clear_metadata` flag; `clear_group` does the same for the group label, which had the same gap.

Metadata is **never** emitted as a metric, span, or log attribute. It is client-supplied and
unbounded in cardinality, and every attribute the service exports is a boolean or a small closed
enum (see [Observability](#observability)).

Metadata round-trips through `Export`/`Import`/`Transfer`. It is stored as a nullable JSON column on
all three drivers, added in place on first startup after an upgrade, so no migration step is needed.
