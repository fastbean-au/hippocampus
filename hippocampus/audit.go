package hippocampus

import (
	"context"

	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/fastbean-au/hippocampus/auth"
	"github.com/fastbean-au/hippocampus/contract"
)

// The audit trail for administrative mutations (TODO-3 item 169).
//
// A Purge, a predicate delete, a Clear or an emptied queue was logged at Trace if at all, so the only
// record of who emptied a store was the absence of the store. Every call to an RPC the policy table
// marks as an admin-tier mutation (auth.AdminMutations) is now one Info line carrying audit=true, the
// RPC, the caller's client_id, the outcome code, the request and the response: the filter a deletion
// ran with and what it removed are the two facts an audit trail exists for.
//
// It is a decorator rather than a call in each handler, because both transports serve one value -
// the gateway calls the server directly and never runs the gRPC interceptor chain - so wrapping that
// value is the single place that reaches both. A refused or failed call is audited too: an attempt
// is part of the trail. TestEveryAdminMutationIsAudited drives every RPC in that set through this
// type by name, so a new administrative RPC fails the build until it has an override here.

// auditedServer is a Server whose administrative mutations are audited.
type auditedServer struct {
	*Server
}

// Audited returns the server as both transports should serve it: unchanged, except that every
// administrative mutation leaves an audit line.
func Audited(s *Server) contract.HippocampusServer {
	return auditedServer{Server: s}
}

// audit runs one administrative call and records it.
func audit[Req proto.Message, Res proto.Message](
	ctx context.Context,
	rpc string,
	in Req,
	call func(context.Context, Req) (Res, error),
) (Res, error) {
	res, err := call(ctx, in)

	clientId := auth.ClientIDFromContext(ctx)
	if clientId == "" {
		clientId = "none"
	}

	fields := log.Fields{
		"audit":     true,
		"rpc":       rpc,
		"client_id": clientId,
		"outcome":   status.Code(err).String(),
		"request":   auditJSON(in),
	}

	if err == nil {
		fields["response"] = auditJSON(res)
	}

	log.WithFields(fields).
		Info("administrative call")

	return res, err
}

// auditJSON renders a message on one line for the log. These messages carry filters, manifest ids,
// object keys and counts, never a memory body or a credential.
func auditJSON(message proto.Message) string {
	if message == nil {
		return ""
	}

	rendered, err := protojson.MarshalOptions{EmitUnpopulated: false}.Marshal(message)
	if err != nil {
		return "(unrenderable)"
	}

	return string(rendered)
}

func (a auditedServer) Purge(ctx context.Context, in *contract.EmptyRequest) (*contract.GeneralResponse, error) {
	return audit(ctx, "Purge", in, a.Server.Purge)
}

func (a auditedServer) Sleep(ctx context.Context, in *contract.EmptyRequest) (*contract.GeneralResponse, error) {
	return audit(ctx, "Sleep", in, a.Server.Sleep)
}

func (a auditedServer) DeleteForgottenMemories(
	ctx context.Context,
	in *contract.DeleteForgottenMemoriesRequest,
) (*contract.DeleteForgottenMemoriesResponse, error) {
	return audit(ctx, "DeleteForgottenMemories", in, a.Server.DeleteForgottenMemories)
}

func (a auditedServer) DeleteCallbackQueue(
	ctx context.Context,
	in *contract.DeleteCallbackQueueRequest,
) (*contract.DeleteCallbackQueueResponse, error) {
	return audit(ctx, "DeleteCallbackQueue", in, a.Server.DeleteCallbackQueue)
}

func (a auditedServer) DeleteMemoriesByFilter(
	ctx context.Context,
	in *contract.DeleteMemoriesByFilterRequest,
) (*contract.DeleteMemoriesByFilterResponse, error) {
	return audit(ctx, "DeleteMemoriesByFilter", in, a.Server.DeleteMemoriesByFilter)
}

func (a auditedServer) DeleteEventsByFilter(
	ctx context.Context,
	in *contract.DeleteEventsByFilterRequest,
) (*contract.DeleteEventsByFilterResponse, error) {
	return audit(ctx, "DeleteEventsByFilter", in, a.Server.DeleteEventsByFilter)
}

func (a auditedServer) Export(ctx context.Context, in *contract.ExportRequest) (*contract.ExportResponse, error) {
	return audit(ctx, "Export", in, a.Server.Export)
}

func (a auditedServer) Transfer(ctx context.Context, in *contract.TransferRequest) (*contract.TransferResponse, error) {
	return audit(ctx, "Transfer", in, a.Server.Transfer)
}

func (a auditedServer) Clear(ctx context.Context, in *contract.ClearRequest) (*contract.ClearResponse, error) {
	return audit(ctx, "Clear", in, a.Server.Clear)
}
