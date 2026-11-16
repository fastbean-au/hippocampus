// The guards between this package and the contract.
//
// Nothing in this package names an RPC or a route, which is the point of it - but it means a contract
// change this package cannot serve fails no compile. These tests are where it fails instead: every
// unary RPC must have an HTTP binding the gateway transport can read, every binding must be one the
// OpenAPI document (generated from the same annotations) agrees with, and the one constant restated
// from Go must still match.

import { strict as assert } from "node:assert";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

import { create } from "@bufbuild/protobuf";

import {
  CLIENT_VERSION_HEADER,
  GetMemoriesRequestSchema,
  GetMemoryLinksRequestSchema,
  Hippocampus,
  LinkDirection,
  MemorySchema,
  ReplaceMemoriesWithSummaryRequestSchema,
  SearchMemoriesRequestSchema,
  routeOf,
} from "../src/index.js";
import { gatewayRequest } from "../src/routes.js";

const contractDir = join(dirname(fileURLToPath(import.meta.url)), "..", "..", "..", "..", "contract");

interface Swagger {
  paths: Record<string, Record<string, { operationId?: string }>>;
}

const swagger = JSON.parse(
  readFileSync(join(contractDir, "hippocampus.swagger.json"), "utf8"),
) as Swagger;

test("every unary RPC has an HTTP binding, and only Watch streams", () => {
  const streaming: string[] = [];

  for (const method of Hippocampus.methods) {
    if (method.methodKind !== "unary") {
      streaming.push(method.name);

      continue;
    }

    assert.ok(routeOf(method), `${method.name} has no google.api.http binding the gateway can serve`);
  }

  // A second streaming RPC is a decision for this package - the gateway transport refuses every
  // stream - so it should arrive here as a failure rather than as a runtime surprise.
  assert.deepEqual(streaming, ["Watch"]);
});

test("every binding is one the OpenAPI document declares", () => {
  for (const method of Hippocampus.methods) {
    const route = routeOf(method);

    if (route === undefined) {
      continue;
    }

    // protoc-gen-openapiv2 spells a path variable by its JSON name ({eventId}), where the annotation
    // and grpc-gateway spell it by its proto name ({event_id}); the route is the same either way.
    const documented = route.template.replace(/\{([^}=]+)\}/g, (_, name: string) => {
      return `{${method.input.fields.find((f) => f.name === name)?.jsonName ?? name}}`;
    });
    const operation = swagger.paths[documented]?.[route.verb.toLowerCase()];

    assert.ok(operation, `${method.name}: ${route.verb} ${documented} is not in hippocampus.swagger.json`);
    assert.equal(operation.operationId, `Hippocampus_${method.name}`);
  }
});

test("the client version header matches contract.ClientVersionHeader", () => {
  const source = readFileSync(join(contractDir, "clientversion.go"), "utf8");
  const match = source.match(/const ClientVersionHeader = "([^"]+)"/);

  assert.ok(match, "contract/clientversion.go no longer declares ClientVersionHeader as a string");
  assert.equal(CLIENT_VERSION_HEADER, match[1]);
});

test("a path variable is lifted out of the body", () => {
  const method = Hippocampus.method.updateMemory;
  const rendered = gatewayRequest(method, create(MemorySchema, { id: "a/b c", body: "x", significance: 3 }));

  assert.equal(rendered.verb, "PATCH");
  assert.equal(rendered.path, "/v1/memories/a%2Fb%20c");
  assert.deepEqual(JSON.parse(rendered.body ?? ""), { body: "x", significance: 3 });
});

test("a body binding of one field sends that field and queries the rest", () => {
  const method = Hippocampus.method.replaceMemoriesWithSummary;
  const rendered = gatewayRequest(
    method,
    create(ReplaceMemoriesWithSummaryRequestSchema, {
      eventId: "e1",
      summary: { body: "the gist", significance: 9 },
    }),
  );

  assert.equal(rendered.path, "/v1/events/e1/summary");
  assert.deepEqual(JSON.parse(rendered.body ?? ""), { body: "the gist", significance: 9 });
});

test("a GET renders repeated, int64 and enum fields as the gateway parses them", () => {
  const listing = gatewayRequest(
    Hippocampus.method.getMemories,
    create(GetMemoriesRequestSchema, {
      metadata: ["lg.v=1", "role=user"],
      timestampMin: 1_700_000_000_000_000_000n,
      limit: 5,
    }),
  );

  assert.equal(listing.verb, "GET");
  assert.equal(listing.body, undefined);
  assert.deepEqual(listing.query.getAll("metadata"), ["lg.v=1", "role=user"]);
  assert.equal(listing.query.get("timestampMin"), "1700000000000000000");
  assert.equal(listing.query.get("limit"), "5");

  const links = gatewayRequest(
    Hippocampus.method.getMemoryLinks,
    create(GetMemoryLinksRequestSchema, { id: "m1", direction: LinkDirection.INBOUND }),
  );

  assert.equal(links.path, "/v1/memories/m1/links");
  assert.equal(links.query.get("direction"), "LINK_DIRECTION_INBOUND");
  assert.equal(links.query.has("id"), false);
});

test("a body of * sends everything that is not in the path", () => {
  const rendered = gatewayRequest(
    Hippocampus.method.searchMemories,
    create(SearchMemoriesRequestSchema, { query: "cat", reinforce: true }),
  );

  assert.equal(rendered.verb, "POST");
  assert.equal(rendered.path, "/v1/memories/search");
  assert.equal(rendered.query.toString(), "");
  assert.deepEqual(JSON.parse(rendered.body ?? ""), { query: "cat", reinforce: true });
});
