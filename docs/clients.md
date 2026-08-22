# Clients in other languages

Go stubs ship in the repository (`contract/`), so a Go program imports
`contract.NewHippocampusClient` and is done. **Every other language generates its own client** —
nothing is published to PyPI, npm, Maven or NuGet yet. This page is the generation recipe: how to
turn the contract into a working client in a few minutes, and the handful of things about
Hippocampus's API that are worth knowing before you do.

If you want access without writing a client at all, the [`hippo` CLI](cli.md) exposes the full RPC
surface from a shell and the [MCP bridge](mcp.md) exposes it to an LLM host.

## Which surface to generate against

Every RPC is served twice — natively over gRPC and as JSON over the `/v1` gateway — by the same
in-process server, so the two are equivalent in behaviour and differ only in ergonomics.

|                | **gRPC**                                            | **`/v1` JSON gateway**                                      |
| -------------- | --------------------------------------------------- | ----------------------------------------------------------- |
| Generated from | `contract/hippocampus.proto`                        | `/v1/openapi.json` (or no generation at all)                |
| Best for       | services and agents in a language with a gRPC stack | browsers, shells, languages without one                     |
| Transport      | HTTP/2, binary                                      | HTTP/1.1 or 2, JSON                                         |
| Enabled by     | always (`port`)                                     | `gateway.port` — **off unless configured**                  |
| Types          | generated messages, exact                           | JSON, with the encoding notes [below](#json-encoding-notes) |

Every RPC is unary — there are no streaming calls — so nothing is lost by choosing the JSON gateway.
**A browser must use the gateway**: no gRPC-web endpoint is served, and there is no proxy in the
deployment to add one.

## The contract

- File: [`contract/hippocampus.proto`](../contract/hippocampus.proto).
- Proto package `hippocampus.v1`; service `Hippocampus`; every method is
  `/hippocampus.v1.Hippocampus/<Method>`.
- It imports two annotation files, both vendored beside it: `google/api/annotations.proto` (under
  `contract/google/api/`), which describes the gateway's routes, and
  `protoc-gen-openapiv2/options/annotations.proto` (under `contract/protoc-gen-openapiv2/options/`),
  which carries the OpenAPI document's `securityDefinitions`. Point the include path at `contract/`
  and both resolve. **Neither affects the wire format** — they are annotations consumed by the
  gateway and OpenAPI generators — so a generator that ignores them still produces a correct client,
  and a toolchain that cannot resolve them at all can strip the two `import` lines and the
  `openapiv2_swagger` option without changing a single message or method.

### Discovering it from a running instance

The server registers the gRPC **reflection** service when `reflection.enabled` is set, and by
default whenever `auth.method` is `none` — so `grpcurl` and every gRPC GUI work against a local or
demo instance with nothing handed to them:

```sh
grpcurl -plaintext localhost:50051 list
grpcurl -plaintext localhost:50051 describe hippocampus.v1.Hippocampus
```

Reflection is a **streaming** RPC, so it reaches neither the auth interceptor nor either rate
limiter — it is the one thing on the gRPC port that is both unauthenticated and unthrottled, which is
why it defaults **off** on an instance configured with `auth.method` `hmac` or `idp`. That is a
surface judgement and not a secrecy one: this contract is published with the source, so the schema
was never confidential. `reflection.enabled` overrides the derivation either way, and the choice is
named in the startup log. See [Server reflection](configuration.md#server-reflection).

Where it is off, hand the tool the `.proto`, or a descriptor set built from it:

```sh
buf build contract --as-file-descriptor-set -o hippocampus.binpb
grpcurl -protoset hippocampus.binpb -plaintext localhost:50051 list
```

For a browser form rather than a shell, [`grpcui`](https://github.com/fullstorydev/grpcui) builds one
from the same reflection response, and takes the same `-proto`/`-protoset` fallbacks where reflection
is off:

```sh
grpcui -plaintext localhost:50051
```

It is a separate tool you install yourself — nothing in this repository depends on it, and the
service does not host it. Against an instance with authentication on, pass the token the way you
would to `grpcurl` (`-H 'authorization: Bearer <token>'`), and note that `grpcui` serves its own web
UI with **no authentication of its own**: whoever can reach the port it binds can invoke every RPC
your token is authorised for, `Purge` included. Bind it to loopback and treat it as a development
tool.

## Recipe 1 — `buf`, with nothing else to install

[`buf`](https://buf.build/docs/installation) fetches the code generators it needs, so this works
without `protoc` or any locally installed plugin. Write a `buf.gen.yaml`:

```yaml
version: v2
plugins:
  - remote: buf.build/protocolbuffers/python
    out: gen
  - remote: buf.build/grpc/python
    out: gen
```

```sh
buf generate path/to/hippocampus/contract --path path/to/hippocampus/contract/hippocampus.proto
```

`--path` restricts generation to the service's own file, so the vendored annotation protos are
resolved but not regenerated — get the googleapis ones from your language's package instead
(`pip install googleapis-common-protos`, and so on), and the openapiv2 options need no runtime
counterpart at all, being generator input rather than anything a client calls. Swap the two remote
plugins for your language's from the [plugin directory](https://buf.build/plugins) —
`buf.build/grpc/java`,
`buf.build/grpc/csharp`, `buf.build/community/neoeinstein-tonic`, and the rest.

## Recipe 2 — the language's own toolchain

### Python

```sh
pip install grpcio grpcio-tools googleapis-common-protos
python -m grpc_tools.protoc -I path/to/hippocampus/contract \
  --python_out=. --pyi_out=. --grpc_python_out=. hippocampus.proto
```

That writes `hippocampus_pb2.py`, `hippocampus_pb2.pyi` and `hippocampus_pb2_grpc.py`. The
`googleapis-common-protos` package supplies `google.api.annotations_pb2`, which the generated module
imports at runtime.

```python
import grpc
import hippocampus_pb2 as pb
import hippocampus_pb2_grpc as rpc

stub = rpc.HippocampusStub(grpc.insecure_channel("localhost:50051"))

who = stub.WhoAmI(pb.EmptyRequest())
print(who.role, [pb.SearchMode.Name(m) for m in who.search_modes])

stored = stub.StoreMemory(pb.Memory(significance=50, body="the deploy at 14:03 rolled back cleanly"))
if stored.rejected:                                   # below memory.minimumSignificance - not an error
    print("dropped as insignificant")

listed = stub.GetMemories(pb.GetMemoriesRequest(limit=10), timeout=5)
recalled = stub.RecallMemories(pb.RecallMemoriesRequest(ids=[stored.id]))   # reinforces them
found = stub.SearchMemories(pb.SearchMemoriesRequest(query="deploy", limit=5))
```

### Node.js and TypeScript

`@grpc/proto-loader` reads the `.proto` at startup, so there is **no code generation step at all**:

```sh
npm install @grpc/grpc-js @grpc/proto-loader
```

```js
import * as grpc from "@grpc/grpc-js";
import * as protoLoader from "@grpc/proto-loader";

const def = protoLoader.loadSync("hippocampus.proto", {
  includeDirs: ["path/to/hippocampus/contract"],
  longs: String, // int64 timestamps do not fit in a JS number
  enums: String,
  defaults: true,
});
const { Hippocampus } = grpc.loadPackageDefinition(def).hippocampus.v1;
const client = new Hippocampus(
  "localhost:50051",
  grpc.credentials.createInsecure(),
);

const call = (method, req) =>
  new Promise((res, rej) =>
    client[method](req, (e, r) => (e ? rej(e) : res(r))),
  );

console.log(await call("WhoAmI", {}));
const stored = await call("StoreMemory", { significance: 50, body: "hello" });
const listed = await call("GetMemories", { limit: 10 });
```

For static TypeScript types instead, generate them with `buf` (Recipe 1) using
`buf.build/community/stephenh-ts-proto`, or take the types from the OpenAPI document
([below](#typed-clients-from-the-openapi-document)).

### Other languages

The service is ordinary gRPC, so each language's standard toolchain applies unchanged — point it at
`contract/hippocampus.proto` with `contract/` on the include path:

| Language               | Toolchain                                                                                            |
| ---------------------- | ---------------------------------------------------------------------------------------------------- |
| Java / Kotlin          | `protoc-gen-grpc-java`, or the Gradle/Maven protobuf plugin                                          |
| C#                     | the `Grpc.Tools` NuGet package                                                                       |
| Rust                   | `tonic-build` in a `build.rs`                                                                        |
| Ruby                   | `grpc-tools` (`grpc_tools_ruby_protoc`)                                                              |
| Go (outside this repo) | `protoc-gen-go` + `protoc-gen-go-grpc`, or just import `github.com/fastbean-au/hippocampus/contract` |

## Authentication and TLS

Both are off by default; a client needs this section only when the deployment enables them (see
[Authentication](configuration.md#authentication) and [TLS](configuration.md#tls)). Mint a token
with:

```sh
hippocampus --mint-token --client-id my-client --role writer --ttl 24h -c config.json
```

The token goes in an `authorization: Bearer <token>` header — request metadata on gRPC, an ordinary
header on the gateway. Either attach it per call, or bind it to the channel so every call carries
it:

```python
with open("ca.pem", "rb") as f:                                 # only for a private/self-signed CA
    channel_creds = grpc.ssl_channel_credentials(root_certificates=f.read())

# Per call:
stub.WhoAmI(pb.EmptyRequest(), metadata=(("authorization", f"Bearer {token}"),))

# Or bound to the channel (gRPC permits call credentials on a secure channel only):
creds = grpc.composite_channel_credentials(channel_creds, grpc.access_token_call_credentials(token))
stub = rpc.HippocampusStub(grpc.secure_channel("localhost:50051", creds))
```

```js
const ssl = grpc.credentials.createSsl(readFileSync("ca.pem")); // omit the argument to use the system roots
const bearer = grpc.credentials.createFromMetadataGenerator((_, cb) => {
  const md = new grpc.Metadata();
  md.set("authorization", `Bearer ${token}`);
  cb(null, md);
});
const client = new Hippocampus(
  "localhost:50051",
  grpc.credentials.combineChannelCredentials(ssl, bearer),
);
```

A missing or invalid token is `UNAUTHENTICATED` (`401`); a valid token whose
[role](configuration.md#authorisation) is too low for the RPC is `PERMISSION_DENIED` (`403`). Call
`WhoAmI` to learn the tier a token actually resolves to rather than discovering it from a rejection.

## The JSON gateway

Base path `/v1`, JSON bodies, field names lowerCamelCase. The full mapping of RPC to method and path
is in [Configurability](configuration.md#configurability), and the OpenAPI document describes it
live.

```python
import requests

s = requests.Session()
s.headers["Authorization"] = f"Bearer {token}"     # only when auth is enabled
s.verify = "ca.pem"                                # only for a private CA

stored = s.post("https://host:8080/v1/memories",
                json={"significance": 50, "body": "the deploy rolled back cleanly"}).json()
memories = s.get("https://host:8080/v1/memories", params={"limit": 10}).json()["memories"]
```

### JSON encoding notes

These follow from protojson and catch every new client at least once:

- **`int64` fields are JSON strings**, not numbers — `"timeStamp": "1786008996898834000"`. That
  includes every timestamp (all of which are **UnixNano**) and `timestampMin`/`timestampMax`.
- **Enums are their names**: `"isBinary": "FALSE"`, `"mode": "SEARCH_MODE_HYBRID"`. Integers are
  accepted on input.
- **`isBinary` is a three-valued enum**, not a boolean: `UNSPECIFIED` / `FALSE` / `TRUE`.
- Unset fields are omitted from request bodies and default on the server; a zero and an absent value
  are indistinguishable, which is why `significance: 0` means _unranked_ rather than _rank zero_.
- On GET and DELETE routes the non-path fields are query parameters
  (`/v1/memories?limit=10&group=deploys`), not a body.

### Two error shapes

Rejections from the auth and purge middleware are terse, because they happen before the gateway's
own handler runs — this is the body of a `401`, a `403`, and the `503` returned while a purge is in
progress:

```json
{ "error": "unauthorized" }
```

Everything else is the grpc-gateway status body, carrying the gRPC code the service returned — here
`InvalidArgument`, served as a `400`:

```json
{ "code": 3, "message": "memory not valid - no body provided", "details": [] }
```

Treat the HTTP status as authoritative and read `message` when present. Note that the code→status
mapping is lossy in the direction clients see it: `400` stands for `InvalidArgument`,
`FailedPrecondition` and `OutOfRange` alike.

### Typed clients from the OpenAPI document

The document is served at `/v1/openapi.json` and is byte-identical to
[`contract/hippocampus.swagger.json`](../contract/hippocampus.swagger.json) in the repository. It is
served **without a token even when authentication is enabled**, so a tool can fetch it from a live
instance directly; `gateway.openapi.enabled: false` removes the route for a deployment that would
rather serve nothing there, and the repository copy still works. See
[The OpenAPI document](configuration.md#the-openapi-document).

It is **Swagger 2.0** (protoc-gen-openapiv2's output), which
[openapi-generator](https://openapi-generator.tech/) consumes directly:

```sh
openapi-generator-cli generate -i hippocampus.swagger.json -g python -o ./hippocampus-client
```

Generators that require OpenAPI 3 need one conversion step first:

```sh
npx swagger2openapi hippocampus.swagger.json -o openapi3.json
npx openapi-typescript openapi3.json -o hippocampus.d.ts     # TypeScript types for every route
```

### A browser API explorer (Swagger UI)

[Swagger UI](https://github.com/swagger-api/swagger-ui) turns the same document into a browser form
that can call the API. The compose stack ships it behind a profile:

```sh
docker compose --profile swagger up --build     # then browse to http://localhost:8082
```

To run it against an instance of your own, two things have to line up.

**Point it at the running gateway, not at a copy of the file.** The generated document declares no
`host`, and Swagger UI resolves each operation against the origin that served the *document* — so a
spec loaded from the container's own filesystem makes every "Try it out" call address the container,
which answers `404`. Fetching it over HTTP from the gateway is what makes the interactive half work:

```sh
docker run --rm -p 8082:8080 \
  -e SWAGGER_JSON_URL=http://localhost:8080/v1/openapi.json \
  swaggerapi/swagger-ui
```

That URL is resolved by the **browser**, so it must be an address the browser can reach — not a
container-network hostname.

**Allow the origin.** Swagger UI is served from a different origin than the gateway, so both the spec
fetch and every subsequent call are cross-origin. Add its origin to
[`gateway.corsOrigins`](configuration.md#cross-origin-requests-cors):

```json
"gateway": {
    "corsOrigins": ["http://localhost:8082"]
}
```

Against an instance with authentication on, use the **Authorize** button and enter
`Bearer <token>` — the document declares a bearer `securityDefinition`, so the value is sent as the
`Authorization` header on every call. Swagger UI has no authentication of its own, so bind it to
loopback and treat it as a development tool, exactly as with `grpcui` above.

## Behaviour worth knowing before you generate

Hippocampus forgets on purpose, so a few responses mean something other than what a generated stub
suggests:

- **Insignificance is not an error.** `StoreMemory` and `StoreEvent` return `rejected: true` with an
  empty id when the item is below `memory.minimumSignificance` / `event.minimumSignificance`. The
  call succeeded; the memory was quietly dropped. Check `rejected`, not just the status.
- **Memories disappear.** A consolidation cycle deletes them, so an id held by a client can stop
  resolving at any time. That is the product, not a fault — treat a missing id as expected.
- **Recall is a write.** `RecallMemories` (and `SearchMemories` with `reinforce`) resets the decay
  clock and raises effective significance. Do not use it as a plain read; `GetMemories` is the read.
- **Read-only fields are ignored on write**: `timeRecalled`, `recallCount`, `isSummary`, and
  `isBinary` after creation.
- **`significance: 0` on `UpdateMemory` leaves the existing significance unchanged** — it cannot
  reset a memory to unranked.
- **`group` is a label unless the deployment scopes tokens to it.** By default it only narrows
  queries: it grants nothing and hides nothing. But a deployment using
  [group scoping](configuration.md#group-scoping) binds a token to particular labels, and then three
  things change for your client, none of them visible in the schema:
  - **`NotFound` may mean "not yours".** An out-of-scope record is reported exactly as a nonexistent
    one, deliberately, so that the error cannot be used to prove a record exists. Do not treat
    `NotFound` as proof that an id was never valid.
  - **Your writes are stamped for you.** Omit `group` and the server fills in the token's own; send
    a group the token does not hold and the write is refused with `PermissionDenied`. A client that
    hard-codes a group will break when handed a scoped token — prefer to omit it.
  - **Listings and `totalCount` are narrowed**, and filtering by a group outside the scope returns
    an empty page rather than an error. `Purge`, `Sleep` and `PreviewConsolidation` are refused
    outright.
- **Feature-detect with `WhoAmI`** rather than probing: `searchModes` reports which
  `SearchMemories` modes this deployment can actually serve (an empty list means content search is
  unavailable entirely), `role` reports the caller's effective tier, and `groupScoped`/`groups`
  report the token's group scope. Read `groupScoped`, **not** whether `groups` is empty — an empty
  list means unscoped (the whole store), which is the opposite of scoped-to-nothing.
  Three more describe the deployment rather than the caller, and are the same for everyone calling a
  given instance: `summariserEnabled` (an embedded LLM is configured, so `SummariseMemories` can
  serve), `consolidationEnabled` (this instance runs the sleep cycle — when it is false, `Sleep`,
  `PreviewConsolidation` **and** `ExplainConsolidation` are all `FailedPrecondition`, because a
  replica's store is consolidated by another instance under that instance's configuration), and
  `tombstonesEnabled` (the forgotten log is being recorded). The last is the odd one out and the
  reason it is reported at all: `GetForgottenMemories` does not refuse when the log is off, it
  returns an empty page — so "nothing was written down" and "nothing has been forgotten" are
  indistinguishable without it.
- **A purge blocks everything.** While `Purge` runs, every other RPC is rejected with `Unavailable`
  (`503`). It is brief; retry.
- **Set deadlines.** The service bounds its own storage operations
  (`storage.queryTimeoutSeconds`, 60s by default) but imposes no deadline on an RPC, and
  consolidation and export are long operations — so the wait is the client's to bound.

## Keeping a generated client current

The contract is covered by the [compatibility policy](../CHANGELOG.md#compatibility) and guarded in
CI by `buf breaking` against the previous release tag, so a field is never renumbered and an RPC is
never removed by accident. Pre-1.0, deliberate breaks do ship in minor releases and are listed under
**Breaking** in the [changelog](../CHANGELOG.md) — regenerate when you cross one.

The `/v1` gateway is the more stable of the two surfaces: the proto package rename in the current
unreleased version changed every gRPC method path and every generated OpenAPI schema name, and left
`/v1` paths, JSON field names and payloads untouched.
