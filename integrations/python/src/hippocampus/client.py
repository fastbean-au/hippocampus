"""The Hippocampus client.

One class, covering every RPC the contract declares. The methods on the memory and event surface
take and return the dataclasses in `models`; the operator surface - topology, the consolidation
preview and explanation, the forgotten log, the callback queue, transfer and archive - returns its
protobuf message unchanged, because those are read occasionally by a person and the message's field
comments are better documentation than a second set of names would be. `stub` reaches the generated
client directly for anything this does not wrap.

Nothing here is stateful beyond the channel: recall, reinforcement and decay all live in the
service, which is what lets a client be constructed per request if that suits the caller.
"""

from __future__ import annotations

from typing import Any, Iterable, List, Mapping, Optional, Sequence

import grpc

from hippocampus import _convert, errors
from hippocampus._proto import hippocampus_pb2 as pb
from hippocampus._proto import hippocampus_pb2_grpc as rpc
from hippocampus.models import (
    BatchStored,
    Event,
    Identity,
    Link,
    LinkDirection,
    Links,
    Memory,
    Page,
    Placement,
    SearchMode,
    SignificanceExtremum,
    SortDirection,
    Stored,
    events_page,
    link_protos,
    memories_page,
)

DEFAULT_TIMEOUT = 30.0


