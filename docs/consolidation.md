# Memory consolidation

How Hippocampus decides what to forget: the value model, the six deletion algorithms, the
byte-capacity target, checkpoint-triggered eviction, and summarization. For the knobs that
drive these, see [Configurability](configuration.md); for operational tuning, the
[Operations guide](operations.md).

Memory consolidation is done through a process that runs regularly at a configured frequency, and which can also be run manually. Through this process when all of an event's memories are deleted, the event will be deleted. Events that have no memories age out independently under the same decay rules: an event's value is its own significance plus its weighted relationship significance, and its age is measured from the most recent of its start and end times. Memories without an associated event can be given a default event significance equivalence, this can either be an explicit value or set at the value for a specified percentile of the existing events.

The frequency is set by `sleep.periodSeconds`. Setting it to `0` (or any non-positive value) disables the automatic timed cycle entirely: the service then only consolidates when the manual `Sleep` RPC is called, or when the WAL trigger fires (see [Checkpoint-triggered eviction](#checkpoint-triggered-eviction)). This suits an instance that should not forget on its own — for example one used purely for import/archival, or one whose sleep cadence is driven externally.

`consolidation.enabled` (default `true`) is a coarser switch: set it to `false` and the instance runs **no** sleep cycle at all — no timed cycle, no WAL trigger, and the manual `Sleep` RPC is rejected with `FailedPrecondition`. This is the read/write-replica half of [horizontal scaling](../README.md#horizontal-scaling): several instances share one PostgreSQL/MySQL database, exactly one runs with `consolidation.enabled: true` (and holds the single-consolidator lock), and the rest run with it `false` to serve reads and writes without ever consolidating.

## Percentile

The percentile should not be used initially as it requires there to be a collection of events to calculate a value, and, a larger number of events for that value to be meaningful. This value will be calculated at the beginning of every sleep cycle, and may be different every time a sleep cycle is run. While the store has no events the percentile cannot be calculated and the sleep cycle retains the current value (the configured fixed value, or the last successfully calculated percentile).

## Consolidation algorithms

Where the significances are properties of the event (or a default value which is configurable) and memory, the `age` is a property of the memory (while the units are configurable). Both the values of `threshold` and `a` (aggressiveness) are configurable. The choice of which algorithm to apply (`consolidation.method`, 1–6) is also configurable.

The six methods cover the shapes most use cases reach for: a power law (1) that matches published human-forgetting-curve research; two constant-rate linear decays (2, 3), kept for backwards compatibility, that forget in fixed amounts per age unit; a true exponential half-life decay (4), the standard recency-weighting curve for caches, feeds, and recommendation scoring; a logarithmic long-tail decay (5) for archival or audit-log stores that want to keep nearly everything; and a sigmoid "consolidation window" decay (6) that holds a memory near full value until a configured age, then lets it go quickly — closest in spirit to the biological process the service is named for.

An event's relationship significance ($Significance_r$, the sum of the significances of its
relationships to other events) also contributes to the value of its memories, scaled by the
configurable weight $w$ (`consolidation.relationshipSignificanceWeight`). A weight of 0 disables
the relationship contribution.

Recalling a memory (the `RecallMemories` RPC) reinforces it in two ways: the memory's decay clock
resets, so `age` is measured from the most recent recall rather than from creation; and each
recall adds `consolidation.recallSignificanceWeight` ($w_c$, applied to the recall count $c$) to
the memory's effective significance. A weight of 0 disables the significance boost (the decay
reset still applies).

Forgetting also becomes more aggressive as the store fills. At the start of each sleep cycle
utilisation is measured on two axes — the memory count against `consolidation.capacityMemories`
and the store's used bytes against `consolidation.capacityBytes` (0 disables either axis; row
count alone is a poor proxy for storage when bodies range from bytes to hundreds of kilobytes)
— and the deletion threshold is multiplied by a pressure factor driven by whichever axis is
fuller:

$$ pressure = 1 + max \left( { count \over capacity_{count} }, { bytes \over capacity_{bytes} } \right) ^ p $$

The exponent $p$ (`consolidation.capacityPressureExponent`) controls how sharply pressure ramps
up: with a high exponent the pressure is negligible until the store approaches capacity, reaches
double the configured threshold at capacity, and keeps growing if the store overfills.

