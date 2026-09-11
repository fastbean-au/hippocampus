# Hippocampus

> **Hippocampus provides human-like memory for digital data. It uses events with linked memories, each of which have their own significance rating on an open scale, reinforced through recall, and a sleep cycle to shed memories that are no longer worth keeping. It is a lossy data store by design, meant for long-term storage.**

[![Coverage Status](https://coveralls.io/repos/github/fastbean-au/hippocampus/badge.svg?branch=main)](https://coveralls.io/github/fastbean-au/hippocampus)
![Dependabot](https://img.shields.io/badge/dependabot-enabled-brightgreen)
[![Known Vulnerabilities](https://snyk.io/test/github/fastbean-au/hippocampus/badge.svg)](https://snyk.io/test/github/fastbean-au/hippocampus)
[![Go Reference](https://pkg.go.dev/badge/github.com/fastbean-au/hippocampus.svg)](https://pkg.go.dev/github.com/fastbean-au/hippocampus)
![GitHub go.mod Go version](https://img.shields.io/github/go-mod/go-version/fastbean-au/hippocampus)

<img src="docs/go-hippocampus.png" width="400" height="200" align="left" alt="Go gopher riding a seahorse" />

<br clear="left" />

## See it running

There's a live instance at **[hippocampus-demo.com](https://hippocampus-demo.com)** with nothing to
install. To watch forgetting happen locally, `./demo/run.sh` from a clone runs the service and a
load generator with the decay clock compressed, so a cycle plays out in minutes rather than days.

## Install

```sh
brew install fastbean-au/tap/hippocampus && brew services start hippocampus
```

Or a `.deb`/`.rpm`, the container image, a release binary, or `go build` — they're all one command
each, and they're all on the **[install page](docs/install.md)**. From there,
[Getting started](docs/getting-started.md) covers the first requests and the configuration to grow
from.

## What forgetting costs you

A store that forgets is only worth having if what it keeps is what you turn out to need. That's
measurable, so it's been measured — by replaying an agent workload fitted to a real corpus into a
live instance and scoring the survivors against the standard cache-replacement baselines at the same
store size:

> **Every access-based policy is statistically indistinguishable from random** at retaining the
> memories that matter but are not touched often. LRU scores 20.2% against random's 19.9%; LFU
> manages 18.4%. Hippocampus scores 27.6% at the same store size, and **+11.1 points over LRU** at a
> larger one.

Importance isn't in the access log, so a policy reading only the access log can't see it. Method,
baselines, the checks that stop it being circular, and the limitations are all in
**[Retention quality](docs/retention.md)**.

## Documentation

**[The documentation index](docs/README.md)** is the map — it's arranged by what you're trying to
do, and it covers the deployment artefacts and the integration subprojects as well as the guides.
Four worth naming here:

- **[Use cases & deployment modes](docs/use-cases.md)** — start here if you're deciding rather than
  deploying. The _Worth knowing before you start_ section is the set of properties that shape what
  you can build on this.
- **[Memory consolidation](docs/consolidation.md)** — how it decides what to forget: the value
  model, the six decay algorithms, and the capacity axes.
- **[Configurability](docs/configuration.md)** — the exhaustive key reference.
- **[Security](docs/security.md)** — everything is off by default, so anything reachable beyond
  localhost needs a deliberate pass over this page.
