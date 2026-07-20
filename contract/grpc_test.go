package contract_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/spf13/viper"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/hippocampus"
)

// newGRPCTestServer starts a real gRPC server - a real TCP listener plus grpc.NewServer and
// contract.RegisterHippocampusServer, exactly the shape cmd/hippocampus/main.go wires up for the
// native gRPC listener - hosting a real *hippocampus.Server over an in-memory SQLite db.DB. It
// returns a connected contract.HippocampusClient (dialed with insecure transport credentials, as
// there is no TLS in this test) plus the listen address, which
// TestRegisterHippocampusHandlerFromEndpoint needs to dial a second, independent connection.
//
// This - not the direct *hippocampus.Server call newGatewayTestServer uses - is what exercises
// hippocampus_grpc.pb.go: the client stub methods (each an Invoke over the connection),
// RegisterHippocampusServer, the grpc.ServiceDesc, and the generated _Hippocampus_Xxx_Handler
// dispatch functions the grpc-go runtime calls to decode a request and invoke the matching
// *hippocampus.Server method.
func newGRPCTestServer(t *testing.T) (contract.HippocampusClient, string) {
	t.Helper()

	viper.Reset()
	viper.Set("consolidation.enabled", true)
	viper.Set("sleep.periodSeconds", 0)

	database, err := db.New("")
	if err != nil {
		t.Fatalf("failed to create in-memory DB: %s", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	hipo := hippocampus.New(database, nil, nil)
	t.Cleanup(hipo.Stop)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %s", err)
	}

	grpcServer := grpc.NewServer()
	contract.RegisterHippocampusServer(grpcServer, hipo)

	serveErr := make(chan error, 1)
	go func() { serveErr <- grpcServer.Serve(lis) }()
	t.Cleanup(func() {
		grpcServer.Stop()
		<-serveErr
	})

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to dial %s: %s", lis.Addr().String(), err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return contract.NewHippocampusClient(conn), lis.Addr().String()
}

// TestGRPCEndToEnd drives every RPC directly through a real contract.HippocampusClient connected
// to a real grpc.Server, in dependency order, mirroring TestGatewayEndToEnd's REST walk but over
// native gRPC - the transport main.go's own gRPC listener actually serves.
func TestGRPCEndToEnd(t *testing.T) {
	client, _ := newGRPCTestServer(t)
	exerciseGRPCRPCs(t, client)
}

// exerciseGRPCRPCs drives every RPC directly through client, in dependency order. It is shared by
// TestGRPCEndToEnd (a grpc.NewServer with no interceptor configured, so the generated
// _Hippocampus_Xxx_Handler functions take their interceptor-is-nil branch, invoking the
// HippocampusServer method directly) and TestGRPCEndToEndWithInterceptor (a server configured with
// grpc.UnaryInterceptor, so the same handlers take their other branch instead: building a
// grpc.UnaryServerInfo/handler closure and invoking the interceptor) - between them exercising both
// branches of every generated handler in hippocampus_grpc.pb.go.
func exerciseGRPCRPCs(t *testing.T, client contract.HippocampusClient) {
	t.Helper()

	ctx := context.Background()

	storeEventRes, err := client.StoreEvent(ctx, &contract.Event{Name: "event one", Significance: 5, Group: "test-group"})
	if err != nil {
		t.Fatalf("StoreEvent: %s", err)
	}

	eventID := storeEventRes.GetId()
	if eventID == "" {
		t.Fatal("StoreEvent: expected a non-empty id")
	}

	secondEventRes, err := client.StoreEvent(ctx, &contract.Event{Name: "event two", Significance: 3})
	if err != nil {
		t.Fatalf("StoreEvent (second): %s", err)
	}

	secondEventID := secondEventRes.GetId()

	getEventRes, err := client.GetEventById(ctx, &contract.GetEventByIdRequest{Id: eventID, Memories: true})
	if err != nil {
		t.Fatalf("GetEventById: %s", err)
	}

	if got := getEventRes.GetEvent().GetId(); got != eventID {
		t.Errorf("GetEventById: got id %q, want %q", got, eventID)
	}

	getEventsRes, err := client.GetEvents(ctx, &contract.GetEventsRequest{Limit: 10, OrderBy: "significance"})
	if err != nil {
		t.Fatalf("GetEvents: %s", err)
	}

	if got := getEventsRes.GetTotalCount(); got < 2 {
		t.Errorf("GetEvents: total_count = %d, want >= 2", got)
	}

	if _, err := client.EndEvent(ctx, &contract.EndEventRequest{Id: eventID}); err != nil {
		t.Fatalf("EndEvent: %s", err)
	}

	if _, err := client.UpdateEventSignificance(ctx, &contract.UpdateEventSignificanceRequest{Id: eventID, Significance: 7}); err != nil {
		t.Fatalf("UpdateEventSignificance: %s", err)
	}

	if _, err := client.MergeEvents(ctx, &contract.MergeEventsRequest{MergeTo: eventID, MergeFrom: secondEventID}); err != nil {
		t.Fatalf("MergeEvents: %s", err)
	}

	storeMemRes, err := client.StoreMemory(ctx, &contract.Memory{Significance: 4, Body: "first memory", EventId: eventID, Group: "test-group"})
	if err != nil {
		t.Fatalf("StoreMemory: %s", err)
	}

	memoryID := storeMemRes.GetId()
	if memoryID == "" {
		t.Fatal("StoreMemory: expected a non-empty id")
	}

	secondMemRes, err := client.StoreMemory(ctx, &contract.Memory{Significance: 2, Body: "second memory", EventId: eventID})
	if err != nil {
		t.Fatalf("StoreMemory (second): %s", err)
	}

	secondMemoryID := secondMemRes.GetId()

	if _, err := client.UpdateMemory(ctx, &contract.Memory{Id: memoryID, Body: "first memory, updated"}); err != nil {
		t.Fatalf("UpdateMemory: %s", err)
	}

	getMemsRes, err := client.GetMemories(ctx, &contract.GetMemoriesRequest{Limit: 10})
	if err != nil {
		t.Fatalf("GetMemories: %s", err)
	}

	if got := getMemsRes.GetTotalCount(); got < 2 {
		t.Errorf("GetMemories: total_count = %d, want >= 2", got)
	}

	recallRes, err := client.RecallMemories(ctx, &contract.RecallMemoriesRequest{Ids: []string{memoryID}})
	if err != nil {
		t.Fatalf("RecallMemories: %s", err)
	}

	if len(recallRes.GetMemories()) != 1 {
		t.Errorf("RecallMemories: got %d memories, want 1", len(recallRes.GetMemories()))
	}

	// No search index is configured on this test server, so FailedPrecondition is the expected,
	// real outcome here - still real client/handler code, just its error branch.
	if _, err := client.SearchMemories(ctx, &contract.SearchMemoriesRequest{Query: "memory"}); err == nil {
		t.Error("SearchMemories: expected an error (search index not configured)")
	}

	summaryRes, err := client.ReplaceMemoriesWithSummary(ctx, &contract.ReplaceMemoriesWithSummaryRequest{
		EventId: eventID,
		Summary: &contract.Memory{Significance: 9, Body: "condensed summary"},
	})
	if err != nil {
		t.Fatalf("ReplaceMemoriesWithSummary: %s", err)
	}

	if summaryRes.GetId() == "" {
		t.Error("ReplaceMemoriesWithSummary: expected a non-empty summary memory id")
	}

	if _, err := client.GetSummarizationCandidates(ctx, &contract.EmptyRequest{}); err != nil {
		t.Fatalf("GetSummarizationCandidates: %s", err)
	}

	if _, err := client.DeleteMemories(ctx, &contract.DeleteMemoriesRequest{Ids: []string{secondMemoryID}}); err != nil {
		t.Fatalf("DeleteMemories: %s", err)
	}

	if _, err := client.DeleteEvent(ctx, &contract.DeleteEventRequest{Id: secondEventID, Memories: true}); err != nil {
		t.Fatalf("DeleteEvent: %s", err)
	}

	sleepRes, err := client.Sleep(ctx, &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("Sleep: %s", err)
	}

	if !sleepRes.GetOk() {
		t.Error("Sleep: expected ok=true")
	}

	// Transfer/archive surface: exercised for their real, reachable-without-infra error branches,
	// since neither S3 nor a transfer target is configured for this test server.
	if _, err := client.Export(ctx, &contract.ExportRequest{}); err == nil {
		t.Error("Export: expected an error (no object store configured)")
	}

	if _, err := client.Import(ctx, &contract.ImportRequest{ObjectKey: "x"}); err == nil {
		t.Error("Import: expected an error (no object store configured)")
	}

	importBatchRes, err := client.ImportBatch(ctx, &contract.ImportBatchRequest{
		Events:   []*contract.Event{{Id: "imported-event", Name: "imported", TimeStart: 1, Significance: 1}},
		Memories: []*contract.Memory{{Id: "imported-memory", EventId: "imported-event", TimeStamp: 1, Significance: 1, Body: "x"}},
	})
	if err != nil {
		t.Fatalf("ImportBatch: %s", err)
	}

	if importBatchRes.GetEventsImported() != 1 || importBatchRes.GetMemoriesImported() != 1 {
		t.Errorf("ImportBatch: got events=%d memories=%d, want 1/1", importBatchRes.GetEventsImported(), importBatchRes.GetMemoriesImported())
	}

	if _, err := client.Transfer(ctx, &contract.TransferRequest{}); err == nil {
		t.Error("Transfer: expected an error (no transfer target configured)")
	}

	if _, err := client.Clear(ctx, &contract.ClearRequest{ManifestId: "does-not-exist"}); err == nil {
		t.Error("Clear: expected an error (unknown manifest)")
	}

	// Purge last: it wipes everything the rest of this test relies on.
	purgeRes, err := client.Purge(ctx, &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("Purge: %s", err)
	}

	if !purgeRes.GetOk() {
		t.Error("Purge: expected ok=true")
	}
}

// TestGatewayClientForwarding wires the HTTP gateway the other way round from
// TestGatewayEndToEnd: contract.RegisterHippocampusHandlerClient forwards each decoded request over
// a real contract.HippocampusClient/grpc.ClientConn to a real grpc.Server, rather than calling a
// *hippocampus.Server directly. It reuses exerciseGatewayRPCs (gateway_test.go) so the same REST
// walk now exercises the "request_Hippocampus_*" (client-forwarding) half of every generated
// handler in hippocampus.pb.gw.go, which TestGatewayEndToEnd's direct wiring never reaches.
func TestGatewayClientForwarding(t *testing.T) {
	client, _ := newGRPCTestServer(t)

	gwMux := runtime.NewServeMux()
	if err := contract.RegisterHippocampusHandlerClient(context.Background(), gwMux, client); err != nil {
		t.Fatalf("failed to register HTTP gateway (client forwarding): %s", err)
	}

	server := httptest.NewServer(gwMux)
	t.Cleanup(server.Close)

	exerciseGatewayRPCs(t, server)
}

// TestRegisterHippocampusHandlerFromEndpoint covers the remaining two registration entry points
// (RegisterHippocampusHandlerFromEndpoint, which dials endpoint itself, and RegisterHippocampusHandler,
// the thin wrapper between it and RegisterHippocampusHandlerClient) with a couple of smoke calls -
// TestGatewayClientForwarding above already drives the generated handlers exhaustively via a
// pre-built client, so this only needs to prove the dial-it-yourself entry point wires up the same
// way.
func TestRegisterHippocampusHandlerFromEndpoint(t *testing.T) {
	_, addr := newGRPCTestServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	gwMux := runtime.NewServeMux()

	dialOpts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	if err := contract.RegisterHippocampusHandlerFromEndpoint(ctx, gwMux, addr, dialOpts); err != nil {
		t.Fatalf("RegisterHippocampusHandlerFromEndpoint: %s", err)
	}

	server := httptest.NewServer(gwMux)
	t.Cleanup(server.Close)

	var purgeRes contract.GeneralResponse

	status, body := doJSON(t, server, http.MethodPost, "/v1/purge", map[string]any{}, &purgeRes)
	if status != http.StatusOK {
		t.Fatalf("Purge: status = %d, body = %s", status, body)
	}

	if !purgeRes.GetOk() {
		t.Error("Purge: expected ok=true")
	}
}

// TestGRPCEndToEndWithInterceptor re-runs the same RPC walk as TestGRPCEndToEnd, but against a
// grpc.Server configured with a real grpc.UnaryServerInterceptor (main.go always configures at
// least InterceptorLogger, so this is the shape a production listener actually serves). Each
// generated _Hippocampus_Xxx_Handler function in hippocampus_grpc.pb.go branches on whether an
// interceptor is configured; TestGRPCEndToEnd's plain grpc.NewServer() takes the nil branch
// (invoking the HippocampusServer method directly), so this test is what reaches the other one -
// building the grpc.UnaryServerInfo/handler closure and invoking the interceptor.
func TestGRPCEndToEndWithInterceptor(t *testing.T) {
	viper.Reset()
	viper.Set("consolidation.enabled", true)
	viper.Set("sleep.periodSeconds", 0)

	database, err := db.New("")
	if err != nil {
		t.Fatalf("failed to create in-memory DB: %s", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	hipo := hippocampus.New(database, nil, nil)
	t.Cleanup(hipo.Stop)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %s", err)
	}

	var interceptorCalls int

	passThrough := func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		interceptorCalls++

		return handler(ctx, req)
	}

	grpcServer := grpc.NewServer(grpc.UnaryInterceptor(passThrough))
	contract.RegisterHippocampusServer(grpcServer, hipo)

	serveErr := make(chan error, 1)
	go func() { serveErr <- grpcServer.Serve(lis) }()
	t.Cleanup(func() {
		grpcServer.Stop()
		<-serveErr
	})

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to dial %s: %s", lis.Addr().String(), err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	client := contract.NewHippocampusClient(conn)

	exerciseGRPCRPCs(t, client)

	if interceptorCalls == 0 {
		t.Error("expected the unary interceptor to have been invoked at least once")
	}
}

// TestUnimplementedHippocampusServer covers contract.UnimplementedHippocampusServer, the embeddable
// fallback every HippocampusServer implementation must embed for forward compatibility: every
// method must return codes.Unimplemented rather than panicking, which is what lets the generated
// _Hippocampus_Xxx_Handler dispatch functions safely type-assert srv.(HippocampusServer) even for
// RPCs a given implementation (deliberately or not yet) leaves unimplemented.
func TestUnimplementedHippocampusServer(t *testing.T) {
	ctx := context.Background()
	srv := contract.UnimplementedHippocampusServer{}

	assertUnimplemented := func(t *testing.T, name string, err error) {
		t.Helper()

		if err == nil {
			t.Fatalf("%s: expected an error, got nil", name)
		}

		if got := status.Code(err); got != codes.Unimplemented {
			t.Errorf("%s: got code %s, want %s", name, got, codes.Unimplemented)
		}
	}

	_, err := srv.Purge(ctx, &contract.EmptyRequest{})
	assertUnimplemented(t, "Purge", err)

	_, err = srv.Sleep(ctx, &contract.EmptyRequest{})
	assertUnimplemented(t, "Sleep", err)

	_, err = srv.StoreEvent(ctx, &contract.Event{})
	assertUnimplemented(t, "StoreEvent", err)

	_, err = srv.EndEvent(ctx, &contract.EndEventRequest{})
	assertUnimplemented(t, "EndEvent", err)

	_, err = srv.UpdateEventSignificance(ctx, &contract.UpdateEventSignificanceRequest{})
	assertUnimplemented(t, "UpdateEventSignificance", err)

	_, err = srv.MergeEvents(ctx, &contract.MergeEventsRequest{})
	assertUnimplemented(t, "MergeEvents", err)

	_, err = srv.DeleteEvent(ctx, &contract.DeleteEventRequest{})
	assertUnimplemented(t, "DeleteEvent", err)

	_, err = srv.GetEventById(ctx, &contract.GetEventByIdRequest{})
	assertUnimplemented(t, "GetEventById", err)

	_, err = srv.GetEvents(ctx, &contract.GetEventsRequest{})
	assertUnimplemented(t, "GetEvents", err)

	_, err = srv.StoreMemory(ctx, &contract.Memory{})
	assertUnimplemented(t, "StoreMemory", err)

	_, err = srv.UpdateMemory(ctx, &contract.Memory{})
	assertUnimplemented(t, "UpdateMemory", err)

	_, err = srv.DeleteMemories(ctx, &contract.DeleteMemoriesRequest{})
	assertUnimplemented(t, "DeleteMemories", err)

	_, err = srv.GetMemories(ctx, &contract.GetMemoriesRequest{})
	assertUnimplemented(t, "GetMemories", err)

	_, err = srv.RecallMemories(ctx, &contract.RecallMemoriesRequest{})
	assertUnimplemented(t, "RecallMemories", err)

	_, err = srv.SearchMemories(ctx, &contract.SearchMemoriesRequest{})
	assertUnimplemented(t, "SearchMemories", err)

	_, err = srv.ReplaceMemoriesWithSummary(ctx, &contract.ReplaceMemoriesWithSummaryRequest{})
	assertUnimplemented(t, "ReplaceMemoriesWithSummary", err)

	_, err = srv.GetSummarizationCandidates(ctx, &contract.EmptyRequest{})
	assertUnimplemented(t, "GetSummarizationCandidates", err)

	_, err = srv.Export(ctx, &contract.ExportRequest{})
	assertUnimplemented(t, "Export", err)

	_, err = srv.Import(ctx, &contract.ImportRequest{})
	assertUnimplemented(t, "Import", err)

	_, err = srv.ImportBatch(ctx, &contract.ImportBatchRequest{})
	assertUnimplemented(t, "ImportBatch", err)

	_, err = srv.Transfer(ctx, &contract.TransferRequest{})
	assertUnimplemented(t, "Transfer", err)

	_, err = srv.Clear(ctx, &contract.ClearRequest{})
	assertUnimplemented(t, "Clear", err)
}

// TestGatewayClientForwardingMalformedRequests mirrors TestGatewayClientForwarding the way
// TestGatewayMalformedRequests mirrors TestGatewayEndToEnd: it runs the same request-decode
// error-path walk (exerciseGatewayErrorPaths, gateway_test.go) against the client-forwarding
// gateway wiring instead of the direct-to-*hippocampus.Server one, so the "request_" (not
// "local_request_") half of every generated decode-error branch in hippocampus.pb.gw.go gets
// exercised too.
func TestGatewayClientForwardingMalformedRequests(t *testing.T) {
	client, _ := newGRPCTestServer(t)

	gwMux := runtime.NewServeMux()
	if err := contract.RegisterHippocampusHandlerClient(context.Background(), gwMux, client); err != nil {
		t.Fatalf("failed to register HTTP gateway (client forwarding): %s", err)
	}

	server := httptest.NewServer(gwMux)
	t.Cleanup(server.Close)

	exerciseGatewayErrorPaths(t, server)
}

// TestRegisterHippocampusHandlerFromEndpointDialError exercises
// RegisterHippocampusHandlerFromEndpoint's error path: grpc.NewClient itself rejecting the dial
// (here, deliberately omitting transport credentials - a realistic misconfiguration, not a
// synthetic one) before RegisterHippocampusHandler is ever reached, and the deferred cleanup that
// closes the freshly-dialed connection on that error.
func TestRegisterHippocampusHandlerFromEndpointDialError(t *testing.T) {
	gwMux := runtime.NewServeMux()

	// No grpc.WithTransportCredentials(...) - grpc.NewClient refuses to dial without transport
	// security configured one way or another.
	err := contract.RegisterHippocampusHandlerFromEndpoint(context.Background(), gwMux, "127.0.0.1:0", nil)
	if err == nil {
		t.Fatal("expected an error when dialing without transport credentials")
	}
}

// TestGRPCClientTransportError dials a client at an address nobody is listening on (a TCP listener
// opened then immediately closed, freeing the port), so every call below fails at the transport
// level before ever reaching a server. That is the "if err != nil { return nil, err }" branch every
// generated *hippocampusClient method in hippocampus_grpc.pb.go has right after its cc.Invoke call -
// TestGRPCEndToEnd's happy-path walk only reaches it for the handful of RPCs (SearchMemories,
// Export, Import, Transfer, Clear) whose *application* error happens to originate below the
// transport, so this is what reaches it for the rest.
func TestGRPCClientTransportError(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %s", err)
	}

	addr := lis.Addr().String()
	if err := lis.Close(); err != nil {
		t.Fatalf("failed to close listener: %s", err)
	}

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to build client: %s", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	client := contract.NewHippocampusClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	assertTransportErr := func(t *testing.T, name string, err error) {
		t.Helper()

		if err == nil {
			t.Errorf("%s: expected a transport error dialing a closed listener, got nil", name)
		}
	}

	_, err = client.Purge(ctx, &contract.EmptyRequest{})
	assertTransportErr(t, "Purge", err)

	_, err = client.Sleep(ctx, &contract.EmptyRequest{})
	assertTransportErr(t, "Sleep", err)

	_, err = client.StoreEvent(ctx, &contract.Event{})
	assertTransportErr(t, "StoreEvent", err)

	_, err = client.EndEvent(ctx, &contract.EndEventRequest{})
	assertTransportErr(t, "EndEvent", err)

	_, err = client.UpdateEventSignificance(ctx, &contract.UpdateEventSignificanceRequest{})
	assertTransportErr(t, "UpdateEventSignificance", err)

	_, err = client.MergeEvents(ctx, &contract.MergeEventsRequest{})
	assertTransportErr(t, "MergeEvents", err)

	_, err = client.DeleteEvent(ctx, &contract.DeleteEventRequest{})
	assertTransportErr(t, "DeleteEvent", err)

	_, err = client.GetEventById(ctx, &contract.GetEventByIdRequest{})
	assertTransportErr(t, "GetEventById", err)

	_, err = client.GetEvents(ctx, &contract.GetEventsRequest{})
	assertTransportErr(t, "GetEvents", err)

	_, err = client.StoreMemory(ctx, &contract.Memory{})
	assertTransportErr(t, "StoreMemory", err)

	_, err = client.UpdateMemory(ctx, &contract.Memory{})
	assertTransportErr(t, "UpdateMemory", err)

	_, err = client.DeleteMemories(ctx, &contract.DeleteMemoriesRequest{})
	assertTransportErr(t, "DeleteMemories", err)

	_, err = client.GetMemories(ctx, &contract.GetMemoriesRequest{})
	assertTransportErr(t, "GetMemories", err)

	_, err = client.RecallMemories(ctx, &contract.RecallMemoriesRequest{})
	assertTransportErr(t, "RecallMemories", err)

	_, err = client.SearchMemories(ctx, &contract.SearchMemoriesRequest{})
	assertTransportErr(t, "SearchMemories", err)

	_, err = client.ReplaceMemoriesWithSummary(ctx, &contract.ReplaceMemoriesWithSummaryRequest{})
	assertTransportErr(t, "ReplaceMemoriesWithSummary", err)

	_, err = client.GetSummarizationCandidates(ctx, &contract.EmptyRequest{})
	assertTransportErr(t, "GetSummarizationCandidates", err)

	_, err = client.Export(ctx, &contract.ExportRequest{})
	assertTransportErr(t, "Export", err)

	_, err = client.Import(ctx, &contract.ImportRequest{})
	assertTransportErr(t, "Import", err)

	_, err = client.ImportBatch(ctx, &contract.ImportBatchRequest{})
	assertTransportErr(t, "ImportBatch", err)

	_, err = client.Transfer(ctx, &contract.TransferRequest{})
	assertTransportErr(t, "Transfer", err)

	_, err = client.Clear(ctx, &contract.ClearRequest{})
	assertTransportErr(t, "Clear", err)
}
