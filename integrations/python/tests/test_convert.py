"""The wire encodings the wrapper exists to hide."""

from __future__ import annotations

import datetime

import pytest

from hippocampus import _convert
from hippocampus._proto import hippocampus_pb2 as pb


def test_zero_nanos_is_not_a_moment():
    """0 means never-recalled and not-yet-ended, so it must not decode as 1970."""

    assert _convert.from_nanos(0) is None


def test_nanos_round_trip_is_utc_aware():
    when = datetime.datetime(2026, 9, 8, 4, 30, tzinfo=datetime.timezone.utc)

    decoded = _convert.from_nanos(_convert.to_nanos(when))

    assert decoded == when
    assert decoded.tzinfo is not None


def test_nanos_are_nanoseconds_not_seconds_or_millis():
    when = datetime.datetime(2026, 1, 1, tzinfo=datetime.timezone.utc)

    assert _convert.to_nanos(when) == int(when.timestamp()) * 1_000_000_000


def test_none_encodes_as_unset():
    assert _convert.to_nanos(None) == 0


def test_a_bool_is_refused_as_a_timestamp():
    """bool is an int subclass, so without this it would silently encode as 1 nanosecond."""

    with pytest.raises(TypeError):
        _convert.to_nanos(True)


@pytest.mark.parametrize(
    "value,expected",
    [(None, pb.Bool.UNSPECIFIED), (True, pb.Bool.TRUE), (False, pb.Bool.FALSE)],
)
def test_tristate_distinguishes_absent_from_false(value, expected):
    assert _convert.to_tristate(value) == expected


def test_tristate_reads_back_as_a_plain_bool():
    """On a record, UNSPECIFIED and FALSE both mean not-binary."""

    assert _convert.from_tristate(pb.Bool.TRUE) is True
    assert _convert.from_tristate(pb.Bool.FALSE) is False
    assert _convert.from_tristate(pb.Bool.UNSPECIFIED) is False


def test_metadata_pairs_split_on_the_first_equals():
    """A value may itself contain '=', which is why the server splits on the first one."""

    assert _convert.pairs_to_metadata(["url=a=b&c=d"]) == {"url": "a=b&c=d"}


def test_metadata_encodes_as_pairs():
    assert _convert.metadata_to_pairs({"source": "slack"}) == ["source=slack"]
    assert _convert.metadata_to_pairs(None) == []
