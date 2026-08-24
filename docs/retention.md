# Retention quality — what a bounded store keeps

![Hippocampus](go-hippocampus.png)

[Performance](performance.md) answers "how fast, and does it stay bounded". This one answers the
question that decides whether a forgetting store is worth having at all: **of everything it threw
away, how much did you actually need later?**

Stated that way, a store that forgets is a **cache replacement policy**, and cache replacement has a
settled way of being evaluated — replay a trace, then measure what survives against the standard
baselines at the same store size. That is what this is. The harness lives in the companion
[`hippocampus-gen`](https://github.com/fastbean-au/hippocampus-gen) repository (`cmd/agentfit` and
`cmd/agent`); everything below is reproducible from it.

## Two questions, not one

The single most important thing here is that a bounded store is asked **two different questions**,
and averaging them into one number hides whichever it is bad at:

| Question       | What it asks                                                 | Who can answer it                            |
| :------------- | :----------------------------------------------------------- | :------------------------------------------- |
| **next-touch** | "What will be looked up next?"                               | Recency, almost by definition                |
| **must-keep**  | "What matters, whether or not it has been touched recently?" | Only something reading a significance signal |

The first is a cache-hit question. The second is the one a **memory** store exists for — the
credential consulted twice a year, the decision record nobody re-reads, the constraint that stays
true regardless of traffic. They are reported separately throughout.

## The headline

At a store holding 21.5% of everything written, on a workload fitted to a real agent's session
history:

| Policy                        | next-touch | must-keep |
| :---------------------------- | ---------: | --------: |
| **Hippocampus**               |  **73.7%** | **27.6%** |
| LRU (keep most recently used) |      89.9% |     20.2% |
| LFU (keep most often used)    |      66.6% |     18.4% |
| Significance only             |      16.1% |     36.7% |
| Random                        |      20.3% |     19.9% |

**Every access-based policy is statistically indistinguishable from random on the must-keep
question.** LRU scores 20.2% against random's 19.9%; LFU manages 18.4%, which is _worse_ than random.
That is not a deficiency in those algorithms — it is arithmetic. Importance is not in the access log,
so a policy that reads only the access log cannot see it.

Hippocampus is ahead of every access-based policy on must-keep at every store size tested, and the
margin widens as the store grows:

| Store size | Hippocampus must-keep | LRU must-keep |    Margin |
| ---------: | --------------------: | ------------: | --------: |
|      10.1% |                 12.8% |         10.0% |  **+2.8** |
|      21.5% |                 27.6% |         20.2% |  **+7.4** |
|      42.2% |                 52.6% |         41.5% | **+11.1** |

The cost is next-touch retention: LRU wins that axis, and should, because "most recently used" is a
restatement of the question. A policy that reads both signals cannot beat a specialist on the
specialist's own ground; the point is that it is the only one that is competent at both.

## The workload

A benchmark whose author writes both the trace and the answer key proves nothing. So the workload's
dynamics are **fitted to a real corpus** — 3,274 file references across 77 Claude Code sessions and
28 days of work on this project — rather than chosen. `cmd/agentfit` performs the fit; the resulting
parameters ship with the generator, the corpus (private working data) does not.

What the fit found, and what the generated trace reproduces by construction:

| Quantity                                     |                   Measured |
| :------------------------------------------- | -------------------------: |
| Zipf exponent over entity popularity         |                       1.09 |
| Entities referenced exactly once             |                        33% |
| Re-references in the same session            | 73%, median **11 seconds** |
| Re-references in a later session             |   27%, median **17 hours** |
| Re-references landing more than a day later  |                      11.5% |
| Re-references landing more than a week later |                       3.2% |

**Reuse is bimodal, and that is the whole argument for the exercise.** Most re-reference is seconds
later; a long tail lands days to weeks out. A recency window sized for the burst discards precisely
what is wanted next week — and a third of everything stored is never wanted again at all, which is
the case _for_ forgetting.

## Where the importance signal comes from

The must-keep question needs a notion of importance that is **not** a function of access, or the
exercise is circular: a significance derived from access frequency tells the store nothing the recall
stream has not already told it.

The corpus supplies one that can be measured rather than declared — whether the agent **mutated** an
entity (`Edit`/`Write`) or only read it. Editing a file is a different act from consulting it, and
the measurement confirms the two are near-independent: mutation share correlates with reference count
at **r = −0.15**. Importance genuinely is not visible in the access pattern.

That signal is used to justify the model, not directly as the model: the corpus's own mutation shares
are too saturated to discriminate finely (half of all entities are mutated on every reference, and
only 6.5% are never mutated). The generator therefore draws latent importance from a graded
distribution, preserving the measured independence from access, and the store sees it only through a
**noisy** significance — how noisy being the parameter swept below.

## Is it rigged? Three checks

**Random scores its own kept share.** A policy retaining 21.5% of the store retains 19.9% of the
questions. If it did not, the scoring would be wrong and every other figure suspect.