In each equation below the numerator is:

$$ S = Significance_e + Significance_m + w \cdot Significance_r + w_c \cdot c $$

$$ threshold \gt { S \over age ^ a } $$ (1)

| Threshold | Significance | Aggressiveness | Lifetime                                  |
| --------- | ------------ | -------------- | ----------------------------------------- |
| 1,000     | 1,000,000    | 0.1            | 1,000,000,000,000,000,000,000,000,000,000 |
| 1,000     | 100,000      | 0.1            | 100,000,000,000,000,000,000               |
| 1,000     | 1,000,000    | 0.2            | 1,000,000,000,000,000                     |
| 1,000     | 100,000      | 0.2            | 10,000,000,000                            |
| 1,000     | 1,000,000    | 0.5            | 1,000,000                                 |
| 1,000     | 100,000      | 0.5            | 10,000                                    |
| 1,000     | 1,000        | 1.0            | 1,000                                     |
| 1,000     | 2,000        | 1.0            | 1                                         |
| 1,000     | 5,000        | 1.0            | 5                                         |
| 1,000     | 10,000       | 1.0            | 10                                        |
| 1,000     | 20,000       | 1.0            | 20                                        |
| 1,000     | 50,000       | 1.0            | 50                                        |
| 1,000     | 100,000      | 1.0            | 100                                       |
| 1,000     | 1,000,000    | 1.0            | 1,000                                     |
| 1,000     | 100,000      | 1.2            | 47                                        |
| 1,000     | 1,000,000    | 1.2            | 317                                       |
| 1,000     | 100,000      | 1.5            | 22                                        |
| 1,000     | 1,000,000    | 1.5            | 100                                       |
| 1,000     | 100,000      | 2.0            | 10                                        |
| 1,000     | 1,000,000    | 2.0            | 32                                        |

$$ threshold \gt { S \over \left( age * e ^ a \right) } $$ (2)

| Threshold | Significance | Aggressiveness | Lifetime |
| --------- | ------------ | -------------- | -------- |
| 1,000     | 1,000,000    | 0.1            | 905      |
| 1,000     | 100,000      | 0.1            | 91       |
| 1,000     | 1,000,000    | 0.2            | 819      |
| 1,000     | 100,000      | 0.2            | 82       |
| 1,000     | 1,000,000    | 0.5            | 607      |
| 1,000     | 100,000      | 0.5            | 61       |
| 1,000     | 1,000        | 1.0            | 1        |
| 1,000     | 2,000        | 1.0            | 1        |
| 1,000     | 5,000        | 1.0            | 2        |
| 1,000     | 10,000       | 1.0            | 4        |
| 1,000     | 20,000       | 1.0            | 8        |
| 1,000     | 50,000       | 1.0            | 19       |
| 1,000     | 100,000      | 1.0            | 37       |
| 1,000     | 1,000,000    | 1.0            | 368      |
| 1,000     | 100,000      | 1.2            | 31       |
| 1,000     | 1,000,000    | 1.2            | 302      |
| 1,000     | 100,000      | 1.5            | 23       |
| 1,000     | 1,000,000    | 1.5            | 224      |
| 1,000     | 100,000      | 2.0            | 14       |
| 1,000     | 1,000,000    | 2.0            | 136      |

$$ threshold \gt { S \over age * \left( 1 + \log a \right) } $$ (3)

| Threshold | Significance | Aggressiveness | Lifetime |
| --------- | ------------ | -------------- | -------- |
| 1,000     | 1,000,000    | 0.1            | 0        |
| 1,000     | 100,000      | 0.1            | 0        |
| 1,000     | 1,000,000    | 0.2            | 0        |
| 1,000     | 100,000      | 0.2            | 0        |
| 1,000     | 1,000,000    | 0.5            | 3,260    |
| 1,000     | 100,000      | 0.5            | 326      |
| 1,000     | 1,000        | 1.0            | 1        |
| 1,000     | 2,000        | 1.0            | 2        |
| 1,000     | 5,000        | 1.0            | 5        |
| 1,000     | 10,000       | 1.0            | 10       |
| 1,000     | 20,000       | 1.0            | 20       |
| 1,000     | 50,000       | 1.0            | 50       |
| 1,000     | 100,000      | 1.0            | 100      |
| 1,000     | 1,000,000    | 1.0            | 1,000    |
| 1,000     | 100,000      | 1.2            | 85       |
| 1,000     | 1,000,000    | 1.2            | 846      |
| 1,000     | 100,000      | 1.5            | 72       |
| 1,000     | 1,000,000    | 1.5            | 712      |
| 1,000     | 100,000      | 2.0            | 60       |
| 1,000     | 1,000,000    | 2.0            | 591      |

