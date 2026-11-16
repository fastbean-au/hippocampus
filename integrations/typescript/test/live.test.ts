// Both transports against a real service, when one is named.
//
// The fake fetch in gateway.test.ts is only as right as its reading of grpc-gateway, so this suite
// runs one round trip over each wire. Set HIPPOCAMPUS_GATEWAY_URL ("http://localhost:8080") and/or
// HIPPOCAMPUS_GRPC_URL ("http://localhost:50051"), plus HIPPOCAMPUS_TOKEN for an authenticating
// service. Each run writes under a fresh group and deletes what it wrote.

import { strict as assert } from "node:assert";
import { randomUUID } from "node:crypto";
import test from "node:test";

import { Code, ConnectError, createGatewayTransport, createHippocampusClient, metadataFilter, wasStored } from "../src/index.js";
import type { HippocampusClient } from "../src/index.js";
import { createHippocampusGrpcTransport } from "../src/grpc.js";

const token = process.env.HIPPOCAMPUS_TOKEN;
const transports: [string, string | undefined, () => HippocampusClient][] = [
  [
    "gateway",
    process.env.HIPPOCAMPUS_GATEWAY_URL,
    () => createHippocampusClient(createGatewayTransport({ baseUrl: process.env.HIPPOCAMPUS_GATEWAY_URL ?? "", token })),
  ],
  [
    "grpc",
    process.env.HIPPOCAMPUS_GRPC_URL,
    () => createHippocampusClient(createHippocampusGrpcTransport({ baseUrl: process.env.HIPPOCAMPUS_GRPC_URL ?? "", token })),
  ],
];

for (const [name, url, connect] of transports) {
  test(`a round trip over the ${name} transport`, { skip: url === undefined ? `${name} address not set` : false }, async () => {
    const client = connect();
    const group = `ts-test-${randomUUID().slice(0, 12)}`;

    const stored = await client.storeMemory({
      body: "a tabby cat called Miso",
      significance: 50,
      group,
      metadata: { source: "live-test" },
    });

    assert.ok(wasStored(stored));

    const page = await client.getMemories({ group, metadata: metadataFilter({ source: "live-test" }) });

    assert.equal(page.totalCount, 1);
    assert.equal(page.memories[0].id, stored.id);
    assert.ok(page.memories[0].timeStamp > 0n);

    const recalled = await client.recallMemories({ ids: [stored.id] });

    assert.equal(recalled.memories[0].recallCount, 1);

    await client.updateMemory({ id: stored.id, body: "a tabby cat called Miso, aged four" });

    const links = await client.getMemoryLinks({ id: stored.id });

    assert.equal(links.links.length, 0);

    await client.deleteMemories({ ids: [stored.id] });

    await assert.rejects(client.getEventById({ id: randomUUID() }), (err: unknown) => {
      return err instanceof ConnectError && err.code === Code.NotFound;
    });
  });
}

test("Watch streams over gRPC", { skip: process.env.HIPPOCAMPUS_GRPC_URL === undefined ? "gRPC address not set" : false }, async () => {
  const client = transports[1][2]();
  const abort = new AbortController();
  const events = client.watch({}, { signal: abort.signal });
  const group = `ts-watch-${randomUUID().slice(0, 12)}`;

  const first = (async () => {
    for await (const event of events) {
      return event;
    }

    return undefined;
  })();

  // The subscription has to be registered before the write it is waiting for.
  await new Promise((resolve) => setTimeout(resolve, 200));

  const stored = await client.storeMemory({ body: "watched", significance: 50, group });
  const event = await first;

  abort.abort();

  assert.ok(event, "the stream ended without an event");
  assert.ok(JSON.stringify(event, (_, v) => (typeof v === "bigint" ? v.toString() : v)).includes(stored.id));

  await client.deleteMemories({ ids: [stored.id] });
});
