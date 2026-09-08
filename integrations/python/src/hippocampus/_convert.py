"""Conversions between the wire's encodings and Python's.

Four of the contract's encodings catch every new client at least once, and removing them is most
of what the wrapper is for:

- Every timestamp is an int64 of UnixNano, which is neither seconds nor milliseconds and does not
  fit a JavaScript number - here it becomes an aware datetime in UTC.
- Zero is not a timestamp. time_end 0 means "has not ended", time_recalled 0 means "never
  recalled" - here both become None, so the absence is in the type rather than in a magic value.
- Bool is a tri-state enum, not proto3's bool, because an unset bool and an explicit false are the
  same byte on the wire and the list filters need to tell them apart - here it is Optional[bool].
- Metadata filters travel as repeated "key=value" strings (a map cannot be bound from a URL query
  string) - here they are a dict, split on the FIRST '=' so a value may contain one.
"""

from __future__ import annotations

import datetime as _datetime
from typing import Dict, Iterable, List, Mapping, Optional, Union

from hippocampus._proto import hippocampus_pb2 as pb

NANOS_PER_SECOND = 1_000_000_000

# What a caller may hand to any field the contract carries as UnixNano.
Timestamp = Union[_datetime.datetime, int, float, None]


def to_nanos(value: Timestamp) -> int:
    """Encode a datetime (or an epoch-seconds number) as UnixNano; None and 0 mean unset.

    A naive datetime is interpreted as local time, which is what datetime.timestamp() does and so
    what `datetime.now()` round-trips as. Pass an aware datetime to be explicit.
    """

    if value is None:
        return 0

    if isinstance(value, _datetime.datetime):
        return int(value.timestamp() * NANOS_PER_SECOND)

    if isinstance(value, bool):
        raise TypeError("a bool is not a timestamp")

    return int(value * NANOS_PER_SECOND)


def from_nanos(value: int) -> Optional[_datetime.datetime]:
    """Decode UnixNano into an aware UTC datetime; 0 becomes None.

    Zero is the contract's "no such moment" - never recalled, not yet ended - and returning None
    for it is what keeps a caller from formatting 1970 into a UI.
    """

    if not value:
        return None

    return _datetime.datetime.fromtimestamp(value / NANOS_PER_SECOND, tz=_datetime.timezone.utc)


def to_tristate(value: Optional[bool]) -> "pb.Bool.ValueType":
    """Encode Optional[bool] as the tri-state Bool: None applies no restriction."""

    if value is None:
        return pb.Bool.UNSPECIFIED

    return pb.Bool.TRUE if value else pb.Bool.FALSE


def from_tristate(value: "pb.Bool.ValueType") -> bool:
    """Decode the tri-state Bool as read back off a record.

    UNSPECIFIED and FALSE are the same thing on Memory.is_binary - the server treats both as
    not-binary - so a record's flag is a plain bool. The three-valued reading belongs to the
    filters, which is what to_tristate is for.
    """

    return value == pb.Bool.TRUE


def metadata_to_pairs(metadata: Optional[Mapping[str, str]]) -> List[str]:
    """Encode a metadata filter as the repeated "key=value" form the request carries."""

    if not metadata:
        return []

    return [f"{key}={value}" for key, value in metadata.items()]


def pairs_to_metadata(pairs: Optional[Iterable[str]]) -> Dict[str, str]:
    """Decode "key=value" pairs into a dict, splitting on the first '=' as the server does."""

    out: Dict[str, str] = {}

    if not pairs:
        return out

    for pair in pairs:
        key, _, value = pair.partition("=")
        out[key] = value

    return out
