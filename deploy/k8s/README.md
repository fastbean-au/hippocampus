# Kubernetes manifests

Kick-start manifests for running Hippocampus on Kubernetes. They are plain
[Kustomize](https://kubectl.docs.kubernetes.io/references/kustomize/) — no Helm, no extra tooling;
everything applies with `kubectl apply -k` (Kustomize is built into `kubectl`). This mirrors the
repo's two documented deployment models (see [`docs/use-cases.md`](../../docs/use-cases.md)):

| Overlay             | Model                          | Workload                             | Storage                   |
| ------------------- | ------------------------------ | ------------------------------------ | ------------------------- |
| `overlays/sqlite`   | Embedded / instance-per-tenant | one `StatefulSet` (1 replica)        | a `PersistentVolumeClaim` |
| `overlays/postgres` | Centralised / horizontal scale | consolidator + replica `Deployment`s | shared PostgreSQL         |

Both build on `base/` (namespace, a token-less `ServiceAccount`, and the client-facing `Service`).

## Layout

```text
deploy/k8s/
├── base/                     namespace, serviceaccount, service (shared)
├── overlays/
│   ├── sqlite/               embedded single instance + PVC
│   └── postgres/             1 consolidator + N replicas over shared Postgres
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

There is no `Ingress` here on purpose — expose the gRPC and/or HTTP ports through whatever your
cluster already uses (an `Ingress`/`Gateway`, a `LoadBalancer` Service, a mesh). The gateway's
`/healthz` (liveness) and `/readyz` (readiness, database-aware) are the probe endpoints.

## The two models, and why the workload kind differs

- **SQLite (`StatefulSet`).** The embedded, instance-per-tenant model the project favours (TODO #9):
  each instance owns one database file and there must never be two writers of it. A `StatefulSet`
  with `replicas: 1` and a `volumeClaimTemplate` gives a stable identity, keeps the same
  `PersistentVolume` across restarts, and guarantees at most one pod per ordinal. **Do not scale it
  past 1** — SQLite cannot be shared; a second pod sharing the volume would fail to start on the
  storage lock (`hippocampus.lock` in `storage.directory`, see
  [The SQLite storage lock](../../docs/operations.md#the-sqlite-storage-lock)) and crash-loop. To run
  several tenants, apply the overlay again into another namespace (or with a different
  `namePrefix`); one hippocampus per mind.

- **PostgreSQL (`Deployment`s).** Horizontal scaling (TODO #11): the pods are stateless, so they are
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
[README → Configurability](../../README.md#configurability).

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

`overlays/postgres/secret.yaml` is **demo-grade** (placeholder `CHANGE-ME` values) so the overlay
applies end to end without extra steps. **Replace it before any real use** — manage the real secret
with your own tooling (Sealed Secrets, External Secrets, SOPS, a cloud secret store) and never commit
it. The SQLite overlay needs no secret unless you enable auth.

### Authentication

Auth is off (`auth.method: none`) in both shipped configs. To turn on HMAC bearer tokens, set
`auth.method: hmac` in the overlay's `config.json` (the `signing-secret` Secret is already wired into
the pod env, so it activates immediately) and mint tokens with
`hippocampus --mint-token` (see [README → Authentication](../../README.md#authentication)). For an
IdP, set `auth.method: idp` and the `auth.jwksUrl`/`auth.issuer` keys. Enable TLS via the `tls.*`
keys and mount the cert/key from a Secret, or terminate TLS at your ingress/mesh.

### Using an external (managed) database

For production, drop the bundled demo Postgres: delete the `postgres.yaml` line from
`overlays/postgres/kustomization.yaml`, and point `HIPPOCAMPUS_STORAGE_POSTGRES_DSN` at your managed
instance (edit the DSN in the two deployments, or override it from your own Secret). MySQL works the
same way — set `storage.driver: mysql` and `storage.mysql.dsn` (`HIPPOCAMPUS_STORAGE_MYSQL_DSN`);
requires MySQL 8.0.20+.

## Observability

Both configs have OTEL off. To ship metrics/traces to a collector, set
`observability.metrics.enabled`/`observability.tracing.enabled` to `true` and
`observability.otlp.endpoint` to your collector's OTLP/gRPC address (e.g.
`otel-collector.observability.svc:4317`) in the overlay's `config.json`, or as
`HIPPOCAMPUS_OBSERVABILITY_*` env vars.

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
or module earns its keep once these are published as a versioned artifact with many downstream
consumers tuning many values — not while the surface is this small. Wrap them in Terraform's
`kubernetes_manifest`/`kustomization_build` or a thin Helm chart externally if your platform
standardises on one; nothing here blocks that.
