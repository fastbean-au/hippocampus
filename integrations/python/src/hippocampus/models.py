"""The types a caller reads and writes.

These are dataclasses rather than the generated protobuf messages, and only for the memory and
event surface - the operator surface (topology, the consolidation preview, the forgotten log, the
callback queue, transfer) returns its protobuf message unchanged. The line is the same one the MCP
bridge draws: what an application uses every day is worth an ergonomic type, and what an operator
reads occasionally is better served by the message whose field comments are the documentation.

Every enum takes its values from the generated descriptor rather than restating them, so a renamed
or renumbered value is a build failure here rather than a wrong number on the wire.
"""

from __future__ import annotations

import datetime
import enum
from dataclasses import dataclass, field
from typing import Any, Dict, Iterator, List, Optional, Sequence

from hippocampus import _convert
from hippocampus._proto import hippocampus_pb2 as pb


class SearchMode(enum.IntEnum):
    """How a search decides which memories match.

    Which of these a deployment can actually serve is a property of the deployment and not of the
    caller: KEYWORD needs a content-search backend, SEMANTIC and HYBRID additionally need an
    embedding model and OpenSearch's vector index. Identity.search_modes reports the available set,
    so a client chooses rather than discovering an unavailable mode through a rejection.
    """

    KEYWORD = pb.SEARCH_MODE_KEYWORD
    SEMANTIC = pb.SEARCH_MODE_SEMANTIC
    HYBRID = pb.SEARCH_MODE_HYBRID


class LinkDirection(enum.IntEnum):
    """Which of an item's links a read returns.

    The graph is stored directed, so the two directions are distinguishable, but value is
    symmetric - both ends of a link gain its significance - which is why BOTH is the default.
    """

    BOTH = pb.LINK_DIRECTION_BOTH
    OUTBOUND = pb.LINK_DIRECTION_OUTBOUND
    INBOUND = pb.LINK_DIRECTION_INBOUND


class SignificanceExtremum(enum.IntEnum):
    """Select the tied set at the highest or lowest significance, rather than a range."""

    HIGHEST = pb.SIGNIFICANCE_EXTREMUM_HIGHEST
    LOWEST = pb.SIGNIFICANCE_EXTREMUM_LOWEST


class SortDirection(enum.IntEnum):
    """Reverse a listing's natural sort direction.

    Leaving this unset is not "ascending": each order_by field has a natural direction - the
    magnitude and time fields sort descending, the lexical ones ascending - and unset keeps it.
    """

    ASC = pb.SORT_DIRECTION_ASC
    DESC = pb.SORT_DIRECTION_DESC


class PlacementMode(enum.IntEnum):
    """How a Placement positions an item relative to existing significance values."""

    ABOVE = pb.SignificancePlacement.ABOVE
    BELOW = pb.SignificancePlacement.BELOW
    BETWEEN = pb.SignificancePlacement.BETWEEN


@dataclass
class Placement:
    """Rank an item relative to the significance values already in use.

    Significance is a dense integer with no room between adjacent values, so this is how a caller
    says "just above the 5s" and has the server open a gap for it. Write-only: it is never
    populated on a record read back. `significance_levels()` lists the values a placement can
    anchor on.
    """

    mode: PlacementMode
    anchor: int = 0
    anchor_id: str = ""
    upper: int = 0
    upper_id: str = ""

    @classmethod
    def above(cls, anchor: int = 0, anchor_id: str = "") -> "Placement":
        """Rank just above the anchor, below the next-higher value."""

        return cls(PlacementMode.ABOVE, anchor=anchor, anchor_id=anchor_id)

    @classmethod
    def below(cls, anchor: int = 0, anchor_id: str = "") -> "Placement":
        """Rank just below the anchor, above the next-lower value."""

        return cls(PlacementMode.BELOW, anchor=anchor, anchor_id=anchor_id)

    @classmethod
    def between(
        cls,
        anchor: int = 0,
        upper: int = 0,
        anchor_id: str = "",
        upper_id: str = "",
    ) -> "Placement":
        """Rank strictly between the anchor (lower) and upper bounds."""

        return cls(
            PlacementMode.BETWEEN,
            anchor=anchor,
            upper=upper,
            anchor_id=anchor_id,
            upper_id=upper_id,
        )

    def to_proto(self) -> pb.SignificancePlacement:
        return pb.SignificancePlacement(
            mode=int(self.mode),
            anchor=self.anchor,
            anchor_id=self.anchor_id,
            upper=self.upper,
            upper_id=self.upper_id,
        )