Method 3's `1 + \log a` factor goes non-positive for any aggressiveness at or below `1/e`
(≈ 0.368) — the `0.1` and `0.2` rows above, whose "lifetime" is really "never forgotten". Because
that would silently disable value-based consolidation entirely, startup validation rejects a
method-3 aggressiveness at or below `1/e`; pick a larger value or a different method.

$$ threshold \gt { S \over e ^ {age \cdot a} } $$ (4)

Unlike methods 2 and 3, which are linear in age despite their names suggesting otherwise, method
4 is genuinely exponential: a constant _relative_ decay rate, so the value halves every
$ \ln(2) / a $ age units regardless of how old the memory already is. It is the curve most
recency-scoring systems mean by "decay."

| Threshold | Significance | Aggressiveness | Lifetime |
| --------- | ------------ | -------------- | -------- |
| 1,000     | 1,000,000    | 0.1            | 69       |
| 1,000     | 100,000      | 0.1            | 46       |
| 1,000     | 1,000,000    | 0.2            | 35       |
| 1,000     | 100,000      | 0.2            | 23       |
| 1,000     | 1,000,000    | 0.5            | 14       |
| 1,000     | 100,000      | 0.5            | 9        |
| 1,000     | 1,000        | 1.0            | 0        |
| 1,000     | 2,000        | 1.0            | 1        |
| 1,000     | 5,000        | 1.0            | 2        |
| 1,000     | 10,000       | 1.0            | 2        |
| 1,000     | 20,000       | 1.0            | 3        |
| 1,000     | 50,000       | 1.0            | 4        |
| 1,000     | 100,000      | 1.0            | 5        |
| 1,000     | 1,000,000    | 1.0            | 7        |
| 1,000     | 100,000      | 1.2            | 4        |
| 1,000     | 1,000,000    | 1.2            | 6        |
| 1,000     | 100,000      | 1.5            | 3        |
| 1,000     | 1,000,000    | 1.5            | 5        |
| 1,000     | 100,000      | 2.0            | 2        |
| 1,000     | 1,000,000    | 2.0            | 3        |

$$ threshold \gt { S \over a \cdot \ln(age + e) } $$ (5)

