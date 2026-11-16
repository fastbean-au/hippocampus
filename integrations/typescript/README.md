# hippocampus-client

The TypeScript client for [Hippocampus](https://github.com/fastbean-au/hippocampus) — a memory
service that stores what matters and forgets what stops mattering. Generated from the service's
contract, over its JSON gateway (anywhere `fetch` exists) or native gRPC (Node).

**It is not on npm yet.** Every service release attaches the packed tarball, so until the upload is
switched on, install the one for the release you run:

```sh
npm install https://github.com/fastbean-au/hippocampus/releases/download/vX.Y.Z/hippocampus-client-X.Y.Z.tgz
```

## Using it

```ts
import { createGatewayTransport, createHippocampusClient, wasStored } from "hippocampus-client";

const client = createHippocampusClient(createGatewayTransport({ baseUrl: "http://localhost:8080" }));

const stored = await client.storeMemory({ body: "the deploy at 14:03 rolled back cleanly", significance: 50 });

if (wasStored(stored)) {
  const found = await client.searchMemories({ query: "deploy", limit: 5, reinforce: true });

  console.log(found.memories.map((memory) => memory.body));
}
```

From Node, native gRPC is a separate entry point, and the only transport that can serve `Watch`:

```sh
npm install @connectrpc/connect-node
```

```ts
import { createHippocampusClient } from "hippocampus-client";
import { createHippocampusGrpcTransport } from "hippocampus-client/grpc";

const client = createHippocampusClient(createHippocampusGrpcTransport({ baseUrl: "http://localhost:50051" }));

for await (const event of client.watch({})) {
  console.log(event);
}
```

The full guide — the transports, the four encodings, errors, authentication and TLS, and the three
behaviours that are the product rather than faults — is
[docs/typescript.md](https://github.com/fastbean-au/hippocampus/blob/main/docs/typescript.md).

## Developing

```sh
npm ci
npm test
```

The build generates `src/gen/` from `../../contract/hippocampus.proto` with `buf` and
`protoc-gen-es`, both devDependencies; it is never committed. Set `HIPPOCAMPUS_GATEWAY_URL` and
`HIPPOCAMPUS_GRPC_URL` to run the live suite against a running service as well.