@dataclass
class Link:
    """A directed, significance-carrying edge from the item declaring it to the item it names.

    The significance is weighted into the decay maths for BOTH ends, with diminishing returns, so
    a well-connected memory decays more slowly without any number of links making it unforgettable.
    Both ends must exist, and a link is deleted when either end is forgotten - it can never dangle.
    """

    id: str
    significance: int = 0

    def to_proto(self) -> pb.Link:
        return pb.Link(id=self.id, significance=self.significance)

    @classmethod
    def from_proto(cls, message: pb.Link) -> "Link":
        return cls(id=message.id, significance=message.significance)


@dataclass
class LinkEdge:
    """One stored link as read back, which unlike Link has to say which way it points.

    Link names only the far end, because the near end is the item carrying it; a read can return
    both directions at once, so direction is explicit here.
    """

    id: str
    significance: int
    direction: LinkDirection
    created: Optional[datetime.datetime]

    @classmethod
    def from_proto(cls, message: pb.LinkEdge) -> "LinkEdge":
        return cls(
            id=message.id,
            significance=message.significance,
            direction=LinkDirection(message.direction),
            created=_convert.from_nanos(message.created),
        )


@dataclass
class Links:
    """An item's links, with the total the decay maths actually damps and weights."""

    edges: List[LinkEdge] = field(default_factory=list)
    link_significance: int = 0

    def __iter__(self) -> Iterator[LinkEdge]:
        return iter(self.edges)

    def __len__(self) -> int:
        return len(self.edges)

    @classmethod
    def from_proto(cls, message: pb.GetLinksResponse) -> "Links":
        return cls(
            edges=[LinkEdge.from_proto(edge) for edge in message.links],
            link_significance=message.link_significance,
        )


@dataclass
class Memory:
    """A blob with a significance and a timestamp, optionally attached to an event.

    Recalling it reinforces it: the decay clock resets and its effective significance rises. The
    read-only fields below are populated on a read and ignored on a write.
    """

    body: str = ""
    significance: int = 0
    id: str = ""
    event_id: Optional[str] = None
    group: Optional[str] = None
    metadata: Dict[str, str] = field(default_factory=dict)
    timestamp: Optional[datetime.datetime] = None
    binary: bool = False
    links: List[Link] = field(default_factory=list)
    placement: Optional[Placement] = None

    # The size of a payload this memory only POINTS AT, held in another system - a trace, a blob, a
    # document. Never interpreted by the service: it feeds the external capacity axis
    # (consolidation.capacityExternalBytes), so this store can decide the retention of storage it
    # does not hold. 0 on an update leaves the stored value unchanged, exactly as significance does.
    external_bytes: int = 0

    # Read-only.
    recalled_at: Optional[datetime.datetime] = None
    recall_count: int = 0
    is_summary: bool = False
    link_significance: int = 0

    def to_proto(self) -> pb.Memory:
        message = pb.Memory(
            id=self.id,
            time_stamp=_convert.to_nanos(self.timestamp),
            significance=self.significance,
            event_id=self.event_id or "",
            body=self.body,
            is_binary=_convert.to_tristate(True) if self.binary else pb.Bool.UNSPECIFIED,
            group=self.group or "",
            external_bytes=self.external_bytes,
            links=[link.to_proto() for link in self.links],
        )

        if self.metadata:
            message.metadata.update(self.metadata)

        if self.placement is not None:
            message.placement.CopyFrom(self.placement.to_proto())

        return message

    @classmethod
    def from_proto(cls, message: pb.Memory) -> "Memory":
        return cls(
            body=message.body,
            significance=message.significance,
            id=message.id,
            event_id=message.event_id or None,
            group=message.group or None,
            metadata=dict(message.metadata),
            timestamp=_convert.from_nanos(message.time_stamp),
            binary=_convert.from_tristate(message.is_binary),
            external_bytes=message.external_bytes,
            links=[Link.from_proto(link) for link in message.links],
            recalled_at=_convert.from_nanos(message.time_recalled),
            recall_count=message.recall_count,
            is_summary=message.is_summary,
            link_significance=message.link_significance,
        )


