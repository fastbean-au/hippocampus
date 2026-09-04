// Hippocampus configuration wizard.
//
// Everything here runs in the browser and nothing leaves it: the wizard asks a series of questions,
// keeps the answers in memory (and, secrets aside, in localStorage), and generates the config.json
// plus the deployment artefacts that carry it. There is no server side to this page - the Go binary
// that serves it only serves files.
//
// The field schema below is the single source of truth for the form, the generated config, and the
// validation rules; the service's own defaults are recorded against each key, so the wizard can show
// what has been changed and emit either a complete or a minimal config. Keep the defaults and the
// validation rules in step with cmd/hippocampus/main.go (validateConfig and the driver switch) and
// docs/configuration.md.

"use strict";

/* ------------------------------------------------------------------ helpers */

const $ = (selector) => document.querySelector(selector);

function el(tag, attrs, ...children) {
  const node = document.createElement(tag);

  for (const [name, value] of Object.entries(attrs || {})) {
    if (name === "class") {
      node.className = value;
    } else if (name === "text") {
      node.textContent = value;
    } else if (name.startsWith("on")) {
      node.addEventListener(name.slice(2), value);
    } else if (value === true) {
      node.setAttribute(name, "");
    } else if (value !== false && value !== null && value !== undefined) {
      node.setAttribute(name, value);
    }
  }

  for (const child of children.flat()) {
    if (child === null || child === undefined || child === false) {
      continue;
    }

    node.append(
      child.nodeType ? child : document.createTextNode(String(child)),
    );
  }

  return node;
}

let toastTimer = null;

function toast(message) {
  const node = $("#toast");
  node.textContent = message;
  node.classList.add("show");

  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => node.classList.remove("show"), 2200);
}

