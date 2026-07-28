// Pure normalisation of the gateway's protojson responses into the plugin's plain shapes. Kept
// free of any Obsidian import so it can be unit-tested directly. The gateway emits camelCase field
// names, int64 fields as JSON *strings*, and enums as their string names.

import type { EventView, MemoryView, SummarisationCandidate } from "./types";

// toNumber parses the gateway's string-encoded int64 fields (and tolerates already-numeric input).
export function toNumber(value: unknown): number {
	if (typeof value === "number") {
		return value;
	}

	if (typeof value === "string" && value !== "") {
		const n = Number(value);

		return Number.isFinite(n) ? n : 0;
	}

	return 0;
}

export function toMemoryView(raw: Record<string, unknown>): MemoryView {
	return {
		id: String(raw.id ?? ""),
		body: String(raw.body ?? ""),
		significance: toNumber(raw.significance),
		eventId: String(raw.eventId ?? ""),
		group: String(raw.group ?? ""),
		timeStamp: toNumber(raw.timeStamp),
		timeRecalled: toNumber(raw.timeRecalled),
		recallCount: toNumber(raw.recallCount),
		isSummary: raw.isSummary === true,
		// isBinary is a tri-state enum on the wire ("TRUE"/"FALSE"/"UNSPECIFIED"); only TRUE is binary.
		isBinary: raw.isBinary === "TRUE" || raw.isBinary === 2,
	};
}

export function toEventView(raw: Record<string, unknown>): EventView {
	return {
		id: String(raw.id ?? ""),
		name: String(raw.name ?? ""),
		description: String(raw.description ?? ""),
		significance: toNumber(raw.significance),
		group: String(raw.group ?? ""),
		timeStart: toNumber(raw.timeStart),
		timeEnd: toNumber(raw.timeEnd),
	};
}

export function memoriesFrom(json: Record<string, unknown>): MemoryView[] {
	const rows = Array.isArray(json.memories) ? json.memories : [];

	return rows.map((row) => toMemoryView(row as Record<string, unknown>));
}

export function eventsFrom(json: Record<string, unknown>): EventView[] {
	const rows = Array.isArray(json.events) ? json.events : [];

	return rows.map((row) => toEventView(row as Record<string, unknown>));
}

export function candidatesFrom(json: Record<string, unknown>): SummarisationCandidate[] {
	const rows = Array.isArray(json.candidates) ? json.candidates : [];

	return rows.map((row) => {
		const c = row as Record<string, unknown>;

		return {
			eventId: String(c.eventId ?? ""),
			eventName: String(c.eventName ?? ""),
			memoryCount: toNumber(c.memoryCount),
		};
	});
}
