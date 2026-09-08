"""Typed exceptions over gRPC status codes.

Every call raises one of these rather than grpc.RpcError, so a caller branches on a class instead
of comparing status codes, and the original error is always available as __cause__.

Three of them mean something specific to this service, which is the reason the mapping is worth
having at all rather than passing grpc.RpcError through:

- NotFound may mean "not yours". Under group scoping an out-of-scope record is reported exactly as
  a nonexistent one, deliberately, so that the error cannot be used to prove a record exists.
- Unavailable is routinely a purge, not an outage. While Purge runs every other RPC is rejected
  with this code; it is brief, and retrying is right.
- FailedPrecondition is how a deployment says it cannot serve a feature - a search mode with no
  backend, SummariseMemories with no embedded LLM, Sleep on a replica. Feature-detect with
  who_am_i() rather than discovering it here.
"""

from __future__ import annotations

from typing import Optional

import grpc


class HippocampusError(Exception):
    """Base class for every error this client raises."""

    def __init__(self, message: str, code: Optional[grpc.StatusCode] = None) -> None:
        super().__init__(message)

        self.message = message
        self.code = code


class Unauthenticated(HippocampusError):
    """No token was presented, or the one presented did not verify."""


class PermissionDenied(HippocampusError):
    """The token verified, but its role is too low for this RPC, or its group scope excludes it."""


class NotFound(HippocampusError):
    """No such record - or, under group scoping, none this token may see. The two are deliberately
    indistinguishable, so this is not proof that an id was never valid."""


class AlreadyExists(HippocampusError):
    """The store already holds that id. Writes do not upsert; ImportBatch is what does."""


class InvalidArgument(HippocampusError):
    """The request failed validation. Over the gateway this shares its HTTP status with
    FailedPrecondition and OutOfRange, but on gRPC the three are distinct."""


class FailedPrecondition(HippocampusError):
    """The deployment cannot serve this - an unconfigured search mode, no embedded summariser, or
    a consolidation RPC on a replica. who_am_i() reports which of these apply before you call."""


class ResourceExhausted(HippocampusError):
    """A rate limit or a size cap was hit."""


class Unavailable(HippocampusError):
    """The service is not accepting calls. Most often a purge in progress, which is brief and
    worth retrying, rather than an instance being down."""


class DeadlineExceeded(HippocampusError):
    """The call's deadline passed. Consolidation, export and transfer are long operations and the
    service imposes no deadline of its own, so the bound is the client's."""


class ServiceError(HippocampusError):
    """Any status without a more specific class here."""


_BY_CODE = {
    grpc.StatusCode.UNAUTHENTICATED: Unauthenticated,
    grpc.StatusCode.PERMISSION_DENIED: PermissionDenied,
    grpc.StatusCode.NOT_FOUND: NotFound,
    grpc.StatusCode.ALREADY_EXISTS: AlreadyExists,
    grpc.StatusCode.INVALID_ARGUMENT: InvalidArgument,
    grpc.StatusCode.FAILED_PRECONDITION: FailedPrecondition,
    grpc.StatusCode.RESOURCE_EXHAUSTED: ResourceExhausted,
    grpc.StatusCode.UNAVAILABLE: Unavailable,
    grpc.StatusCode.DEADLINE_EXCEEDED: DeadlineExceeded,
}


def for_code(code: grpc.StatusCode) -> type:
    """The exception class a status code maps to."""

    return _BY_CODE.get(code, ServiceError)


def translate(error: grpc.RpcError) -> HippocampusError:
    """Turn a grpc.RpcError into the matching typed exception, preserving code and message."""

    # grpc.RpcError is only usefully typed when it is also a grpc.Call, which every error raised by
    # a blocking unary call is. Anything else (a channel-level failure with no status) still has to
    # produce an exception of ours rather than escape as a raw RpcError.
    if isinstance(error, grpc.Call):
        code = error.code()
        message = error.details() or str(error)
    else:
        code = None
        message = str(error)

    if code is None:
        return ServiceError(message)

    return for_code(code)(message, code)
