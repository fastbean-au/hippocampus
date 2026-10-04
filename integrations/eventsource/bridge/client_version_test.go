package bridge

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/fastbean-au/hippocampus/contract"
)

// versionCapturingServer records the client-version header of the one call it receives.
type versionCapturingServer struct {
	contract.UnimplementedHippocampusServer

	seen chan []string
}

func (v *versionCapturingServer) WhoAmI(ctx context.Context, _ *contract.EmptyRequest) (*contract.WhoAmIResponse, error) {
	v.seen <- metadata.ValueFromIncomingContext(ctx, contract.ClientVersionHeader)

	return &contract.WhoAmIResponse{}, nil
}

// TestDial_ReportsTheClientVersion is end to end over a real connection, because the failure this
// guards against is the interceptor never being installed - which no test of the interceptor alone
// can see, and which leaves the bridge's node on the service's deployment view with no version.
func TestDial_ReportsTheClientVersion(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	capture := &versionCapturingServer{seen: make(chan []string, 1)}
	server := grpc.NewServer()

	contract.RegisterHippocampusServer(server, capture)

	go func() { _ = server.Serve(listener) }()

	t.Cleanup(server.Stop)

	conn, client, err := Dial(ClientConfig{
		Address:       listener.Addr().String(),
		Token:         "tok",
		ClientVersion: "hippocampus-nats-bridge/v0.52.0",
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	defer func() { _ = conn.Close() }()

	if _, err := client.WhoAmI(context.Background(), &contract.EmptyRequest{}); err != nil {
		t.Fatalf("WhoAmI: %v", err)
	}

	if got := <-capture.seen; len(got) != 1 || got[0] != "hippocampus-nats-bridge/v0.52.0" {
		t.Errorf("the service received version header %v, want [hippocampus-nats-bridge/v0.52.0]", got)
	}
}