@dataclass
class Event:
    """A named time span with its own significance, which memories may be attached to.

    `ended_at` is None while the event is still running, which is what the wire's time_end of 0
    means. `memory_count` is populated only when a read asks for it, so a 0 here is
    indistinguishable from an event holding no memories unless you requested the count.
    """

    name: str = ""
    significance: int = 0
    id: str = ""
    description: str = ""
    group: Optional[str] = None
    metadata: Dict[str, str] = field(default_factory=dict)
    started_at: Optional[datetime.datetime] = None
    ended_at: Optional[datetime.datetime] = None
    links: List[Link] = field(default_factory=list)
    placement: Optional[Placement] = None
    memories: List[Memory] = field(default_factory=list)

    # Read-only.
    memories_consolidated: bool = False
    link_significance: int = 0
    memory_count: int = 0

    def to_proto(self) -> pb.Event:
        message = pb.Event(
            id=self.id,
            time_start=_convert.to_nanos(self.started_at),
            time_end=_convert.to_nanos(self.ended_at),
            significance=self.significance,
            name=self.name,
            description=self.description,
            group=self.group or "",
            links=[link.to_proto() for link in self.links],
            memories=[memory.to_proto() for memory in self.memories],
        )

        if self.metadata:
            message.metadata.update(self.metadata)

        if self.placement is not None:
            message.placement.CopyFrom(self.placement.to_proto())

        return message

    @classmethod
    def from_proto(cls, message: pb.Event) -> "Event":
        return cls(
            name=message.name,
            significance=message.significance,
            id=message.id,
            description=message.description,
            group=message.group or None,
            metadata=dict(message.metadata),
            started_at=_convert.from_nanos(message.time_start),
            ended_at=_convert.from_nanos(message.time_end),
            links=[Link.from_proto(link) for link in message.links],
            memories=[Memory.from_proto(memory) for memory in message.memories],
            memories_consolidated=message.memories_consolidated,
            link_significance=message.link_significance,
            memory_count=message.memory_count,
        )


@dataclass
class Stored:
    """The outcome of storing one memory or event.

    `rejected` is not an error: an item below the deployment's minimum significance is quietly
    forgotten, like a brain that simply does not retain the insignificant, and the call succeeds
    with an empty id. Truthiness is "it was kept", so `if client.store_memory(...):` reads
    correctly and the trap is hard to walk into.
    """

    id: str = ""
    rejected: bool = False
    memory_count: int = 0

    def __bool__(self) -> bool:
        return not self.rejected and bool(self.id)


@dataclass
class BatchResult:
    """One memory's outcome within a batch write.

    Exactly one of three states holds: stored (id set), rejected for insignificance (rejected true,
    code 0), or failed (code non-zero, error naming why). A failure here is not the call's error -
    that is the point of the batch - so a caller checking only the call's status learns nothing.
    """

    id: str = ""
    rejected: bool = False
    code: int = 0
    error: str = ""

    def __bool__(self) -> bool:
        return not self.rejected and self.code == 0 and bool(self.id)

    @property
    def failed(self) -> bool:
        return self.code != 0