Method 5 forgets in proportion to the _logarithm_ of age, so even very old memories retain a
sliver of value and lifetimes grow astronomically with significance — the long-tail archival
option, for stores that want to keep almost everything except the least significant, oldest
items. `age + e` (Euler's number) keeps the logarithm's argument, and so its value, always
$ \geq 1 $ without needing to special-case `age = 0`.

| Threshold | Significance | Aggressiveness | Lifetime            |
| --------- | ------------ | -------------- | ------------------- |
| 1,000     | 1,000,000    | 0.1            | practically eternal |
| 1,000     | 100,000      | 0.1            | practically eternal |
| 1,000     | 1,000,000    | 0.2            | practically eternal |
| 1,000     | 100,000      | 0.2            | 1.4 × 10^217        |
| 1,000     | 1,000,000    | 0.5            | practically eternal |
| 1,000     | 100,000      | 0.5            | 7.2 × 10^86         |
| 1,000     | 1,000        | 1.0            | 0                   |
| 1,000     | 2,000        | 1.0            | 5                   |
| 1,000     | 5,000        | 1.0            | 146                 |
| 1,000     | 10,000       | 1.0            | 22,024              |
| 1,000     | 20,000       | 1.0            | 485,165,193         |
| 1,000     | 50,000       | 1.0            | 5.2 × 10^21         |
| 1,000     | 100,000      | 1.0            | 2.7 × 10^43         |
| 1,000     | 1,000,000    | 1.0            | practically eternal |
| 1,000     | 100,000      | 1.2            | 1.6 × 10^36         |
| 1,000     | 1,000,000    | 1.2            | practically eternal |
| 1,000     | 100,000      | 1.5            | 9.0 × 10^28         |
| 1,000     | 1,000,000    | 1.5            | 3.4 × 10^289        |
| 1,000     | 100,000      | 2.0            | 5.2 × 10^21         |
| 1,000     | 1,000,000    | 2.0            | 1.4 × 10^217        |

At `aggressiveness` and age scales this large the exact figure stops being meaningful; the table
keeps them to make the shape of the curve — barely decaying at all across the range where the
other five methods have long since consolidated everything — visible. "practically eternal"
marks rows where $ S / (threshold \cdot a) $ is large enough that $ e ^ {\, S / (threshold \cdot
a)} $ itself overflows a 64-bit float (beyond roughly $ e^{709} $), well before the lifetime
figure would even need representing.

$$ threshold \gt { S \over 1 + e ^ {\, k \left( age / a - 1 \right)} } $$ (6)

Method 6 is a sigmoid: `aggressiveness` sets the age-unit midpoint of the curve rather than a
rate, and a fixed steepness $ k = 5 $ (`sigmoidSteepness` in `sleep.go`) shapes the transition so
it is self-similar regardless of the midpoint's scale — roughly 99% of significance survives a
full window before the midpoint, and roughly 1% remains a full window after it. It models a
memory that is fragile for a while (the "consolidation window") and, once past it, comparatively
durable — the closest of the six to the biological process the service's name refers to.

| Threshold | Significance | Aggressiveness | Lifetime |
| --------- | ------------ | -------------- | -------- |
| 1,000     | 1,000,000    | 0.1            | 0        |
| 1,000     | 100,000      | 0.1            | 0        |
| 1,000     | 1,000,000    | 0.2            | 0        |
| 1,000     | 100,000      | 0.2            | 0        |
| 1,000     | 1,000,000    | 0.5            | 1        |
| 1,000     | 100,000      | 0.5            | 1        |
| 1,000     | 1,000        | 1.0            | 0        |
| 1,000     | 2,000        | 1.0            | 1        |
| 1,000     | 5,000        | 1.0            | 1        |
| 1,000     | 10,000       | 1.0            | 1        |
| 1,000     | 20,000       | 1.0            | 2        |
| 1,000     | 50,000       | 1.0            | 2        |
| 1,000     | 100,000      | 1.0            | 2        |
| 1,000     | 1,000,000    | 1.0            | 2        |
| 1,000     | 100,000      | 1.2            | 2        |
| 1,000     | 1,000,000    | 1.2            | 3        |
| 1,000     | 100,000      | 1.5            | 3        |
| 1,000     | 1,000,000    | 1.5            | 4        |
| 1,000     | 100,000      | 2.0            | 4        |
| 1,000     | 1,000,000    | 2.0            | 5        |

Because the curve is smooth rather than a hard cutoff, memories on the same side of the window
still rank meaningfully against each other for capacity eviction (see
[Capacity target](#capacity-target)) — nothing ties at exactly the same value the way a true
step function would.

## Capacity target

Value-threshold forgetting alone cannot bound the store's size: when everything remaining is
young, significant, or recently recalled, nothing falls below the deletion threshold no matter
how full the store is. `consolidation.capacityBytes` (0 disables the mechanism) sets a hard
target. After the normal consolidation passes, each sleep cycle measures the store's live bytes
(on SQLite, the database's pages excluding those already freed but not yet vacuumed; on
Postgres, an estimate from the live rows themselves — see [Storage](configuration.md#storage)) and, if the
target is exceeded, deletes memories in ascending order of their current decayed value until
the excess is reclaimed, then compacts the database.

`consolidation.capacityBytesFloor` adds hysteresis: once the target is crossed, eviction
reclaims down to the floor rather than the target itself, leaving headroom so the store does
not re-cross the target moments later and evict a sliver every cycle. A floor of 0 (or one
above the target) disables the hysteresis and eviction reclaims to the target.

Eviction uses the same value function as consolidation, so the least significant, least
connected, and least recently recalled memories are forgotten first, and an event stripped of
its last memory is deleted along with it. Unlike consolidation, eviction ignores
`consolidation.minimumAgeInDays` — the bound must be achievable even when everything in the
store is fresh. The one age-based protection eviction _does_ honour is the minimum retention
floor below.

## Minimum retention

`consolidation.minimumRetentionInDays` (0, the default, disables it) is a hard floor that
protects recent items from being reaped at all — by value-based consolidation _and_ by capacity
eviction. Any memory or event whose age is less than this many days is never deleted by a sleep
cycle, whatever its decayed value and however full the store is: **retention overrides the
capacity target**. It is the guarantee to reach for when data must be kept for a fixed window
regardless of significance (a compliance or audit requirement, say).

This is a stronger, separate guarantee from `consolidation.minimumAgeInDays`. `minimumAgeInDays`
only defers _value-based_ consolidation and is deliberately ignored by capacity eviction, so an
item younger than it can still be evicted when the store is over its byte target.
`minimumRetentionInDays` closes that gap: a retained memory is excluded from the eviction
candidate pool entirely rather than merely ranked last, so eviction cannot touch it even if that
leaves the store above `capacityBytes`. Setting `minimumRetentionInDays` at or above
`minimumAgeInDays` makes the latter redundant.

Age for retention is measured from the same decay timestamp everything else uses — a memory's
creation time or its most recent recall, an event's start or end — so recalling a memory renews
its retention window along with its decay clock. Because that timestamp is never earlier than
creation, a retained item is always kept for at least `minimumRetentionInDays` after it was
created. An event holding even one retained memory is itself kept alive: eviction still counts
the retained memory toward the event's total, so the event is never seen as fully evicted and
deleted out from under it.

Retention is a floor, not a cap: it can hold the store above `capacityBytes` if enough recent,
protected data accumulates. Size it against your write rate so the retained working set fits the
capacity you have provisioned.

## Checkpoint-triggered eviction

This mechanism is SQLite-only (it measures SQLite's on-disk WAL file; the Postgres driver
rejects the setting at startup). WAL mode makes every acknowledged write durable immediately,
but the WAL file itself only shrinks back down when a sleep cycle's `Preserve` step checkpoints
it — so under a sustained high write
rate, the on-disk WAL can grow well past the logical `capacityBytes` target in the gap between
timed sleep cycles. `consolidation.walTriggerBytes` (0 disables the mechanism) sets a size for
the WAL file itself: whenever it's exceeded, an out-of-cycle sleep runs immediately rather than
waiting for the next `sleep.periodSeconds` tick, so the checkpoint (and any consolidation or
eviction it finds due) happens sooner. The check runs every few seconds against the WAL file's
size on disk, independent of `sleep.periodSeconds`, and — like the manual `Sleep` RPC — shares a
single in-flight sleep cycle with the timer rather than running a second, overlapping one.

## Summarization

Consolidation and eviction can only delete memories outright. Summarization offers a third
option — for an event whose memories have accumulated enough to be worth condensing, and gone
quiet for long enough that they are unlikely to be added to again, replace them with a single
memory that carries the gist instead of losing the detail entirely. The service has no
visibility into memory content (bodies are opaque), so it cannot write that gist itself; a
client must supply it, typically after inspecting the event's memories via `GetEventById`.

Two RPCs support this:

- `GetSummarizationCandidates` returns the events identified by the most recent sleep cycle as
  candidates: at least `consolidation.summarizationMinMemories` unsummarized memories (0
  disables the scan), all of them last touched — created or recalled — more than
  `consolidation.summarizationMinAgeInDays` ago. Requiring every memory in the group to have
  gone quiet avoids flagging an event that is still being actively written to. The list is
  capped at `consolidation.summarizationMaxCandidates` events (0 leaves it unbounded) and is a
  point-in-time snapshot, refreshed each sleep cycle and not updated in between.
- `ReplaceMemoriesWithSummary` deletes every memory associated with the given event and inserts
  the caller-supplied summary memory in their place, in a single transaction. The summary is
  validated (and checked against `memory.minimumSignificance`) before anything is deleted, so a
  rejected summary leaves the original memories untouched. The new memory is flagged
  `is_summary`; it decays and can be recalled like any other memory, and — since it no longer
  meets the memory-count threshold on its own — will not be re-offered as a candidate until
  enough fresh, unsummarized memories accumulate against the same event again.

Summarization runs between consolidation and eviction each sleep cycle: it surfaces candidates
after decay-based consolidation has already cleared out the truly worthless memories, and before
capacity pressure would otherwise force eviction to delete valuable-but-numerous memories from
those events outright.
