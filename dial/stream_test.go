package dial

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/fastbean-au/hippocampus/contract"
)

// watchCapturingServer records the metadata of the one Watch it receives, then ends the stream.
type watchCapturingServer struct {
	contract.UnimplementedHippocampusServer

	seen chan metadata.MD
}

func (w *watchCapturingServer) Watch(_ *contract.WatchRequest, stream grpc.ServerStreamingServer[contract.WatchEvent]) error {
	md, _ := metadata.FromIncomingContext(stream.Context())

	w.seen <- md

	return nil
}

// TestDial_AStreamCarriesTheTokenAndVersion: the token and the version header were attached by
// unary interceptors only, so Watch - the first streaming RPC - reached the service without either,
// and was refused as unauthenticated by any service with auth on.
func TestDial_AStreamCarriesTheTokenAndVersion(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	capture := &watchCapturingServer{seen: make(chan metadata.MD, 1)}
	server := grpc.NewServer()

	contract.RegisterHippocampusServer(server, capture)

	go func() { _ = server.Serve(listener) }()

	t.Cleanup(server.Stop)

	conn, client, err := Dial(Config{
		Address:       listener.Addr().String(),
		Token:         "tok",
		ClientVersion: "hippo/v0.53.5",
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	defer func() { _ = conn.Close() }()

	stream, err := client.Watch(context.Background(), &contract.WatchRequest{})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	_, _ = stream.Recv()

	md := <-capture.seen

	if got := md.Get("authorization"); len(got) != 1 || got[0] != "Bearer tok" {
		t.Errorf("the stream carried authorization %v, want [Bearer tok]", got)
	}

	if got := md.Get(contract.ClientVersionHeader); len(got) != 1 || got[0] != "hippo/v0.53.5" {
		t.Errorf("the stream carried version %v, want [hippo/v0.53.5]", got)
	}
}

// TestBearerStreamInterceptorFailsTheStreamWhenNoTokenCanBeObtained: the stream twin of
// TestBearerInterceptorFailsTheRPCWhenNoTokenCanBeObtained.
func TestBearerStreamInterceptorFailsTheStreamWhenNoTokenCanBeObtained(t *testing.T) {
	src, _ := newOIDCSource(OIDCConfig{Issuer: "http://127.0.0.1:1", ClientID: "id", ClientSecret: "s"})

	called := false

	streamer := func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		called = true

		return nil, nil
	}

	if _, err := bearerStreamInterceptor(src)(context.Background(), &grpc.StreamDesc{}, nil, "/svc/M", streamer); err == nil {
		t.Fatal("expected the stream to fail when no token could be obtained")
	}

	if called {
		t.Error("the stream was opened without a token")
	}
}

// TestBearerStreamInterceptorOmitsAnEmptyToken: as the unary interceptor does.
func TestBearerStreamInterceptorOmitsAnEmptyToken(t *testing.T) {
	var seen metadata.MD

	streamer := func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		seen, _ = metadata.FromOutgoingContext(ctx)

		return nil, nil
	}

	if _, err := bearerStreamInterceptor(staticSource{})(context.Background(), &grpc.StreamDesc{}, nil, "/svc/M", streamer); err != nil {
		t.Fatalf("interceptor: %s", err)
	}

	if got := seen.Get("authorization"); len(got) != 0 {
		t.Errorf("authorization = %v, want none", got)
	}
}