@dataclass
class BatchStored:
    """The outcome of a batch write, one result per requested memory, positionally."""

    results: List[BatchResult] = field(default_factory=list)
    stored: int = 0
    rejected: int = 0
    failed: int = 0

    def __iter__(self) -> Iterator[BatchResult]:
        return iter(self.results)

    def __len__(self) -> int:
        return len(self.results)

    def __getitem__(self, index: int) -> BatchResult:
        return self.results[index]

    @classmethod
    def from_proto(cls, message: pb.StoreMemoriesResponse) -> "BatchStored":
        return cls(
            results=[
                BatchResult(
                    id=result.id,
                    rejected=result.rejected,
                    code=result.code,
                    error=result.error,
                )
                for result in message.results
            ],
            stored=message.stored,
            rejected=message.rejected,
            failed=message.failed,
        )


@dataclass
class Page:
    """One page of a listing, plus how many records matched the filter in total.

    `total` ignores limit and offset, which is what makes it useful for pagination; under group
    scoping it counts only what this token may see.
    """

    items: List[Any] = field(default_factory=list)
    total: int = 0

    def __iter__(self) -> Iterator[Any]:
        return iter(self.items)

    def __len__(self) -> int:
        return len(self.items)

    def __getitem__(self, index: int) -> Any:
        return self.items[index]

    def __bool__(self) -> bool:
        return bool(self.items)


@dataclass
class Identity:
    """What who_am_i() reports: who the caller is, and what this deployment can do.

    The distinction matters when deciding what to offer. `client_id`, `role`, `groups` and
    `group_scoped` describe the CALLER; everything below them describes the DEPLOYMENT and is the
    same for everyone calling that instance.

    Read `group_scoped`, never whether `groups` is empty: an empty list means unscoped - the whole
    store - which is the opposite of scoped to nothing.
    """

    client_id: str = ""
    role: str = ""
    auth_enabled: bool = False
    groups: List[str] = field(default_factory=list)
    group_scoped: bool = False

    search_modes: List[SearchMode] = field(default_factory=list)
    summariser_enabled: bool = False
    consolidation_enabled: bool = False
    tombstones_enabled: bool = False
    callbacks_enabled: bool = False
    topology_tier: str = ""
    version: str = ""

    @property
    def search_enabled(self) -> bool:
        """Whether content search can serve at all. An empty mode list means it cannot."""

        return bool(self.search_modes)

    def supports(self, mode: SearchMode) -> bool:
        """Whether this deployment can serve a given search mode."""

        return mode in self.search_modes

    @classmethod
    def from_proto(cls, message: pb.WhoAmIResponse) -> "Identity":
        return cls(
            client_id=message.client_id,
            role=message.role,
            auth_enabled=message.auth_enabled,
            groups=list(message.groups),
            group_scoped=message.group_scoped,
            search_modes=[SearchMode(mode) for mode in message.search_modes],
            summariser_enabled=message.summariser_enabled,
            consolidation_enabled=message.consolidation_enabled,
            tombstones_enabled=message.tombstones_enabled,
            callbacks_enabled=message.callbacks_enabled,
            topology_tier=message.topology_tier,
            version=message.version,
        )


def memories_page(message: pb.GetMemoriesResponse) -> Page:
    """Decode a memory listing."""

    return Page(
        items=[Memory.from_proto(memory) for memory in message.memories],
        total=message.total_count,
    )


def events_page(message: pb.GetEventsResponse) -> Page:
    """Decode an event listing."""

    return Page(
        items=[Event.from_proto(event) for event in message.events],
        total=message.total_count,
    )


def link_protos(links: Optional[Sequence[Link]]) -> List[pb.Link]:
    """Encode a sequence of links for a request."""

    if not links:
        return []

    return [link.to_proto() for link in links]
