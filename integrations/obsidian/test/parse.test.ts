import assert from "node:assert/strict";
import { test } from "node:test";

import { candidatesFrom, memoriesFrom, toMemoryView, toNumber } from "../src/parse";

test("toNumber parses int64-as-string, numbers, and junk", () => {
	assert.equal(toNumber("1784927608547449000"), 1784927608547449000);
	assert.equal(toNumber(42), 42);
	assert.equal(toNumber("0"), 0);
	assert.equal(toNumber(""), 0);
	assert.equal(toNumber(undefined), 0);
	assert.equal(toNumber("not-a-number"), 0);
});

test("toMemoryView normalises the gateway's camelCase/string/enum encoding", () => {
	// A row exactly as the gateway emits it (int64 as strings, isBinary as an enum name).
	const raw = {
		id: "3125f947",
		timeStamp: "1784927608547449000",
		significance: 8,
		eventId: "",
		body: "second memory",
		isBinary: "FALSE",
		timeRecalled: "1784927608642142000",
		recallCount: 1,
		isSummary: false,
		group: "daily",
		placement: null,
	};

	const view = toMemoryView(raw);

	assert.equal(view.id, "3125f947");
	assert.equal(view.timeStamp, 1784927608547449000);
	assert.equal(view.timeRecalled, 1784927608642142000);
	assert.equal(view.significance, 8);
	assert.equal(view.group, "daily");
	assert.equal(view.recallCount, 1);
	assert.equal(view.isBinary, false);
	assert.equal(view.isSummary, false);
});

test("toMemoryView treats only the TRUE enum as binary", () => {
	assert.equal(toMemoryView({ isBinary: "TRUE" }).isBinary, true);
	assert.equal(toMemoryView({ isBinary: "UNSPECIFIED" }).isBinary, false);
	assert.equal(toMemoryView({}).isBinary, false);
});

test("memoriesFrom tolerates a missing memories array", () => {
	assert.deepEqual(memoriesFrom({}), []);
	assert.equal(memoriesFrom({ memories: [{ id: "a", body: "x" }] }).length, 1);
});

test("candidatesFrom maps candidate rows", () => {
	const out = candidatesFrom({
		candidates: [{ eventId: "e1", eventName: "Project", memoryCount: "12" }],
	});

	assert.deepEqual(out, [{ eventId: "e1", eventName: "Project", memoryCount: 12 }]);
});
