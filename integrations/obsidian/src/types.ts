// Plain response shapes the plugin works with, projected from the gateway's protojson output.
//
// The gateway (grpc-gateway, default marshaler) emits camelCase field names, serialises int64
// fields as JSON *strings* (timeStamp, timeRecalled, timeStart, timeEnd), and encodes enums as
// their string names (isBinary as "TRUE"/"FALSE"). The client normalises all of that into the
// numbers/booleans below before anything else in the plugin sees it.

export interface MemoryView {
	id: string;
	body: string;
	significance: number;
	eventId: string;
	group: string;
	timeStamp: number;
	timeRecalled: number;
	recallCount: number;
	isSummary: boolean;
	isBinary: boolean;
}

export interface EventView {
	id: string;
	name: string;
	description: string;
	significance: number;
	group: string;
	timeStart: number;
	timeEnd: number;
}

export interface StoreResult {
	id: string;
	rejected: boolean;
}

export interface SummarisationCandidate {
	eventId: string;
	eventName: string;
	memoryCount: number;
}

// Input shapes for the write endpoints. Timestamps are deliberately omitted so the server defaults
// them to "now" (sidestepping the int64-as-string encoding on the write path).

export interface StoreMemoryInput {
	body: string;
	significance?: number;
	group?: string;
	eventId?: string;
}

export interface SearchMemoriesInput {
	query: string;
	limit?: number;
	group?: string;
	eventId?: string;
	reinforce?: boolean;
}

export interface ListInput {
	group?: string;
	significanceMin?: number;
	significanceMax?: number;
	orderBy?: string;
	limit?: number;
	offset?: number;
}

export interface StoreEventInput {
	name: string;
	description?: string;
	significance?: number;
	group?: string;
}
