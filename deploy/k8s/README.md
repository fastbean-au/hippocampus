# Kubernetes manifests

Kick-start manifests for running Hippocampus on Kubernetes. They are plain
[Kustomize](https://kubectl.docs.kubernetes.io/references/kustomize/) — no Helm, no extra tooling;
everything applies with `kubectl apply -k` (Kustomize is built into `kubectl`). This mirrors the
repo's two documented deployment models (see [`docs/use-cases.md`](../../docs/use-cases.md)):

| Overlay             | Model                          | Workload                             | Storage                   |
| ------------------- | ------------------------------ | ------------------------------------ | ------------------------- |
| `overlays/sqlite`   | Embedded / instance-per-tenant | one `StatefulSet` (1 replica)        | a `PersistentVolumeClaim` |
| `overlays/postgres` | Centralised / horizontal scale | consolidator + replica `Deployment`s | shared PostgreSQL         |
| `overlays/mysql`    | Centralised / horizontal scale | consolidator + replica `Deployment`s | shared MySQL (8.0.20+)    |

All three build on `base/`: the namespace, a token-less `ServiceAccount`, the client-facing
`Service`, and a default-deny `NetworkPolicy` with the allowances the service needs (see
[Network policy](#network-policy)). `examples/` holds an `ExternalSecret` and an `Ingress` to adapt.
None of them is applied by an overlay.

Every overlay pins the image to a release through kustomize's `images:` stanza (`newTag`), never
`latest`. A node that pulled `latest` afresh could run a newer build than its peers, and a newer
build migrates the schema forward, after which rolling back is refused (`ErrSchemaTooNew`).
`scripts/release.sh` moves the pin as it cuts a release, through `scripts/pin-k8s-image.sh`. To run
another version, change `newTag`, or run `kustomize edit set image
ghcr.io/fastbean-au/hippocampus:<version>` in the overlay.

## Layout

```text
deploy/k8s/
├── base/                     namespace, serviceaccount, service, networkpolicy (shared)
├── overlays/
│   ├── sqlite/               embedded single instance + PVC
│   ├── postgres/             1 consolidator + N replicas over shared Postgres
│   └── mysql/                1 consolidator + N replicas over shared MySQL
└── examples/                 an ExternalSecret and an Ingress, applied by hand
```

## Quick start

Embedded SQLite (simplest — one instance, one volume):

```sh
kubectl apply -k deploy/k8s/overlays/sqlite
kubectl -n hippocampus rollout status statefulset/hippocampus
```

Centralised PostgreSQL (one consolidator + replicas, with a bundled demo Postgres):

```sh
kubectl apply -k deploy/k8s/overlays/postgres
kubectl -n hippocampus rollout status deployment/hippocampus-consolidator
```

Reach the service in-cluster at `hippocampus.hippocampus.svc:50051` (gRPC) or `:8080` (HTTP/JSON
gateway). To poke it from your laptop:

```sh
kubectl -n hippocampus port-forward svc/hippocampus 8080:8080
curl -s localhost:8080/healthz
```

No overlay applies an `Ingress`. Expose the gRPC and/or HTTP ports through whatever your cluster
already uses (an `Ingress`/`Gateway`, a `LoadBalancer` Service, a mesh). `examples/ingress.yaml` is a
starting point for ingress-nginx with cert-manager, and its header lists what to do before exposing
anything. The gateway's `/healthz` (liveness) and `/readyz` (readiness, database-aware) are the
probe endpoints.

## The two models, and why the workload kind differs

- **SQLite (`StatefulSet`).** The embedded, instance-per-tenant model the project favours:
  each instance owns one database file and there must never be two writers of it. A `StatefulSet`
  with `replicas: 1` and a `volumeClaimTemplate` gives a stable identity, keeps the same
  `PersistentVolume` across restarts, and guarantees at most one pod per ordinal. **Do not scale it
  past 1** — SQLite cannot be shared; a second pod sharing the volume would fail to start on the
  storage lock (`hippocampus.lock` in `storage.directory`, see
  [The SQLite storage lock](../../docs/operations.md#the-sqlite-storage-lock)) and crash-loop. To run
  several tenants, apply the overlay again into another namespace (or with a different
  `namePrefix`); one hippocampus per mind.

- **PostgreSQL (`Deployment`s).** Horizontal scaling: the pods are stateless, so they are
  `Deployment`s. Exactly one — the **consolidator** — runs the sleep cycle and holds the Postgres
  advisory lock (`HIPPOCAMPUS_CONSOLIDATION_ENABLED=true`, `replicas: 1`, `Recreate` strategy so two
  never overlap during a rollout). Any number of **replicas** serve the full read/write RPC surface
  without consolidating (`=false`, skip the lock, `Sleep` RPC returns `FailedPrecondition`). The
  base `Service` selects `app.kubernetes.io/name: hippocampus`, which both carry, so traffic
  load-balances across all of them. Scale the replicas freely; keep the consolidator at 1. Promote a
  replica after a consolidator failure by flipping its env to `true` and restarting — it takes the
  now-free lock (assignment is static, not automatic failover; see
  [`docs/operations.md`](../../docs/operations.md#horizontal-scaling-with-replicas)).

## Configuration

Each overlay ships a `config.json` wired in through a Kustomize `configMapGenerator`, so editing it
changes the ConfigMap's content-hash name and `kubectl apply` rolls the workload automatically —
no stale config left running. The full key reference is in the top-level
[Configurability](../../docs/configuration.md).

Secrets are injected as **environment overrides** rather than baked into the ConfigMap. Every config
key maps to `HIPPOCAMPUS_<PATH>` with dots as underscores (viper precedence is env > file), so:

| Config key              | Env var                             | Used by                        |
| ----------------------- | ----------------------------------- | ------------------------------ |
| `storage.postgres.dsn`  | `HIPPOCAMPUS_STORAGE_POSTGRES_DSN`  | postgres overlay (from Secret) |
| `auth.signingSecret`    | `HIPPOCAMPUS_AUTH_SIGNINGSECRET`    | HMAC auth                      |
| `consolidation.enabled` | `HIPPOCAMPUS_CONSOLIDATION_ENABLED` | consolidator vs. replica       |

In the postgres overlay the DSN is composed from a single `postgres-password` Secret value (via
`$(VAR)` interpolation in the pod env), so there is one place to rotate the password — it feeds both
the bundled Postgres and the DSN.

### Secrets

The server overlays' `secret.yaml` is **demo-grade** (placeholder `CHANGE-ME` values) so the overlay
applies end to end without extra steps. **Replace it before any real use** — manage the real secret
with your own tooling (Sealed Secrets, External Secrets, SOPS, a cloud secret store) and never commit
it. `examples/external-secret.yaml` is the External Secrets Operator version: it produces the same
`hippocampus-secrets` Secret with the same keys, so the deployments need no change. The SQLite
overlay needs no secret unless you enable auth.

### Authentication

Auth is off (`auth.method: none`) in both shipped configs. To turn on HMAC bearer tokens, set
`auth.method: hmac` in the overlay's `config.json` (the `signing-secret` Secret is already wired into
the pod env, so it activates immediately) and mint tokens with
`hippocampus --mint-token` (see [Authentication](../../docs/configuration.md#authentication)). For an
IdP, set `auth.method: idp` and the `auth.jwksUrl`/`auth.issuer` keys. Enable TLS via the `tls.*`
keys and mount the cert/key from a Secret, or terminate TLS at your ingress/mesh.

### Using an external (managed) database

For production, drop the bundled demo Postgres: delete the `postgres.yaml` line from
`overlays/postgres/kustomization.yaml`, and point `HIPPOCAMPUS_STORAGE_POSTGRES_DSN` at your managed
instance (edit the DSN in the two deployments, or override it from your own Secret). MySQL works the
same way — set `storage.driver: mysql` and `storage.mysql.dsn` (`HIPPOCAMPUS_STORAGE_MYSQL_DSN`);
requires MySQL 8.0.20+.

### Upgrading the bundled Postgres across a major version

The bundled Postgres runs `postgres:18-alpine`, the same major as the compose files. **Changing
that tag on a volume that already holds a cluster is a data migration, not an image bump**: a
cluster one major wrote cannot be read by the next, and the pod crash-loops on
`database files are incompatible with server version`. That message is the good outcome, and the
manifest is arranged to produce it. The compose files moved to 18's layout instead, with the volume
at `/var/lib/postgresql` and the cluster in a major-specific subdirectory. Here that layout would
find an empty directory beside the old cluster and initialise a new one. The service would then
start against an empty store with nothing saying why. `postgres.yaml` keeps `PGDATA` fixed for that
reason; its comment has the detail.

`pg_upgrade --link` is what 18's layout exists to enable, but it needs both majors' binaries in one
container, which the alpine images do not carry. So the path is dump and restore:

```sh
NS=hippocampus

# 1. Stop every writer. The service pods are stateless, so scaling them to zero loses nothing.
kubectl -n "$NS" scale deployment/hippocampus-consolidator deployment/hippocampus-replica --replicas=0

# 2. Dump from the OLD cluster, while the old image is still the one running. Then check the dump
#    can be read back before anything is deleted - the next step is the irreversible one.
kubectl -n "$NS" exec hippocampus-postgres-0 -- pg_dump -U hippocampus -d hippocampus -Fc > hippocampus.dump
kubectl -n "$NS" exec -i hippocampus-postgres-0 -- pg_restore --list < hippocampus.dump > /dev/null

# 3. Remove the old cluster: the StatefulSet, then its volume.
kubectl -n "$NS" delete statefulset hippocampus-postgres
kubectl -n "$NS" delete pvc pgdata-hippocampus-postgres-0

# 4. Bring up ONLY the new Postgres. A plain `apply -k` would also restore the service's replica
#    counts, and the consolidator would create its schema in the empty database before the restore
#    could, which the restore then collides with.
kubectl kustomize deploy/k8s/overlays/postgres \
  | kubectl apply -f - --selector app.kubernetes.io/name=hippocampus-postgres
kubectl -n "$NS" rollout status statefulset/hippocampus-postgres

# 5. Restore, then bring the service back with the whole overlay.
kubectl -n "$NS" exec -i hippocampus-postgres-0 -- \
  pg_restore -U hippocampus -d hippocampus --no-owner --exit-on-error < hippocampus.dump
kubectl apply -k deploy/k8s/overlays/postgres
kubectl -n "$NS" rollout status deployment/hippocampus-consolidator
```

Keep `hippocampus.dump` until the service is serving again. Nothing in the store depends on the
downtime: decay is measured against wall-clock time, so the memories age through the outage exactly
as they would have while it ran.

A side effect worth knowing: a restore rebuilds every index packed, so it is also the most thorough
reindex this store can get. See
[index bloat on the server drivers](../../docs/operations.md#index-bloat-on-the-server-drivers) for
how much that recovers, and how quickly it comes back.

A managed Postgres has its provider's own major-upgrade path. None of this section applies to it.

### Backing up the SQLite overlay

There is deliberately no backup `CronJob`. The store's volume is `ReadWriteOnce`, so on most
clusters a second pod cannot mount it while the `StatefulSet` holds it, and a job that cannot be
scheduled is a backup that silently never runs. Two routes work instead:

- **Take one from inside the running pod.** `--backup` opens the store read-only, takes no lock and
  writes a consistent copy, so it is safe beside the live process:

  ```sh
  kubectl -n hippocampus exec hippocampus-0 -- \
    hippocampus -c /etc/hippocampus/config.json --backup /data/backup-$(date +%Y%m%d).db
  kubectl -n hippocampus cp hippocampus-0:/data/backup-$(date +%Y%m%d).db ./backup.db
  ```

  Delete the copy from `/data` afterwards: it shares the store's volume and so its capacity.
- **Let the service take its own.** Set `archive.scheduledExport.intervalHours` with `s3.bucket` (or
  an `archive.directory` on a volume of its own), and the consolidating instance exports the whole
  store on that schedule and keeps the newest `archive.scheduledExport.keep`. This is the one that
  needs no operator, and the shipped `HippocampusScheduledExportStale` alert says when it stops
  working. See [Backup, restore, and migration](../../docs/operations.md#backup-restore-and-migration).

The Postgres overlay backs up with `pg_dump` or the provider's snapshots, like any Postgres.

## Observability

Metrics reach a backend by either of two routes, and the overlays ship with the **pull** one on
because it is the one a cluster is most likely already able to use.

### Scraping (on by default)

Both overlays set `observability.prometheus.enabled: true`, so each pod serves its metrics for
Prometheus at `:9464/metrics` — on a listener of its own, never on the gateway, so the metrics are
not exposed to whoever can reach the API. The pods carry the conventional
`prometheus.io/scrape`/`port`/`path` annotations, which is all an annotation-based scrape config
needs, and the `hippocampus` Service exposes a named `metrics` port for a prometheus-operator
cluster:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: hippocampus
  namespace: hippocampus
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: hippocampus
  endpoints:
    - port: metrics
      interval: 30s
```

That is not applied here, because it is a CRD half the clusters this is meant to run on do not have
and `kubectl apply -k` would fail on them. Add it yourself, along with the alert rules from
[`deploy/observability/`](../observability/README.md), whose expressions are written against exactly
the series this endpoint serves.

Set `observability.prometheus.enabled: false` (or
`HIPPOCAMPUS_OBSERVABILITY_PROMETHEUS_ENABLED=false`) to close the listener. There is no
authentication on it, which is why it is a separate port: restrict it with a NetworkPolicy, or bind
it to a single interface with `observability.prometheus.bindAddress`, if the pod network is not
trusted.

### Pushing to a collector

Tracing is off in both configs, and the OTLP metric exporter with it. To ship metrics/traces to a
collector instead of (or as well as) serving them for scraping, set
`observability.metrics.enabled`/`observability.tracing.enabled` to `true` and
`observability.otlp.endpoint` to your collector's OTLP/gRPC address (e.g.
`otel-collector.observability.svc:4317`) in the overlay's `config.json`, or as
`HIPPOCAMPUS_OBSERVABILITY_*` env vars. Traces have no scrape equivalent, so a deployment that wants
them needs a collector whichever way its metrics travel.

### Network policy

`base/networkpolicy.yaml` denies every pod in the namespace all traffic, then allows exactly what the
shipped shapes need:

- DNS, for every pod;
- the gRPC and HTTP ports, from this namespace and from any namespace labelled
  `hippocampus.fastbean-au/client: "true"`;
- the scrape port, from any namespace labelled `hippocampus.fastbean-au/scrape: "true"`;
- Hippocampus to the bundled database, and the database from Hippocampus alone.

So label the namespaces your clients, ingress controller and Prometheus run in:

```sh
kubectl label namespace ingress-nginx hippocampus.fastbean-au/client=true
kubectl label namespace monitoring hippocampus.fastbean-au/scrape=true
```

Everything the service dials outside the namespace has to be allowed explicitly, because only the
deployment knows where it is: a managed database, OpenSearch, an LLM endpoint, an OTLP collector, a
callbacks receiver, a transfer target, an object store. Add an egress `NetworkPolicy` naming each one,
for example an `ipBlock` for a managed database's address on 5432. Kubelet probes are host traffic,
which most CNIs admit regardless of policy; one that does not needs the node CIDR allowed.

## Security posture

The pods run non-root (uid 1000, matching the image), with `readOnlyRootFilesystem: true`
(only the SQLite PVC is writable), all Linux capabilities dropped, `allowPrivilegeEscalation: false`,
and the `RuntimeDefault` seccomp profile. The `ServiceAccount` sets
`automountServiceAccountToken: false` — Hippocampus never calls the Kubernetes API, so the pod carries
no token to leak.

## Terraform / Helm?

Deliberately neither, for now. These manifests are a kick-start, not a distribution: Kustomize keeps
them readable and `kubectl`-native with zero extra tooling, the two overlays cover the project's two
deployment models, and everything a Terraform module or Helm chart would parameterise (image tag,
replica count, config, secrets, DB endpoint) is a one-line Kustomize edit or an env override. A chart
or module earns its keep once these are published as a versioned artefact with many downstream
consumers tuning many values — not while the surface is this small. Wrap them in Terraform's
`kubernetes_manifest`/`kustomization_build` or a thin Helm chart externally if your platform
standardises on one; nothing here blocks that.