**Noise collapses the advantage, exactly as it must.** With significance set to pure noise, every
policy falls to random on must-keep, and the margins vanish:

| Policy            | must-keep, perfect signal | must-keep, pure noise |
| :---------------- | ------------------------: | --------------------: |
| Significance only |                     42.0% |             **25.3%** |
| Hippocampus       |                     32.2% |             **24.6%** |
| LRU               |                     22.8% |                 24.2% |
| Random            |                     21.8% |                 23.1% |

Hippocampus's margin over LRU falls from +9.4 to **+0.4**. Next-touch is unmoved (69.3% → 74.6%),
since recency does not care how good the significance signal is. A result that could not be made to
disappear would not be a measurement.

**A deliberately trivial control.** Beyond the classical baselines, the harness scores a
rank-normalised linear blend of recency and significance — ten lines of code that combine the same
two signals. If that matched the decay model, the decay curves would not be earning their place. It
does not: across aggressiveness settings, three store sizes and a run at four times the time
resolution, the difference straddles zero (−2.8 to +2.8 points) and turns in the store's favour as
the store grows.

## Configuration matters more than the algorithm

The most consequential finding is not about which decay curve to pick. Every method divides
significance by a function of age, so **significance is compared as a ratio**, and a store whose
significance values are spread evenly — with `linkSignificanceWeight` and `recallSignificanceWeight`
left at their defaults on a wide scale — gives up several points of must-keep retention against the
identical store configured as [Consolidation](consolidation.md#choosing-significance-values) advises:

| Configuration                                 | must-keep | vs the trivial blend |
| :-------------------------------------------- | --------: | -------------------: |
| Evenly spread significance, default weights   |     31.2% |             −3.5 pts |
| Geometric spread, default weights             |     29.4% |             −2.3 pts |
| Geometric spread, weights scaled to the range |     27.6% |         **−0.1 pts** |

(The must-keep column falls while the comparison improves because the better-configured store also
scores far higher on next-touch — 65.3% to 73.7%. The right-hand column compares like with like, at
matched next-touch.)

**Read the significance guidance before tuning the decay curve.** It is worth more.

## Limitations

Stated plainly, because they bound what the numbers above support.

- **One workload, fitted to one corpus** — a single developer's month of sessions on one project. The
  reuse dynamics are measured; that they generalise is an assumption.
- **The importance distribution is declared, not measured.** Its _independence_ from access is
  measured (r = −0.15); its shape is chosen, because the corpus's own signal is too saturated to
  fit. A different shape moves the absolute numbers, though not the structural result that
  access-only policies cannot see importance at all.
- **Retrieval is keyword search over synthetic bodies.** Semantic and hybrid modes are not exercised.
- **Replay compresses time.** Every recency policy is scored on the timings the replay actually
  produced rather than the trace's idealised schedule, so the comparison is fair, but a run is still
  minutes rather than months. A run at four times the resolution moved results by 0.2 points.
- **Eviction only.** Runs set `deletionThreshold` to zero so the store's size is set by the capacity
  target alone, which is what makes it comparable with "keep the best N" baselines. A deployment
  using value-based consolidation as well is not what was measured.

## Watching it instead

The same comparison runs continuously at [agent.hippocampus-demo.com](https://agent.hippocampus-demo.com/ui)
and [agent-flat.hippocampus-demo.com](https://agent-flat.hippocampus-demo.com/ui): one writer, two
stores, identical memories, and only one of them told which matter. It is the qualitative version of
the must-keep column above — search both for something old and significant, and it is in one and gone
from the other. Both take the read-only `demo` / `demo` sign-in.

Beside it, [observer.hippocampus-demo.com](https://observer.hippocampus-demo.com/ui) closes the loop
the benchmark leaves open. Everything above **assumes** a deployment that can say something useful
about a memory as it writes it; the observer is a small LLM agent actually doing that — reading a
news feed, recalling what it already concluded, and rating each new observation on five bands that
become its stored significance. It is the write-side judgement this benchmark models synthetically,
being made for real, and being wrong about it sometimes.

## Reproducing

```sh
git clone https://github.com/fastbean-au/hippocampus-gen && cd hippocampus-gen

# Fit the workload to your own agent's transcripts (optional; fitted parameters ship)
go run ./cmd/agentfit --transcripts ~/.claude/projects/<project> \
  --entity-prefix /myproject --out data/params.json

# Describe the workload without touching a service
go run ./cmd/agent --dry-run --memories 20000 --days 60

# Replay it into an instance and score the result
go run ./cmd/agent -s localhost:50051 --memories 20000 --days 60 \
  --sim-days-per-wall-minute 40 --out results.json
```

The instance needs `consolidation.unitsOfAgeInDays` matched to the replay speed — the harness reads
the setting back before it starts and refuses a run that would measure a decay rate nobody chose,
naming the value to use.
