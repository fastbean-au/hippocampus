import { strict as assert } from "node:assert";
import test from "node:test";

import { Bool, fromBool, fromNanos, metadataFilter, toBool, toNanos, wasStored } from "../src/index.js";

test("timestamps round-trip at millisecond precision, and zero is absent", () => {
  const when = new Date("2026-11-16T06:00:00.123Z");

  assert.equal(toNanos(when), 1_794_808_800_123_000_000n);
  assert.deepEqual(fromNanos(toNanos(when)), when);
  assert.equal(toNanos(undefined), 0n);
  assert.equal(fromNanos(0n), undefined);
});

test("tri-state booleans keep absence distinct from false", () => {
  assert.equal(toBool(undefined), Bool.UNSPECIFIED);
  assert.equal(toBool(false), Bool.FALSE);
  assert.equal(toBool(true), Bool.TRUE);
  assert.equal(fromBool(Bool.UNSPECIFIED), undefined);
  assert.equal(fromBool(Bool.FALSE), false);
});

test("a metadata filter is key=value pairs, split by the server on the first =", () => {
  assert.deepEqual(metadataFilter({ role: "user", "lg.v": "1", eq: "a=b" }), ["role=user", "lg.v=1", "eq=a=b"]);
});

test("a rejected write is not stored, though the call succeeded", () => {
  assert.equal(wasStored({ id: "m1", rejected: false }), true);
  assert.equal(wasStored({ id: "", rejected: true }), false);
});
