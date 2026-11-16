// The gateway transport against a fake fetch: what it sends, and how it reads what comes back.

import { strict as assert } from "node:assert";
import test from "node:test";

import {
  Bool,
  CLIENT_VERSION_HEADER,
  Code,
  ConnectError,
  DEFAULT_CLIENT_VERSION,
  createGatewayTransport,
  createHippocampusClient,
  errorOf,
} from "../src/index.js";

interface Seen {
  url: string;
  method: string;
  headers: Headers;
  body: string | undefined;
}

function fakeFetch(status: number, body: unknown, seen: Seen[]): typeof globalThis.fetch {
  return async (input, init) => {
    seen.push({
      url: String(input),
      method: init?.method ?? "GET",
      headers: new Headers(init?.headers),
      body: init?.body === undefined ? undefined : String(init.body),
    });

    return new Response(typeof body === "string" ? body : JSON.stringify(body), { status });
  };
}

test("a call is the binding's verb and path, with identity headers", async () => {
  const seen: Seen[] = [];
  const client = createHippocampusClient(
    createGatewayTransport({
      baseUrl: "http://gateway:8080/",
      token: "t0k3n",
      fetch: fakeFetch(200, { id: "m1", rejected: false }, seen),
    }),
  );

  const stored = await client.storeMemory({ body: "the deploy rolled back", significance: 50 });

  assert.equal(stored.id, "m1");
  assert.equal(seen.length, 1);
  assert.equal(seen[0].method, "POST");
  assert.equal(seen[0].url, "http://gateway:8080/v1/memories");
  assert.equal(seen[0].headers.get("authorization"), "Bearer t0k3n");
  assert.equal(seen[0].headers.get(CLIENT_VERSION_HEADER), DEFAULT_CLIENT_VERSION);
  assert.equal(seen[0].headers.get("content-type"), "application/json");
  assert.deepEqual(JSON.parse(seen[0].body ?? ""), { body: "the deploy rolled back", significance: 50 });
});

test("no token sends no authorization, and a caller can name its own product", async () => {
  const seen: Seen[] = [];
  const client = createHippocampusClient(
    createGatewayTransport({
      baseUrl: "http://gateway:8080",
      clientVersion: "my-agent/1.4.0",
      fetch: fakeFetch(200, {}, seen),
    }),
  );

  await client.whoAmI({});

  assert.equal(seen[0].headers.has("authorization"), false);
  assert.equal(seen[0].headers.get(CLIENT_VERSION_HEADER), "my-agent/1.4.0");
});

test("the response is protojson: int64 strings and enum names decode to the generated types", async () => {
  const client = createHippocampusClient(
    createGatewayTransport({
      baseUrl: "http://gateway:8080",
      fetch: fakeFetch(
        200,
        {
          memories: [{ id: "m1", timeStamp: "1700000000000000000", isBinary: "TRUE", unknownField: 1 }],
          totalCount: 1,
        },
        [],
      ),
    }),
  );

  const page = await client.getMemories({});

  assert.equal(page.totalCount, 1);
  assert.equal(page.memories[0].timeStamp, 1_700_000_000_000_000_000n);
  assert.equal(page.memories[0].isBinary, Bool.TRUE);
});

test("a gateway error arrives as the gRPC code the service chose", async () => {
  const client = createHippocampusClient(
    createGatewayTransport({
      baseUrl: "http://gateway:8080",
      fetch: fakeFetch(404, { code: 5, message: "event not found", details: [] }, []),
    }),
  );

  await assert.rejects(client.getEventById({ id: "nope" }), (err: unknown) => {
    assert.ok(err instanceof ConnectError);
    assert.equal(err.code, Code.NotFound);
    assert.equal(err.rawMessage, "event not found");

    return true;
  });
});

test("a body that is not the gateway's shape falls back to the HTTP status", () => {
  assert.equal(errorOf(503, "<html>bad gateway</html>").code, Code.Unavailable);
  assert.equal(errorOf(401, "").code, Code.Unauthenticated);
  assert.equal(errorOf(418, "").code, Code.Unknown);
});

test("a fetch that throws is Unavailable", async () => {
  const client = createHippocampusClient(
    createGatewayTransport({
      baseUrl: "http://gateway:8080",
      fetch: async () => {
        throw new TypeError("fetch failed");
      },
    }),
  );

  await assert.rejects(client.whoAmI({}), (err: unknown) => err instanceof ConnectError && err.code === Code.Unavailable);
});

test("Watch is refused by name rather than attempted", async () => {
  const client = createHippocampusClient(
    createGatewayTransport({ baseUrl: "http://gateway:8080", fetch: fakeFetch(200, {}, []) }),
  );

  await assert.rejects(
    async () => {
      for await (const _ of client.watch({})) {
        // unreachable
      }
    },
    (err: unknown) => err instanceof ConnectError && err.code === Code.Unimplemented && /gRPC transport/.test(err.rawMessage),
  );
});

test("an interceptor sees, and may change, the request", async () => {
  const seen: Seen[] = [];
  const client = createHippocampusClient(
    createGatewayTransport({
      baseUrl: "http://gateway:8080",
      fetch: fakeFetch(200, {}, seen),
      interceptors: [
        (next) => async (req) => {
          req.header.set("x-trace", "abc");

          return next(req);
        },
      ],
    }),
  );

  await client.whoAmI({});

  assert.equal(seen[0].headers.get("x-trace"), "abc");
});

test("a timeout aborts the fetch as DeadlineExceeded", async () => {
  const client = createHippocampusClient(
    createGatewayTransport({
      baseUrl: "http://gateway:8080",
      timeoutMs: 20,
      fetch: (_, init) =>
        new Promise((_, reject) => {
          init?.signal?.addEventListener("abort", () => reject(init.signal?.reason));
        }),
    }),
  );

  await assert.rejects(client.whoAmI({}), (err: unknown) => err instanceof ConnectError && err.code === Code.DeadlineExceeded);
});
