import { Bool } from "./gen/hippocampus_pb.js";

// The four places the wire's encoding is not what a caller means, and the reason this package has
// any hand-written surface at all beyond its transports. The Python client's dataclasses absorb the
// same four; here they are helpers over the generated messages, so a caller keeps the generated
// types - whose field comments are the contract's documentation - and reaches for these only where
// the encoding would otherwise mislead.

// toNanos renders a time as the UnixNano int64 every timestamp field carries. undefined is 0n,
// which every such field reads as "unset" - a filter bound left open, a write the server stamps.
export function toNanos(time: Date | undefined): bigint {
  if (time === undefined) {
    return 0n;
  }

  return BigInt(time.getTime()) * 1_000_000n;
}

// fromNanos reads a UnixNano timestamp back, with 0n as undefined rather than 1970: a memory never
// recalled has timeRecalled 0n, and an event not yet ended has timeEnd 0n, and both are absences.
// Millisecond precision is all a Date holds, so the nanoseconds below it are dropped.
export function fromNanos(nanos: bigint): Date | undefined {
  if (nanos === 0n) {
    return undefined;
  }

  return new Date(Number(nanos / 1_000_000n));
}

// toBool renders an optional boolean as the contract's tri-state Bool, for the list filters whose
// bounds cannot express absence: recalled=false asks "never recalled", where recallCountMax=0 means
// "no upper bound". undefined applies no restriction.
export function toBool(value: boolean | undefined): Bool {
  if (value === undefined) {
    return Bool.UNSPECIFIED;
  }

  return value ? Bool.TRUE : Bool.FALSE;
}

// fromBool reads a tri-state Bool back as an optional boolean.
export function fromBool(value: Bool): boolean | undefined {
  switch (value) {

    case Bool.TRUE:
      return true;

    case Bool.FALSE:
      return false;

    default:
      return undefined;

  }
}

// metadataFilter renders an exact-match metadata filter as the "key=value" pairs the list, search
// and delete requests take. A key repeated in the request ANDs, so a record is the one filter.
export function metadataFilter(filter: Record<string, string>): string[] {
  return Object.entries(filter).map(([key, value]) => `${key}=${value}`);
}

// wasStored is the test a write's response wants, and the trap it avoids: a memory or event below
// the deployment's minimum significance is quietly dropped, so the call SUCCEEDS with an empty id
// and rejected true. Catching errors alone never notices.
export function wasStored(response: { id: string; rejected: boolean }): boolean {
  return !response.rejected && response.id !== "";
}