function download(filename, contents) {
  const url = URL.createObjectURL(new Blob([contents], { type: "text/plain" }));
  const link = el("a", { href: url, download: filename });

  document.body.append(link);
  link.click();
  link.remove();

  // Revoke on the next tick: Safari needs the object URL to outlive the synchronous click.
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

async function copy(text) {
  try {
    await navigator.clipboard.writeText(text);
    toast("Copied to the clipboard");
  } catch {
    toast("The browser refused clipboard access — select and copy manually");
  }
}

// envName maps a dotted config key to the environment variable that overrides it, exactly as
// configureEnvOverrides does in main.go: the HIPPOCAMPUS_ prefix, dots replaced by underscores, all
// uppercased.
function envName(key) {
  return "HIPPOCAMPUS_" + key.replace(/\./g, "_").toUpperCase();
}

// yamlString quotes a scalar for YAML. JSON's string escaping is a subset of YAML's double-quoted
// style, so this is exact rather than approximate.
function yamlString(value) {
  return JSON.stringify(String(value));
}

function randomSecret(bytes) {
  const buffer = new Uint8Array(bytes);
  crypto.getRandomValues(buffer);

  return btoa(String.fromCharCode(...buffer)).replace(/=+$/, "");
}

function formatBytes(value) {
  if (!value) {
    return "unbounded";
  }

  const units = [
    ["GiB", 1024 ** 3],
    ["MiB", 1024 ** 2],
    ["KiB", 1024],
  ];

  for (const [suffix, size] of units) {
    if (value >= size) {
      return `${(value / size).toFixed(value % size === 0 ? 0 : 1)} ${suffix}`;
    }
  }

  return `${value} bytes`;
}

/* ------------------------------------------------------- deployment targets */

// Each target carries the filesystem conventions its artefacts use, so choosing one moves
// storage.directory (and the paths in the generated instructions) to where that platform expects it.
const TARGETS = [
  {
    id: "compose",
    label: "Docker / Podman Compose",
    blurb:
      "One container plus its dependencies on a single host. The quickest production-shaped start.",
    dataDir: "/data",
    configPath: "/etc/hippocampus/config.json",
  },
  {
    id: "k8s",
    label: "Kubernetes",
    blurb:
      "Manifests in the shape of deploy/k8s: a StatefulSet for SQLite, or a consolidator plus replicas over a shared database.",
    dataDir: "/data",
    configPath: "/etc/hippocampus/config.json",
  },
  {
    id: "systemd",
    label: "Linux systemd",
    blurb:
      "One VM, one service, no container runtime — the single static binary under a hardened unit.",
    dataDir: "/var/lib/hippocampus",
    configPath: "/etc/hippocampus/config.json",
  },
  {
    id: "launchd",
    label: "macOS launchd",
    blurb:
      "A per-user LaunchAgent that starts at login. The Homebrew layout, for a personal instance.",
    dataDir: "/usr/local/var/hippocampus",
    configPath: "/usr/local/etc/hippocampus/config.json",
  },
  {
    id: "binary",
    label: "Plain binary",
    blurb:
      "Just the config file and a command line. Local development, or a runtime you manage yourself.",
    dataDir: "./data",
    configPath: "./config.json",
  },
];

const targetById = (id) =>
  TARGETS.find((target) => target.id === id) || TARGETS[0];

/* ------------------------------------------------------------------ presets */

// A preset is a starting point, not a lock: it sets values the operator then edits. Each lists only
// the keys it moves away from the service default.
const PRESETS = [
  {
    id: "local",
    label: "Local development",
    blurb:
      "Embedded SQLite, HTTP gateway on 8080, no auth, a short sleep cycle so forgetting is visible.",
    target: "binary",
    values: {
      "gateway.port": 8080,
      "sleep.periodSeconds": 300,
      "logging.level": "debug",
      "consolidation.minimumAgeInDays": 0,
    },
  },
  {
    id: "single-vm",
    label: "Single VM service",
    blurb:
      "SQLite under systemd with a byte capacity target, WAL-triggered checkpoints, and HMAC tokens.",
    target: "systemd",
    values: {
      "gateway.port": 8080,
      "auth.method": "hmac",
      "consolidation.capacityBytes": 5 * 1024 ** 3,
      "consolidation.capacityBytesFloor": Math.round(4.5 * 1024 ** 3),
      "consolidation.walTriggerBytes": 256 * 1024 * 1024,
      "storage.queryTimeoutSeconds": 60,
    },
  },
  {
    id: "scaled",
    label: "Scaled (PostgreSQL)",
    blurb:
      "One consolidator plus read/write replicas over a shared database, IdP tokens, content search on.",
    target: "k8s",
    scale: "consolidator",
    values: {
      "storage.driver": "postgres",
      "gateway.port": 8080,
      "auth.method": "idp",
      "opensearch.enabled": true,
      "observability.metrics.enabled": true,
      "observability.tracing.enabled": true,
      "consolidation.capacityMemories": 5000000,
    },
  },
  {
    id: "edge",
    label: "Edge / IoT collector",
    blurb:
      "A small SQLite store that forgets hard and transfers what it keeps to a central instance.",
    target: "systemd",
    values: {
      "gateway.port": 8080,
      "sleep.periodSeconds": 900,
      "consolidation.method": 4,
      "consolidation.aggressiveness": 0.5,
      "consolidation.minimumAgeInDays": 0,
      "consolidation.capacityBytes": 256 * 1024 * 1024,
      "consolidation.capacityBytesFloor": 192 * 1024 * 1024,
      "consolidation.walTriggerBytes": 32 * 1024 * 1024,
      "transfer.targetAddress": "central.example.com:50051",
      "transfer.maxManifestRows": 200000,
    },
  },
  {
    id: "archive",
    label: "Archive / audit store",
    blurb:
      "Long-tail decay and a hard retention floor: almost everything is kept, for a long time.",
    target: "compose",
    values: {
      "gateway.port": 8080,
      "storage.driver": "postgres",
      "consolidation.method": 5,
      "consolidation.aggressiveness": 2,
      "consolidation.minimumAgeInDays": 90,
      "consolidation.minimumRetentionInDays": 365,
      "consolidation.capacityMemories": 0,
      "sleep.periodSeconds": 86400,
    },
  },
];

/* ------------------------------------------------------------------- schema */

// Field types: bool, int, float, text, select, list (string array), map (string→string object).
// `secret: true` keeps the value out of localStorage and, unless the operator opts in, out of the
// generated config.json — it is emitted as an environment override instead.
const STEPS = [
  {
    id: "start",
    title: "Start",
    custom: "start",
    cards: [],
  },
  {
    id: "storage",
    title: "Storage",
    cards: [
      {
        title: "Store",
        blurb:
          "SQLite is embedded — one file, one instance, nothing to run alongside. PostgreSQL and MySQL let several instances share one store, of which exactly one may consolidate. The driver also decides what search you get: see the note under it.",
        fields: [
          {
            key: "storage.driver",
            label: "Driver",
            type: "select",
            def: "sqlite",
            svc: "sqlite",
            always: true,
            options: [
              ["sqlite", "sqlite — embedded, single instance"],
              ["postgres", "postgres — shared, horizontally scalable"],
              ["mysql", "mysql — shared, 8.0.20 or newer"],
            ],
            help: "Search differs by driver, and it is worth knowing before you commit. SQLite has keyword content search built in — no cluster, no configuration. PostgreSQL and MySQL have none of their own, so they need OpenSearch for any content search at all. Semantic (meaning-based) search needs OpenSearch on every driver, so an embedded deployment trades it away for having nothing to run alongside.",
          },
          {
            key: "storage.directory",
            label: "Data directory",
            type: "text",
            def: "./data",
            svc: "./data",
            always: true,
            when: (s) => value(s, "storage.driver") === "sqlite",
            help: "Where hippocampus.db and its WAL live. It must be writable by the service user; an empty value selects the test-only in-memory database and is refused at startup.",
          },
          {
            key: "storage.postgres.dsn",
            label: "PostgreSQL DSN",
            type: "text",
            def: "",
            secret: true,
            when: (s) => value(s, "storage.driver") === "postgres",
            placeholder:
              "postgres://user:password@host:5432/hippocampus?sslmode=require",
          },
          {
            key: "storage.mysql.dsn",
            label: "MySQL DSN",
            type: "text",
            def: "",
            secret: true,
            when: (s) => value(s, "storage.driver") === "mysql",
            placeholder:
              "user:password@tcp(host:3306)/hippocampus?parseTime=true",
          },
          {
            key: "consolidation.enabled",
            label: "This instance consolidates (runs the sleep cycle)",
            type: "bool",
            def: true,
            svc: true,
            help: "Exactly one instance per store may consolidate. Replicas set this false: they serve the full read/write surface, skip the instance lock, and reject the Sleep RPC.",
          },
        ],
      },
      {
        title: "Connections",
        fields: [
          {
            key: "storage.queryTimeoutSeconds",
            label: "Query timeout (seconds)",
            type: "int",
            def: 60,
            svc: 60,
            help: "Server-side bound on every database operation, whichever expires first — this or the caller's deadline. 0 disables it.",
          },
          {
            key: "storage.pool.maxOpenConns",
            label: "Max open connections",
            type: "int",
            def: 25,
            svc: 25,
            when: (s) => value(s, "storage.driver") !== "sqlite",
            help: "Caps this instance's share of the shared database's connections. Ignored by SQLite, which is pinned to one.",
          },
          {
            key: "storage.pool.maxIdleConns",
            label: "Max idle connections",
            type: "int",
            def: 0,
            when: (s) => value(s, "storage.driver") !== "sqlite",
            help: "0 uses database/sql's default (2).",
          },
        ],
      },
      {
        title: "Compression",
        fields: [
          {
            key: "storage.compression.enabled",
            label: "Compress memory bodies",
            type: "bool",
            def: true,
            svc: true,
            help: "Stores bodies gzip-compressed, trading a few percent of CPU on reads and writes for storage — which is the resource the capacity target manages, so it buys retention. Safe to change at any time: each row records how it was stored, so rows written either way stay readable.",
          },
          {
            key: "storage.compression.minBytes",
            label: "Minimum body size (bytes)",
            type: "int",
            def: 512,
            svc: 512,
            when: (s) => value(s, "storage.compression.enabled"),
            help: "Bodies below this are stored verbatim — they compress poorly once gzip's own header is counted. Values under 64 are raised to it. Binary bodies are never compressed, and a body compression fails to shrink is stored verbatim whatever its size.",
          },
        ],
      },
    ],
  },
  {
    id: "server",
    title: "Server & API",
    cards: [
      {
        title: "Listeners",
        fields: [
          { key: "port", label: "gRPC port", type: "int", def: 50051 },
          {
            key: "bindAddress",
            label: "gRPC bind address",
            type: "text",
            def: "",
            placeholder: "(all interfaces)",
            help: "Set 127.0.0.1 to accept only loopback traffic — e.g. behind a sidecar or mesh that terminates TLS.",
          },
          {
            key: "gateway.port",
            label: "HTTP/JSON gateway port",
            type: "int",
            def: 0,
            help: "0 disables the gateway. 8080 is conventional. The gateway also serves /healthz, /readyz, the OpenAPI description, and the web console — container and Kubernetes probes need it.",
          },
          {
            key: "gateway.bindAddress",
            label: "Gateway bind address",
            type: "text",
            def: "",
            placeholder: "(all interfaces)",
            when: (s) => value(s, "gateway.port") > 0,
          },
          {
            key: "gateway.openapi.enabled",
            label: "Serve the OpenAPI description",
            type: "bool",
            def: true,
            svc: true,
            when: (s) => value(s, "gateway.port") > 0,
            help: "Serves /v1/openapi.json, which clients generate from and browser API explorers read. It is served without a token — the document is generated from a proto published with the source, so requiring one protected nothing while breaking every OpenAPI tool. Turn it off to serve nothing at that path.",
          },
          {
            key: "gateway.corsOrigins",
            label: "Allowed browser origins (CORS)",
            type: "list",
            def: [],
            when: (s) => value(s, "gateway.port") > 0,
            placeholder: "http://localhost:8082",
            help: "One origin per line, scheme://host[:port] with no trailing slash. Empty (the default) sends no CORS headers, so only pages served by this gateway can call /v1. Add an origin only for a browser client you host elsewhere, such as a Swagger UI container. Credentials are never allowed, so a console session cookie cannot be used from another origin.",
          },
        ],
      },
      {
        title: "Limits and hardening",
        blurb: "All optional: each key left at 0 keeps grpc-go's own default.",
        fields: [
          {
            key: "maxRecvMsgBytes",
            label: "Max gRPC receive size (bytes)",
            type: "int",
            def: 0,
            help: "0 → grpc-go's 4 MiB. Raise it if single memories or ImportBatch pages are larger.",
          },
          {
            key: "maxConcurrentStreams",
            label: "Max concurrent streams",
            type: "int",
            def: 0,
          },
          {
            key: "gateway.maxRequestBytes",
            label: "Max HTTP request body (bytes)",
            type: "int",
            def: 0,
            when: (s) => value(s, "gateway.port") > 0,
            help: "0 leaves it unbounded, since a legitimate ImportBatch body can be large. Set a ceiling when the gateway is reachable by untrusted callers.",
          },
          {
            key: "listing.countCacheSeconds",
            label: "Listing total cache (seconds)",
            type: "int",
            def: 5,
            svc: 5,
            help: "GetMemories/GetEvents report a total matching the filter, which costs a second unbounded pass. It is cached per filter for this long, so a total can be up to this stale; 0 disables the cache and counts every request. A page short enough to prove its own total never counts at all.",
          },
          {
            key: "keepalive.minTimeSeconds",
            label: "Keepalive minimum interval (seconds)",
            type: "int",
            def: 0,
          },
          {
            key: "keepalive.permitWithoutStream",
            label: "Permit keepalive pings on idle connections",
            type: "bool",
            def: false,
          },
          {
            key: "shutdown.timeoutSeconds",
            label: "Shutdown timeout (seconds)",
            type: "int",
            def: 10,
            svc: 10,
            help: "Bounds each drain phase: gateway, then gRPC, then the observability flush.",
          },
        ],
      },
      {
        title: "Rate limiting",
        blurb:
          "A hierarchy of token buckets: a request must pass every level you configure. The instance ceiling is checked before a token is verified (so a flood is bounded before it costs anything); the per-tier and per-client levels are checked after, since both need to know who is calling. A level left at 0 requests/second imposes no limit.",
        fields: [
          {
            key: "rateLimit.enabled",
            label: "Enable rate limiting",
            type: "bool",
            def: false,
          },
          {
            key: "rateLimit.global.requestsPerSecond",
            label: "Instance ceiling (requests/second)",
            type: "float",
            def: 0,
            when: (s) => value(s, "rateLimit.enabled"),
            help: "Everything the instance will serve, whoever is asking. This is the denial-of-service ceiling; size it above your real peak, since hitting it affects well-behaved callers too.",
          },
          {
            key: "rateLimit.global.burst",
            label: "Instance burst",
            type: "int",
            def: 0,
            when: (s) => value(s, "rateLimit.enabled"),
            help: "How much of the rate may be spent at once. 0 means one second's worth.",
          },
          {
            key: "rateLimit.perClient.requestsPerSecond",
            label: "Per-client (requests/second)",
            type: "float",
            def: 0,
            when: (s) => value(s, "rateLimit.enabled"),
            help: "Each caller gets its own bucket, so one caller cannot consume the instance ceiling and starve the rest. Keyed on the authenticated client id, or on the caller's address when authentication is off.",
          },
          {
            key: "rateLimit.perClient.burst",
            label: "Per-client burst",
            type: "int",
            def: 0,
            when: (s) => value(s, "rateLimit.enabled"),
          },
          {
            key: "rateLimit.tiers.reader.requestsPerSecond",
            label: "Reader tier (requests/second)",
            type: "float",
            def: 0,
            when: (s) =>
              value(s, "rateLimit.enabled") &&
              value(s, "auth.method") !== "none",
            help: "A ceiling on everything readers do between them. Needs authentication: a tier comes from a token's roles.",
          },
          {
            key: "rateLimit.tiers.reader.burst",
            label: "Reader tier burst",
            type: "int",
            def: 0,
            when: (s) =>
              value(s, "rateLimit.enabled") &&
              value(s, "auth.method") !== "none",
          },
          {
            key: "rateLimit.tiers.writer.requestsPerSecond",
            label: "Writer tier (requests/second)",
            type: "float",
            def: 0,
            when: (s) =>
              value(s, "rateLimit.enabled") &&
              value(s, "auth.method") !== "none",
          },
          {
            key: "rateLimit.tiers.writer.burst",
            label: "Writer tier burst",
            type: "int",
            def: 0,
            when: (s) =>
              value(s, "rateLimit.enabled") &&
              value(s, "auth.method") !== "none",
          },
          {
            key: "rateLimit.tiers.admin.requestsPerSecond",
            label: "Admin tier (requests/second)",
            type: "float",
            def: 0,
            when: (s) =>
              value(s, "rateLimit.enabled") &&
              value(s, "auth.method") !== "none",
            help: "Best left at 0. An operator answering an incident should not be queued behind a limit meant for application traffic.",
          },
          {
            key: "rateLimit.tiers.admin.burst",
            label: "Admin tier burst",
            type: "int",
            def: 0,
            when: (s) =>
              value(s, "rateLimit.enabled") &&
              value(s, "auth.method") !== "none",
          },
          {
            key: "rateLimit.trustForwardedFor",
            label: "Trust X-Forwarded-For",
            type: "bool",
            def: false,
            when: (s) => value(s, "rateLimit.enabled"),
            help: "Only behind a proxy that overwrites the header. On a directly reachable listener it lets any caller pick its own bucket, and so as many of them as it likes.",
          },
          {
            key: "rateLimit.maxClients",
            label: "Tracked callers",
            type: "int",
            def: 0,
            when: (s) => value(s, "rateLimit.enabled"),
            help: "0 uses the internal default (10000). Bounds the per-client bucket table, so it cannot become a memory-exhaustion surface of its own; the least recently seen caller is dropped first.",
          },
          {
            key: "rateLimit.clientIdleSeconds",
            label: "Caller idle window (seconds)",
            type: "int",
            def: 0,
            when: (s) => value(s, "rateLimit.enabled"),
            help: "0 uses the internal default (300). How long a caller's bucket is kept after its last request.",
          },
        ],
      },
      {
        title: "Health and logging",
        fields: [
          {
            key: "readiness.pingTimeoutSeconds",
            label: "Readiness ping timeout (seconds)",
            type: "int",
            def: 0,
            help: "0 uses the internal default (2s).",
          },
          {
            key: "readiness.cacheSeconds",
            label: "Readiness cache (seconds)",
            type: "int",
            def: 0,
            help: "0 uses the internal default (3s), collapsing a burst of probes into one ping.",
          },
          {
            key: "logging.level",
            label: "Log level",
            type: "select",
            def: "info",
            options: [
              ["trace", "trace"],
              ["debug", "debug"],
              ["info", "info"],
              ["warn", "warn"],
              ["error", "error"],
            ],
          },
          {
            key: "logging.json",
            label: "Structured JSON logs",
            type: "bool",
            def: false,
            help: "Turn this on when shipping logs to a collector that parses fields.",
          },
          {
            key: "stats.intervalSeconds",
            label: "Stats interval (seconds)",
            type: "int",
            def: 300,
            svc: 300,
            help: "How often the event/memory count line is logged, and the maximum age of the cached counts the gauges read. 0 disables the log line.",
          },
        ],
      },
    ],
  },
  {
    id: "security",
    title: "Security",
    cards: [
      {
        title: "Authentication",
        blurb:
          "Both transports enforce the same policy. Health endpoints are always reachable without a token.",
        fields: [
          {
            key: "auth.method",
            label: "Method",
            type: "select",
            def: "none",
            options: [
              ["none", "none — no authentication"],
              ["hmac", "hmac — HS256 tokens minted by the service"],
              ["idp", "idp — RS256 tokens from an identity provider"],
            ],
          },
          {
            key: "auth.signingSecret",
            label: "Signing secret",
            type: "text",
            def: "",
            secret: true,
            generate: 32,
            when: (s) => value(s, "auth.method") === "hmac",
            help: "HS256 is keyed with the raw secret: use at least 32 random bytes. Generate one here, or leave it blank and inject it as an environment override.",
          },
          {
            key: "auth.activeKid",
            label: "Active key id",
            type: "text",
            def: "",
            when: (s) => value(s, "auth.method") === "hmac",
            help: "Optional. Names which of auth.signingKeys new tokens are minted with, so a secret can be rotated in while tokens signed by the old one still verify.",
          },
          {
            key: "auth.jwksUrl",
            label: "JWKS URL",
            type: "text",
            def: "",
            when: (s) => value(s, "auth.method") === "idp",
            placeholder:
              "https://idp.example.com/realms/hippocampus/protocol/openid-connect/certs",
            help: "Either this or the issuer below. With the issuer alone, the endpoint is resolved by OIDC discovery.",
          },
          {
            key: "auth.issuer",
            label: "Issuer",
            type: "text",
            def: "",
            when: (s) => value(s, "auth.method") === "idp",
            placeholder: "https://idp.example.com/realms/hippocampus",
            help: "When set it is also enforced against every token's iss claim.",
          },
          {
            key: "auth.audience",
            label: "Audience",
            type: "text",
            def: "",
            when: (s) => value(s, "auth.method") === "idp",
            help: "Enforced against aud when set. Auth0 needs the API identifier here to mint a verifiable JWT; Keycloak does not.",
          },
          {
            key: "auth.jwksRefreshIntervalSeconds",
            label: "JWKS refresh (seconds)",
            type: "int",
            def: 300,
            svc: 300,
            when: (s) => value(s, "auth.method") === "idp",
          },
          {
            key: "auth.revocationFile",
            label: "Revocation file",
            type: "text",
            def: "",
            when: (s) => value(s, "auth.method") !== "none",
            placeholder: "/etc/hippocampus/revocations.json",
            help: "A JSON file of revoked jti / client_id values, reloaded when its mtime changes. Composes with both methods.",
          },
          {
            key: "auth.revocationRefreshSeconds",
            label: "Revocation reload interval (seconds)",
            type: "int",
            def: 30,
            svc: 30,
            when: (s) =>
              value(s, "auth.method") !== "none" &&
              value(s, "auth.revocationFile") !== "",
          },
        ],
      },
      {
        title: "Authorisation",
        blurb:
          "Roles map onto three tiers: reader ⊂ writer ⊂ admin. A token resolving to no known tier is denied every RPC.",
        when: (s) => value(s, "auth.method") !== "none",
        fields: [
          {
            key: "auth.roleClaim",
            label: "Role claim",
            type: "text",
            def: "roles",
            svc: "roles",
            help: "The token claim carrying the caller's roles. A dotted path reaches into a nested object — Keycloak, for instance, uses realm_access.roles.",
          },
          {
            key: "auth.roleMapping",
            label: "Role → tier mapping",
            type: "map",
            def: {},
            help: "One role=tier per line, for providers whose role names are not already reader/writer/admin. For example: hippocampus-ops=admin",
          },
          {
            key: "auth.readerRecallReinforces",
            label: "A reader's recall reinforces the memory",
            type: "bool",
            def: false,
            help: "Off, a reader's RecallMemories is a plain read: the decay clock is not reset and no recall boost is applied.",
          },
        ],
      },
      {
        title: "Group scoping",
        blurb:
          "Restrict a token to records carrying particular group labels — a soft partition for teams or systems sharing one store. Inert until tokens carry a groups claim, so an existing deployment is unchanged. NOT isolation: the decay dynamics stay store-global, so a busy group still affects what another forgets. Hard isolation means one instance per tenant.",
        when: (s) => value(s, "auth.method") !== "none",
        fields: [
          {
            key: "auth.groupsClaim",
            label: "Groups claim",
            type: "text",
            def: "groups",
            svc: "groups",
            when: (s) => value(s, "auth.method") === "idp",
            help: "The token claim carrying the caller's group scope. A dotted path reaches into a nested object, exactly as the role claim does.",
          },
          {
            key: "auth.requireGroupScope",
            label: "Require every token to carry a group scope",
            type: "bool",
            def: false,
            help: "On, a token arriving with no groups claim is rejected. Worth setting once a store is partitioned: an unscoped token is the most privileged shape there is — it sees everything — so its absence should be an error rather than a silent grant.",
          },
        ],
      },
      {
        title: "Browser sign-in",
        blurb:
          "How the embedded console (/ui) obtains a token under idp. Leave both off for a service with no human users.",
        when: (s) =>
          value(s, "auth.method") === "idp" && value(s, "gateway.port") > 0,
        fields: [
          {
            key: "auth.ui.clientId",
            label: "SPA client id",
            type: "text",
            def: "",
            help: "A public client at the provider. The console runs Authorisation Code + PKCE in the browser and keeps the token in sessionStorage.",
          },
          {
            key: "auth.ui.scopes",
            label: "SPA scopes",
            type: "text",
            def: "openid profile",
          },
          {
            key: "auth.oauth2.enabled",
            label: "Server-side sign-in instead (confidential client)",
            type: "bool",
            def: false,
            help: "The service runs the flow itself and the session rides an HttpOnly cookie the page cannot read. Prefer it when the provider requires a confidential client, or to keep tokens out of page-readable storage.",
          },
          {
            key: "auth.oauth2.clientId",
            label: "Confidential client id",
            type: "text",
            def: "",
            when: (s) => value(s, "auth.oauth2.enabled"),
          },
          {
            key: "auth.oauth2.clientSecret",
            label: "Client secret",
            type: "text",
            def: "",
            secret: true,
            when: (s) => value(s, "auth.oauth2.enabled"),
          },
          {
            key: "auth.oauth2.redirectUrl",
            label: "Redirect URL",
            type: "text",
            def: "",
            when: (s) => value(s, "auth.oauth2.enabled"),
            placeholder: "https://hippocampus.example.com/auth/callback",
            help: "Must be registered at the provider.",
          },
          {
            key: "auth.oauth2.scopes",
            label: "Scopes",
            type: "text",
            def: "openid profile email",
            when: (s) => value(s, "auth.oauth2.enabled"),
            help: "Add offline_access for a refresh token.",
          },
        ],
      },
      {
        title: "TLS",
        blurb:
          "Terminate TLS here, or leave it off and terminate it upstream in a proxy, sidecar, or ingress.",
        fields: [
          {
            key: "tls.enabled",
            label: "Serve TLS on both transports",
            type: "bool",
            def: false,
          },
          {
            key: "tls.certFile",
            label: "Certificate file",
            type: "text",
            def: "",
            when: (s) => value(s, "tls.enabled"),
            placeholder: "/etc/hippocampus/tls/tls.crt",
          },
          {
            key: "tls.keyFile",
            label: "Private key file",
            type: "text",
            def: "",
            when: (s) => value(s, "tls.enabled"),
            placeholder: "/etc/hippocampus/tls/tls.key",
          },
        ],
      },
    ],
  },
  {
    id: "memory",
    title: "Memory & forgetting",
    custom: "preview",
    cards: [
      {
        title: "Intake",
        fields: [
          {
            key: "memory.limit.sizeBytes",
            label: "Maximum memory body (bytes)",
            type: "int",
            def: 1048576,
          },
          {
            key: "memory.minimumSignificance",
            label: "Minimum memory significance accepted",
            type: "int",
            def: 0,
            help: "A memory below this is rejected at StoreMemory rather than stored and forgotten later. A significance value, so it moves with the scale you choose on the Decay step.",
          },
          {
            key: "event.minimumSignificance",
            label: "Minimum event significance accepted",
            type: "int",
            def: 0,
          },
          {
            key: "sleep.periodSeconds",
            label: "Sleep cycle period (seconds)",
            type: "int",
            def: 3600,
            help: "How often consolidation runs. 0 or less disables the timed cycle entirely — supported for an instance driven only by the manual Sleep RPC or the WAL trigger.",
          },
        ],
      },
      {
        title: "Decay",
        blurb:
          "Value falls as a memory ages; anything below the threshold is forgotten by the next sleep cycle. Age is measured from the most recent recall, not from creation. Every method divides significance by a function of age, so significance is compared as a RATIO, never a difference: spread your values geometrically (1,000 / 3,000 / 10,000 / 30,000, not 1,000 / 2,000 / 3,000 / 4,000), and remember that the span between your smallest and largest sets how far significance can outweigh age.",
        fields: [
          {
            key: "consolidation.method",
            label: "Algorithm",
            type: "select",
            def: 1,
            svc: 1,
            options: [
              [1, "1 — power law (human forgetting curve)"],
              [2, "2 — linear, e^a rate"],
              [3, "3 — linear, 1+ln(a) rate"],
              [4, "4 — exponential half-life"],
              [5, "5 — logarithmic long tail"],
              [6, "6 — sigmoid consolidation window"],
            ],
            help: "1 matches published forgetting-curve research; 4 is the usual recency weighting for caches and feeds; 5 keeps nearly everything, for archives; 6 holds a memory near full value until the window closes, then lets it go quickly.",
          },
          {
            key: "consolidation.aggressiveness",
            label: "Aggressiveness (a)",
            type: "float",
            def: 1.0,
            svc: 1.0,
            help: "How fast value decays. For method 6 it is the window midpoint, in age units; for method 3 it must exceed 1/e (~0.368).",
          },
          {
            key: "consolidation.deletionThreshold",
            label: "Deletion threshold",
            type: "float",
            def: 10,
            help: "Value below which an item is consolidated away, before capacity pressure scales it. In significance-over-age units, so it moves with your significance scale: multiply every significance by ten and this must be multiplied by ten too, or things start being forgotten sooner or later than before without anyone changing it. Capacity eviction is unaffected — it ranks candidates against each other, never against this.",
          },
          {
            key: "consolidation.unitsOfAgeInDays",
            label: "Days per age unit",
            type: "float",
            def: 1.0,
            svc: 1.0,
            help: "The clock's scale. Below 1 compresses it — 0.002 makes an age unit about three minutes, which is how the demo shows a week of forgetting in an afternoon.",
          },
          {
            key: "consolidation.minimumAgeInDays",
            label: "Minimum age before consolidation (days)",
            type: "int",
            def: 14,
            help: "Defers value-based consolidation only; capacity eviction ignores it.",
          },
          {
            key: "consolidation.minimumRetentionInDays",
            label: "Hard retention floor (days)",
            type: "int",
            def: 0,
            help: "Nothing inside this window is ever deleted — not by consolidation, not by capacity eviction, whatever the pressure. 0 disables the floor.",
          },
          {
            key: "consolidation.linkSignificanceWeight",
            label: "Link significance weight",
            type: "float",
            def: 1.0,
            help: "How much an item's links raise its effective significance. The summed link significance is damped first (weight x ln(1 + total)), so a large total adds only a little: at weight 1.0 a total of 100 adds about 4.6, and 10000 adds about 9.2. Those are ADDED in significance units, so what they are worth depends entirely on your significance scale — about 9% of the top on a 1-100 scale, about 0.03% of it on a 1,000-30,000 scale. Set this relative to the scale you chose; the default of 1.0 assumes nothing about it. Applies to both memory links and event links. 0 disables the contribution.",
          },
          {
            key: "consolidation.linkRecallPropagation",
            label: "Spreading activation on recall",
            type: "float",
            def: 0,
            help: "How far a recalled memory's direct neighbours have their decay clocks advanced toward now, as a fraction from 0 to 1. 0 (the default) disables it, so recall reinforces only what was asked for. Their recall counts are never touched.",
          },
          {
            key: "consolidation.recallSignificanceWeight",
            label: "Recall significance weight",
            type: "float",
            def: 1.0,
            help: "Added to effective significance per recall. In significance units, so its worth depends on your scale: at 1.0, twenty recalls add 20 — a fifth of the top of a 1-100 scale, and rounding error on a 1,000-30,000 one. Set it relative to the scale you chose. 0 keeps the decay-clock reset but drops the boost.",
          },
          {
            key: "consolidation.defaultEventSignificanceValue",
            label: "Default event significance",
            type: "int",
            def: 0,
            help: "Applied to memories with no event.",
          },
          {
            key: "consolidation.defaultEventSignificancePercentile",
            label: "…or a percentile of existing events",
            type: "float",
            def: 0,
            help: "Non-zero overrides the fixed value: each cycle recomputes the default from this percentile of the events actually present.",
          },
        ],
      },
      {
        title: "Capacity",
        blurb:
          "As the store fills, the deletion threshold is scaled up so forgetting becomes more aggressive; past that, eviction deletes in ascending value order.",
        fields: [
          {
            key: "consolidation.capacityMemories",
            label: "Capacity (memories)",
            type: "int",
            def: 100000,
            help: "0 disables the row-count axis.",
          },
          {
            key: "consolidation.capacityBytes",
            label: "Capacity (bytes)",
            type: "int",
            def: 0,
            help: "0 disables the byte axis and capacity eviction with it. The better axis when bodies vary in size.",
          },
          {
            key: "consolidation.capacityBytesFloor",
            label: "Eviction floor (bytes)",
            type: "int",
            def: 0,
            when: (s) => value(s, "consolidation.capacityBytes") > 0,
            help: "Eviction runs down to here rather than stopping at the target, so it is not re-triggered by every write. Typically 80–90% of the capacity.",
          },
          {
            key: "consolidation.capacityPressureExponent",
            label: "Pressure exponent",
            type: "float",
            def: 4.0,
            help: "Higher keeps pressure negligible until the store is nearly full. At capacity the threshold is doubled.",
          },
          {
            key: "consolidation.walTriggerBytes",
            label: "WAL trigger (bytes)",
            type: "int",
            def: 0,
            when: (s) => value(s, "storage.driver") === "sqlite",
            help: "Runs an out-of-cycle sleep as soon as the SQLite WAL file passes this size, so a sustained write burst is checkpointed without waiting for the next timed cycle. SQLite only.",
          },
        ],
      },
      {
        title: "Summarisation candidates",
        blurb:
          "Each cycle can look for events whose memories have all gone quiet, and offer them for condensing into one summary memory.",
        fields: [
          {
            key: "consolidation.summarisationMinMemories",
            label: "Minimum memories per event",
            type: "int",
            def: 0,
            help: "0 disables the scan.",
          },
          {
            key: "consolidation.summarisationMinAgeInDays",
            label: "Quiet for at least (days)",
            type: "int",
            def: 30,
            when: (s) => value(s, "consolidation.summarisationMinMemories") > 0,
          },
          {
            key: "consolidation.summarisationMaxCandidates",
            label: "Maximum candidates cached",
            type: "int",
            def: 100,
            when: (s) => value(s, "consolidation.summarisationMinMemories") > 0,
          },
        ],
      },
      {
        title: "Forgotten log",
        blurb:
          "Keep a record of what each cycle deleted — id, group, event, significance, the value and threshold that decided it, and when. No bodies are kept: this says a memory was forgotten, not what it said.",
        fields: [
          {
            key: "consolidation.tombstones.enabled",
            label: "Record what is forgotten",
            type: "bool",
            def: false,
            help: "Off by default: it costs a row per forgotten memory. Turning it off later leaves what was already recorded in place — clearing the log is always a separate, explicit request.",
          },
          {
            key: "consolidation.tombstones.maxRows",
            label: "Keep at most (records)",
            type: "int",
            def: 100000,
            svc: 100000,
            when: (s) => value(s, "consolidation.tombstones.enabled"),
            help: "Trimmed at the end of each cycle. 0 removes the bound, which lets the log grow until the disk stops it.",
          },
          {
            key: "consolidation.tombstones.maxAgeInDays",
            label: "Keep for at most (days)",
            type: "int",
            def: 30,
            svc: 30,
            when: (s) => value(s, "consolidation.tombstones.enabled"),
            help: "Applied alongside the record cap — a record past either bound is trimmed. 0 removes this bound.",
          },
        ],
      },
      {
        title: "Deletion callbacks",
        blurb:
          "Tell an HTTP endpoint what the store has forgotten, instead of waiting to be asked. Deliveries are recorded in the same transaction as the deletion and sent by a background worker, so a receiver that is down is a backlog rather than a silent loss.",
        fields: [
          {
            key: "callbacks.enabled",
            label: "Send callbacks",
            type: "bool",
            def: false,
            help: "Off by default. Enabling it needs a URL and nothing else; every bound below is already set.",
          },
          {
            key: "callbacks.url",
            label: "Receiver URL",
            type: "text",
            def: "",
            when: (s) => value(s, "callbacks.enabled"),
            help: "Each delivery is POSTed here as JSON. The service refuses to start with callbacks on and no URL, because it would queue a row per forgotten memory that nothing could ever drain.",
          },
          {
            key: "callbacks.token",
            label: "Bearer token",
            type: "text",
            def: "",
            secret: true,
            when: (s) => value(s, "callbacks.enabled"),
            help: "Sent as an Authorization header, if the receiver wants one. Leave it blank and inject it as an environment override.",
          },
          {
            key: "callbacks.signingSecret",
            label: "Signing secret",
            type: "text",
            def: "",
            secret: true,
            when: (s) => value(s, "callbacks.enabled"),
            help: "Signs each delivery (HMAC-SHA256 over the timestamp and the body) so a receiver can verify it came from this instance unaltered. Use at least 32 random bytes.",
          },
          {
            key: "callbacks.timeoutSeconds",
            label: "Delivery timeout (seconds)",
            type: "int",
            def: 10,
            svc: 10,
            when: (s) => value(s, "callbacks.enabled"),
            help: "Bounds one attempt. A receiver that hangs must not be able to hold the dispatcher open past a shutdown.",
          },
          {
            key: "callbacks.allDeletions",
            label: "Report every deletion, not just decay",
            type: "bool",
            def: false,
            when: (s) => value(s, "callbacks.enabled"),
            help: "Off, only consolidation and eviction speak — a client that called DeleteMemories already knows. On, client deletes, clears, cascades and purges are reported too, each tagged with its cause.",
          },
          {
            key: "callbacks.includeBodies",
            label: "Include memory bodies",
            type: "bool",
            def: false,
            when: (s) => value(s, "callbacks.enabled"),
            help: "Off by default: it costs a body read per forgotten memory and real space in the queue. On, a receiver can archive what was forgotten rather than only learn that it was.",
          },
          {
            key: "callbacks.maxBodyBytes",
            label: "Largest body to carry (bytes)",
            type: "int",
            def: 65536,
            svc: 65536,
            when: (s) =>
              value(s, "callbacks.enabled") && value(s, "callbacks.includeBodies"),
            help: "A body over this is omitted and flagged, never truncated — a receiver cannot tell a truncated body from a whole one. 0 removes the cap.",
          },
          {
            key: "callbacks.events.memoryForgotten",
            label: "Report forgotten memories",
            type: "bool",
            def: true,
            svc: true,
            when: (s) => value(s, "callbacks.enabled"),
            help: "Turning a kind off stops the rows being written, which is cheaper than writing them for a receiver to discard.",
          },
          {
            key: "callbacks.events.eventForgotten",
            label: "Report forgotten events",
            type: "bool",
            def: true,
            svc: true,
            when: (s) => value(s, "callbacks.enabled"),
            help: "An event goes when its last memory does, or when a caller deletes it.",
          },
          {
            key: "callbacks.events.sleepCompleted",
            label: "Report finished sleep cycles",
            type: "bool",
            def: true,
            svc: true,
            when: (s) => value(s, "callbacks.enabled"),
            help: "One delivery per cycle carrying the counts and the ids it forgot, chunked so a large cycle never becomes one unbounded request.",
          },
          {
            key: "callbacks.maxIdsPerDelivery",
            label: "Ids per sleep-cycle delivery",
            type: "int",
            def: 500,
            svc: 500,
            when: (s) =>
              value(s, "callbacks.enabled") &&
              value(s, "callbacks.events.sleepCompleted"),
            help: "A cycle forgetting more than this is split into several numbered deliveries sharing one cycle id.",
          },
          {
            key: "callbacks.maxRows",
            label: "Queue at most (deliveries)",
            type: "int",
            def: 1000000,
            svc: 1000000,
            when: (s) => value(s, "callbacks.enabled"),
            help: "Reached only when the receiver cannot keep up. Past it the oldest undelivered callbacks are abandoned — and unlike a dropped index update, nothing recovers those. 0 removes the bound.",
          },
          {
            key: "callbacks.maxAgeHours",
            label: "Queue for at most (hours)",
            type: "int",
            def: 24,
            svc: 24,
            when: (s) => value(s, "callbacks.enabled"),
            help: "Applied alongside the delivery cap. Hours rather than days: this queue is meant to drain in seconds, so a day of backlog is already an outage. 0 removes this bound.",
          },
          {
            key: "callbacks.tls.enabled",
            label: "Customise TLS for the receiver",
            type: "bool",
            def: false,
            when: (s) => value(s, "callbacks.enabled"),
            help: "Only needed for an https:// receiver whose certificate the system pool does not trust. A public certificate needs nothing here.",
          },
          {
            key: "callbacks.tls.caCertFile",
            label: "CA certificate file",
            type: "text",
            def: "",
            when: (s) =>
              value(s, "callbacks.enabled") && value(s, "callbacks.tls.enabled"),
            help: "A PEM bundle trusted in place of the system pool, for a receiver serving a private-CA certificate.",
          },
        ],
      },
    ],
  },
  {
    id: "extras",
    title: "Search & summaries",
    cards: [
      {
        title: "Semantic search",
        blurb:
          "Finds memories by meaning rather than by the words they happen to use, so a search for 'deployment problem' can surface a memory that only ever said 'the rollout broke'. It needs two things that keyword search does not: an embedding model to turn text into vectors, and OpenSearch to index and search them. Both halves are required — without OpenSearch there is nowhere to put the vectors, whatever the driver.",
        fields: [
          {
            key: "ollama.embedding.enabled",
            label: "Enable semantic search",
            type: "bool",
            def: false,
            help: (s) =>
              value(s, "opensearch.enabled")
                ? "Memory bodies are embedded as they are stored, so the model server is on the write path — a slow or unreachable one costs you the vector for that memory, never the memory itself."
                : "Requires OpenSearch, which is off above. Turn it on first, or leave semantic search disabled — keyword search is unaffected either way.",
          },
          {
            key: "ollama.embedding.address",
            label: "Model server address",
            type: "text",
            def: "http://localhost:11434",
            svc: "http://localhost:11434",
            when: (s) => value(s, "ollama.embedding.enabled"),
            help: "The Ollama server that produces the vectors. It may be the same one used for summarisation.",
          },
          {
            key: "ollama.embedding.model",
            label: "Embedding model",
            type: "text",
            def: "nomic-embed-text",
            svc: "nomic-embed-text",
            when: (s) => value(s, "ollama.embedding.enabled"),
            help: "Must be an embedding model, not a generation model. Changing it later invalidates every stored vector — they are not comparable across models and rarely even share a dimension count — so a change means re-embedding the store.",
          },
          {
            key: "ollama.embedding.dimensions",
            label: "Vector dimensions",
            type: "int",
            def: 768,
            svc: 768,
            when: (s) => value(s, "ollama.embedding.enabled"),
            help: "Must match the model: nomic-embed-text is 768, all-minilm 384, mxbai-embed-large 1024. The OpenSearch index fixes this at creation, so changing it later means rebuilding the index with --backfill-search --reindex.",
          },
          {
            key: "ollama.embedding.timeoutSeconds",
            label: "Timeout (seconds)",
            type: "int",
            def: 30,
            svc: 30,
            when: (s) => value(s, "ollama.embedding.enabled"),
            help: "Bounds one embedding call. Much tighter than summarisation's, because this one sits on the write path.",
          },
          {
            key: "ollama.embedding.maxTextBytes",
            label: "Maximum text embedded (bytes)",
            type: "int",
            def: 0,
            help: "0 uses the internal default. A body longer than this is truncated on a rune boundary before it is embedded, so only its opening is searchable by meaning — raise it for long documents.",
            when: (s) => value(s, "ollama.embedding.enabled"),
          },
          {
            key: "ollama.embedding.batchSize",
            label: "Batch size",
            type: "int",
            def: 32,
            svc: 32,
            when: (s) => value(s, "ollama.embedding.enabled"),
            help: "Texts per request to the model server. Matters most for a backfill over a whole store, where one request per memory would be all round trip.",
          },
        ],
      },
      {
        title: "Search ranking",
        blurb:
          "How much the store's own view of a memory counts towards what a search returns first. Text relevance always leads; these decide how much significance and recall get to reorder results that matched about equally well. Set both to 0 for pure relevance order.",
        fields: [
          {
            key: "search.significanceWeight",
            label: "Significance weight",
            type: "float",
            def: 0.3,
            svc: 0.3,
            help: "How much a memory's significance promotes it among comparable matches.",
          },
          {
            key: "search.recallWeight",
            label: "Recall weight",
            type: "float",
            def: 0.2,
            svc: 0.2,
            help: "How much a memory being recalled often promotes it. Recall counts are damped, so the difference between never and once counts for more than between 500 and 1000.",
          },
        ],
      },
      {
        title: "Content search (OpenSearch)",
        blurb:
          "A secondary index over memory bodies. On the SQLite driver keyword content search is built in and needs none of this; enable OpenSearch to scale it out, to get content search at all on PostgreSQL/MySQL, or to unlock semantic search — which needs OpenSearch's vector index on every driver. Either way it is strictly secondary: results are always re-read from the primary store, so a stale entry drops out rather than being served.",
        fields: [
          {
            key: "opensearch.enabled",
            label: "Enable content search",
            type: "bool",
            def: false,
            help: (s) =>
              value(s, "storage.driver") === "sqlite"
                ? "Your driver already has keyword search built in, so this is optional — turn it on to scale search out beyond one instance, or to add semantic search."
                : "Your driver has no built-in content search, so without this SearchMemories is rejected outright.",
          },
          {
            key: "opensearch.addresses",
            label: "Cluster addresses",
            type: "list",
            def: ["http://localhost:9200"],
            when: (s) => value(s, "opensearch.enabled"),
            help: "One URL per line.",
          },
          {
            key: "opensearch.index",
            label: "Index name",
            type: "text",
            def: "hippocampus-memories",
            svc: "hippocampus-memories",
            when: (s) => value(s, "opensearch.enabled"),
          },
          {
            key: "opensearch.username",
            label: "Username",
            type: "text",
            def: "",
            when: (s) => value(s, "opensearch.enabled"),
          },
          {
            key: "opensearch.password",
            label: "Password",
            type: "text",
            def: "",
            secret: true,
            when: (s) => value(s, "opensearch.enabled"),
          },
          {
            key: "opensearch.queueSize",
            label: "Propagation queue size",
            type: "int",
            def: 1024,
            svc: 1024,
            when: (s) => value(s, "opensearch.enabled"),
            help: "Index writes are asynchronous and never block a request; an overflowing queue drops operations, which the reconciliation sweep later heals.",
          },
          {
            key: "opensearch.reconcileIntervalSeconds",
            label: "Reconciliation sweep (seconds)",
            type: "int",
            def: 3600,
            svc: 3600,
            when: (s) => value(s, "opensearch.enabled"),
            help: "The consolidating instance re-indexes the primary store on this interval, healing anything the queue dropped. 0 disables it.",
          },
          {
            key: "opensearch.tls.caCertFile",
            label: "CA certificate file",
            type: "text",
            def: "",
            when: (s) => value(s, "opensearch.enabled"),
            help: "For a cluster with a private or self-signed CA.",
          },
          {
            key: "opensearch.tls.certFile",
            label: "Client certificate file",
            type: "text",
            def: "",
            when: (s) => value(s, "opensearch.enabled"),
            help: "Mutual TLS only.",
          },
          {
            key: "opensearch.tls.keyFile",
            label: "Client key file",
            type: "text",
            def: "",
            when: (s) => value(s, "opensearch.enabled"),
          },
          {
            key: "opensearch.tls.insecureSkipVerify",
            label: "Skip certificate verification",
            type: "bool",
            def: false,
            when: (s) => value(s, "opensearch.enabled"),
            help: "Development only — it disables the check that the cluster is who it claims to be.",
          },
        ],
      },
      {
        title: "Embedded summariser (Ollama)",
        blurb:
          "Lets the service author its own summaries. It is the one component that reads memory content, and it sends bodies to the Ollama server — keep that server inside your trust boundary.",
        fields: [
          {
            key: "ollama.enabled",
            label: "Enable the embedded LLM",
            type: "bool",
            def: false,
          },
          {
            key: "ollama.address",
            label: "Ollama address",
            type: "text",
            def: "http://localhost:11434",
            svc: "http://localhost:11434",
            when: (s) => value(s, "ollama.enabled"),
          },
          {
            key: "ollama.model",
            label: "Model",
            type: "text",
            def: "llama3.2",
            svc: "llama3.2",
            when: (s) => value(s, "ollama.enabled"),
            help: "Pull it on the Ollama server before enabling this.",
          },
          {
            key: "ollama.autoSummarise",
            label: "Summarise candidates automatically each sleep cycle",
            type: "bool",
            def: false,
            when: (s) => value(s, "ollama.enabled"),
          },
          {
            key: "ollama.timeoutSeconds",
            label: "Generation timeout (seconds)",
            type: "int",
            def: 120,
            svc: 120,
            when: (s) => value(s, "ollama.enabled"),
          },
          {
            key: "ollama.maxMemories",
            label: "Maximum memories per prompt",
            type: "int",
            def: 200,
            when: (s) => value(s, "ollama.enabled"),
          },
          {
            key: "ollama.promptCharLimit",
            label: "Prompt character limit",
            type: "int",
            def: 32000,
            when: (s) => value(s, "ollama.enabled"),
          },
          {
            key: "ollama.temperature",
            label: "Temperature",
            type: "float",
            def: 0,
            when: (s) => value(s, "ollama.enabled"),
            help: "0 uses the model's own default.",
          },
          {
            key: "ollama.systemPrompt",
            label: "System prompt",
            type: "text",
            def: "",
            when: (s) => value(s, "ollama.enabled"),
            help: "Empty uses the built-in memory-consolidation instruction.",
          },
        ],
      },
    ],
  },
  {
    id: "observability",
    title: "Observability",
    cards: [
      {
        title: "OpenTelemetry",
        blurb:
          "Tracing and metrics are independent, both exported over OTLP/gRPC. With both off, the instrumentation is a no-op and nothing is dialled.",
        fields: [
          {
            key: "observability.tracing.enabled",
            label: "Enable tracing",
            type: "bool",
            def: false,
          },
          {
            key: "observability.tracing.samplingRatio",
            label: "Sampling ratio",
            type: "float",
            def: 1.0,
            when: (s) => value(s, "observability.tracing.enabled"),
            help: "Applies to traces started here; a caller's sampling decision is honoured.",
          },
          {
            key: "observability.metrics.enabled",
            label: "Enable metrics",
            type: "bool",
            def: false,
          },
          {
            key: "observability.metrics.exportIntervalSeconds",
            label: "Metrics export interval (seconds)",
            type: "int",
            def: 60,
            when: (s) => value(s, "observability.metrics.enabled"),
          },
          {
            key: "observability.otlp.endpoint",
            label: "OTLP endpoint",
            type: "text",
            def: "localhost:4317",
            when: (s) =>
              value(s, "observability.tracing.enabled") ||
              value(s, "observability.metrics.enabled"),
            help: "Empty falls back to the standard OTEL_EXPORTER_OTLP_* environment variables.",
          },
          {
            key: "observability.otlp.insecure",
            label: "Plaintext to the collector",
            type: "bool",
            def: true,
            when: (s) =>
              value(s, "observability.tracing.enabled") ||
              value(s, "observability.metrics.enabled"),
          },
        ],
      },
      {
        title: "Deployment view",
        blurb:
          "GetTopology reports what this instance is attached to and the last known health of each part, backing the console's Deployment tab and `hippo topology`. It describes only what this instance knows — itself and what it dials — so anything that dials IN has to be declared below.",
        fields: [
          {
            key: "topology.enabled",
            label: "Enable the topology view",
            type: "bool",
            def: true,
            svc: true,
          },
          {
            key: "topology.minimumTier",
            label: "Minimum tier",
            type: "select",
            options: ["reader", "writer", "admin"],
            def: "reader",
            svc: "reader",
            when: (s) => value(s, "topology.enabled"),
            help: "Addresses are redacted of credentials before they reach the response, which is what makes reader defensible. A group-scoped caller is refused whatever this says — there is no per-group topology.",
          },
          {
            key: "topology.probeIntervalSeconds",
            label: "Probe interval (seconds)",
            type: "int",
            def: 30,
            svc: 30,
            when: (s) => value(s, "topology.enabled"),
            help: "Dependencies are probed by a background prober on this cadence, never on the request path, so a console viewer never opens a connection to every dependency.",
          },
          {
            key: "topology.probeTimeoutSeconds",
            label: "Probe timeout (seconds)",
            type: "int",
            def: 2,
            svc: 2,
            when: (s) => value(s, "topology.enabled"),
          },
          {
            key: "topology.heartbeatSeconds",
            label: "Peer heartbeat (seconds)",
            type: "int",
            def: 30,
            svc: 30,
            when: (s) =>
              value(s, "topology.enabled") &&
              value(s, "storage.driver") !== "sqlite",
            help: "Each instance writes one row to a shared postgres/mysql store on this cadence and reads the others, which is how a horizontally-scaled deployment names its peers and warns when nothing is consolidating. Not available on sqlite, which is single-instance.",
          },
          {
            key: "topology.probeTransferTarget",
            label: "Probe the transfer target",
            type: "bool",
            def: false,
            when: (s) =>
              value(s, "topology.enabled") &&
              value(s, "transfer.targetAddress") !== "",
            help: "Opt-in because the probe dials a remote instance on a timer.",
          },
        ],
      },
    ],
  },
  {
    id: "transfer",
    title: "Transfer & archive",
    cards: [
      {
        title: "Transfer to a central instance",
        blurb:
          "Move a whole store — timestamps, recall history, links and all — to another instance over gRPC, or through S3 when the two are never connected at once. Leave empty if you do not use it.",
        fields: [
          {
            key: "transfer.targetAddress",
            label: "Target address",
            type: "text",
            def: "",
            placeholder: "central.example.com:50051",
          },
          {
            key: "transfer.token",
            label: "Bearer token for the target",
            type: "text",
            def: "",
            secret: true,
            when: (s) => value(s, "transfer.targetAddress") !== "",
          },
          {
            key: "transfer.tls.enabled",
            label: "TLS to the target",
            type: "bool",
            def: false,
            when: (s) => value(s, "transfer.targetAddress") !== "",
          },
          {
            key: "transfer.tls.caCertFile",
            label: "Target CA certificate",
            type: "text",
            def: "",
            when: (s) => value(s, "transfer.tls.enabled"),
          },
          {
            key: "transfer.batchSize",
            label: "Records per batch",
            type: "int",
            def: 500,
            when: (s) => value(s, "transfer.targetAddress") !== "",
          },
          {
            key: "transfer.maxBatchBytes",
            label: "Maximum batch bytes",
            type: "int",
            def: 0,
            when: (s) => value(s, "transfer.targetAddress") !== "",
            help: "0 uses the internal default. A page of large bodies is split so no message overflows the receiver's gRPC frame.",
          },
          {
            key: "transfer.maxManifestRows",
            label: "Maximum manifest rows",
            type: "int",
            def: 0,
            help: "0 is unlimited. Each captured memory holds its id and recall state in memory, so cap this on a very large store; an over-cap run is refused before anything is uploaded.",
          },
        ],
      },
      {
        title: "S3 archive",
        blurb:
          "Export and Import stream a gzip archive through an object store. Credentials come from the standard AWS chain, not from this file.",
        fields: [
          { key: "s3.bucket", label: "Bucket", type: "text", def: "" },
          {
            key: "s3.region",
            label: "Region",
            type: "text",
            def: "",
            when: (s) => value(s, "s3.bucket") !== "",
          },
          {
            key: "s3.keyPrefix",
            label: "Key prefix",
            type: "text",
            def: "hippocampus/",
            when: (s) => value(s, "s3.bucket") !== "",
          },
          {
            key: "s3.endpoint",
            label: "Endpoint",
            type: "text",
            def: "",
            when: (s) => value(s, "s3.bucket") !== "",
            help: "Set for MinIO or another S3-compatible store; empty uses AWS.",
          },
          {
            key: "s3.usePathStyle",
            label: "Path-style addressing",
            type: "bool",
            def: false,
            when: (s) => value(s, "s3.bucket") !== "",
            help: "Required by MinIO.",
          },
        ],
      },
    ],
  },
  {
    id: "review",
    title: "Review & download",
    custom: "review",
    cards: [],
  },
];

// FIELDS indexes every field by key, for defaults, type coercion, and secret handling.
const FIELDS = new Map();

for (const step of STEPS) {
  for (const card of step.cards) {
    for (const field of card.fields) {
      field.step = step.id;

      // A card-level condition applies to every field it holds, so fold it into the field's own.
      // Everything downstream - rendering, the generated config, the secret sweep - then asks one
      // question, and a key whose whole card is irrelevant never reaches the config file.
      if (card.when) {
        const own = field.when;
        field.when = own ? (s) => card.when(s) && own(s) : card.when;
      }

      FIELDS.set(field.key, field);
    }
  }
}

/* -------------------------------------------------------------------- state */

const STORAGE_KEY = "hippocampus-wizard-v1";

const state = {
  step: 0,
  target: "compose",
  preset: "",
  includeSecrets: false,
  minimal: false,
  reviewTab: "config",
  values: {},
};

function value(s, key) {
  if (Object.prototype.hasOwnProperty.call(s.values, key)) {
    return s.values[key];
  }

  const field = FIELDS.get(key);

  return field ? field.def : undefined;
}

const val = (key) => value(state, key);

function setValue(key, next) {
  const field = FIELDS.get(key);

  if (field && JSON.stringify(next) === JSON.stringify(field.def)) {
    delete state.values[key];
  } else {
    state.values[key] = next;
  }

  persist();
}

// changed reports whether a key has been moved off the service's own default — what the "minimal"
// config emits, and what the review step counts.
const changed = (key) =>
  Object.prototype.hasOwnProperty.call(state.values, key);

// persist saves the wizard's answers so a reload does not lose them. Secrets are deliberately
// excluded: this page may be served from a public site, and a signing secret or DSN has no business
// outliving the tab in localStorage.
function persist() {
  const safe = {};

  for (const [key, entry] of Object.entries(state.values)) {
    const field = FIELDS.get(key);

    if (field && field.secret) {
      continue;
    }

    safe[key] = entry;
  }

  try {
    localStorage.setItem(
      STORAGE_KEY,
      JSON.stringify({
        target: state.target,
        preset: state.preset,
        minimal: state.minimal,
        values: safe,
      }),
    );
  } catch {
    // A browser with storage disabled or full simply loses the ability to resume; the wizard works
    // exactly the same within the tab, so there is nothing to report.
  }
}

function restore() {
  let saved = null;

  try {
    saved = JSON.parse(localStorage.getItem(STORAGE_KEY) || "null");
  } catch {
    saved = null;
  }

  if (!saved) {
    return;
  }

  state.target = saved.target || state.target;
  state.preset = saved.preset || "";
  state.minimal = Boolean(saved.minimal);
  state.values = saved.values || {};
}

function applyPreset(preset) {
  state.values = {};
  state.preset = preset.id;
  state.target = preset.target;

  for (const [key, entry] of Object.entries(preset.values)) {
    setValue(key, entry);
  }

  applyTargetPaths(preset.target);
  persist();
}

// applyTargetPaths moves the filesystem-shaped settings to the chosen platform's convention, unless
// the operator has already typed a path of their own (one that matches no target's default).
function applyTargetPaths(targetId) {
  const target = targetById(targetId);
  const current = val("storage.directory");
  const isTargetDefault = TARGETS.some(
    (candidate) => candidate.dataDir === current,
  );

  if (!changed("storage.directory") || isTargetDefault) {
    setValue("storage.directory", target.dataDir);
  }
}

/* --------------------------------------------------------------- validation */

// validate mirrors the service's own startup checks (validateConfig and the driver switch in
// cmd/hippocampus/main.go) plus the operational advice from docs/configuration.md, so a config that
// passes here starts, and one that would only warn at startup says so here first.
function validate() {
  const issues = [];
  const add = (level, step, text) => issues.push({ level, step, text });

  const driver = val("storage.driver");
  const method = Number(val("consolidation.method"));
  const aggressiveness = Number(val("consolidation.aggressiveness"));
  const authMethod = val("auth.method");
  const gatewayPort = Number(val("gateway.port"));

  if (driver === "sqlite" && !String(val("storage.directory")).trim()) {
    add(
      "error",
      "storage",
      "storage.directory must be set for the sqlite driver — an empty directory selects the test-only in-memory database, and the service refuses to start.",
    );
  }

  if (driver === "postgres" && !val("storage.postgres.dsn")) {
    add(
      "error",
      "storage",
      "storage.postgres.dsn is required by the postgres driver. Supply it here, or inject it as HIPPOCAMPUS_STORAGE_POSTGRES_DSN.",
    );
  }

  if (driver === "mysql" && !val("storage.mysql.dsn")) {
    add(
      "error",
      "storage",
      "storage.mysql.dsn is required by the mysql driver. Supply it here, or inject it as HIPPOCAMPUS_STORAGE_MYSQL_DSN.",
    );
  }

  if (driver !== "sqlite" && Number(val("consolidation.walTriggerBytes")) > 0) {
    add(
      "error",
      "memory",
      `consolidation.walTriggerBytes is rejected at startup with the ${driver} driver: there is no client-visible WAL file to measure.`,
    );
  }

  if (driver === "sqlite" && !val("consolidation.enabled")) {
    add(
      "warn",
      "storage",
      "SQLite cannot be shared between instances, so a non-consolidating SQLite instance is not a replica — it is simply one that never forgets on its own. Horizontal scaling needs postgres or mysql.",
    );
  }

  if (Number(val("consolidation.unitsOfAgeInDays")) <= 0) {
    add(
      "error",
      "memory",
      "consolidation.unitsOfAgeInDays must be greater than 0.",
    );
  }

  if (!(aggressiveness > 0)) {
    add(
      "error",
      "memory",
      "consolidation.aggressiveness must be greater than 0.",
    );
  } else if (method === 3 && aggressiveness <= Math.exp(-1)) {
    add(
      "error",
      "memory",
      "consolidation.aggressiveness must exceed 1/e (~0.368) for method 3 — at or below it the decay factor goes non-positive and nothing is ever consolidated.",
    );
  }

  if (Number(val("consolidation.minimumRetentionInDays")) < 0) {
    add(
      "error",
      "memory",
      "consolidation.minimumRetentionInDays must not be negative.",
    );
  }

  if (val("callbacks.enabled")) {
    const url = String(val("callbacks.url")).trim();

    if (!url) {
      add(
        "error",
        "memory",
        "callbacks.url is required when callbacks.enabled is set \u2014 the service refuses to start without it, because it would queue a delivery per forgotten memory that nothing could ever drain. Supply it here, or inject it as HIPPOCAMPUS_CALLBACKS_URL.",
      );
    } else if (!/^https?:\/\//.test(url)) {
      add(
        "error",
        "memory",
        "callbacks.url must be an http:// or https:// URL.",
      );
    } else if (url.startsWith("http://") && val("callbacks.signingSecret")) {
      add(
        "warn",
        "memory",
        "callbacks.signingSecret is set on a plain http:// URL. The signature proves the body was not altered, but the body itself \u2014 which may carry memory bodies \u2014 travels in the clear.",
      );
    }

    const secret = String(val("callbacks.signingSecret") || "");

    if (secret && secret.length < 32) {
      add(
        "warn",
        "memory",
        "callbacks.signingSecret is shorter than 32 characters. It keys an HMAC: use at least 32 random bytes, or a receiver's verification is easier to forge than it looks.",
      );
    }

    if (
      val("callbacks.includeBodies") &&
      Number(val("callbacks.maxBodyBytes")) <= 0
    ) {
      add(
        "warn",
        "memory",
        "callbacks.includeBodies is set with no callbacks.maxBodyBytes, so a single large memory can be carried whole into the queue and the delivery.",
      );
    }

    if (
      Number(val("callbacks.maxRows")) === 0 &&
      Number(val("callbacks.maxAgeHours")) === 0
    ) {
      add(
        "warn",
        "memory",
        "Both callback queue bounds are 0, so an unreachable receiver grows the queue until the disk stops it. The queue is excluded from the capacity target, not from the disk.",
      );
    }

    if (val("callbacks.tls.enabled") && !val("callbacks.tls.caCertFile")) {
      add(
        "warn",
        "memory",
        "callbacks.tls is enabled with no caCertFile, which verifies against the system certificate pool \u2014 the same as leaving the block off. Set caCertFile only for a receiver serving a private-CA certificate.",
      );
    }

    if (!val("consolidation.enabled")) {
      add(
        "warn",
        "memory",
        "Callbacks are enabled on an instance that does not consolidate. A replica neither records nor dispatches them: forgetting happens on the consolidator, and its dispatcher drains the shared queue.",
      );
    }
  }

  const capacityBytes = Number(val("consolidation.capacityBytes"));
  const capacityFloor = Number(val("consolidation.capacityBytesFloor"));

  if (
    capacityBytes > 0 &&
    capacityFloor > 0 &&
    capacityFloor >= capacityBytes
  ) {
    add(
      "error",
      "memory",
      "consolidation.capacityBytesFloor must be below consolidation.capacityBytes — it is the headroom eviction frees, not a second target.",
    );
  }

  if (
    capacityBytes === 0 &&
    Number(val("consolidation.capacityMemories")) === 0
  ) {
    add(
      "warn",
      "memory",
      "Both capacity axes are 0, so there is no capacity pressure and no eviction: the store grows until the disk does not. Set at least consolidation.capacityBytes for a bounded store.",
    );
  }

  if (capacityBytes > 0 && capacityFloor === 0) {
    add(
      "warn",
      "memory",
      "consolidation.capacityBytes is set without a floor, so eviction stops the moment it reaches the target and re-triggers on the next write. Set consolidation.capacityBytesFloor to about 85% of the target.",
    );
  }

  if (gatewayPort > 0 && gatewayPort === Number(val("port"))) {
    add(
      "error",
      "server",
      "The gateway and gRPC listeners cannot share a port.",
    );
  }

  if (
    gatewayPort === 0 &&
    (state.target === "k8s" || state.target === "compose")
  ) {
    add(
      "error",
      "server",
      "The gateway is disabled, but the container and Kubernetes probes generated here poll /healthz and /readyz over HTTP. Set gateway.port (8080 is conventional).",
    );
  } else if (gatewayPort === 0) {
    add(
      "info",
      "server",
      "The HTTP gateway is off, so /healthz, /readyz, the OpenAPI description, and the web console are unavailable — gRPC clients only.",
    );
  }

  if (authMethod === "hmac") {
    const secret = String(val("auth.signingSecret") || "");

    if (!secret) {
      add(
        "warn",
        "security",
        "No signing secret is set here. That is fine if you inject HIPPOCAMPUS_AUTH_SIGNINGSECRET at runtime — but the service will not verify a token without one.",
      );
    } else if (secret.length < 32) {
      add(
        "warn",
        "security",
        "The signing secret is shorter than 32 bytes. HS256 is keyed with the raw secret, so a short one is brute-forceable; the service logs a warning at startup.",
      );
    }
  }

  if (authMethod === "idp" && !val("auth.jwksUrl") && !val("auth.issuer")) {
    add(
      "error",
      "security",
      "The idp method needs auth.jwksUrl, or auth.issuer to resolve it by OIDC discovery. The initial key fetch failing fails startup.",
    );
  }

  if (val("auth.oauth2.enabled")) {
    if (authMethod !== "idp") {
      add(
        "error",
        "security",
        "Server-side sign-in (auth.oauth2) is available only under auth.method 'idp'.",
      );
    }

    if (!val("auth.oauth2.clientId") || !val("auth.oauth2.redirectUrl")) {
      add(
        "error",
        "security",
        "Server-side sign-in needs both a confidential client id and the redirect URL registered at the provider.",
      );
    }
  }

  if (val("tls.enabled") && (!val("tls.certFile") || !val("tls.keyFile"))) {
    add(
      "error",
      "security",
      "tls.enabled needs both tls.certFile and tls.keyFile.",
    );
  }

  if (authMethod !== "none" && !val("tls.enabled")) {
    add(
      "warn",
      "security",
      "Authentication is on but TLS is off, so bearer tokens travel in clear text. That is fine when TLS is terminated upstream (ingress, sidecar, or mesh) — the service only logs a warning — but not on an exposed listener.",
    );
  }

  if (authMethod === "none" && state.target !== "binary") {
    add(
      "warn",
      "security",
      "Authentication is off: every RPC, including Purge, is open to anything that can reach the port. Bind to loopback or set auth.method for anything beyond a local instance.",
    );
  }

  if (val("rateLimit.enabled")) {
    const rates = [
      "rateLimit.global.requestsPerSecond",
      "rateLimit.perClient.requestsPerSecond",
      "rateLimit.tiers.reader.requestsPerSecond",
      "rateLimit.tiers.writer.requestsPerSecond",
      "rateLimit.tiers.admin.requestsPerSecond",
    ];

    if (!rates.some((key) => Number(val(key)) > 0)) {
      add(
        "warn",
        "server",
        "Rate limiting is enabled but no level has a rate, so nothing is limited. The service starts and logs the same warning.",
      );
    }

    if (authMethod === "none") {
      const tiers = rates
        .filter((key) => key.startsWith("rateLimit.tiers."))
        .some((key) => Number(val(key)) > 0);

      if (tiers) {
        add(
          "error",
          "server",
          "Per-tier limits need authentication: a tier comes from a token's roles, so with auth.method 'none' no caller ever matches one.",
        );
      }

      if (Number(val("rateLimit.perClient.requestsPerSecond")) > 0) {
        add(
          "info",
          "server",
          "With authentication off, the per-client limit is keyed on the caller's address rather than an identity — which is per-host, not per-application.",
        );
      }
    }

    if (val("rateLimit.trustForwardedFor")) {
      add(
        "warn",
        "server",
        "rateLimit.trustForwardedFor believes a caller-supplied header. Only set it behind a proxy that overwrites X-Forwarded-For, or a caller can mint itself an unlimited number of buckets.",
      );
    }
  }

  if (
    val("opensearch.enabled") &&
    (val("opensearch.addresses") || []).length === 0
  ) {
    add(
      "error",
      "extras",
      "Content search is enabled with no cluster addresses.",
    );
  }

  if (val("opensearch.enabled") && val("opensearch.tls.insecureSkipVerify")) {
    add(
      "warn",
      "extras",
      "opensearch.tls.insecureSkipVerify disables certificate verification — development only.",
    );
  }

  if (val("opensearch.enabled") && !val("consolidation.enabled")) {
    add(
      "info",
      "extras",
      "The reconciliation sweep that heals a sparse index runs only on the consolidating instance, so this replica will not perform it.",
    );
  }

  if (val("ollama.enabled") && !val("ollama.address")) {
    add("error", "extras", "The embedded summariser needs ollama.address.");
  }

  if (
    val("ollama.autoSummarise") &&
    Number(val("consolidation.summarisationMinMemories")) === 0
  ) {
    add(
      "warn",
      "extras",
      "Automatic summarisation has nothing to work on: consolidation.summarisationMinMemories is 0, so the candidate scan never runs.",
    );
  }

  if (val("s3.bucket") && !val("s3.region") && !val("s3.endpoint")) {
    add(
      "warn",
      "transfer",
      "An S3 bucket with neither a region nor an endpoint relies entirely on the ambient AWS environment resolving one.",
    );
  }

  if (Number(val("sleep.periodSeconds")) <= 0 && val("consolidation.enabled")) {
    add(
      "info",
      "memory",
      "The timed sleep cycle is disabled. Consolidation then runs only when the Sleep RPC is called (or the WAL trigger fires), which is a supported mode — just make sure something calls it.",
    );
  }

  if (driver !== "sqlite" && val("consolidation.enabled")) {
    add(
      "info",
      "storage",
      "This instance holds the single-consolidator lock. Every other instance against the same database must set consolidation.enabled false, or it will fail to start.",
    );
  }

  return issues;
}

/* ---------------------------------------------------------- config building */

// serviceDefault is what the service does when a key is absent from the config file - which is NOT
// the same as the value the wizard suggests. Only a handful of keys are given a default by the
// service itself (the viper.SetDefault calls in cmd/hippocampus/main.go), recorded as `svc` on the
// field; every other absent key reads as its type's zero value. That distinction is what makes a
// minimal config safe to ship: consolidation.method, aggressiveness, and unitsOfAgeInDays all look
// like sensible "defaults" here, but the service has none of its own and refuses to start when they
// are missing, so they must always be written out.
function serviceDefault(field) {
  if (Object.prototype.hasOwnProperty.call(field, "svc")) {
    return field.svc;
  }

  switch (field.type) {
    case "bool":
      return false;

    case "int":
    case "float":
      return 0;

    case "list":
      return [];

    case "map":
      return {};

    default:
      return "";
  }
}

// atServiceDefault reports whether leaving a key out of the file would give the same behaviour as
// writing the chosen value.
function atServiceDefault(key) {
  const field = FIELDS.get(key);

  return (
    !field.always &&
    JSON.stringify(val(key)) === JSON.stringify(serviceDefault(field))
  );
}

// buildConfig turns the flat, dotted key space into the nested object the service reads. In minimal
// mode only the keys that actually change the service's behaviour are emitted; otherwise every key
// the wizard knows about is, which produces a file that reads like the repo's own config.json.
// Either way the result must start the service as written - the minimal form is a smaller file, not
// a different configuration.
function buildConfig() {
  const config = {};
  const secrets = collectSecrets();

  for (const [key, field] of FIELDS) {
    if (field.when && !field.when(state)) {
      continue;
    }

    if (field.secret && !state.includeSecrets && secrets.has(key)) {
      // Emit the key so its shape is documented, but leave the value empty — it arrives from the
      // environment at runtime.
      assign(config, key, typeof field.def === "string" ? "" : field.def);

      continue;
    }

    if (state.minimal && atServiceDefault(key)) {
      continue;
    }

    assign(config, key, val(key));
  }

  return JSON.stringify(config, null, 4) + "\n";
}

function assign(target, key, entry) {
  const parts = key.split(".");
  let node = target;

  for (const part of parts.slice(0, -1)) {
    if (
      typeof node[part] !== "object" ||
      node[part] === null ||
      Array.isArray(node[part])
    ) {
      node[part] = {};
    }

    node = node[part];
  }

  node[parts[parts.length - 1]] = entry;
}

// collectSecrets returns the secret-typed keys that actually hold a value, which are the ones the
// environment file carries.
function collectSecrets() {
  const secrets = new Map();

  for (const [key, field] of FIELDS) {
    if (!field.secret || (field.when && !field.when(state))) {
      continue;
    }

    const entry = val(key);

    if (entry) {
      secrets.set(key, entry);
    }
  }

  return secrets;
}

function buildEnvFile() {
  const secrets = collectSecrets();

  const lines = [
    "# Secrets for Hippocampus, injected as environment overrides rather than committed to",
    "# config.json. Viper reads HIPPOCAMPUS_<KEY> for any key, with dots replaced by underscores,",
    "# and an environment variable beats the config file.",
    "#",
    "# Treat this file as a credential: mode 0600, out of version control, and ideally replaced by",
    "# your platform's secret store (Kubernetes Secret, systemd credentials, Docker secret).",
    "",
  ];

  if (secrets.size === 0) {
    lines.push("# No secrets are set in this configuration.");
  }

  for (const [key, entry] of secrets) {
    lines.push(`${envName(key)}=${entry}`);
  }

  return lines.join("\n") + "\n";
}

/* ------------------------------------------------------ artefact generation */

function composeFile() {
  const driver = val("storage.driver");
  const gatewayPort = Number(val("gateway.port"));
  const secrets = collectSecrets();
  const lines = [];

  lines.push(
    "# Generated by the Hippocampus configuration wizard.",
    "#",
    "#   docker compose up -d      (or: podman compose up -d)",
    "#",
    "# config.json is mounted read-only; secrets come from .env as environment overrides, so the",
    "# config file itself carries none.",
    "",
    "services:",
    "  hippocampus:",
    "    image: ghcr.io/fastbean-au/hippocampus:latest",
    "    restart: unless-stopped",
    "    ports:",
    `      - "${val("port")}:${val("port")}" # gRPC`,
  );

  if (gatewayPort > 0) {
    lines.push(
      `      - "${gatewayPort}:${gatewayPort}" # HTTP/JSON gateway, /healthz, /readyz, /ui`,
    );
  }

  lines.push(
    "    volumes:",
    "      - ./config.json:/etc/hippocampus/config.json:ro",
  );

  if (driver === "sqlite") {
    lines.push("      - hippocampus-data:/data");
  }

  if (secrets.size > 0) {
    lines.push("    env_file:", "      - .env");
  }

  if (gatewayPort > 0) {
    lines.push(
      "    healthcheck:",
      `      test: ["CMD", "wget", "-q", "--spider", "http://localhost:${gatewayPort}/readyz"]`,
      "      interval: 30s",
      "      timeout: 5s",
      "      retries: 3",
      "      start_period: 10s",
    );
  }

  const dependencies = [];

  if (driver === "postgres") {
    dependencies.push("postgres");
  }

  if (driver === "mysql") {
    dependencies.push("mysql");
  }

  if (val("opensearch.enabled")) {
    dependencies.push("opensearch");
  }

  if (val("ollama.enabled")) {
    dependencies.push("ollama");
  }

  if (dependencies.length > 0) {
    lines.push(
      "    depends_on:",
      ...dependencies.map((name) => `      - ${name}`),
    );
  }

  if (driver === "postgres") {
    lines.push(
      "",
      "  # Demo-grade database. Point storage.postgres.dsn at a managed instance for anything real,",
      "  # and delete this service.",
      "  postgres:",
      "    image: postgres:17-alpine",
      "    restart: unless-stopped",
      "    environment:",
      "      POSTGRES_DB: hippocampus",
      "      POSTGRES_USER: hippocampus",
      "      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD:?set POSTGRES_PASSWORD in .env}",
      "    volumes:",
      "      - postgres-data:/var/lib/postgresql/data",
      "    healthcheck:",
      '      test: ["CMD-SHELL", "pg_isready -U hippocampus"]',
      "      interval: 10s",
      "      timeout: 5s",
      "      retries: 5",
    );
  }

  if (driver === "mysql") {
    lines.push(
      "",
      "  # Demo-grade database (MySQL 8.0.20 or newer is required).",
      "  mysql:",
      "    image: mysql:8.4",
      "    restart: unless-stopped",
      "    environment:",
      "      MYSQL_DATABASE: hippocampus",
      "      MYSQL_USER: hippocampus",
      "      MYSQL_PASSWORD: ${MYSQL_PASSWORD:?set MYSQL_PASSWORD in .env}",
      '      MYSQL_RANDOM_ROOT_PASSWORD: "yes"',
      "    volumes:",
      "      - mysql-data:/var/lib/mysql",
      "    healthcheck:",
      '      test: ["CMD", "mysqladmin", "ping", "-h", "localhost"]',
      "      interval: 10s",
      "      timeout: 5s",
      "      retries: 5",
    );
  }

  if (val("opensearch.enabled")) {
    lines.push(
      "",
      "  # Single-node OpenSearch with the security plugin disabled — demo only. Secure it (HTTPS +",
      "  # credentials) before it holds anything real; see deploy/compose/docker-compose.opensearch-secured.yaml.",
      "  opensearch:",
      "    image: opensearchproject/opensearch:2",
      "    restart: unless-stopped",
      "    environment:",
      "      discovery.type: single-node",
      '      DISABLE_SECURITY_PLUGIN: "true"',
      '      OPENSEARCH_JAVA_OPTS: "-Xms512m -Xmx512m"',
      "    ulimits:",
      "      memlock: -1",
      "    volumes:",
      "      - opensearch-data:/usr/share/opensearch/data",
    );
  }

  if (val("ollama.enabled")) {
    lines.push(
      "",
      `  # Pull the model once the stack is up: docker compose exec ollama ollama pull ${val("ollama.model")}`,
      "  ollama:",
      "    image: ollama/ollama:latest",
      "    restart: unless-stopped",
      "    volumes:",
      "      - ollama-models:/root/.ollama",
    );
  }

  const volumes = [];

  if (driver === "sqlite") {
    volumes.push("hippocampus-data");
  }

  if (driver === "postgres") {
    volumes.push("postgres-data");
  }

  if (driver === "mysql") {
    volumes.push("mysql-data");
  }

  if (val("opensearch.enabled")) {
    volumes.push("opensearch-data");
  }

  if (val("ollama.enabled")) {
    volumes.push("ollama-models");
  }

  if (volumes.length > 0) {
    lines.push("", "volumes:", ...volumes.map((name) => `  ${name}:`));
  }

  return lines.join("\n") + "\n";
}

function kubernetesManifests() {
  const driver = val("storage.driver");
  const gatewayPort = Number(val("gateway.port"));
  const grpcPort = Number(val("port"));
  const secrets = collectSecrets();
  const sqlite = driver === "sqlite";

  const configIndented = buildConfig()
    .trimEnd()
    .split("\n")
    .map((line) => `    ${line}`)
    .join("\n");

  const parts = [];

  parts.push(
    `# Generated by the Hippocampus configuration wizard — plain manifests in the shape of the`,
    `# repo's deploy/k8s kustomization, ready for: kubectl apply -f hippocampus.yaml`,
    `#`,
    `# The ConfigMap is not content-hashed here (Kustomize does that for you), so after editing it`,
    `# roll the workload yourself: kubectl -n hippocampus rollout restart ${sqlite ? "statefulset" : "deployment"}/hippocampus`,
    `apiVersion: v1`,
    `kind: Namespace`,
    `metadata:`,
    `  name: hippocampus`,
    `---`,
    `apiVersion: v1`,
    `kind: ServiceAccount`,
    `metadata:`,
    `  name: hippocampus`,
    `  namespace: hippocampus`,
    `# The service never calls the Kubernetes API, so the pod carries no token to leak.`,
    `automountServiceAccountToken: false`,
    `---`,
    `apiVersion: v1`,
    `kind: ConfigMap`,
    `metadata:`,
    `  name: hippocampus-config`,
    `  namespace: hippocampus`,
    `data:`,
    `  config.json: |`,
    configIndented,
  );

  if (secrets.size > 0) {
    parts.push(
      `---`,
      `# Replace these placeholders before applying, or better, generate this Secret from your own`,
      `# store (Sealed Secrets, External Secrets, SOPS) rather than committing it.`,
      `apiVersion: v1`,
      `kind: Secret`,
      `metadata:`,
      `  name: hippocampus-secrets`,
      `  namespace: hippocampus`,
      `type: Opaque`,
      `stringData:`,
      ...[...secrets.keys()].map(
        (key) => `  ${envName(key)}: ${yamlString("CHANGE-ME")}`,
      ),
    );
  }

  parts.push(
    `---`,
    `apiVersion: v1`,
    `kind: Service`,
    `metadata:`,
    `  name: hippocampus`,
    `  namespace: hippocampus`,
    `spec:`,
    `  selector:`,
    `    app.kubernetes.io/name: hippocampus`,
    `  ports:`,
    `    - name: grpc`,
    `      port: ${grpcPort}`,
    `      targetPort: grpc`,
    ...(gatewayPort > 0
      ? [
          `    - name: http`,
          `      port: ${gatewayPort}`,
          `      targetPort: http`,
        ]
      : []),
  );

  const container = (consolidates) => {
    const env = [];

    for (const key of secrets.keys()) {
      env.push(
        `            - name: ${envName(key)}`,
        `              valueFrom:`,
        `                secretKeyRef:`,
        `                  name: hippocampus-secrets`,
        `                  key: ${envName(key)}`,
      );
    }

    if (!sqlite) {
      env.push(
        `            - name: HIPPOCAMPUS_CONSOLIDATION_ENABLED`,
        `              value: ${yamlString(String(consolidates))}`,
      );
    }

    return [
      `      serviceAccountName: hippocampus`,
      `      automountServiceAccountToken: false`,
      `      securityContext:`,
      `        runAsNonRoot: true`,
      `        runAsUser: 1000`,
      `        fsGroup: 1000`,
      `        seccompProfile:`,
      `          type: RuntimeDefault`,
      `      containers:`,
      `        - name: hippocampus`,
      `          image: ghcr.io/fastbean-au/hippocampus:latest`,
      `          args: ["-c", "/etc/hippocampus/config.json"]`,
      `          ports:`,
      `            - name: grpc`,
      `              containerPort: ${grpcPort}`,
      ...(gatewayPort > 0
        ? [
            `            - name: http`,
            `              containerPort: ${gatewayPort}`,
          ]
        : []),
      ...(env.length > 0 ? [`          env:`, ...env] : []),
      ...(gatewayPort > 0
        ? [
            `          livenessProbe:`,
            `            # Pure process liveness — never restarts on a transient store outage.`,
            `            httpGet:`,
            `              path: /healthz`,
            `              port: http`,
            `            initialDelaySeconds: 5`,
            `            periodSeconds: 15`,
            `          readinessProbe:`,
            `            # Database-aware: drains the pod when the store is unreachable.`,
            `            httpGet:`,
            `              path: /readyz`,
            `              port: http`,
            `            initialDelaySeconds: 5`,
            `            periodSeconds: 10`,
          ]
        : []),
      `          securityContext:`,
      `            allowPrivilegeEscalation: false`,
      `            readOnlyRootFilesystem: true`,
      `            capabilities:`,
      `              drop: ["ALL"]`,
      `          resources:`,
      `            requests:`,
      `              cpu: 100m`,
      `              memory: 128Mi`,
      `            limits:`,
      `              cpu: "1"`,
      `              memory: 512Mi`,
      `          volumeMounts:`,
      `            - name: config`,
      `              mountPath: /etc/hippocampus`,
      `              readOnly: true`,
      ...(sqlite
        ? [
            `            - name: data`,
            `              mountPath: ${val("storage.directory")}`,
          ]
        : []),
      `      volumes:`,
      `        - name: config`,
      `          configMap:`,
      `            name: hippocampus-config`,
    ];
  };

  if (sqlite) {
    parts.push(
      `---`,
      `# A StatefulSet, not a Deployment: the instance owns its volume and there must never be two`,
      `# pods writing one SQLite file. Never scale this past 1 — SQLite cannot be shared. To serve`,
      `# another tenant, apply this again into a different namespace.`,
      `apiVersion: apps/v1`,
      `kind: StatefulSet`,
      `metadata:`,
      `  name: hippocampus`,
      `  namespace: hippocampus`,
      `spec:`,
      `  serviceName: hippocampus`,
      `  replicas: 1`,
      `  selector:`,
      `    matchLabels:`,
      `      app.kubernetes.io/name: hippocampus`,
      `  template:`,
      `    metadata:`,
      `      labels:`,
      `        app.kubernetes.io/name: hippocampus`,
      `    spec:`,
      ...container(true),
      `  volumeClaimTemplates:`,
      `    - metadata:`,
      `        name: data`,
      `      spec:`,
      `        accessModes: ["ReadWriteOnce"]`,
      `        resources:`,
      `          requests:`,
      `            # Keep this comfortably above consolidation.capacityBytes.`,
      `            storage: ${suggestedVolumeSize()}`,
    );

    return parts.join("\n") + "\n";
  }

  const workload = (name, replicas, consolidates, comment) => [
    `---`,
    comment,
    `apiVersion: apps/v1`,
    `kind: Deployment`,
    `metadata:`,
    `  name: ${name}`,
    `  namespace: hippocampus`,
    `spec:`,
    `  replicas: ${replicas}`,
    ...(consolidates
      ? [
          `  strategy:`,
          `    # Never two consolidators at once, not even briefly during a rollout.`,
          `    type: Recreate`,
        ]
      : []),
    `  selector:`,
    `    matchLabels:`,
    `      app.kubernetes.io/name: hippocampus`,
    `      app.kubernetes.io/component: ${consolidates ? "consolidator" : "replica"}`,
    `  template:`,
    `    metadata:`,
    `      labels:`,
    `        app.kubernetes.io/name: hippocampus`,
    `        app.kubernetes.io/component: ${consolidates ? "consolidator" : "replica"}`,
    `    spec:`,
    ...container(consolidates),
  ];

  parts.push(
    ...workload(
      "hippocampus-consolidator",
      1,
      true,
      `# The single consolidator: it holds the database's instance lock and runs the sleep cycle.\n# Exactly one, always.`,
    ),
    ...workload(
      "hippocampus-replica",
      2,
      false,
      `# Replicas serve the full read/write RPC surface without consolidating. Scale these freely.`,
    ),
    `---`,
    `apiVersion: policy/v1`,
    `kind: PodDisruptionBudget`,
    `metadata:`,
    `  name: hippocampus-replica`,
    `  namespace: hippocampus`,
    `spec:`,
    `  minAvailable: 1`,
    `  selector:`,
    `    matchLabels:`,
    `      app.kubernetes.io/name: hippocampus`,
    `      app.kubernetes.io/component: replica`,
  );

  return parts.join("\n") + "\n";
}

function suggestedVolumeSize() {
  const capacity = Number(val("consolidation.capacityBytes"));

  if (capacity <= 0) {
    return "5Gi";
  }

  // Headroom over the capacity target for the WAL, indexes, and the free pages a vacuum has not
  // reclaimed yet.
  return `${Math.max(1, Math.ceil((capacity * 2) / 1024 ** 3))}Gi`;
}

function systemdUnit() {
  const secrets = collectSecrets();

  return [
    "# Generated by the Hippocampus configuration wizard. Install as",
    "# /etc/systemd/system/hippocampus.service, then:",
    "#   systemctl daemon-reload && systemctl enable --now hippocampus",
    "[Unit]",
    "Description=Hippocampus — memory service with intentional forgetting",
    "Documentation=https://github.com/fastbean-au/hippocampus",
    "After=network-online.target",
    "Wants=network-online.target",
    "",
    "[Service]",
    "Type=exec",
    "ExecStart=/usr/bin/hippocampus -c /etc/hippocampus/config.json",
    "",
    "# A transient, unprivileged system user; systemd creates and owns the state and configuration",
    "# directories — the native analogue of a non-root, read-only-rootfs container.",
    "DynamicUser=yes",
    "StateDirectory=hippocampus",
    "ConfigurationDirectory=hippocampus",
    `WorkingDirectory=${val("storage.directory")}`,
    ...(secrets.size > 0
      ? [
          "",
          "# Secrets as environment overrides. Keep this file mode 0600, root-owned, and out of version",
          "# control; systemd credentials (LoadCredential=) are the stronger option.",
          "EnvironmentFile=/etc/hippocampus/hippocampus.env",
        ]
      : []),
    "",
    "# Graceful shutdown: the gateway drains, then the gRPC server, each bounded by",
    `# shutdown.timeoutSeconds (${val("shutdown.timeoutSeconds")}). Leave headroom for the combined drain.`,
    "KillSignal=SIGTERM",
    `TimeoutStopSec=${Math.max(30, Number(val("shutdown.timeoutSeconds")) * 3)}`,
    "Restart=on-failure",
    "RestartSec=5",
    "",
    "# Hardening. The binary is pure Go with CGO disabled, so MemoryDenyWriteExecute is safe.",
    "NoNewPrivileges=yes",
    "ProtectSystem=strict",
    "ProtectHome=yes",
    "PrivateTmp=yes",
    "PrivateDevices=yes",
    "ProtectKernelTunables=yes",
    "ProtectKernelModules=yes",
    "ProtectKernelLogs=yes",
    "ProtectControlGroups=yes",
    "ProtectClock=yes",
    "ProtectHostname=yes",
    "ProtectProc=invisible",
    "RestrictNamespaces=yes",
    "RestrictRealtime=yes",
    "RestrictSUIDSGID=yes",
    "LockPersonality=yes",
    "MemoryDenyWriteExecute=yes",
    "RemoveIPC=yes",
    "CapabilityBoundingSet=",
    "AmbientCapabilities=",
    "RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX",
    "SystemCallFilter=@system-service",
    "SystemCallErrorNumber=EPERM",
    "SystemCallArchitectures=native",
    "UMask=0077",
    "",
    "[Install]",
    "WantedBy=multi-user.target",
    "",
  ].join("\n");
}

function launchdPlist() {
  const secrets = collectSecrets();
  const target = targetById("launchd");

  return [
    '<?xml version="1.0" encoding="UTF-8"?>',
    '<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">',
    "<!--",
    "  Generated by the Hippocampus configuration wizard. A per-user LaunchAgent: starts at login,",
    "  restarts on exit. Install to ~/Library/LaunchAgents/ and load with:",
    "    launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/au.fastbean.hippocampus.plist",
    "  Paths follow the Homebrew layout; on Apple Silicon substitute /opt/homebrew.",
    "-->",
    '<plist version="1.0">',
    "<dict>",
    "    <key>Label</key>",
    "    <string>au.fastbean.hippocampus</string>",
    "",
    "    <key>ProgramArguments</key>",
    "    <array>",
    "        <string>/usr/local/bin/hippocampus</string>",
    "        <string>-c</string>",
    `        <string>${target.configPath}</string>`,
    "    </array>",
    "",
    "    <key>RunAtLoad</key>",
    "    <true/>",
    "    <key>KeepAlive</key>",
    "    <true/>",
    "    <key>ThrottleInterval</key>",
    "    <integer>10</integer>",
    ...(secrets.size > 0
      ? [
          "",
          "    <!-- Secrets as environment overrides. A plist in the home directory is world-readable by",
          "         anything running as this user; prefer the macOS keychain for anything sensitive. -->",
          "    <key>EnvironmentVariables</key>",
          "    <dict>",
          ...[...secrets.keys()].flatMap((key) => [
            `        <key>${envName(key)}</key>`,
            "        <string>CHANGE-ME</string>",
          ]),
          "    </dict>",
        ]
      : []),
    "",
    "    <key>StandardOutPath</key>",
    "    <string>/usr/local/var/log/hippocampus/out.log</string>",
    "    <key>StandardErrorPath</key>",
    "    <string>/usr/local/var/log/hippocampus/err.log</string>",
    "</dict>",
    "</plist>",
    "",
  ].join("\n");
}

// nextSteps is the runbook for whichever target was chosen: what to put where, and the commands to
// bring it up and prove it is working.
function nextSteps() {
  const target = targetById(state.target);
  const gatewayPort = Number(val("gateway.port"));
  const secrets = collectSecrets();
  const authMethod = val("auth.method");
  const lines = [`# Deploying this configuration — ${target.label}`, ""];

  const probe =
    gatewayPort > 0
      ? `curl -s http://localhost:${gatewayPort}/healthz`
      : `# no HTTP gateway configured; use a gRPC health probe (grpcurl grpc.health.v1.Health/Check)`;

  if (state.target === "compose") {
    lines.push(
      "1. Put `config.json`, `docker-compose.yaml`" +
        (secrets.size > 0 ? ", and `.env`" : "") +
        " in one directory.",
      secrets.size > 0
        ? "2. Replace every placeholder in `.env`, then `chmod 600 .env`."
        : "2. Review `config.json`.",
      "3. Bring it up:",
      "",
      "   ```sh",
      "   docker compose up -d",
      "   docker compose logs -f hippocampus",
      "   ```",
      "",
      "4. Check it is answering:",
      "",
      "   ```sh",
      `   ${probe}`,
      "   ```",
    );
  } else if (state.target === "k8s") {
    lines.push(
      "1. Review `hippocampus.yaml` — especially the Secret placeholders and the storage class.",
      "2. Apply it:",
      "",
      "   ```sh",
      "   kubectl apply -f hippocampus.yaml",
      "   kubectl -n hippocampus rollout status " +
        (val("storage.driver") === "sqlite"
          ? "statefulset/hippocampus"
          : "deployment/hippocampus-consolidator") +
        "",
      "   ```",
      "",
      "3. Reach it locally:",
      "",
      "   ```sh",
      `   kubectl -n hippocampus port-forward svc/hippocampus ${gatewayPort || val("port")}:${gatewayPort || val("port")}`,
      "   ```",
      "",
      "The ConfigMap here is a plain manifest, so edits need a manual rollout restart. The repo's",
      "`deploy/k8s` kustomization content-hashes it instead, which rolls the workload automatically.",
    );
  } else if (state.target === "systemd") {
    lines.push(
      "1. Install the binary and the config (the `.deb`/`.rpm` on the GitHub release do both):",
      "",
      "   ```sh",
      "   sudo install -m 0755 hippocampus /usr/bin/hippocampus",
      "   sudo install -d /etc/hippocampus",
      "   sudo install -m 0644 config.json /etc/hippocampus/config.json",
      ...(secrets.size > 0
        ? [
            "   sudo install -m 0600 hippocampus.env /etc/hippocampus/hippocampus.env",
          ]
        : []),
      "   sudo install -m 0644 hippocampus.service /etc/systemd/system/hippocampus.service",
      "   ```",
      "",
      "2. Start it:",
      "",
      "   ```sh",
      "   sudo systemctl daemon-reload",
      "   sudo systemctl enable --now hippocampus",
      "   systemctl status hippocampus",
      "   journalctl -u hippocampus -f",
      "   ```",
      "",
      "3. Check it is answering:",
      "",
      "   ```sh",
      `   ${probe}`,
      "   ```",
      "",
      `\`StateDirectory=hippocampus\` gives the unit \`/var/lib/hippocampus\`, which is where`,
      `\`storage.directory\` (\`${val("storage.directory")}\`) must point.`,
    );
  } else if (state.target === "launchd") {
    lines.push(
      "1. Install the binary and config (Homebrew: `brew install fastbean-au/tap/hippocampus`):",
      "",
      "   ```sh",
      `   mkdir -p ${val("storage.directory")} /usr/local/var/log/hippocampus $(dirname ${target.configPath})`,
      `   cp config.json ${target.configPath}`,
      "   cp au.fastbean.hippocampus.plist ~/Library/LaunchAgents/",
      "   ```",
      "",
      "2. Load the agent:",
      "",
      "   ```sh",
      "   launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/au.fastbean.hippocampus.plist",
      "   launchctl print gui/$(id -u)/au.fastbean.hippocampus",
      "   ```",
      "",
      "3. Check it is answering:",
      "",
      "   ```sh",
      `   ${probe}`,
      "   ```",
    );
  } else {
    lines.push(
      "1. Save `config.json` next to the binary.",
      ...(secrets.size > 0
        ? [
            "2. Export the variables from `.env` into the shell that runs the service.",
            "3. Run it:",
          ]
        : ["2. Run it:"]),
      "",
      "   ```sh",
      `   mkdir -p ${val("storage.directory")}`,
      "   hippocampus -c config.json",
      "   ```",
      "",
      "   Or from a checkout: `go run ./cmd/hippocampus -c config.json`",
      "",
      "3. Check it is answering:",
      "",
      "   ```sh",
      `   ${probe}`,
      "   ```",
    );
  }

  lines.push("", "## After it starts", "");

  if (authMethod === "hmac") {
    lines.push(
      "Mint a token — the service binary is also the token minter:",
      "",
      "```sh",
      "hippocampus -c config.json --mint-token --client-id my-client --role writer --ttl 24h",
      "```",
      "",
    );
  }

  if (authMethod === "idp") {
    lines.push(
      "Tokens come from your identity provider. The service verifies them as RS256 against its JWKS,",
      "and maps the `" +
        val("auth.roleClaim") +
        "` claim onto the reader/writer/admin tiers — check that mapping first,",
      "since a token resolving to no known tier is denied every RPC.",
      "",
    );
  }

  if (gatewayPort > 0) {
    lines.push(
      `The HTTP surface is on :${gatewayPort} — \`/v1/...\` for the RPCs, \`/v1/openapi.json\` for the schema,`,
      "and `/ui` for the web console.",
      "",
    );
  }

  if (val("opensearch.enabled")) {
    lines.push(
      "Content search is on. If the index is being added to a store that already has memories, backfill it once:",
      "",
      "```sh",
      "hippocampus -c config.json --backfill-search",
      "```",
      "",
    );
  }

  if (val("ollama.enabled")) {
    lines.push(
      `Pull the summariser's model on the Ollama server before first use: \`ollama pull ${val("ollama.model")}\`.`,
      "",
    );
  }

  lines.push(
    "## Watch the first sleep cycle",
    "",
    "Consolidation is the whole point, so make sure you see one run before you walk away. With",
    `\`sleep.periodSeconds\` at ${val("sleep.periodSeconds")}, the first cycle lands ${describeSeconds(Number(val("sleep.periodSeconds")))} after start-up;`,
    "the log line reports what it deleted. You can also trigger one by hand with the `Sleep` RPC",
    "(`hippo sleep`, or `POST /v1/sleep`).",
    "",
    "Full reference: https://github.com/fastbean-au/hippocampus/blob/main/docs/configuration.md",
    "",
  );

  return lines.join("\n");
}

function describeSeconds(seconds) {
  if (seconds <= 0) {
    return "never (the timed cycle is disabled)";
  }

  if (seconds < 120) {
    return `${seconds} seconds`;
  }

  if (seconds < 7200) {
    return `${Math.round(seconds / 60)} minutes`;
  }

  return `${Math.round(seconds / 3600)} hours`;
}

// artefacts returns the downloadable files for the current answers, in review-tab order.
function artefacts() {
  const files = [
    {
      id: "config",
      tab: "config.json",
      filename: "config.json",
      body: buildConfig(),
      language: "json",
    },
  ];

  if (collectSecrets().size > 0) {
    files.push({
      id: "env",
      tab: "Secrets",
      filename: state.target === "systemd" ? "hippocampus.env" : ".env",
      body: buildEnvFile(),
      language: "sh",
    });
  }

  if (state.target === "compose") {
    files.push({
      id: "deploy",
      tab: "docker-compose.yaml",
      filename: "docker-compose.yaml",
      body: composeFile(),
      language: "yaml",
    });
  } else if (state.target === "k8s") {
    files.push({
      id: "deploy",
      tab: "hippocampus.yaml",
      filename: "hippocampus.yaml",
      body: kubernetesManifests(),
      language: "yaml",
    });
  } else if (state.target === "systemd") {
    files.push({
      id: "deploy",
      tab: "hippocampus.service",
      filename: "hippocampus.service",
      body: systemdUnit(),
      language: "ini",
    });
  } else if (state.target === "launchd") {
    files.push({
      id: "deploy",
      tab: "LaunchAgent",
      filename: "au.fastbean.hippocampus.plist",
      body: launchdPlist(),
      language: "xml",
    });
  }

  files.push({
    id: "steps",
    tab: "Next steps",
    filename: "DEPLOY.md",
    body: nextSteps(),
    language: "markdown",
  });

  return files;
}

/* ------------------------------------------------------------ decay preview */

// calculateValue mirrors hippocampus/sleep.go exactly. Age is in age units, not days.
function calculateValue(significance, age, method, aggressiveness) {
  switch (method) {
    case 1:
      return significance / Math.pow(age, aggressiveness);

    case 2:
      return significance / (age * Math.pow(Math.E, aggressiveness));

    case 3: {
      const factor = 1 + Math.log(aggressiveness);

      return Number.isNaN(factor) || factor <= 0
        ? Number.MAX_VALUE
        : significance / (age * factor);
    }

    case 4:
      return significance / Math.exp(age * aggressiveness);

    case 5: {
      const factor = aggressiveness * Math.log(age + Math.E);

      return Number.isNaN(factor) || factor <= 0
        ? Number.MAX_VALUE
        : significance / factor;
    }

    case 6:
      // sigmoidSteepness is 5.0 in the service.
      return significance / (1 + Math.exp(5 * (age / aggressiveness - 1)));

    default:
      return Number.MAX_VALUE;
  }
}

// lifetimeDays returns how long a memory of the given effective significance survives: the age at
// which its value first falls below the deletion threshold, floored by the minimum age and the hard
// retention window, exactly as shouldConsolidate applies them. Every curve is monotonically
// decreasing in age, so a bisection is exact to the resolution asked of it.
function lifetimeDays(significance) {
  const method = Number(val("consolidation.method"));
  const aggressiveness = Number(val("consolidation.aggressiveness"));
  const threshold = Number(val("consolidation.deletionThreshold"));
  const unit = Number(val("consolidation.unitsOfAgeInDays"));
  const floor = Math.max(
    Number(val("consolidation.minimumAgeInDays")),
    Number(val("consolidation.minimumRetentionInDays")),
  );

  if (!(unit > 0) || !(aggressiveness > 0) || !(threshold > 0)) {
    return null;
  }

  const below = (ageUnits) =>
    calculateValue(significance, ageUnits, method, aggressiveness) < threshold;

  // Bracket first: double the age until the value drops under the threshold, giving up at a
  // horizon no deployment cares about (a thousand years).
  const horizon = (365 * 1000) / unit;
  let high = 1e-6;

  while (!below(high)) {
    high *= 2;

    if (high > horizon) {
      return Infinity;
    }
  }

  let low = high / 2;

  for (let i = 0; i < 80; i += 1) {
    const mid = (low + high) / 2;

    if (below(mid)) {
      high = mid;
    } else {
      low = mid;
    }
  }

  return Math.max(high * unit, floor);
}

function formatDays(days) {
  if (days === null) {
    return "—";
  }

  if (days === Infinity) {
    return "effectively never";
  }

  if (days < 1) {
    const hours = days * 24;

    return hours < 1
      ? `${Math.round(hours * 60)} min`
      : `${hours.toFixed(1)} h`;
  }

  if (days < 400) {
    return `${days < 10 ? days.toFixed(1) : Math.round(days)} days`;
  }

  return `${(days / 365).toFixed(1)} years`;
}

function drawDecayChart(canvas) {
  const context = canvas.getContext("2d");
  const width = canvas.width;
  const height = canvas.height;
  const method = Number(val("consolidation.method"));
  const aggressiveness = Number(val("consolidation.aggressiveness"));
  const threshold = Number(val("consolidation.deletionThreshold"));
  const unit = Number(val("consolidation.unitsOfAgeInDays"));
  const significance = Number(state.previewSignificance || 100);

  const style = getComputedStyle(document.body);
  const colours = {
    grid: style.getPropertyValue("--border").trim() || "#2a303c",
    muted: style.getPropertyValue("--muted").trim() || "#8a93a6",
    accent: style.getPropertyValue("--accent").trim() || "#6ea8fe",
    bad: style.getPropertyValue("--bad").trim() || "#e0685f",
  };

  context.clearRect(0, 0, width, height);

  const pad = { left: 46, right: 12, top: 12, bottom: 28 };
  const plotWidth = width - pad.left - pad.right;
  const plotHeight = height - pad.top - pad.bottom;

  // Show a window a little past the point the memory is forgotten, so the crossing is on screen.
  const life = lifetimeDays(significance);
  const span =
    life === null || life === Infinity ? 100 : Math.max(life * 1.6, unit);
  const maxValue = Math.max(significance, threshold * 1.4);

  const x = (days) => pad.left + (days / span) * plotWidth;
  const y = (v) =>
    pad.top + plotHeight - (Math.min(v, maxValue) / maxValue) * plotHeight;

  context.strokeStyle = colours.grid;
  context.fillStyle = colours.muted;
  context.lineWidth = 1;
  context.font =
    "11px -apple-system, BlinkMacSystemFont, Segoe UI, Roboto, sans-serif";

  context.beginPath();
  context.moveTo(pad.left, pad.top);
  context.lineTo(pad.left, pad.top + plotHeight);
  context.lineTo(pad.left + plotWidth, pad.top + plotHeight);
  context.stroke();

  for (let i = 1; i <= 4; i += 1) {
    const gridY = pad.top + (plotHeight * i) / 5;

    context.beginPath();
    context.moveTo(pad.left, gridY);
    context.lineTo(pad.left + plotWidth, gridY);
    context.stroke();
  }

  // Threshold line: everything below it is forgotten by the next sleep cycle.
  context.strokeStyle = colours.bad;
  context.setLineDash([4, 4]);
  context.beginPath();
  context.moveTo(pad.left, y(threshold));
  context.lineTo(pad.left + plotWidth, y(threshold));
  context.stroke();
  context.setLineDash([]);

  context.fillStyle = colours.bad;
  context.fillText(
    "threshold",
    pad.left + 4,
    Math.max(y(threshold) - 4, pad.top + 10),
  );

  // The decay curve.
  context.strokeStyle = colours.accent;
  context.lineWidth = 2;
  context.beginPath();

  for (let pixel = 0; pixel <= plotWidth; pixel += 1) {
    const days = (pixel / plotWidth) * span;
    const ageUnits = days / unit;
    const current =
      ageUnits <= 0
        ? significance
        : Math.min(
            calculateValue(significance, ageUnits, method, aggressiveness),
            significance,
          );

    if (pixel === 0) {
      context.moveTo(x(days), y(current));
    } else {
      context.lineTo(x(days), y(current));
    }
  }

  context.stroke();

  context.fillStyle = colours.muted;
  context.fillText("0", pad.left - 4, height - 10);
  context.fillText(
    `${formatDays(span)}`,
    pad.left + plotWidth - 46,
    height - 10,
  );
  context.fillText("age", pad.left + plotWidth / 2 - 10, height - 10);

  context.save();
  context.translate(12, pad.top + plotHeight / 2 + 20);
  context.rotate(-Math.PI / 2);
  context.fillText("value", 0, 0);
  context.restore();
}

/* ---------------------------------------------------------------- rendering */

function renderNav() {
  const nav = $("#step-nav");
  const issues = validate();

  nav.replaceChildren(
    ...STEPS.map((step, index) => {
      const failing = issues.some(
        (issue) => issue.step === step.id && issue.level === "error",
      );

      return el(
        "li",
        {},
        el(
          "button",
          {
            type: "button",
            class: index === state.step ? "active" : "",
            onclick: () => {
              state.step = index;
              render();
            },
          },
          el("span", { class: "num", text: String(index + 1) }),
          el("span", { text: step.title }),
          failing
            ? el("span", { class: "bad-dot", title: "This step has a problem" })
            : null,
        ),
      );
    }),
  );

  const errors = issues.filter((issue) => issue.level === "error").length;
  const warnings = issues.filter((issue) => issue.level === "warn").length;
  const pill = $("#issue-pill");

  pill.className =
    "pill" + (errors > 0 ? " error" : warnings === 0 ? " ok" : "");
  pill.textContent =
    errors > 0
      ? `${errors} problem${errors === 1 ? "" : "s"}`
      : warnings > 0
        ? `${warnings} warning${warnings === 1 ? "" : "s"}`
        : "No problems";
}

function renderField(field) {
  const id = `field-${field.key.replace(/\./g, "-")}`;
  const current = val(field.key);

  const label = el(
    "label",
    { for: id },
    field.label,
    el("span", { class: "key", text: field.key }),
    changed(field.key) ? el("span", { class: "key", text: "• changed" }) : null,
  );

  // help may be a function of the current state, so a field whose advice depends on another
  // answer — the search cards, where the driver decides what is even available — can say the
  // right thing rather than hedging across every case.
  const helpText =
    typeof field.help === "function" ? field.help(state) : field.help;

  const help = helpText ? el("div", { class: "help", text: helpText }) : null;

  if (field.type === "bool") {
    return el(
      "div",
      { class: "field inline" },
      el("input", {
        id,
        type: "checkbox",
        checked: Boolean(current),
        onchange: (event) => {
          setValue(field.key, event.target.checked);
          render();
        },
      }),
      el("div", {}, label, help),
    );
  }

  let input;

  if (field.type === "select") {
    input = el(
      "select",
      {
        id,
        onchange: (event) => {
          const chosen = field.options.find(
            ([option]) => String(option) === event.target.value,
          );
          setValue(field.key, chosen ? chosen[0] : event.target.value);
          render();
        },
      },
      ...field.options.map(([option, text]) =>
        el(
          "option",
          {
            value: String(option),
            selected: String(option) === String(current),
          },
          text,
        ),
      ),
    );
  } else if (field.type === "list") {
    input = el("textarea", {
      id,
      placeholder: field.placeholder || "",
      onchange: (event) => {
        setValue(
          field.key,
          event.target.value
            .split("\n")
            .map((line) => line.trim())
            .filter(Boolean),
        );
        render();
      },
    });
    input.value = (current || []).join("\n");
  } else if (field.type === "map") {
    input = el("textarea", {
      id,
      placeholder: field.placeholder || "role=tier",
      onchange: (event) => {
        const mapping = {};

        for (const line of event.target.value.split("\n")) {
          const [name, tier] = line.split("=");

          if (name && name.trim() && tier && tier.trim()) {
            mapping[name.trim()] = tier.trim();
          }
        }

        setValue(field.key, mapping);
        render();
      },
    });
    input.value = Object.entries(current || {})
      .map(([name, tier]) => `${name}=${tier}`)
      .join("\n");
  } else {
    const numeric = field.type === "int" || field.type === "float";

    input = el("input", {
      id,
      type: numeric ? "number" : "text",
      step: field.type === "float" ? "any" : field.type === "int" ? "1" : false,
      placeholder: field.placeholder || (numeric ? String(field.def) : ""),
      oninput: (event) => {
        const raw = event.target.value;
        setValue(
          field.key,
          numeric ? (raw === "" ? field.def : Number(raw)) : raw,
        );
        renderNav();
        refreshPreview();
      },
      onchange: () => render(),
    });
    input.value =
      current === undefined || current === null ? "" : String(current);
  }

  const wrapper = el("div", { class: "field" }, label, input, help);

  if (field.generate) {
    wrapper.insertBefore(
      el(
        "div",
        { class: "output-actions" },
        el(
          "button",
          {
            class: "btn ghost small",
            type: "button",
            onclick: () => {
              setValue(field.key, randomSecret(field.generate));
              render();
              toast(
                `Generated a ${field.generate}-byte secret — it is kept in this tab only`,
              );
            },
          },
          `Generate ${field.generate} random bytes`,
        ),
      ),
      help,
    );
  }

  return wrapper;
}

function renderCards(step) {
  const cards = [];

  for (const card of step.cards) {
    if (card.when && !card.when(state)) {
      continue;
    }

    const visible = card.fields.filter(
      (field) => !field.when || field.when(state),
    );

    if (visible.length === 0) {
      continue;
    }

    cards.push(
      el(
        "section",
        { class: "card" },
        el("h2", { text: card.title }),
        card.blurb ? el("p", { class: "blurb", text: card.blurb }) : null,
        el("div", { class: "grid" }, ...visible.map(renderField)),
      ),
    );
  }

  return cards;
}

function renderIssues(stepId) {
  const issues = validate().filter((issue) => issue.step === stepId);

  if (issues.length === 0) {
    return null;
  }

  return el(
    "section",
    { class: "card" },
    el("h2", { text: "Worth knowing" }),
    ...issues.map((issue) =>
      el("div", { class: `issue ${issue.level}` }, issue.text),
    ),
  );
}

function renderStart() {
  const presetTiles = el(
    "div",
    { class: "tiles" },
    ...PRESETS.map((preset) =>
      el(
        "button",
        {
          type: "button",
          class: "tile" + (state.preset === preset.id ? " selected" : ""),
          onclick: () => {
            applyPreset(preset);
            render();
            toast(`Loaded the "${preset.label}" starting point`);
          },
        },
        el("strong", { text: preset.label }),
        el("span", { text: preset.blurb }),
      ),
    ),
  );

  const targetTiles = el(
    "div",
    { class: "tiles" },
    ...TARGETS.map((target) =>
      el(
        "button",
        {
          type: "button",
          class: "tile" + (state.target === target.id ? " selected" : ""),
          onclick: () => {
            state.target = target.id;
            applyTargetPaths(target.id);
            render();
          },
        },
        el("strong", { text: target.label }),
        el("span", { text: target.blurb }),
      ),
    ),
  );

  return [
    el(
      "section",
      { class: "card" },
      el("h2", { text: "What are you building?" }),
      el("p", {
        class: "blurb",
        text: "Pick a starting point, then change whatever you like. Every question has a sensible default, so you can also skip straight to the last step and take the defaults.",
      }),
      presetTiles,
      el("p", {
        class: "note",
        text: "Choosing a starting point replaces the answers you have given so far.",
      }),
    ),
    el(
      "section",
      { class: "card" },
      el("h2", { text: "Where will it run?" }),
      el("p", {
        class: "blurb",
        text: "This decides which deployment artefacts are generated at the end, and where on disk the service expects its data and configuration.",
      }),
      targetTiles,
    ),
    el(
      "section",
      { class: "card" },
      el("h2", { text: "Already have a config.json?" }),
      el("p", {
        class: "blurb",
        text: "Import it to review, validate, and re-generate the deployment artefacts around it. Unknown keys are ignored, and the file never leaves your browser.",
      }),
      el(
        "div",
        { class: "output-actions" },
        el(
          "button",
          {
            class: "btn ghost",
            type: "button",
            onclick: () => $("#import-file").click(),
          },
          "Choose a file…",
        ),
      ),
    ),
  ];
}

function renderPreview() {
  const significances = [10, 100, 1000, 10000];
  const canvas = el("canvas", { width: 560, height: 260 });

  const rows = significances.map((significance) =>
    el(
      "tr",
      {},
      el("td", { class: "num", text: significance.toLocaleString() }),
      el("td", { class: "num", text: formatDays(lifetimeDays(significance)) }),
    ),
  );

  const section = el(
    "section",
    { class: "card" },
    el("h2", { text: "What this forgets, and when" }),
    el("p", {
      class: "blurb",
      text: "Effective significance is the memory's own plus its event's, plus the damped link contributions and the recall boost — so a recalled or well-linked memory sits higher on this table than an isolated, never-recalled one, and its clock restarts at each recall. Link totals are damped (weight x ln(1 + total)) before they count, so being connected raises a memory's standing without ever making it unforgettable.",
    }),
    el(
      "div",
      { class: "preview" },
      canvas,
      el(
        "div",
        { class: "lifetimes" },
        el(
          "div",
          { class: "field" },
          el(
            "label",
            { for: "preview-significance" },
            "Charted effective significance",
          ),
          el("input", {
            id: "preview-significance",
            type: "number",
            value: String(state.previewSignificance || 100),
            oninput: (event) => {
              state.previewSignificance = Number(event.target.value) || 0;
              refreshPreview();
            },
          }),
        ),
        el(
          "table",
          { class: "lifetimes-table" },
          el(
            "thead",
            {},
            el(
              "tr",
              {},
              el("th", { text: "Effective significance" }),
              el("th", { text: "Forgotten after" }),
            ),
          ),
          el("tbody", {}, ...rows),
        ),
        el("p", {
          class: "note",
          text: "Lifetimes assume no capacity pressure. As the store fills, the threshold is scaled up and everything is forgotten sooner; past the capacity target, eviction deletes in ascending value order regardless.",
        }),
      ),
    ),
  );

  // The canvas needs to be in the document before it can be drawn on.
  queueMicrotask(() => drawDecayChart(canvas));

  return section;
}

function refreshPreview() {
  const canvas = document.querySelector(".preview canvas");

  if (canvas) {
    drawDecayChart(canvas);
  }

  const rows = document.querySelectorAll(".lifetimes-table tbody tr");
  const significances = [10, 100, 1000, 10000];

  rows.forEach((row, index) => {
    row.children[1].textContent = formatDays(
      lifetimeDays(significances[index]),
    );
  });
}

function renderReview() {
  const files = artefacts();
  const issues = validate();
  const errors = issues.filter((issue) => issue.level === "error");
  const active = files.find((file) => file.id === state.reviewTab) || files[0];

  const summary = el(
    "section",
    { class: "card" },
    el("h2", { text: "Summary" }),
    el(
      "div",
      { class: "grid" },
      ...summaryItems().map(([term, description]) =>
        el(
          "div",
          { class: "field" },
          el("label", {}, term),
          el("div", { class: "help", text: description }),
        ),
      ),
    ),
  );

  const checks = el(
    "section",
    { class: "card" },
    el("h2", {
      text: errors.length > 0 ? "Fix these before deploying" : "Checks",
    }),
    ...(issues.length === 0
      ? [
          el(
            "div",
            { class: "issue info" },
            "Nothing to flag. This configuration starts as written.",
          ),
        ]
      : issues.map((issue) =>
          el(
            "div",
            { class: `issue ${issue.level}` },
            el("strong", {
              text: `${STEPS.find((step) => step.id === issue.step).title}: `,
            }),
            issue.text,
          ),
        )),
  );

  const tabs = el(
    "div",
    { class: "tabs" },
    ...files.map((file) =>
      el(
        "button",
        {
          type: "button",
          class: file.id === active.id ? "active" : "",
          onclick: () => {
            state.reviewTab = file.id;
            render();
          },
        },
        file.tab,
      ),
    ),
  );

  const output = el(
    "section",
    { class: "card" },
    tabs,
    el(
      "div",
      { class: "output-actions" },
      el(
        "button",
        {
          class: "btn",
          type: "button",
          onclick: () => download(active.filename, active.body),
        },
        `Download ${active.filename}`,
      ),
      el(
        "button",
        {
          class: "btn ghost",
          type: "button",
          onclick: () => copy(active.body),
        },
        "Copy",
      ),
      el(
        "button",
        {
          class: "btn ghost",
          type: "button",
          onclick: () => {
            files.forEach((file, index) =>
              setTimeout(() => download(file.filename, file.body), index * 250),
            );
            toast(`Downloading ${files.length} files`);
          },
        },
        "Download everything",
      ),
      el(
        "label",
        { class: "note" },
        el("input", {
          type: "checkbox",
          checked: state.minimal,
          onchange: (event) => {
            state.minimal = event.target.checked;
            persist();
            render();
          },
        }),
        " only keys the service does not already default",
      ),
      el(
        "label",
        { class: "note" },
        el("input", {
          type: "checkbox",
          checked: state.includeSecrets,
          onchange: (event) => {
            state.includeSecrets = event.target.checked;
            render();
          },
        }),
        " write secrets into config.json",
      ),
    ),
    el("pre", { class: "output" }, active.body),
  );

  return [summary, checks, output];
}

function summaryItems() {
  const driver = val("storage.driver");
  const gatewayPort = Number(val("gateway.port"));
  const capacityBytes = Number(val("consolidation.capacityBytes"));

  return [
    ["Deployment", targetById(state.target).label],
    [
      "Storage",
      driver === "sqlite"
        ? `sqlite at ${val("storage.directory")}`
        : `${driver}, ${val("consolidation.enabled") ? "this instance consolidates" : "replica (no consolidation)"}`,
    ],
    [
      "Ports",
      `gRPC ${val("port")}${gatewayPort > 0 ? `, HTTP ${gatewayPort}` : ", no HTTP gateway"}`,
    ],
    [
      "Security",
      `${val("auth.method")} auth, TLS ${val("tls.enabled") ? "on" : "off"}`,
    ],
    [
      "Forgetting",
      `method ${val("consolidation.method")}, a=${val("consolidation.aggressiveness")}, threshold ${val("consolidation.deletionThreshold")}, every ${describeSeconds(Number(val("sleep.periodSeconds")))}`,
    ],
    [
      "Capacity",
      `${Number(val("consolidation.capacityMemories")).toLocaleString()} memories, ${formatBytes(capacityBytes)}`,
    ],
    [
      "Retention floor",
      Number(val("consolidation.minimumRetentionInDays")) > 0
        ? `${val("consolidation.minimumRetentionInDays")} days, absolute`
        : "none",
    ],
    [
      "Extras",
      [
        val("opensearch.enabled") ? "content search" : null,
        val("ollama.enabled") ? "embedded summariser" : null,
        val("observability.metrics.enabled") ||
        val("observability.tracing.enabled")
          ? "OpenTelemetry"
          : null,
        val("transfer.targetAddress") ? "transfer" : null,
        val("s3.bucket") ? "S3 archive" : null,
      ]
        .filter(Boolean)
        .join(", ") || "none",
    ],
    ["Keys changed", `${Object.keys(state.values).length} of ${FIELDS.size}`],
  ];
}

function render() {
  const step = STEPS[state.step];
  const panel = $("#panel");
  const children = [];

  if (step.custom === "start") {
    children.push(...renderStart());
  }

  children.push(...renderCards(step));

  if (step.custom === "preview") {
    children.push(renderPreview());
  }

  if (step.custom === "review") {
    children.push(...renderReview());
  } else {
    const issues = renderIssues(step.id);

    if (issues) {
      children.push(issues);
    }
  }

  panel.replaceChildren(...children);

  $("#prev-step").disabled = state.step === 0;
  $("#next-step").textContent =
    state.step === STEPS.length - 1 ? "Done" : "Next";
  $("#next-step").disabled = state.step === STEPS.length - 1;

  renderNav();
  window.scrollTo({ top: 0, behavior: "instant" });
}

/* ------------------------------------------------------------------- import */

// importConfig flattens a config file back into the wizard's key space. Unknown keys are dropped
// rather than carried through: the wizard only claims to understand what it can also validate.
function importConfig(text) {
  let parsed = null;

  try {
    parsed = JSON.parse(text);
  } catch (error) {
    toast(`That is not valid JSON: ${error.message}`);

    return;
  }

  const flat = new Map();
  const walk = (node, prefix) => {
    for (const [name, entry] of Object.entries(node || {})) {
      const key = prefix ? `${prefix}.${name}` : name;

      if (
        entry !== null &&
        typeof entry === "object" &&
        !Array.isArray(entry) &&
        !FIELDS.has(key)
      ) {
        walk(entry, key);
      } else {
        flat.set(key, entry);
      }
    }
  };

  walk(parsed, "");

  state.values = {};
  state.preset = "";

  let known = 0;
  let unknown = 0;

  for (const [key, entry] of flat) {
    if (!FIELDS.has(key)) {
      unknown += 1;

      continue;
    }

    known += 1;
    setValue(key, entry);
  }

  render();
  toast(
    `Imported ${known} setting${known === 1 ? "" : "s"}${unknown > 0 ? `, ignored ${unknown} the wizard does not manage` : ""}`,
  );
}

/* --------------------------------------------------------------------- boot */

function init() {
  restore();

  $("#next-step").addEventListener("click", () => {
    state.step = Math.min(state.step + 1, STEPS.length - 1);
    render();
  });

  $("#prev-step").addEventListener("click", () => {
    state.step = Math.max(state.step - 1, 0);
    render();
  });

  $("#import-config").addEventListener("click", () =>
    $("#import-file").click(),
  );

  $("#import-file").addEventListener("change", (event) => {
    const file = event.target.files && event.target.files[0];

    if (!file) {
      return;
    }

    file.text().then(importConfig);
    event.target.value = "";
  });

  $("#reset-all").addEventListener("click", () => {
    state.values = {};
    state.preset = "";
    state.step = 0;
    persist();
    render();
    toast("Cleared — back to the service defaults");
  });

  render();
}

init();
