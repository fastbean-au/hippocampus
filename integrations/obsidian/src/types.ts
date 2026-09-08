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

// LinkEdge is one edge of the link graph, as GetMemoryLinks reports it. direction is an enum on the
// wire ("OUTBOUND"/"INBOUND"); the client flattens it to a boolean, which is all the plugin needs -
// whether this end declared the link.
export interface LinkEdge {
  id: string;
  significance: number;
  outbound: boolean;
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
  metadata?: Record<string, string>;
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

// LinkInput is one link to write: a target and the weight the edge carries. The weight is a plain
// number rather than part of the significance registry, so it is never ranked against a memory's.
export interface LinkInput {
  id: string;
  significance: number;
}

export interface StoreEventInput {
  name: string;
  description?: string;
  significance?: number;
  group?: string;
}