class Hippocampus:
    """A connected client.

    Use it as a context manager, or call close() when finished - the channel holds a connection and
    a background thread that will otherwise outlive the code that opened it.

        with Hippocampus("localhost:50051") as client:
            client.store_memory("the deploy at 14:03 rolled back cleanly", significance=50)

    Authentication and TLS are both off in a default deployment, so `address` alone is often all
    that is needed. Where they are on, `token` carries the bearer token and `tls` turns on
    transport security; `ca_cert` trusts a private CA, and `client_cert`/`client_key` present a
    client certificate to a listener configured to ask for one.

    There is deliberately no skip-verify option: the Python gRPC stack does not offer one, and
    faking it by trusting an arbitrary CA would be a different thing wearing the same name. Use
    `ca_cert` for a private CA and `server_name_override` for a certificate whose name does not
    match the address you dial.
    """

    def __init__(
        self,
        address: str,
        *,
        token: Optional[str] = None,
        tls: bool = False,
        ca_cert: Optional[bytes] = None,
        client_cert: Optional[bytes] = None,
        client_key: Optional[bytes] = None,
        server_name_override: Optional[str] = None,
        timeout: Optional[float] = DEFAULT_TIMEOUT,
        options: Optional[Sequence[tuple]] = None,
        channel: Optional[grpc.Channel] = None,
    ) -> None:
        self.timeout = timeout

        # A caller-supplied channel is taken as-is and never closed by us: it belongs to whoever
        # built it, and it is how a test drives this class over an in-process transport.
        self._owns_channel = channel is None

        if channel is None:
            channel = _build_channel(
                address,
                tls=tls or ca_cert is not None or client_cert is not None,
                ca_cert=ca_cert,
                client_cert=client_cert,
                client_key=client_key,
                server_name_override=server_name_override,
                options=options,
            )

        self._channel = channel
        self.stub = rpc.HippocampusStub(channel)

        # The token travels as per-call metadata rather than as channel credentials, which is what
        # keeps one code path for both transports: gRPC permits call credentials on a secure
        # channel only, so binding them to the channel would work against a TLS deployment and
        # raise against a plaintext one - and plaintext behind a TLS-terminating sidecar is a
        # supported deployment here, not a mistake to guard against.
        self._metadata = (("authorization", f"Bearer {token}"),) if token else ()

    # ---------------------------------------------------------------- lifecycle

    def close(self) -> None:
        """Close the channel, unless it was supplied by the caller."""

        if self._owns_channel:
            self._channel.close()

    def __enter__(self) -> "Hippocampus":
        return self

    def __exit__(self, *_: Any) -> None:
        self.close()

    def _call(self, method: Any, request: Any, timeout: Optional[float] = None) -> Any:
        """Invoke one RPC, attaching credentials and the deadline, and translating failures."""

        try:
            return method(
                request,
                timeout=self.timeout if timeout is None else timeout,
                metadata=self._metadata,
            )
        except grpc.RpcError as error:
            raise errors.translate(error) from error

    # ------------------------------------------------------- identity, topology

    def who_am_i(self, *, timeout: Optional[float] = None) -> Identity:
        """Report the caller's tier and scope, and what this deployment can serve.

        This is the feature-detection call: read `search_modes`, `summariser_enabled`,
        `consolidation_enabled`, `tombstones_enabled` and `callbacks_enabled` here rather than
        discovering an unavailable feature through a rejection.
        """

        return Identity.from_proto(
            self._call(self.stub.WhoAmI, pb.EmptyRequest(), timeout)
        )

    def topology(self, *, timeout: Optional[float] = None) -> pb.GetTopologyResponse:
        """What this instance is attached to, and the last known health of each dependency.

        Optional (topology.enabled), its required tier is configurable, and it is refused outright
        to a group-scoped caller - `Identity.topology_tier` reports whether it is available here.
        """

        return self._call(self.stub.GetTopology, pb.EmptyRequest(), timeout)

    def significance_levels(
        self,
        *,
        significance_min: int = 0,
        significance_max: int = 0,
        limit: int = 0,
        offset: int = 0,
        timeout: Optional[float] = None,
    ) -> Page:
        """The distinct significance values in use, ascending.

        These are the anchors a Placement can name. Two adjacent values have no room between them,
        which is what a placement exists to open.
        """

        request = pb.GetSignificanceLevelsRequest(
            significance_min=significance_min,
            significance_max=significance_max,
            limit=limit,
            offset=offset,
        )

        response = self._call(self.stub.GetSignificanceLevels, request, timeout)

        return Page(items=list(response.significances), total=response.total_count)

    # ----------------------------------------------------------------- memories

    def store_memory(
        self,
        memory: Any = None,
        significance: int = 0,
        *,
        id: str = "",
        event_id: Optional[str] = None,
        group: Optional[str] = None,
        metadata: Optional[Mapping[str, str]] = None,
        timestamp: _convert.Timestamp = None,
        binary: bool = False,
        links: Optional[Sequence[Link]] = None,
        placement: Optional[Placement] = None,
        external_bytes: int = 0,
        timeout: Optional[float] = None,
    ) -> Stored:
        """Store one memory, given either a Memory or a body plus its significance.

        A memory below the deployment's minimum significance is quietly dropped: the call succeeds,
        the returned id is empty and `rejected` is true. That is not an error - it is the store
        declining to retain the insignificant - so test the result rather than only the exception.
        """

        if isinstance(memory, Memory):
            message = memory.to_proto()
        else:
            message = Memory(
                body=memory or "",
                significance=significance,
                id=id,
                event_id=event_id,
                group=group,
                metadata=dict(metadata or {}),
                timestamp=timestamp,
                binary=binary,
                links=list(links or []),
                placement=placement,
                external_bytes=external_bytes,
            ).to_proto()

        response = self._call(self.stub.StoreMemory, message, timeout)

        return Stored(id=response.id, rejected=response.rejected)

    def store_memories(
        self,
        memories: Iterable[Memory],
        *,
        timeout: Optional[float] = None,
    ) -> BatchStored:
        """Store up to 500 unrelated memories, each validated and written independently.

        The call fails only for a batch-level fault; a memory that could not be written reports its
        own status in its own result, positionally. This is the write path and nothing is upserted:
        an id the store already holds fails with ALREADY_EXISTS in that memory's result rather than
        replacing a live row. `import_batch` is what upserts.
        """

        request = pb.StoreMemoriesRequest(
            memories=[memory.to_proto() for memory in memories]
        )

        return BatchStored.from_proto(
            self._call(self.stub.StoreMemories, request, timeout)
        )

    def update_memory(
        self,
        memory: Memory,
        *,
        clear_metadata: bool = False,
        clear_group: bool = False,
        timeout: Optional[float] = None,
    ) -> bool:
        """Partially update a memory. Unset fields are left unchanged.

        Two consequences of that rule need their own flags, because emptiness cannot express them:
        `clear_metadata` removes the stored metadata (an absent map and an empty one are the same
        on the wire), and `clear_group` resets the group. A non-empty metadata map REPLACES the
        stored one wholesale - there is no per-key merge.

        `significance` of 0 leaves the existing significance unchanged; it cannot reset a memory to
        unranked, and `external_bytes` of 0 reads the same way. `binary`, `is_summary` and the
        recall fields are not updatable.
        """

        message = memory.to_proto()
        message.clear_metadata = clear_metadata
        message.clear_group = clear_group

        return self._call(self.stub.UpdateMemory, message, timeout).ok

    def delete_memories(
        self,
        ids: Iterable[str],
        *,
        timeout: Optional[float] = None,
    ) -> bool:
        """Delete memories by id."""

        request = pb.DeleteMemoriesRequest(ids=list(ids))

        return self._call(self.stub.DeleteMemories, request, timeout).ok

    def delete_memories_by_filter(
        self,
        *,
        timestamp_min: _convert.Timestamp = None,
        timestamp_max: _convert.Timestamp = None,
        significance_min: int = 0,
        significance_max: int = 0,
        significance_extremum: Optional[SignificanceExtremum] = None,
        group: Optional[str] = None,
        metadata: Optional[Mapping[str, str]] = None,
        event_id: Optional[str] = None,
        has_event: Optional[bool] = None,
        recalled: Optional[bool] = None,
        recall_count_min: int = 0,
        recall_count_max: int = 0,
        time_recalled_min: _convert.Timestamp = None,
        time_recalled_max: _convert.Timestamp = None,
        is_summary: Optional[bool] = None,
        binary: Optional[bool] = None,
        max_deletions: int = 0,
        delete_empty_events: bool = False,
        timeout: Optional[float] = None,
    ) -> pb.DeleteMemoriesByFilterResponse:
        """Delete every memory a filter matches. Admin tier, and irreversible.

        The keyword arguments are `get_memories`' selecting ones, so the listing is the dry run:
        run `get_memories` with the same arguments and read `Page.total` to see what this would
        remove. A call carrying no filter at all is refused with INVALID_ARGUMENT - deleting
        everything is `purge`.

        `max_deletions` bounds one call; the response's `complete` says whether anything still
        matches.
        """

        request = pb.DeleteMemoriesByFilterRequest(
            timestamp_min=_convert.to_nanos(timestamp_min),
            timestamp_max=_convert.to_nanos(timestamp_max),
            significance_min=significance_min,
            significance_max=significance_max,
            group=group or "",
            metadata=_convert.metadata_to_pairs(metadata),
            event_id=event_id or "",
            has_event=_convert.to_tristate(has_event),
            recalled=_convert.to_tristate(recalled),
            recall_count_min=recall_count_min,
            recall_count_max=recall_count_max,
            time_recalled_min=_convert.to_nanos(time_recalled_min),
            time_recalled_max=_convert.to_nanos(time_recalled_max),
            is_summary=_convert.to_tristate(is_summary),
            is_binary=_convert.to_tristate(binary),
            max_deletions=max_deletions,
            delete_empty_events=delete_empty_events,
        )

        if significance_extremum is not None:
            request.significance_extremum = int(significance_extremum)

        return self._call(self.stub.DeleteMemoriesByFilter, request, timeout)

    def get_memories(
        self,
        *,
        timestamp_min: _convert.Timestamp = None,
        timestamp_max: _convert.Timestamp = None,
        significance_min: int = 0,
        significance_max: int = 0,
        significance_extremum: Optional[SignificanceExtremum] = None,
        group: Optional[str] = None,
        metadata: Optional[Mapping[str, str]] = None,
        event_id: Optional[str] = None,
        has_event: Optional[bool] = None,
        recalled: Optional[bool] = None,
        recall_count_min: int = 0,
        recall_count_max: int = 0,
        time_recalled_min: _convert.Timestamp = None,
        time_recalled_max: _convert.Timestamp = None,
        is_summary: Optional[bool] = None,
        binary: Optional[bool] = None,
        linked_to: Optional[str] = None,
        links: bool = False,
        order_by: str = "",
        order_dir: Optional[SortDirection] = None,
        limit: int = 0,
        offset: int = 0,
        timeout: Optional[float] = None,
    ) -> Page:
        """List memories. This is the read - it does not reinforce anything.

        The tri-state filters (`recalled`, `has_event`, `is_summary`, `binary`) exist because their
        bounds cannot express absence: `recall_count_max=0` means "no upper bound", not "never
        recalled", so `recalled=False` is what asks that question. Leaving one None applies no
        restriction.
        """

        request = pb.GetMemoriesRequest(
            timestamp_min=_convert.to_nanos(timestamp_min),
            timestamp_max=_convert.to_nanos(timestamp_max),
            significance_min=significance_min,
            significance_max=significance_max,
            group=group or "",
            metadata=_convert.metadata_to_pairs(metadata),
            event_id=event_id or "",
            has_event=_convert.to_tristate(has_event),
            recalled=_convert.to_tristate(recalled),
            recall_count_min=recall_count_min,
            recall_count_max=recall_count_max,
            time_recalled_min=_convert.to_nanos(time_recalled_min),
            time_recalled_max=_convert.to_nanos(time_recalled_max),
            is_summary=_convert.to_tristate(is_summary),
            is_binary=_convert.to_tristate(binary),
            linked_to=linked_to or "",
            links=links,
            order_by=order_by,
            limit=limit,
            offset=offset,
        )

        if significance_extremum is not None:
            request.significance_extremum = int(significance_extremum)

        if order_dir is not None:
            request.order_dir = int(order_dir)

        return memories_page(self._call(self.stub.GetMemories, request, timeout))

    def recall_memories(
        self,
        ids: Iterable[str],
        *,
        include_linked: bool = False,
        timeout: Optional[float] = None,
    ) -> Page:
        """Recall memories by id, reinforcing them.

        This is a WRITE: it resets each memory's decay clock and raises its effective significance.
        Use `get_memories` when you only want to read. `include_linked` returns each memory's
        neighbours as an associative recall - they are returned but their recall counts are
        untouched.

        An id the store no longer holds simply does not come back. Memories disappearing is the
        product rather than a fault.
        """

        request = pb.RecallMemoriesRequest(ids=list(ids), include_linked=include_linked)

        return memories_page(self._call(self.stub.RecallMemories, request, timeout))

    def search_memories(
        self,
        query: str,
        *,
        limit: int = 0,
        mode: Optional[SearchMode] = None,
        event_id: Optional[str] = None,
        group: Optional[str] = None,
        metadata: Optional[Mapping[str, str]] = None,
        reinforce: bool = False,
        include_linked: bool = False,
        timeout: Optional[float] = None,
    ) -> Page:
        """Search memory content, ranked by relevance blended with significance and recall.

        `mode` defaults to keyword. Which modes a deployment can serve is not universal - check
        `who_am_i().search_modes` rather than having an unavailable mode rejected with
        FAILED_PRECONDITION.

        With `reinforce`, the memories actually returned are recalled - only those, never the wider
        candidate set the ranking considered.
        """

        request = pb.SearchMemoriesRequest(
            query=query,
            limit=limit,
            event_id=event_id or "",
            reinforce=reinforce,
            group=group or "",
            include_linked=include_linked,
            metadata=_convert.metadata_to_pairs(metadata),
        )

        if mode is not None:
            request.mode = int(mode)

        return memories_page(self._call(self.stub.SearchMemories, request, timeout))

    # ------------------------------------------------------------- memory links

    def link_memories(
        self,
        id: str,
        links: Sequence[Link],
        *,
        timeout: Optional[float] = None,
    ) -> bool:
        """Add or re-weight links from one memory to others. An existing pair is overwritten."""

        request = pb.LinkMemoriesRequest(id=id, links=link_protos(links))

        return self._call(self.stub.LinkMemories, request, timeout).ok

    def unlink_memories(
        self,
        id: str,
        ids: Iterable[str],
        *,
        timeout: Optional[float] = None,
    ) -> bool:
        """Remove links between a memory and each target, in either direction."""

        request = pb.UnlinkMemoriesRequest(id=id, ids=list(ids))

        return self._call(self.stub.UnlinkMemories, request, timeout).ok

    def memory_links(
        self,
        id: str,
        *,
        direction: LinkDirection = LinkDirection.BOTH,
        timeout: Optional[float] = None,
    ) -> Links:
        """Read a memory's links. Both directions by default, since value is symmetric."""

        request = pb.GetMemoryLinksRequest(id=id, direction=int(direction))

        return Links.from_proto(self._call(self.stub.GetMemoryLinks, request, timeout))

    # ------------------------------------------------------------------- events

    def store_event(
        self,
        event: Any = None,
        significance: int = 0,
        *,
        id: str = "",
        description: str = "",
        group: Optional[str] = None,
        metadata: Optional[Mapping[str, str]] = None,
        started_at: _convert.Timestamp = None,
        ended_at: _convert.Timestamp = None,
        links: Optional[Sequence[Link]] = None,
        memories: Optional[Sequence[Memory]] = None,
        placement: Optional[Placement] = None,
        timeout: Optional[float] = None,
    ) -> Stored:
        """Store one event, given either an Event or a name plus its significance.

        Nested memories are best-effort: the event commits first, so one that fails validation or
        is dropped for insignificance does not fail the event and is not reported individually -
        `memory_count` on the result is only how many were actually retained. Store memories
        individually where you need per-memory outcomes.
        """

        if isinstance(event, Event):
            message = event.to_proto()
        else:
            message = Event(
                name=event or "",
                significance=significance,
                id=id,
                description=description,
                group=group,
                metadata=dict(metadata or {}),
                started_at=started_at,
                ended_at=ended_at,
                links=list(links or []),
                memories=list(memories or []),
                placement=placement,
            ).to_proto()

        response = self._call(self.stub.StoreEvent, message, timeout)

        return Stored(
            id=response.id,
            rejected=response.rejected,
            memory_count=response.memory_count,
        )

    def update_event(
        self,
        event: Event,
        *,
        clear_metadata: bool = False,
        clear_group: bool = False,
        timeout: Optional[float] = None,
    ) -> bool:
        """Partially update an event, on the same rules as `update_memory`."""

        message = event.to_proto()
        message.clear_metadata = clear_metadata
        message.clear_group = clear_group

        return self._call(self.stub.UpdateEvent, message, timeout).ok

    def end_event(
        self,
        id: str,
        *,
        at: _convert.Timestamp = None,
        timeout: Optional[float] = None,
    ) -> bool:
        """End an event. Omit `at` to use the server's current time."""

        request = pb.EndEventRequest(id=id, time_end=_convert.to_nanos(at))

        return self._call(self.stub.EndEvent, request, timeout).ok

    def update_event_significance(
        self,
        id: str,
        significance: int = 0,
        *,
        placement: Optional[Placement] = None,
        timeout: Optional[float] = None,
    ) -> bool:
        """Re-rank an event, absolutely or by placement.

        A significance of 0 with no placement is a no-op: it leaves the existing value unchanged,
        and there is no way to reset an event to unranked through this call.
        """

        request = pb.UpdateEventSignificanceRequest(id=id, significance=significance)

        if placement is not None:
            request.placement.CopyFrom(placement.to_proto())

        return self._call(self.stub.UpdateEventSignificance, request, timeout).ok

    def merge_events(
        self,
        merge_to: str,
        merge_from: str,
        *,
        timeout: Optional[float] = None,
    ) -> bool:
        """Move one event's memories onto another. The source need not exist."""

        request = pb.MergeEventsRequest(merge_to=merge_to, merge_from=merge_from)

        return self._call(self.stub.MergeEvents, request, timeout).ok

    def delete_event(
        self,
        id: str,
        *,
        memories: bool = False,
        timeout: Optional[float] = None,
    ) -> bool:
        """Delete an event, and with `memories` its memories too - otherwise they are detached."""

        request = pb.DeleteEventRequest(id=id, memories=memories)

        return self._call(self.stub.DeleteEvent, request, timeout).ok

    def get_event(
        self,
        id: str,
        *,
        memories: bool = False,
        memory_counts: bool = False,
        links: bool = False,
        timeout: Optional[float] = None,
    ) -> Event:
        """Fetch one event.

        `memories` loads every one of them into the response, which overruns the receive frame on a
        large event - `get_memories(event_id=...)` is the paged way to read them. `memory_counts`
        reports how many there are without transferring any.
        """

        request = pb.GetEventByIdRequest(
            id=id,
            memories=memories,
            memory_counts=memory_counts,
            links=links,
        )

        return Event.from_proto(
            self._call(self.stub.GetEventById, request, timeout).event
        )

    def delete_events_by_filter(
        self,
        *,
        time_start_min: _convert.Timestamp = None,
        time_start_max: _convert.Timestamp = None,
        time_end_min: _convert.Timestamp = None,
        time_end_max: _convert.Timestamp = None,
        significance_min: int = 0,
        significance_max: int = 0,
        significance_extremum: Optional[SignificanceExtremum] = None,
        group: Optional[str] = None,
        metadata: Optional[Mapping[str, str]] = None,
        ended: Optional[bool] = None,
        name_contains: Optional[str] = None,
        max_deletions: int = 0,
        delete_memories: bool = False,
        timeout: Optional[float] = None,
    ) -> pb.DeleteEventsByFilterResponse:
        """Delete every event a filter matches. Admin tier, and irreversible.

        `get_events` with the same arguments is the dry run, exactly as `get_memories` is for
        `delete_memories_by_filter`, and an empty filter is refused for the same reason.

        `delete_memories` says what happens to each event's memories: set, they go with it; left
        alone, they survive with no event. It says what to do with the selection rather than what to
        select, so a call carrying only that flag is still an empty filter.
        """

        request = pb.DeleteEventsByFilterRequest(
            time_start_min=_convert.to_nanos(time_start_min),
            time_start_max=_convert.to_nanos(time_start_max),
            time_end_min=_convert.to_nanos(time_end_min),
            time_end_max=_convert.to_nanos(time_end_max),
            significance_min=significance_min,
            significance_max=significance_max,
            group=group or "",
            metadata=_convert.metadata_to_pairs(metadata),
            ended=_convert.to_tristate(ended),
            name_contains=name_contains or "",
            max_deletions=max_deletions,
            delete_memories=delete_memories,
        )

        if significance_extremum is not None:
            request.significance_extremum = int(significance_extremum)

        return self._call(self.stub.DeleteEventsByFilter, request, timeout)

    def get_events(
        self,
        *,
        time_start_min: _convert.Timestamp = None,
        time_start_max: _convert.Timestamp = None,
        time_end_min: _convert.Timestamp = None,
        time_end_max: _convert.Timestamp = None,
        significance_min: int = 0,
        significance_max: int = 0,
        significance_extremum: Optional[SignificanceExtremum] = None,
        group: Optional[str] = None,
        metadata: Optional[Mapping[str, str]] = None,
        ended: Optional[bool] = None,
        name_contains: Optional[str] = None,
        linked_to: Optional[str] = None,
        memories: bool = False,
        memory_counts: bool = False,
        links: bool = False,
        order_by: str = "",
        order_dir: Optional[SortDirection] = None,
        limit: int = 0,
        offset: int = 0,
        timeout: Optional[float] = None,
    ) -> Page:
        """List events.

        `ended` is what answers "what is still running?" - `time_end_min` cannot, because an open
        event stores a time_end of 0 and 0 there means "no bound" rather than "not ended".
        """

        request = pb.GetEventsRequest(
            time_start_min=_convert.to_nanos(time_start_min),
            time_start_max=_convert.to_nanos(time_start_max),
            time_end_min=_convert.to_nanos(time_end_min),
            time_end_max=_convert.to_nanos(time_end_max),
            significance_min=significance_min,
            significance_max=significance_max,
            group=group or "",
            metadata=_convert.metadata_to_pairs(metadata),
            ended=_convert.to_tristate(ended),
            name_contains=name_contains or "",
            linked_to=linked_to or "",
            memories=memories,
            memory_counts=memory_counts,
            links=links,
            order_by=order_by,
            limit=limit,
            offset=offset,
        )

        if significance_extremum is not None:
            request.significance_extremum = int(significance_extremum)

        if order_dir is not None:
            request.order_dir = int(order_dir)

        return events_page(self._call(self.stub.GetEvents, request, timeout))

    # -------------------------------------------------------------- event links

    def link_events(
        self,
        id: str,
        links: Sequence[Link],
        *,
        timeout: Optional[float] = None,
    ) -> bool:
        """Add or re-weight links from one event to others."""

        request = pb.LinkEventsRequest(id=id, links=link_protos(links))

        return self._call(self.stub.LinkEvents, request, timeout).ok

    def unlink_events(
        self,
        id: str,
        ids: Iterable[str],
        *,
        timeout: Optional[float] = None,
    ) -> bool:
        """Remove links between an event and each target, in either direction."""

        request = pb.UnlinkEventsRequest(id=id, ids=list(ids))

        return self._call(self.stub.UnlinkEvents, request, timeout).ok

    def event_links(
        self,
        id: str,
        *,
        direction: LinkDirection = LinkDirection.BOTH,
        timeout: Optional[float] = None,
    ) -> Links:
        """Read an event's links."""

        request = pb.GetEventLinksRequest(id=id, direction=int(direction))

        return Links.from_proto(self._call(self.stub.GetEventLinks, request, timeout))

    # ----------------------------------------------------------- summarisation

    def summarisation_candidates(
        self,
        *,
        timeout: Optional[float] = None,
    ) -> pb.GetSummarisationCandidatesResponse:
        """Events the last sleep cycle judged worth condensing into a single summary memory.

        `scan_enabled` false means the scan is not configured here, so an empty list says nothing
        about whether any event qualifies.
        """

        return self._call(
            self.stub.GetSummarisationCandidates, pb.EmptyRequest(), timeout
        )

    def replace_memories_with_summary(
        self,
        event_id: str,
        summary: Memory,
        *,
        timeout: Optional[float] = None,
    ) -> pb.ReplaceMemoriesWithSummaryResponse:
        """Delete every memory for an event and insert one caller-authored summary in their place.

        One transaction, and the summary is validated before anything is deleted. This is the path
        for a deployment with no embedded LLM - the client writes the summary. `summarise_memories`
        is the same operation with the service authoring it.
        """

        request = pb.ReplaceMemoriesWithSummaryRequest(event_id=event_id)
        request.summary.CopyFrom(summary.to_proto())

        return self._call(self.stub.ReplaceMemoriesWithSummary, request, timeout)

    def summarise_memories(
        self,
        event_id: str,
        *,
        significance: int = 0,
        placement: Optional[Placement] = None,
        timeout: Optional[float] = None,
    ) -> pb.SummariseMemoriesResponse:
        """Have the service generate an event's summary and replace its memories with it.

        Needs an embedded LLM (`who_am_i().summariser_enabled`); FAILED_PRECONDITION without one.
        This is the one operation with visibility into memory content.
        """

        request = pb.SummariseMemoriesRequest(
            event_id=event_id, significance=significance
        )

        if placement is not None:
            request.placement.CopyFrom(placement.to_proto())

        return self._call(self.stub.SummariseMemories, request, timeout)

    # ------------------------------------------------------------ consolidation

    def sleep(self, *, timeout: Optional[float] = None) -> bool:
        """Run a consolidation cycle now, resetting the timer.

        Long - give it a deadline of its own. Refused on a replica, whose store is consolidated by
        whichever instance holds the single-consolidator lock.
        """

        return self._call(self.stub.Sleep, pb.EmptyRequest(), timeout).ok

    def purge(self, *, timeout: Optional[float] = None) -> bool:
        """Delete every event and memory. While it runs, every other RPC is rejected."""

        return self._call(self.stub.Purge, pb.EmptyRequest(), timeout).ok

    def consolidation_status(
        self,
        *,
        timeout: Optional[float] = None,
    ) -> pb.GetConsolidationStatusResponse:
        """When the next cycle is due, and what the last one did.

        The only member of the transparency set that does not refuse on a replica: reporting
        `consolidation_enabled` false is the answer there.
        """

        return self._call(self.stub.GetConsolidationStatus, pb.EmptyRequest(), timeout)

    def preview_consolidation(
        self,
        *,
        limit: int = 0,
        timeout: Optional[float] = None,
    ) -> pb.PreviewConsolidationResponse:
        """What a cycle would forget, deleting nothing. Admin tier."""

        request = pb.PreviewConsolidationRequest(limit=limit)

        return self._call(self.stub.PreviewConsolidation, request, timeout)

    def explain_consolidation(
        self,
        memory_ids: Iterable[str],
        *,
        curve_significance: float = 0.0,
        curve_max_age_days: float = 0.0,
        curve_points: int = 0,
        timeout: Optional[float] = None,
    ) -> pb.ExplainConsolidationResponse:
        """Where given memories stand against the decay threshold, and how long they have left.

        Reader tier, and it enumerates nothing - it answers only about ids you supply. The curve
        arguments describe the CONFIGURATION rather than any memory, and are for drawing; a caller
        deciding something wants `days_until_forgotten`.
        """

        request = pb.ExplainConsolidationRequest(memory_ids=list(memory_ids))

        if curve_significance or curve_max_age_days or curve_points:
            request.curve.significance = curve_significance
            request.curve.max_age_days = curve_max_age_days
            request.curve.points = curve_points

        return self._call(self.stub.ExplainConsolidation, request, timeout)

    def forgotten_memories(
        self,
        *,
        memory_id: Optional[str] = None,
        event_id: Optional[str] = None,
        group: Optional[str] = None,
        rule: int = 0,
        since: _convert.Timestamp = None,
        until: _convert.Timestamp = None,
        after_seq: int = 0,
        limit: int = 0,
        timeout: Optional[float] = None,
    ) -> pb.GetForgottenMemoriesResponse:
        """Read the forgotten log - the only surface that can speak about a deleted memory.

        Off by default. When it is off this returns an empty page rather than refusing, so
        `who_am_i().tombstones_enabled` is what distinguishes "nothing was written down" from
        "nothing has been forgotten".
        """

        request = pb.GetForgottenMemoriesRequest(
            memory_id=memory_id or "",
            event_id=event_id or "",
            group=group or "",
            rule=rule,
            since=_convert.to_nanos(since),
            until=_convert.to_nanos(until),
            after_seq=after_seq,
            limit=limit,
        )

        return self._call(self.stub.GetForgottenMemories, request, timeout)

    def delete_forgotten_memories(
        self,
        *,
        before: _convert.Timestamp = None,
        all: bool = False,
        timeout: Optional[float] = None,
    ) -> pb.DeleteForgottenMemoriesResponse:
        """Trim the forgotten log. Refuses a request that names neither a cutoff nor `all`."""

        request = pb.DeleteForgottenMemoriesRequest(
            before_time=_convert.to_nanos(before), all=all
        )

        return self._call(self.stub.DeleteForgottenMemories, request, timeout)

    def callback_queue(
        self,
        *,
        kind: int = 0,
        after_seq: int = 0,
        limit: int = 0,
        timeout: Optional[float] = None,
    ) -> pb.GetCallbackQueueResponse:
        """Outbound callback deliveries still waiting.

        With no sink configured this answers with an empty page rather than refusing, which on
        screen is a queue keeping up - `who_am_i().callbacks_enabled` is what tells the difference.
        """

        request = pb.GetCallbackQueueRequest(
            kind=kind, after_seq=after_seq, limit=limit
        )

        return self._call(self.stub.GetCallbackQueue, request, timeout)

    def delete_callback_queue(
        self,
        *,
        before: _convert.Timestamp = None,
        all: bool = False,
        timeout: Optional[float] = None,
    ) -> pb.DeleteCallbackQueueResponse:
        """Drop queued callback deliveries. An abandoned delivery is a notification nobody gets."""

        request = pb.DeleteCallbackQueueRequest(
            before_time=_convert.to_nanos(before), all=all
        )

        return self._call(self.stub.DeleteCallbackQueue, request, timeout)

    # ------------------------------------------------------- transfer, archive

    def export(
        self,
        *,
        clear: bool = False,
        timeout: Optional[float] = None,
    ) -> pb.ExportResponse:
        """Write the whole store to the configured object store.

        Long - give it its own deadline. With `clear`, exactly the records captured are deleted
        afterwards; a failed clear leaves the manifest cached so `clear(manifest_id)` can retry it.
        """

        return self._call(self.stub.Export, pb.ExportRequest(clear=clear), timeout)

    def import_(
        self,
        object_key: str,
        *,
        timeout: Optional[float] = None,
    ) -> pb.ImportResponse:
        """Read an archive back, upserting by id. Named with a trailing underscore: `import` is a
        Python keyword."""

        request = pb.ImportRequest(object_key=object_key)

        return self._call(self.stub.Import, request, timeout)

    def import_batch(
        self,
        *,
        events: Optional[Iterable[Event]] = None,
        memories: Optional[Iterable[Memory]] = None,
        timeout: Optional[float] = None,
    ) -> pb.ImportBatchResponse:
        """Upsert full records by id, with no defaulting and no minimum-significance gate.

        This is the only write that carries recall history, and it is idempotent - unlike
        `store_memories`, which is the write path and refuses an id the store already holds.
        """

        request = pb.ImportBatchRequest(
            events=[event.to_proto() for event in (events or [])],
            memories=[memory.to_proto() for memory in (memories or [])],
        )

        return self._call(self.stub.ImportBatch, request, timeout)

    def transfer(
        self,
        *,
        clear: bool = False,
        timeout: Optional[float] = None,
    ) -> pb.TransferResponse:
        """Push the whole store to the configured target instance. Long."""

        return self._call(self.stub.Transfer, pb.TransferRequest(clear=clear), timeout)

    def clear(
        self,
        manifest_id: str,
        *,
        timeout: Optional[float] = None,
    ) -> pb.ClearResponse:
        """Delete exactly the records a prior export or transfer captured."""

        request = pb.ClearRequest(manifest_id=manifest_id)

        return self._call(self.stub.Clear, request, timeout)


def _build_channel(
    address: str,
    *,
    tls: bool,
    ca_cert: Optional[bytes],
    client_cert: Optional[bytes],
    client_key: Optional[bytes],
    server_name_override: Optional[str],
    options: Optional[Sequence[tuple]],
) -> grpc.Channel:
    """Open a channel, with transport security when any TLS material was supplied."""

    channel_options: List[tuple] = list(options or [])

    if server_name_override:
        channel_options.append(("grpc.ssl_target_name_override", server_name_override))

    if not tls:
        return grpc.insecure_channel(address, options=channel_options)

    credentials = grpc.ssl_channel_credentials(
        root_certificates=ca_cert,
        private_key=client_key,
        certificate_chain=client_cert,
    )

    return grpc.secure_channel(address, credentials, options=channel_options)


def connect(address: str, **kwargs: Any) -> Hippocampus:
    """Open a client. A function-shaped alias for the constructor."""

    return Hippocampus(address, **kwargs)
